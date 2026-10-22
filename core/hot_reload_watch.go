package core

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

// hotReloadWatcher 版本化二进制自动热重载 watch：周期扫描 + fsnotify 事件即时触发。
// 某插件目录内一旦出现「比当前运行二进制版本更高」的 <基名>-v<版本><ext> 文件，
// 即调用 Manager.HotReload 换成新进程。依赖主流程先 LoadFromConfig 完成首轮加载，
// 否则无插件可监测。仅在 m.config.EnableHotReload 时启用。
type hotReloadWatcher struct {
	m        *Manager
	watcher  *fsnotify.Watcher
	quit     chan struct{}
	done     chan struct{}
	interval time.Duration
	lastScan time.Time
	// failures 记录「插件名|二进制路径」的连续失败次数与下次可重试时间：
	// 坏版本不会每轮扫描都被反复拉起（指数退避，上限 5 分钟）。
	failures map[string]*reloadBackoff
}

// reloadBackoff 单个升级候选（插件名 + 版本化二进制路径）的连续失败状态。
type reloadBackoff struct {
	fails int
	next  time.Time
}

// hotReloadMaxBackoff 单次重试的最长等待时间。
const hotReloadMaxBackoff = 5 * time.Minute

// upgradeCandidate 收集到的待热重载目标（在未持锁时采集，再由外部调用 HotReload 加锁执行）。
type upgradeCandidate struct {
	name    string
	path    string
	version string
}

// StartHotReloadWatcher 启动版本化二进制自动热重载 watch（需已 LoadFromConfig）。
// m.config.EnableHotReload 未开启时直接返回 nil。
func (m *Manager) StartHotReloadWatcher() error {
	if m.config == nil || !m.config.EnableHotReload {
		return nil
	}
	w := &hotReloadWatcher{
		m:        m,
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
		interval: 5 * time.Second,
		failures: make(map[string]*reloadBackoff),
	}
	var err error
	w.watcher, err = fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	m.hotReloadWatcher = w
	// 启动时预埋已加载各插件目录，令「后续更高版本二进制」的事件即时可触发；
	// 而非依赖 5s 周期扫描兜底（新目录的首批文件因此获得事件即时性）。
	m.mu.RLock()
	for _, p := range m.loadedBinaries {
		if p == "" {
			continue
		}
		w.addWatchDir(PDir(p))
	}
	m.mu.RUnlock()
	go w.run()
	m.logger.Info("hot-reload watcher started")
	return nil
}

// addWatchDir 将目录加入 watch，忽略目录不存在等可自愈错误（该类目录在文件出现时
// 仍会经事件或周期扫描补齐）。
func (w *hotReloadWatcher) addWatchDir(dir string) {
	if err := w.watcher.Add(dir); err != nil && !os.IsNotExist(err) {
		w.m.logger.Warn("failed to add watcher for directory", "dir", dir, "error", err)
	}
}

// run 事件循环：fsnotify 事件与周期扫描都收敛到 rescan（事件去抖 + 周期兜底）。
func (w *hotReloadWatcher) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.quit:
			w.watcher.Close()
			return
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.m.logger.Warn("hot-reload watcher error", "error", err)
		case ev, ok := <-w.watcher.Events:
			if !ok {
				return
			}
			// 新文件一旦出现就主动加入 watch，确保后续更高版本继续触发；
			// 事件去抖交给 rescan 内部的节流处理。
			if ev.Op&(fsnotify.Create|fsnotify.Write) != 0 {
				w.addWatchDir(PDir(ev.Name))
			}
			w.rescan()
		case <-ticker.C:
			w.rescan()
		}
	}
}

// throttled 限制频繁扫描：两次扫描间隔不小于 500ms，避免编译写文件期间反复触发。
func (w *hotReloadWatcher) throttled() bool {
	now := time.Now()
	if now.Sub(w.lastScan) < 500*time.Millisecond {
		return false
	}
	w.lastScan = now
	return true
}

// rescan 扫描全部已加载插件目录，收集版本更高的目标并逐个热重载。
// 连续失败的候选按指数退避延后重试；成功或出现新版本时重置退避状态。
func (w *hotReloadWatcher) rescan() {
	if !w.throttled() {
		return
	}
	for _, u := range w.collectUpgrades() {
		if !w.shouldAttempt(u) {
			continue
		}
		w.m.logger.Info("hot-reload detected higher version binary", "name", u.name, "version", u.version, "path", u.path)
		if err := w.m.HotReload(u.name, u.path); err != nil {
			backoff, attempts := w.recordFailure(u)
			w.m.logger.Error("hot-reload failed; backing off",
				"name", u.name, "version", u.version, "attempts", attempts,
				"retry_in", backoff.String(), "error", err)
			continue
		}
		w.clearFailures(u.name)
		w.m.logger.Info("hot-reload applied", "name", u.name, "version", u.version)
	}
}

// shouldAttempt 判断该候选当前是否允许尝试热重载（处于退避窗口内则跳过）。
func (w *hotReloadWatcher) shouldAttempt(u upgradeCandidate) bool {
	b := w.failures[reloadBackoffKey(u)]
	return b == nil || !time.Now().Before(b.next)
}

// recordFailure 记录一次失败并返回本次退避时长与累计失败次数。
// 退避从 interval 起按 2 倍递增，上限 hotReloadMaxBackoff。
func (w *hotReloadWatcher) recordFailure(u upgradeCandidate) (time.Duration, int) {
	if w.failures == nil {
		w.failures = make(map[string]*reloadBackoff)
	}
	key := reloadBackoffKey(u)
	b := w.failures[key]
	if b == nil {
		b = &reloadBackoff{}
		w.failures[key] = b
	}
	b.fails++
	backoff := hotReloadMaxBackoff
	if b.fails <= 16 { // 防止移位溢出；5s<<16 已远超上限
		if d := w.interval << (b.fails - 1); d < hotReloadMaxBackoff {
			backoff = d
		}
	}
	b.next = time.Now().Add(backoff)
	return backoff, b.fails
}

// clearFailures 在插件热重载成功后清除该插件的全部退避记录（含旧版本路径）。
func (w *hotReloadWatcher) clearFailures(name string) {
	prefix := name + "|"
	for k := range w.failures {
		if strings.HasPrefix(k, prefix) {
			delete(w.failures, k)
		}
	}
}

// reloadBackoffKey 升级候选的退避键：插件名 + 版本化二进制路径（新版本天然获得新键）。
func reloadBackoffKey(u upgradeCandidate) string {
	return u.name + "|" + u.path
}

// collectUpgrades 在持读锁快照下，收集所有「目录内最高版本 > 当前运行版本」的待升级候选。
func (w *hotReloadWatcher) collectUpgrades() []upgradeCandidate {
	w.m.mu.RLock()
	defer w.m.mu.RUnlock()

	var out []upgradeCandidate
	for name, p := range w.m.loadedBinaries {
		if p == "" {
			continue
		}
		dir := PDir(p)
		base := filepath.Base(dir)
		best, v := versionedCandidatesInDir(dir, base)
		if best == "" {
			continue
		}
		cur := binaryVersion(p)
		if v.GreaterThan(cur) {
			out = append(out, upgradeCandidate{name: name, path: best, version: v.String()})
		}
	}
	return out
}

// stop 停止 watch goroutine，等待其退出（watcher 实例级，供 Manager.StopHotReloadWatcher 调用）。
func (w *hotReloadWatcher) stop() {
	if w == nil {
		return
	}
	close(w.quit)
	<-w.done
}

// recordLoadedBinaryLocked 记录插件当前运行的二进制路径（加载/热重载成功点调用）。需已持有 m.mu。
func (m *Manager) recordLoadedBinaryLocked(name, binaryPath string) {
	if binaryPath == "" {
		return
	}
	if m.loadedBinaries == nil {
		m.loadedBinaries = make(map[string]string)
	}
	m.loadedBinaries[name] = binaryPath
}

// StopHotReloadWatcher 停止版本化二进制自动热重载 watch（若已启动）。未启动时为空操作。
func (m *Manager) StopHotReloadWatcher() {
	if m.hotReloadWatcher != nil {
		m.hotReloadWatcher.stop()
		m.hotReloadWatcher = nil
	}
}
