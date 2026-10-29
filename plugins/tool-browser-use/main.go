package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"dsc-sdk"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	rodproto "github.com/go-rod/rod/lib/proto"
)

// ============================================================
// 瀏覽器會話管理
// ============================================================

// BrowserSession 瀏覽器會話
type BrowserSession struct {
	ID         string
	Browser    *rod.Browser
	Pages      map[string]*rod.Page
	ActivePage string
	CreatedAt  time.Time
	LastUsed   time.Time
	UserMode   bool   // 是否命中用户模式（复用默认 Chrome 登录态）
	Headless   bool   // 是否无头会话（无头时 UA 里会带 HeadlessChrome，建页时替换掉）
	Engine     string // 本次用的内核 + 版本 + 是否登录态（随结果告知模型）
	Switched   string // 中途换了浏览器/profile 的原因，非空时一并告知
	kill       func() // 结束浏览器进程（本地启动的会话才有；CDP 复用为 nil）
	mu         sync.Mutex
}

// BrowserSessionManager 瀏覽器會話管理器
type BrowserSessionManager struct {
	sessions map[string]*BrowserSession
	mu       sync.RWMutex
	closeMu  sync.Mutex // 用於避免重複關閉
}

var (
	globalBrowserSessionManager *BrowserSessionManager
	browserSessionOnce          sync.Once
)

// GetBrowserSessionManager 獲取全局瀏覽器會話管理器
func GetBrowserSessionManager() *BrowserSessionManager {
	browserSessionOnce.Do(func() {
		globalBrowserSessionManager = &BrowserSessionManager{
			sessions: make(map[string]*BrowserSession),
		}
		// 啟動空閒會話清理協程
		go globalBrowserSessionManager.cleanupIdleSessions()
	})
	return globalBrowserSessionManager
}

// getExeDir 獲取可執行文件所在目錄
func getExeDir() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	return dsc.PDir(exePath), nil
}

// cleanupOldBrowserData 清理24小時前的臨時瀏覽器數據目錄
func cleanupOldBrowserData(exeDir string) error {
	browserDataRoot := dsc.PJoin(exeDir, "temp", "browser-data")
	// 確保根目錄存在
	if err := dsc.MkdirAll(browserDataRoot); err != nil {
		return err
	}

	entries, err := os.ReadDir(browserDataRoot)
	if err != nil {
		return err
	}

	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		fullPath := dsc.PJoin(browserDataRoot, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// 檢查修改時間是否超過24小時
		if now.Sub(info.ModTime()) > 24*time.Hour {
			log.Printf("[BrowserSessionManager] removing old browser data directory: %s", fullPath)
			if err := os.RemoveAll(fullPath); err != nil {
				log.Printf("[BrowserSessionManager] error removing old browser data directory %s: %v", fullPath, err)
			}
		}
	}
	return nil
}

// browserCandidate 一个可用的浏览器及其默认（带登录态的）profile 目录。
//
// 为什么要成对出现：profile 必须和浏览器内核匹配——拿 Chrome 的 user-data-dir 去开
// Edge 会直接报错，反之亦然。所以候选是「二进制 + 它自己的 profile」而不是两个独立列表。
type browserCandidate struct {
	Name       string // Chrome / Edge / Chromium，用于告诉模型「这次用的是哪个内核」
	Bin        string
	ProfileDir string // 默认为空表示该浏览器没有可复用的 profile（例如自定义二进制）
}

// browserCandidates 按优先级列出现存的浏览器：显式指定的 DSC_BROWSER_BIN 最高，
// 其次系统 Chrome，再次 Edge。
//
// 顺序即降级顺序：Chrome 的 profile 被占用时就该去试 Edge 的 profile——多数人不会同时
// 开着两个浏览器，这一步往往就能拿到登录态，比直接退回隔离会话好得多。
func browserCandidates() []browserCandidate {
	var out []browserCandidate
	add := func(c browserCandidate) {
		if c.Bin == "" {
			return
		}
		if st, err := os.Stat(c.Bin); err != nil || st.IsDir() {
			return
		}
		for _, seen := range out {
			if seen.Bin == c.Bin {
				return
			}
		}
		out = append(out, c)
	}

	if p := strings.TrimSpace(os.Getenv("DSC_BROWSER_BIN")); p != "" {
		name, profile := classifyBrowser(p)
		add(browserCandidate{Name: name, Bin: p, ProfileDir: profile})
	}

	switch runtime.GOOS {
	case "windows":
		la := os.Getenv("LOCALAPPDATA")
		for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)"} {
			if base := os.Getenv(env); base != "" {
				add(browserCandidate{Name: "Chrome", Bin: dsc.PJoin(base, "Google", "Chrome", "Application", "chrome.exe"), ProfileDir: chromeProfileDir()})
			}
		}
		if la != "" {
			add(browserCandidate{Name: "Chrome", Bin: dsc.PJoin(la, "Google", "Chrome", "Application", "chrome.exe"), ProfileDir: chromeProfileDir()})
		}
		for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles"} {
			if base := os.Getenv(env); base != "" {
				add(browserCandidate{Name: "Edge", Bin: dsc.PJoin(base, "Microsoft", "Edge", "Application", "msedge.exe"), ProfileDir: edgeProfileDir()})
			}
		}
	case "darwin":
		add(browserCandidate{Name: "Chrome", Bin: "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome", ProfileDir: chromeProfileDir()})
		add(browserCandidate{Name: "Edge", Bin: "/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge", ProfileDir: edgeProfileDir()})
		add(browserCandidate{Name: "Chromium", Bin: "/Applications/Chromium.app/Contents/MacOS/Chromium"})
	default:
		for _, p := range []string{"/usr/bin/google-chrome", "/usr/bin/chromium", "/usr/bin/chromium-browser"} {
			add(browserCandidate{Name: "Chrome", Bin: p, ProfileDir: chromeProfileDir()})
		}
		add(browserCandidate{Name: "Edge", Bin: "/usr/bin/microsoft-edge", ProfileDir: edgeProfileDir()})
	}
	return out
}

// classifyBrowser 由可执行文件路径推断内核名与它的默认 profile 目录。
func classifyBrowser(bin string) (name, profileDir string) {
	lower := strings.ToLower(bin)
	switch {
	case strings.Contains(lower, "msedge") || strings.Contains(lower, "microsoft edge"):
		return "Edge", edgeProfileDir()
	case strings.Contains(lower, "chromium"):
		return "Chromium", ""
	default:
		return "Chrome", chromeProfileDir()
	}
}

// chromeProfileDir / edgeProfileDir 各浏览器默认 user-data-dir（含登录态）；不存在返回空。
func chromeProfileDir() string {
	switch runtime.GOOS {
	case "windows":
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return existingDir(dsc.PJoin(la, "Google", "Chrome", "User Data"))
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return existingDir(dsc.PJoin(home, "Library", "Application Support", "Google", "Chrome"))
		}
	default:
		if home, err := os.UserHomeDir(); err == nil {
			return existingDir(dsc.PJoin(home, ".config", "google-chrome"))
		}
	}
	return ""
}

func edgeProfileDir() string {
	switch runtime.GOOS {
	case "windows":
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return existingDir(dsc.PJoin(la, "Microsoft", "Edge", "User Data"))
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return existingDir(dsc.PJoin(home, "Library", "Application Support", "Microsoft Edge"))
		}
	default:
		if home, err := os.UserHomeDir(); err == nil {
			return existingDir(dsc.PJoin(home, ".config", "microsoft-edge"))
		}
	}
	return ""
}

// existingDir 目录存在则原样返回，否则空串（调用方据此跳过该候选的 user mode）。
func existingDir(p string) string {
	if p == "" {
		return ""
	}
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}

// browserBin 返回第一个现存的浏览器二进制（隔离会话用；不需要匹配 profile）。
func browserBin() string {
	if cs := browserCandidates(); len(cs) > 0 {
		return cs[0].Bin
	}
	return ""
}

// newBrowser 以关闭「默认设备模拟」的方式建控制器。
//
// rod.New() 默认会把每个页面模拟成 MacBook（devices.LaptopWithMDPIScreen）：UA 变成
// Macintosh + Chrome/114、视口固定 1440x900。在 Windows 上跑却报 Mac UA，是反爬最爱
// 抓的自相矛盾特征之一。NoDefaultDevice 关掉它，UA 与视口回到浏览器自己的真实值。
func newBrowser(controlURL string) *rod.Browser {
	return rod.New().NoDefaultDevice().ControlURL(controlURL).MustConnect()
}

// hardenLauncher 抹掉最容易被反爬识别的自动化痕迹。
//
// 两个开关分工，实测（Chrome 155，--headless=new）：
//
//	                             navigator.webdriver
//	A 只带 --enable-automation（rod 默认）      true
//	B 再加 disable-blink-features=…             false   ← 真正起作用的是这一条
//	C 只删 --enable-automation                  true    ← 删了也没用
//	D 两者都做（现状）                          false
//
// 为什么 C 仍是 true：`navigator.webdriver` 的实现挂在 Blink 的 AutomationControlled
// 运行时特性上，只要页面被 CDP 客户端接管（我们必然会接管）它就会被打开，与命令行
// 是否带 --enable-automation 无关。所以必须从引擎侧关掉这个特性（B/D）。
//
// 那为什么还要删 --enable-automation：它不止设 webdriver，还会在页面顶部挂一条
// 「Chrome 正受自动测试软件控制」的提示条、关掉翻译与「恢复上次页面」等交互，属于
// 明面上的自动化痕迹；删掉它没有副作用（同组实测里 focus/plugins/userActivation 都不变）。
//
// 无头模式另外改用 --headless=new：老的 --headless 会把 UA 写成 HeadlessChrome/xx。
func hardenLauncher(l *launcher.Launcher, headed, offscreen bool) *launcher.Launcher {
	l = l.Delete("enable-automation")
	l = l.Set("disable-blink-features", "AutomationControlled")
	l = l.Set("lang", "zh-CN")
	l = l.Set("window-size", "1280,900")
	if headed {
		if offscreen {
			// 有头但挪到可视区外：真浏览器，没有窗口挡在用户面前（任务栏仍会有图标）
			l = l.Set("window-position", "-32000,-32000")
		}
		return l.Headless(false)
	}
	return l.HeadlessNew(true)
}

// browserHeaded 决定本次启动有没有窗口（有头）。默认**无头**。
//
// 早先的直觉是「复用用户 profile 就该有头，否则容易被反爬」。实测（Chrome 155，
// --headless=new）否掉了这个直觉：无头与有头在常用指纹上完全一致——
//
//	plugins=5、WebGL 渲染器是真实显卡、window.chrome=object、
//	outer/inner 1280x900 / 1264x805、visibilityState=visible、webdriver=false。
//
// 差别只剩「有没有一个窗口」，所以默认无头：不弹窗。
//
// 真要更保险时有两条路：
//
//	DSC_BROWSER_OFFSCREEN=1  真·有头浏览器，但窗口挪到屏幕外（看不见，任务栏有图标）；
//	DSC_BROWSER_HEADED=1     看得见的有头窗口（排查问题时用）。
//
// 优先级：HEADLESS > HEADED > OFFSCREEN > 默认（无头）。
func browserHeaded() bool {
	if truthy(os.Getenv("DSC_BROWSER_HEADLESS")) {
		return false
	}
	if truthy(os.Getenv("DSC_BROWSER_HEADED")) {
		return true
	}
	return truthy(os.Getenv("DSC_BROWSER_OFFSCREEN"))
}

// browserOffscreen 有头时是否把窗口挪到屏幕外（DSC_BROWSER_HEADED=1 显式要窗口时不挪）。
func browserOffscreen() bool {
	if truthy(os.Getenv("DSC_BROWSER_HEADED")) {
		return false
	}
	return truthy(os.Getenv("DSC_BROWSER_OFFSCREEN"))
}

// truthy 宽松解析开关型环境变量：1/true/yes/on 都算开。
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// launchResult 一次成功启动的结果。
type launchResult struct {
	browser  *rod.Browser
	userMode bool   // 是否复用了某个浏览器的登录态
	headless bool   // 是否无头（决定建页时是否要改 UA）
	engine   string // 例 "Chrome 155（用户登录态）"，随结果告知模型
	kill     func() // 结束浏览器进程
	switched string // 非空：中途换了浏览器或 profile，附原因
}

// tryLaunch 用指定浏览器与 profile 启动并连接。
// 返回 product（如 "Chrome/155.0.0.0"，取自 /json/version）用于拼 engine 标签。
func tryLaunch(cand browserCandidate, profileDir string, headed, offscreen bool) (b *rod.Browser, kill func(), product string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			ok = false
			b = nil
			kill = nil
		}
	}()
	l := launcher.New()
	if profileDir != "" {
		l = l.UserDataDir(profileDir)
	}
	if cand.Bin != "" {
		l = l.Bin(cand.Bin)
	}
	l = hardenLauncher(l, headed, offscreen)

	controlURL, prod, stop, err := launchChrome(l)
	if err != nil {
		log.Printf("[BrowserSessionManager] 启动 %s 失败: %v%s", cand.Name, err, chromeRunningHint(err))
		return nil, nil, "", false
	}
	return newBrowser(controlURL), stop, prod, true
}

// 测试缝：把「怎么找候选」和「怎么起浏览器」做成可替换的变量，好单测降级链
// （真实实现分别是 browserCandidates / tryLaunch）。
var (
	browserCandidatesFn = browserCandidates
	tryLaunchFn         = tryLaunch
)

// launchBrowserRod 逐个候选浏览器尝试复用登录态，全部失败才退回隔离 profile。
//
// 降级顺序（每一档都先试当前模式、起不来再试无头）：
//  1. Chrome 的登录 profile；
//  2. Edge 的登录 profile——多数人不会同时开着两个浏览器，Chrome 被占用时这一步
//     往往就能拿到登录态，比直接退回隔离会话好得多；
//  3. 隔离 profile（无登录态）。
func launchBrowserRod(sessionID string, userMode bool) (*launchResult, error) {
	// 外部 CDP 端点（用户自己起的 Chrome/远端浏览器）：不启动、不干预，UA/指纹由那边
	// 决定。**必须放在候选浏览器检查之前**：这条路径根本不启动本地浏览器，本机没有
	// Chrome/Edge 也应可用（否则「指定了端点却报找不到浏览器」，mock 集成测试正是撞上
	// 这一点——它不依赖本机安装任何浏览器）。
	if u := strings.TrimSpace(os.Getenv("DSC_BROWSER_CDP_URL")); u != "" {
		log.Printf("[BrowserSessionManager] session %s 连接外部 CDP：%s", sessionID, u)
		return &launchResult{
			browser:  newBrowser(u),
			userMode: false,
			headless: false, // 外部浏览器 UA 是它自己的，不要改
			engine:   "外部浏览器（DSC_BROWSER_CDP_URL）",
		}, nil
	}

	cands := browserCandidatesFn()
	if len(cands) == 0 {
		return nil, fmt.Errorf("找不到可用的浏览器（安装 Chrome/Edge，或用 DSC_BROWSER_BIN / DSC_BROWSER_CDP_URL 指定）")
	}
	headed := browserHeaded()
	offscreen := headed && browserOffscreen()

	if userMode {
		var tried []string
		for _, cand := range cands {
			if cand.ProfileDir == "" {
				continue // 没有可复用的 profile（例如自定义二进制），只在最后作隔离用途
			}
			if b, kill, prod, ok := tryLaunchFn(cand, cand.ProfileDir, headed, offscreen); ok {
				log.Printf("[BrowserSessionManager] user-mode session %s using %s profile %s (headed=%v)", sessionID, cand.Name, cand.ProfileDir, headed)
				res := &launchResult{
					browser:  b,
					userMode: true,
					headless: !headed,
					engine:   engineLabel(cand.Name, prod, true),
					kill:     kill,
				}
				if len(tried) > 0 {
					res.switched = fmt.Sprintf("%s 的登录态被占用，已改用 %s 的登录态", strings.Join(tried, "、"), cand.Name)
					log.Printf("[BrowserSessionManager] %s", res.switched)
				}
				return res, nil
			}
			if headed {
				// 有头起不来（无桌面会话等）再试一次无头，别急着换浏览器
				if b, kill, prod, ok := tryLaunchFn(cand, cand.ProfileDir, false, false); ok {
					log.Printf("[BrowserSessionManager] user-mode session %s using %s profile %s (headed launch failed, falling back to headless)", sessionID, cand.Name, cand.ProfileDir)
					res := &launchResult{
						browser:  b,
						userMode: true,
						headless: true,
						engine:   engineLabel(cand.Name, prod, true),
						kill:     kill,
					}
					if len(tried) > 0 {
						res.switched = fmt.Sprintf("%s 的登录态被占用，已改用 %s 的登录态", strings.Join(tried, "、"), cand.Name)
					}
					return res, nil
				}
			}
			tried = append(tried, cand.Name)
		}
		log.Printf("[BrowserSessionManager] 所有浏览器的登录 profile 都不可用（%s），session %s 退回隔离 profile", strings.Join(tried, "、"), sessionID)
	}

	// 隔离 profile：不需要匹配浏览器，用第一个可用的
	exeDir, err := getExeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable dir: %w", err)
	}
	browserDataDir := dsc.PJoin(exeDir, "temp", "browser-data", sessionID)
	if err := dsc.MkdirAll(browserDataDir); err != nil {
		return nil, fmt.Errorf("failed to create browser data directory: %w", err)
	}
	if err := cleanupOldBrowserData(exeDir); err != nil {
		log.Printf("[BrowserSessionManager] warning: failed to clean up old browser data: %v", err)
	}

	cand := cands[0]
	b, kill, prod, ok := tryLaunchFn(cand, browserDataDir, headed, offscreen)
	if !ok && headed {
		b, kill, prod, ok = tryLaunchFn(cand, browserDataDir, false, false)
	}
	if !ok {
		return nil, fmt.Errorf("failed to launch browser for session %s", sessionID)
	}
	res := &launchResult{
		browser:  b,
		userMode: false,
		headless: !headed,
		engine:   engineLabel(cand.Name, prod, false),
		kill:     kill,
	}
	if userMode {
		res.switched = "所有浏览器的登录 profile 都被占用或不可用，本次使用隔离会话（无登录态）"
	}
	return res, nil
}

// engineLabel 拼「哪个内核 + 什么版本 + 有没有登录态」，随每次结果告知模型。
func engineLabel(name, product string, userMode bool) string {
	ver := shortVersion(product)
	kind := "隔离会话"
	if userMode {
		kind = "用户登录态"
	}
	if ver == "" {
		return fmt.Sprintf("%s（%s）", name, kind)
	}
	return fmt.Sprintf("%s %s（%s）", name, ver, kind)
}

// shortVersion 从 "Chrome/155.0.0.0" 取主版本号 "155"。
func shortVersion(product string) string {
	if _, ver, ok := strings.Cut(product, "/"); ok {
		if major, _, ok := strings.Cut(ver, "."); ok {
			return major
		}
		return ver
	}
	return product
}

// CreateSession 創建新的瀏覽器會話
// 如果會話已存在，直接返回現有會話並更新 LastUsed
func (m *BrowserSessionManager) CreateSession(sessionID string, userMode bool) (*BrowserSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 如果會話已存在，直接返回（在 sess.mu 下更新 LastUsed）
	if sess, ok := m.sessions[sessionID]; ok {
		sess.mu.Lock()
		sess.LastUsed = time.Now()
		sess.mu.Unlock()
		return sess, nil
	}

	// 啟動瀏覽器
	res, err := launchBrowserRod(sessionID, userMode)
	if err != nil {
		return nil, fmt.Errorf("啟動瀏覽器失敗: %w", err)
	}

	now := time.Now()
	sess := &BrowserSession{
		ID:        sessionID,
		Browser:   res.browser,
		Pages:     make(map[string]*rod.Page),
		CreatedAt: now,
		LastUsed:  now,
		UserMode:  res.userMode,
		Headless:  res.headless,
		Engine:    res.engine,
		Switched:  res.switched,
		kill:      res.kill,
	}

	m.sessions[sessionID] = sess
	log.Printf("[BrowserSessionManager] Created session %s (userMode=%v, engine=%s)", sessionID, res.userMode, res.engine)
	return sess, nil
}

// GetSession 獲取會話（不創建）
func (m *BrowserSessionManager) GetSession(sessionID string) (*BrowserSession, bool) {
	m.mu.RLock()
	sess, ok := m.sessions[sessionID]
	m.mu.RUnlock()
	if ok {
		// 更新 LastUsed 必須在 sess.mu 下進行，避免與 cleanupIdleSessions 等讀取競爭
		sess.mu.Lock()
		sess.LastUsed = time.Now()
		sess.mu.Unlock()
	}
	return sess, ok
}

// CloseSession 關閉並移除指定的瀏覽器會話
// 這是防止資源泄漏的關鍵方法，調用方應在 GlobalSession 停止時調用此方法
func (m *BrowserSessionManager) CloseSession(sessionID string) error {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()

	m.mu.Lock()
	sess, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.sessions, sessionID)
	m.mu.Unlock()

	if sess.Browser != nil {
		log.Printf("[BrowserSessionManager] Closing browser session %s", sessionID)
		sess.Browser.Close()
	}
	if sess.kill != nil {
		sess.kill() // 断连不会带走进程，得自己收
		sess.kill = nil
	}
	return nil
}

// CloseAllSessions 關閉所有瀏覽器會話（用於程序退出時清理）
func (m *BrowserSessionManager) CloseAllSessions() {
	// 使用寫鎖拍快照並清空 map，防止 CloseAllSessions 和 CreateSession 之間的窗口泄漏
	m.mu.Lock()
	sessions := make([]*BrowserSession, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	m.sessions = make(map[string]*BrowserSession)
	m.mu.Unlock()

	for _, sess := range sessions {
		if sess.Browser != nil {
			sess.Browser.Close()
		}
	}
	log.Printf("[BrowserSessionManager] All browser sessions closed")
}

// cleanupIdleSessions 定期清理空閒超時的會話
// 空閒超時時間默認 30 分鐘
func (m *BrowserSessionManager) cleanupIdleSessions() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		m.cleanupIdleSessionsOnce()
	}
}

func (m *BrowserSessionManager) cleanupIdleSessionsOnce() {
	idleThreshold := 30 * time.Minute
	now := time.Now()
	toClose := make([]string, 0)

	m.mu.RLock()
	for id, sess := range m.sessions {
		sess.mu.Lock()
		lastUsed := sess.LastUsed
		sess.mu.Unlock()
		if now.Sub(lastUsed) > idleThreshold {
			toClose = append(toClose, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range toClose {
		if err := m.CloseSession(id); err != nil {
			log.Printf("[BrowserSessionManager] Failed to close idle session %s: %v", id, err)
		} else {
			log.Printf("[BrowserSessionManager] Closed idle session %s", id)
		}
	}
}

// CreatePage 在會話中創建新頁面
func (s *BrowserSession) CreatePage(pageID string, url string) (*rod.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 創建新頁面
	page, err := s.Browser.Page(rodproto.TargetCreateTarget{URL: url})
	if err != nil {
		return nil, fmt.Errorf("創建頁面失敗: %w", err)
	}
	// 无头会话的 UA 里带 "HeadlessChrome/xx"，等于自报「我是无头浏览器」，按 UA 拦爬的
	// 站点一眼就看出来。取页面自身 UA 去掉这个标记再回设——版本号与平台照旧是真的，
	// 不另编 UA。有头会话不用动。
	if s.Headless {
		if raw, err := page.Eval("() => navigator.userAgent"); err == nil && raw.Value.Str() != "" {
			ua := strings.Replace(raw.Value.Str(), "HeadlessChrome", "Chrome", 1)
			if err := page.SetUserAgent(&rodproto.NetworkSetUserAgentOverride{
				UserAgent:      ua,
				AcceptLanguage: "zh-CN,zh;q=0.9,en;q=0.8",
			}); err != nil {
				log.Printf("[BrowserSession] 設置 UA 失敗（忽略）: %v", err)
			}
		}
	}

	s.Pages[pageID] = page
	s.ActivePage = pageID
	s.LastUsed = time.Now()

	return page, nil
}

// GetPage 獲取頁面
func (s *BrowserSession) GetPage(pageID string) (*rod.Page, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	page, ok := s.Pages[pageID]
	if ok {
		s.LastUsed = time.Now()
	}
	return page, ok
}

// GetActivePage 獲取當前活動頁面
func (s *BrowserSession) GetActivePage() (*rod.Page, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ActivePage == "" {
		return nil, false
	}
	page, ok := s.Pages[s.ActivePage]
	s.LastUsed = time.Now()
	return page, ok
}

// SetActivePage 設置活動頁面
func (s *BrowserSession) SetActivePage(pageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.Pages[pageID]; !ok {
		return fmt.Errorf("頁面 %s 不存在", pageID)
	}
	s.ActivePage = pageID
	s.LastUsed = time.Now()
	return nil
}

// ClosePage 關閉頁面
func (s *BrowserSession) ClosePage(pageID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	page, ok := s.Pages[pageID]
	if !ok {
		return nil
	}

	if page != nil {
		page.Close()
	}
	delete(s.Pages, pageID)

	// 如果關閉的是活動頁面，切換到其他頁面
	if s.ActivePage == pageID {
		s.ActivePage = ""
		for id := range s.Pages {
			s.ActivePage = id
			break
		}
	}
	s.LastUsed = time.Now()
	return nil
}

// PageInfo 頁面信息
type PageInfo struct {
	ID     string `json:"id"`
	URL    string `json:"url"`
	Title  string `json:"title"`
	Active bool   `json:"active"`
}

// ListPages 列出所有頁面
func (s *BrowserSession) ListPages() []PageInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	var pages []PageInfo
	for id, page := range s.Pages {
		info, _ := page.Info()
		pi := PageInfo{
			ID:     id,
			URL:    info.URL,
			Title:  info.Title,
			Active: id == s.ActivePage,
		}
		pages = append(pages, pi)
	}
	return pages
}

// ============================================================
// 瀏覽器工具實現
// ============================================================

// getOrCreatePage 獲取或創建瀏覽器頁面（使用會話管理器）
// userMode 仅在首次创建会话（启动浏览器）时生效；会话已存在则忽略。
func getOrCreatePage(sessionID, pageID, url string, userMode bool) (*rod.Page, *BrowserSession, error) {
	mgr := GetBrowserSessionManager()
	sess, ok := mgr.GetSession(sessionID)
	if !ok {
		var err error
		sess, err = mgr.CreateSession(sessionID, userMode)
		if err != nil {
			return nil, nil, err
		}
	}

	page, ok := sess.GetPage(pageID)
	if !ok || page == nil {
		var err error
		page, err = sess.CreatePage(pageID, url)
		if err != nil {
			return nil, nil, err
		}
	} else if url != "" {
		if err := page.Navigate(url); err != nil {
			return nil, nil, err
		}
	}

	// 設置超時：不在此 cancel，交由 page.Context 內部管理。
	timeout := 30 // 默認 30 秒超時
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	_ = cancel // 保留變量避免編譯錯誤，但不在此取消
	page = page.Context(ctx)

	return page, sess, nil
}

// WebFetchResult 獲取網頁內容結果
type WebFetchResult struct {
	Success bool   `json:"success"`
	URL     string `json:"url"`
	Content string `json:"content,omitempty"`
	Error   string `json:"error,omitempty"`
	// Note 说明本次会话的「隐性前提」：最常见的是请求了 user_mode 却没能复用登录态
	// （默认 profile 被运行中的 Chrome 占用），此时拿到的是隔离会话，页面要登录属正常。
	// 不告诉模型的话，它会把「需要登录」误判成站点反爬或页面结构问题。
	Note string `json:"note,omitempty"`
	// Format 回显实际使用的取文形态（layout/text/html）。诊断「模型以为拿到版面、
	// 实际拿到 innerText」这类问题时，没有它就只能靠猜。
	Format string `json:"format,omitempty"`
	// Engine 本次用的内核与版本，如 "Chrome 155（用户登录态）"/"Edge 141（隔离会话）"
	Engine string `json:"engine,omitempty"`
}

// sessionNote 说明本次会话的隐性前提（换了浏览器 / 退回了隔离会话）。
//
// 这些话由启动阶段写进 sess.Switched：Chrome 的登录态被占用时我们改用了 Edge，
// 或者两个浏览器的 profile 都不可用、退回隔离会话。不告诉模型的话，它只会看到一张
// 登录页，然后把原因误判成站点反爬或页面结构问题。
func sessionNote(sess *BrowserSession) string {
	if sess == nil {
		return ""
	}
	return sess.Switched
}

// sessionEngine 返回本次会话的内核标签（如 "Chrome 155（用户登录态）"）。
//
// 同一台机器可能因为 profile 占用而换过浏览器，所以每次工具结果都带上它，模型才知道
// 自己在跟哪个内核/哪份登录态打交道。
func sessionEngine(sessionID string) string {
	if sess, ok := GetBrowserSessionManager().GetSession(sessionID); ok && sess != nil {
		return sess.Engine
	}
	return ""
}

// fetchFormat 决定 web_fetch 返回哪种形态的内容。三种需求互斥，用一个枚举表达，
// 而不是再堆一个布尔（布尔只能表达两态，第三种就得再发明一个参数名）。
type fetchFormat string

const (
	// formatLayout 默认：保留页面的视觉行与列（表格/卡片/导航并排的内容不会被拆成
	// 一词一行），代价是列位补白会多出一些空格。
	formatLayout fetchFormat = "layout"
	// formatText 原始 innerText：紧凑、没有补白，但并排的块会被拆行。
	formatText fetchFormat = "text"
	// formatHTML 原始 HTML 标记：需要自行解析结构（选择器、属性、JSON-LD 等）时用。
	formatHTML fetchFormat = "html"
)

// parseFetchFormat 解析 format 参数：空值取默认，大小写与空白宽容，
// 兼容旧的布尔参数 return_html=true（等价于 html）。非法值报错并列出可选值——
// 静默回退会让模型以为自己拿到了 layout，实际是另一种。
func parseFetchFormat(format string, legacyReturnHTML bool) (fetchFormat, error) {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "":
		if legacyReturnHTML {
			return formatHTML, nil
		}
		return formatLayout, nil
	case string(formatLayout), "visual", "default":
		return formatLayout, nil
	case string(formatText), "plain", "innertext":
		return formatText, nil
	case string(formatHTML), "raw", "markup":
		return formatHTML, nil
	}
	return "", fmt.Errorf("invalid format %q: use layout (default), text, or html", format)
}

// composeNotes 把几条提示拼成一句（用「；」分隔），空提示自动略去。
func composeNotes(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "；")
}

// fallbackNote 说明「请求的是版面文本、实际拿到的是 innerText」——这种降级必须让模型
// 知道：它可能因此把并排的导航/表格当成一句话，或者反过来以为页面缺内容。
func fallbackNote(used fetchFormat) string {
	if !strings.HasPrefix(string(used), string(formatText)+"(") {
		return ""
	}
	return "版面还原未成功（页面结构异常或脚本未就绪），本次返回的是原始 innerText：并排的导航/表格会被拆成多行；" +
		"如需版面可稍后重试 format=layout"
}

// webFetchImpl 抓取网页内容。format 决定返回形态（见 fetchFormat）：
//   - layout：按视觉版面还原的文本（默认，便于模型理解表格/卡片等并排结构）；
//   - text  ：原始 innerText（紧凑，丢失并排关系）；
//   - html  ：原始 HTML 标记。
//
// 结果用 json.Marshal 序列化，避免手拼字符串破坏转义。
func webFetchImpl(sessionID, url string, format fetchFormat, userMode bool) (string, error) {
	page, sess, err := getOrCreatePage(sessionID, "default", url, userMode)
	if err != nil {
		e := WebFetchResult{Success: false, URL: url, Error: err.Error()}
		b, _ := json.Marshal(e)
		return string(b), nil
	}

	// 等待页面加载
	if err := page.WaitLoad(); err != nil {
		log.Printf("頁面加載警告: %v", err)
	}

	var content string
	used := format
	switch format {
	case formatHTML:
		content = page.MustEval("() => document.documentElement.outerHTML").Str()
	case formatText:
		content = page.MustEval("() => document.body.innerText").Str()
	default: // formatLayout
		// 按**视觉版面**还原正文（见 layout.go）：innerText 是按 DOM 块结构断行的，
		// flex/grid 页面上并排的导航项、表格单元格会被拆成一词一行，模型读不出
		// 「谁和谁在同一行」。排版失败（页面异常/脚本被禁）才退回 innerText。
		content, _ = renderLayout(page.MustEval(layoutExtractJS).Str())
		if content == "" {
			// SPA 首屏还没渲染完时测量结果可能是空的：等一下再试一次，别急着降级
			time.Sleep(400 * time.Millisecond)
			content, _ = renderLayout(page.MustEval(layoutExtractJS).Str())
		}
		if content == "" {
			log.Printf("web_fetch: 版面还原失败，退回 innerText（url=%s）", url)
			content = page.MustEval("() => document.body.innerText").Str()
			used = fetchFormat("text(fallback)")
		}
	}

	e := WebFetchResult{
		Success: true,
		URL:     url,
		Content: content,
		Format:  string(used),
		Engine:  sessionEngine(sessionID),
		Note:    composeNotes(sessionNote(sess), fallbackNote(used)),
	}
	b, _ := json.Marshal(e)
	return string(b), nil
}

// WebSearchResult 聚合搜索结果
type WebSearchResult struct {
	Success bool               `json:"success"`
	Query   string             `json:"query"`
	Engine  string             `json:"engine,omitempty"`
	Results []SearchResultItem `json:"results"`
	Errors  []string           `json:"errors,omitempty"`
	Sources []string           `json:"sources,omitempty"`
}

type SearchResultItem struct {
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	Source      string `json:"source,omitempty"`
}

// searchEngine 单一搜索引擎的取值定义：怎样构造搜索 URL、等待哪个结果容器、
// 以及如何从 DOM 提取结果。
type searchEngine struct {
	name      string
	urlFmt    string
	waitSel   string
	extractJS string
}

// searchEngines 支持的多搜索引擎。各引擎的提取脚本独立（DOM 结构差异大），
// 单个引擎失败仅导致该引擎无结果，不影响整体聚合。
var searchEngines = map[string]searchEngine{
	"duckduckgo": {
		name:    "duckduckgo",
		urlFmt:  "https://html.duckduckgo.com/html/?q=%s",
		waitSel: ".result, .results",
		extractJS: `() => JSON.stringify(Array.from(document.querySelectorAll('.result')).map(r => {
                        const a = r.querySelector('a.result__a');
                        const s = r.querySelector('.result__snippet, .result__snippet_no_offset');
                        return { title: a ? a.innerText.trim() : '', url: a ? a.href : '', description: s ? s.innerText.trim() : '' };
                }).filter(r => r.url && r.url.startsWith('http')))`,
	},
	"google": {
		name:    "google",
		urlFmt:  "https://www.google.com/search?q=%s",
		waitSel: "a[href]",
		extractJS: `() => JSON.stringify(Array.from(document.querySelectorAll('a[href^="http"]')).map(r => {
                        const a = r.querySelector('h3') || r;
                        return { title: a ? a.innerText.trim() : '', url: r.href || '', description: '' };
                }).filter(r => r.url && r.url.startsWith('http')))`,
	},
	"baidu": {
		name:    "baidu",
		urlFmt:  "https://www.baidu.com/s?wd=%s",
		waitSel: "#content_left",
		extractJS: `() => JSON.stringify(Array.from(document.querySelectorAll('#content_left .result, #content_left .c-container')).map(r => {
                        const a = r.querySelector('h3 a');
                        const s = r.querySelector('.c-abstract, .content-right');
                        return { title: a ? a.innerText.trim() : '', url: a ? a.href : '', description: s ? s.innerText.trim() : '' };
                }).filter(r => r.url && r.url.startsWith('http')))`,
	},
	"so360": {
		name:    "so360",
		urlFmt:  "https://www.so.com/s?q=%s",
		waitSel: ".res-list",
		extractJS: `() => JSON.stringify(Array.from(document.querySelectorAll('.res-list')).map(r => {
                        const a = r.querySelector('h3 a');
                        const s = r.querySelector('.res-desc');
                        return { title: a ? a.innerText.trim() : '', url: a ? a.href : '', description: s ? s.innerText.trim() : '' };
                }).filter(r => r.url && r.url.startsWith('http')))`,
	},
}

// parseEngines 解析 engines 参数（逗号分隔），去重并跳过非法项；空或全部非法时
// 返回默认引擎集。
func parseEngines(engines string) []string {
	if strings.TrimSpace(engines) == "" {
		return []string{"google", "duckduckgo", "baidu", "so360"}
	}
	seen := map[string]bool{}
	var out []string
	for _, e := range strings.Split(engines, ",") {
		e = strings.TrimSpace(e)
		if _, ok := searchEngines[e]; ok && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return []string{"google", "duckduckgo", "baidu", "so360"}
	}
	return out
}

// extractSearchPageElements 从已就绪的搜索结果页提取结果项。
func extractSearchPageElements(page *rod.Page, spec searchEngine) ([]SearchResultItem, error) {
	linksJSON := page.MustEval(spec.extractJS).Str()
	return unmarshalSearchItems(linksJSON)
}

// unmarshalSearchItems 解析引擎提取脚本产出的结果 JSON 数组。
func unmarshalSearchItems(raw string) ([]SearchResultItem, error) {
	var items []SearchResultItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, err
	}
	return items, nil
}

// mergeEngineResults 把单个引擎的结果按 URL 去重并入调用方的 seen 集合，
// 并给每条结果标注来源引擎。返回新增（未重复）的结果与是否带来了新结果。
func mergeEngineResults(items []SearchResultItem, engine string, seen map[string]bool) (added []SearchResultItem, hasNew bool) {
	for i := range items {
		it := items[i]
		it.Source = engine
		if seen[it.URL] {
			continue
		}
		seen[it.URL] = true
		added = append(added, it)
		hasNew = true
	}
	return added, hasNew
}

// webSearchImpl 按指定引擎集搜索并聚合。逐个引擎访问其搜索结果页、提取结果，
// 按 URL 去重后统一返回；单引擎失败（DOM 变化 / 验证码 / 网络）仅记录错误，
// 不阻断其它引擎，满足"多源同时搜索再聚合"的诉求。
func webSearchImpl(sessionID, query string, engines []string, userMode bool) (string, error) {
	aggregated := make([]SearchResultItem, 0, 16)
	seen := make(map[string]bool)
	sources := make([]string, 0)
	var errors []string

	for _, name := range engines {
		spec, ok := searchEngines[name]
		if !ok {
			errors = append(errors, fmt.Sprintf("engine %q not supported", name))
			continue
		}
		searchURL := fmt.Sprintf(spec.urlFmt, url.QueryEscape(query))
		page, _, err := getOrCreatePage(sessionID, "default", searchURL, userMode)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if err := page.WaitLoad(); err != nil {
			log.Printf("頁面加載警告(%s): %v", name, err)
		}
		// 等待结果容器出现（最多 10 秒）
		waitCtx := page.Timeout(10 * time.Second)
		if _, err := waitCtx.Element(spec.waitSel); err != nil {
			errors = append(errors, fmt.Sprintf("%s: results not rendered", name))
			continue
		}
		items, err := extractSearchPageElements(page, spec)
		if err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", name, err))
			continue
		}
		added, hasNew := mergeEngineResults(items, name, seen)
		aggregated = append(aggregated, added...)
		if hasNew {
			sources = append(sources, name)
		}
	}

	res := WebSearchResult{
		Success: len(aggregated) > 0,
		Query:   query,
		Engine:  sessionEngine(sessionID),
		Results: aggregated,
		Errors:  errors,
		Sources: sources,
	}
	b, _ := json.Marshal(res)
	return string(b), nil
}

// BrowserClickResult 點擊結果
type BrowserClickResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	URL     string `json:"url,omitempty"`
	Engine  string `json:"engine,omitempty"`
}

func browserClickImpl(sessionID, url, selector string) (string, error) {
	page, _, err := getOrCreatePage(sessionID, "default", url, true)
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":"%s"}`, err.Error()), nil
	}

	element, err := page.Element(selector)
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":"未找到元素 '%s': %s"}`, selector, err.Error()), nil
	}

	if err := element.ScrollIntoView(); err != nil {
		log.Printf("滾動到元素失敗: %v", err)
	}

	element.MustClick()
	time.Sleep(500 * time.Millisecond)

	info, _ := page.Info()
	result := BrowserClickResult{
		Success: true,
		Message: fmt.Sprintf("成功點擊元素: %s", selector),
		URL:     info.URL,
		Engine:  sessionEngine(sessionID),
	}

	jsonData, _ := json.Marshal(result)
	return string(jsonData), nil
}

// BrowserTypeResult 輸入結果
type BrowserTypeResult struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Value   string `json:"value,omitempty"`
	Engine  string `json:"engine,omitempty"`
}

func browserTypeImpl(sessionID, url, selector, text string, submit bool) (string, error) {
	page, _, err := getOrCreatePage(sessionID, "default", url, true)
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":"%s"}`, err.Error()), nil
	}

	element, err := page.Element(selector)
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":"未找到輸入框 '%s': %s"}`, selector, err.Error()), nil
	}

	element.MustClick()
	element.SelectAllText()
	element.Input(text)

	if submit {
		page.Keyboard.Press(input.Enter)
		time.Sleep(500 * time.Millisecond)
	}

	result := BrowserTypeResult{
		Success: true,
		Message: fmt.Sprintf("成功輸入文本到: %s", selector),
		Value:   text,
		Engine:  sessionEngine(sessionID),
	}

	jsonData, _ := json.Marshal(result)
	return string(jsonData), nil
}

// BrowserScreenshotResult 截圖結果
type BrowserScreenshotResult struct {
	URL       string `json:"url"`
	Success   bool   `json:"success"`
	SavedFile string `json:"savedFile,omitempty"`
	FullPage  bool   `json:"fullPage"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Size      int64  `json:"size"`
}

func browserScreenshotImpl(sessionID, url string, fullPage bool) (string, error) {
	page, _, err := getOrCreatePage(sessionID, "default", url, true)
	if err != nil {
		return fmt.Sprintf(`{"success":false,"error":"%s"}`, err.Error()), nil
	}

	time.Sleep(1 * time.Second)
	width := page.MustEval("() => window.innerWidth").Int()
	height := page.MustEval("() => document.body.scrollHeight").Int()

	var screenshot []byte
	if fullPage {
		screenshot = page.MustScreenshotFullPage()
	} else {
		screenshot = page.MustScreenshot()
	}

	// 保存截圖到工作區目錄（沙箱限制寫入系統臨時目錄，如 %TEMP%，故存到 workspace 內）
	downloadDir := dsc.PJoin(dsc.WorkspaceRoot(), "screenshots")
	if err := dsc.MkdirAll(downloadDir); err != nil {
		return fmt.Sprintf(`{"success":false,"error":"創建下載目錄失敗: %s"}`, err.Error()), nil
	}

	timestamp := time.Now().Format("20060102_150405")
	fileName := fmt.Sprintf("screenshot_%s.png", timestamp)
	filePath := dsc.PJoin(downloadDir, fileName)

	if err := dsc.WriteFile(filePath, screenshot); err != nil {
		return fmt.Sprintf(`{"success":false,"error":"保存截圖失敗: %s"}`, err.Error()), nil
	}

	result := BrowserScreenshotResult{
		URL:       url,
		Success:   true,
		SavedFile: filePath,
		FullPage:  fullPage,
		Width:     width,
		Height:    height,
		Size:      int64(len(screenshot)),
	}

	jsonData, _ := json.Marshal(result)
	return string(jsonData), nil
}

// webFetchView 构造 web_fetch 结果的结构化视图（纯文本）。
func webFetchView(result string) (json.RawMessage, error) {
	var r WebFetchResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	body := r.Content
	if !r.Success {
		body = r.Error
	}
	if r.Note != "" {
		body = "⚠️ " + r.Note + "\n\n" + body
	}
	if r.Format != "" {
		body = "format: " + r.Format + "\n" + body
	}
	badge := &dsc.ViewBadge{Text: "ok", Tone: "green"}
	if !r.Success {
		badge = &dsc.ViewBadge{Text: "error", Tone: "red"}
	}
	return dsc.PlainView("Fetch", badge, body), nil
}

// webSearchView 构造 web_search 结果的结构化视图（表格）。
func webSearchView(result string) (json.RawMessage, error) {
	var r WebSearchResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	rows := make([]dsc.ViewRow, 0, len(r.Results))
	for _, it := range r.Results {
		rows = append(rows, dsc.ViewRow{
			"title":       it.Title,
			"url":         it.URL,
			"description": it.Description,
			"source":      it.Source,
		})
	}
	return dsc.TableView("Search", &dsc.ViewBadge{Text: fmt.Sprintf("%d result(s)", len(r.Results)), Tone: "teal"}, []dsc.ViewColumn{
		{Key: "title", Title: "title"},
		{Key: "url", Title: "url"},
		{Key: "description", Title: "description"},
		{Key: "source", Title: "source"},
	}, rows), nil
}

// browserClickView 构造 browser_click 结果的结构化视图（卡片）。
func browserClickView(result string) (json.RawMessage, error) {
	var r BrowserClickResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	badge := &dsc.ViewBadge{Text: "clicked", Tone: "green"}
	if !r.Success {
		badge = &dsc.ViewBadge{Text: "failed", Tone: "red"}
	}
	fields := []dsc.ViewField{{Key: "message", Value: r.Message}}
	if r.URL != "" {
		fields = append(fields, dsc.ViewField{Key: "url", Value: r.URL})
	}
	return dsc.CardView("Browser", badge, fields), nil
}

// browserTypeView 构造 browser_type 结果的结构化视图（卡片）。
func browserTypeView(result string) (json.RawMessage, error) {
	var r BrowserTypeResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	badge := &dsc.ViewBadge{Text: "typed", Tone: "green"}
	if !r.Success {
		badge = &dsc.ViewBadge{Text: "failed", Tone: "red"}
	}
	fields := []dsc.ViewField{{Key: "message", Value: r.Message}}
	if r.Value != "" {
		fields = append(fields, dsc.ViewField{Key: "value", Value: r.Value})
	}
	return dsc.CardView("Browser", badge, fields), nil
}

// browserScreenshotView 构造 browser_screenshot 结果的结构化视图（卡片）。
func browserScreenshotView(result string) (json.RawMessage, error) {
	var r BrowserScreenshotResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	badge := &dsc.ViewBadge{Text: "saved", Tone: "green"}
	if !r.Success {
		badge = &dsc.ViewBadge{Text: "failed", Tone: "red"}
	}
	fields := []dsc.ViewField{
		{Key: "url", Value: r.URL},
		{Key: "file", Value: r.SavedFile},
		{Key: "size", Value: fmt.Sprintf("%d × %d · %.0f KB", r.Width, r.Height, float64(r.Size)/1024)},
	}
	return dsc.CardView("Screenshot", badge, fields), nil
}

// ============================================================
// 以公共 SDK（dsc-sdk）声明式启动：SDK 自动提供 ToolService /
// PluginMetadata / PluginHookService 与 go-core 组装（重写自旧的
// ToolServiceServer/MetadataServer/ToolMetadataGRPCPlugin 样板）。
// ============================================================
func main() {
	// 定義 web_fetch 工具
	webFetchSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "session_id": {
                                "type": "string",
                                "description": "Browser session ID for persistent state"
                        },
                        "url": {
                                "type": "string",
                                "description": "URL to fetch"
                        },
                        "format": {
                                "type": "string",
                                "enum": ["layout", "text", "html"],
                                "description": "What to return. layout (default): text that keeps the page's visual rows and columns, so items placed side by side stay on one line (best for tables, cards, nav). text: raw innerText - compact, but blocks placed side by side are split into separate lines. html: raw HTML markup for parsing structure yourself."
                        },
                        "user_mode": {
                                "type": "boolean",
                                "description": "Whether to reuse the default Chrome profile (so logged-in sessions are available). Defaults to true. The browser runs headless with the profile, so no window is shown."
                        }
                },
                "required": ["url"]
        }`)
	webFetchHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var params struct {
			SessionID  string `json:"session_id"`
			URL        string `json:"url"`
			Format     string `json:"format"`
			ReturnHTML bool   `json:"return_html"` // 兼容旧参数：等价于 format=html
			UserMode   *bool  `json:"user_mode"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if params.URL == "" {
			return "", fmt.Errorf("url is required")
		}
		sessionID := params.SessionID
		if sessionID == "" {
			sessionID = "default-browser-session"
		}
		userMode := true // 默认用户模式
		if params.UserMode != nil {
			userMode = *params.UserMode
		}
		format, err := parseFetchFormat(params.Format, params.ReturnHTML)
		if err != nil {
			return "", err
		}
		return webFetchImpl(sessionID, params.URL, format, userMode)
	}

	// 定義 web_search 工具
	webSearchSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "session_id": {
                                "type": "string",
                                "description": "Browser session ID for persistent state"
                        },
                        "query": {
                                "type": "string",
                                "description": "Search query"
                        },
                        "engines": {
                                "type": "string",
                                "description": "Comma-separated search engines to aggregate, e.g. \"google,baidu,so360\". Supported: google, duckduckgo, baidu, so360. Defaults to all."
                        },
                        "user_mode": {
                                "type": "boolean",
                                "description": "Whether to reuse the default Chrome profile (so logged-in sessions are available). Defaults to true. The browser runs headless with the profile, so no window is shown."
                        }
                },
                "required": ["query"]
        }`)
	webSearchHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var params struct {
			SessionID string `json:"session_id"`
			Query     string `json:"query"`
			Engines   string `json:"engines"`
			UserMode  *bool  `json:"user_mode"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if strings.TrimSpace(params.Query) == "" {
			return "", fmt.Errorf("query is required")
		}
		sessionID := params.SessionID
		if sessionID == "" {
			sessionID = "default-browser-session"
		}
		userMode := true // 默认用户模式
		if params.UserMode != nil {
			userMode = *params.UserMode
		}
		return webSearchImpl(sessionID, params.Query, parseEngines(params.Engines), userMode)
	}

	// 定義 browser_click 工具
	browserClickSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "session_id": {
                                "type": "string",
                                "description": "Browser session ID for persistent state"
                        },
                        "url": {
                                "type": "string",
                                "description": "Current page URL"
                        },
                        "selector": {
                                "type": "string",
                                "description": "CSS selector of the element to click"
                        }
                },
                "required": ["url", "selector"]
        }`)
	browserClickHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var params struct {
			SessionID string `json:"session_id"`
			URL       string `json:"url"`
			Selector  string `json:"selector"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if params.URL == "" || params.Selector == "" {
			return "", fmt.Errorf("url and selector are required")
		}
		sessionID := params.SessionID
		if sessionID == "" {
			sessionID = "default-browser-session"
		}
		return browserClickImpl(sessionID, params.URL, params.Selector)
	}

	// 定義 browser_type 工具
	browserTypeSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "session_id": {
                                "type": "string",
                                "description": "Browser session ID for persistent state"
                        },
                        "url": {
                                "type": "string",
                                "description": "Current page URL"
                        },
                        "selector": {
                                "type": "string",
                                "description": "CSS selector of the input field"
                        },
                        "text": {
                                "type": "string",
                                "description": "Text to type"
                        },
                        "submit": {
                                "type": "boolean",
                                "description": "Whether to submit the form after typing"
                        }
                },
                "required": ["url", "selector", "text"]
        }`)
	browserTypeHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var params struct {
			SessionID string `json:"session_id"`
			URL       string `json:"url"`
			Selector  string `json:"selector"`
			Text      string `json:"text"`
			Submit    bool   `json:"submit"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if params.URL == "" || params.Selector == "" || params.Text == "" {
			return "", fmt.Errorf("url, selector, and text are required")
		}
		sessionID := params.SessionID
		if sessionID == "" {
			sessionID = "default-browser-session"
		}
		return browserTypeImpl(sessionID, params.URL, params.Selector, params.Text, params.Submit)
	}

	// 定義 browser_screenshot 工具
	browserScreenshotSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "session_id": {
                                "type": "string",
                                "description": "Browser session ID for persistent state"
                        },
                        "url": {
                                "type": "string",
                                "description": "Current page URL"
                        },
                        "full_page": {
                                "type": "boolean",
                                "description": "Whether to take a full page screenshot"
                        }
                },
                "required": ["url"]
        }`)
	browserScreenshotHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var params struct {
			SessionID string `json:"session_id"`
			URL       string `json:"url"`
			FullPage  bool   `json:"full_page"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if params.URL == "" {
			return "", fmt.Errorf("url is required")
		}
		sessionID := params.SessionID
		if sessionID == "" {
			sessionID = "default-browser-session"
		}
		return browserScreenshotImpl(sessionID, params.URL, params.FullPage)
	}

	sdk := dsc.New(dsc.Config{
		Name:    "browser-use",
		Version: "1.0.0",
		Type:    dsc.TypeTool,
		Provides: map[string]string{
			// 提供 "browser" 能力：含 web_fetch/web_search/browser_* 无头浏览器工具
			"browser": "true",
		},
	})
	sdk.Tool(dsc.Tool{Name: "web_fetch", Description: "Fetch a URL with a real browser (JavaScript rendered). Returns layout-preserved text by default; format=text for compact innerText, format=html for raw markup", Schema: webFetchSchema, Handler: webFetchHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return webFetchView(result)
		},
	})
	sdk.Tool(dsc.Tool{Name: "web_search", Description: "Perform a web search using a headless browser and extract search results", Schema: webSearchSchema, Handler: webSearchHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return webSearchView(result)
		},
	})
	sdk.Tool(dsc.Tool{Name: "browser_click", Description: "Click an element on the page by selector", Schema: browserClickSchema, Handler: browserClickHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return browserClickView(result)
		},
	})
	sdk.Tool(dsc.Tool{Name: "browser_type", Description: "Type text into an input field", Schema: browserTypeSchema, Handler: browserTypeHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return browserTypeView(result)
		},
	})
	sdk.Tool(dsc.Tool{Name: "browser_screenshot", Description: "Take a screenshot of the current page or full page", Schema: browserScreenshotSchema, Handler: browserScreenshotHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return browserScreenshotView(result)
		},
	})
	sdk.Serve()
}
