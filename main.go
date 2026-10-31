package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"dsc/core"
	"dsc/session"
	"dsc/tui"

	"github.com/hashicorp/go-hclog"
	"gopkg.in/yaml.v3"
)

// defaultContextWindow 未配置且探测失败时使用的默认上下文窗口大小：128K（按 1024 计）
const defaultContextWindow = 128 * 1024

// envGroupPreferDSC 判断某 provider 的一组 LLM env 键（API_KEY/BASE_URL/MODEL）
// 是否任一以 DSC_ 前缀存在；存在则整组采用 DSC_ 前缀（与 sdk.LoadLLMConfig 一致），
// 禁止 DSC_ 与非 DSC_ 变量混用。三者必须作为一个整体决定前缀。
func envGroupPreferDSC(env map[string]string, prefix string) bool {
	for _, suf := range []string{"API_KEY", "BASE_URL", "MODEL"} {
		// 键名必须与 groupEnv 同构：DSC_<PROVIDER>_<SUFFIX>。此前漏了下划线
		//（拼出 DSC_OPENAIAPI_KEY），函数恒为 false——DSC_ 前缀的配置永远读不到，
		// 一律回退旧名（模型名解析因此常年落到兜底值）。
		if v, ok := env["DSC_"+prefix+"_"+suf]; ok && v != "" {
			return true
		}
	}
	return false
}

// groupEnv 按既定前缀（useDSC）从 env 映射取某 provider 的 suffix 键。
func groupEnv(env map[string]string, prefix string, useDSC bool, suffix string) string {
	name := prefix + "_" + suffix
	if useDSC {
		name = "DSC_" + name
	}
	return env[name]
}

// osEnvGroupPreferDSC 同 envGroupPreferDSC，但针对进程环境变量。
func osEnvGroupPreferDSC(prefix string) bool {
	for _, suf := range []string{"API_KEY", "BASE_URL", "MODEL"} {
		if os.Getenv("DSC_"+prefix+"_"+suf) != "" {
			return true
		}
	}
	return false
}

// osGroupEnv 按既定前缀（useDSC）从进程环境取某 provider 的 suffix 键。
func osGroupEnv(prefix string, useDSC bool, suffix string) string {
	name := prefix + "_" + suffix
	if useDSC {
		name = "DSC_" + name
	}
	return strings.TrimSpace(os.Getenv(name))
}

// contextProbePaths 按探测顺序列出会尝试的 URL（相对 base_url），仅供诊断/测试。
//
// 三种形态都要覆盖（实测都遇过）：
//   - llama.cpp / Strata 一族：`/props` 的 `default_generation_settings.n_ctx` 是**运行时**
//     容量，最权威（与 /v1/models 的 meta 冲突时以它为准）；部分版本 /v1/models 干脆不吐 meta；
//   - 标准 OpenAI 兼容：`/v1/models`，容量在 `data[].meta.n_ctx` / `max_model_len`；
//   - 不带 /v1 的网关/内网服务：`/models`（base 已经含 /v1 时 `/v1/models` 会 404）。
//
// 顺序按「权威性」排：props 先。宁可多拉一次几百 KB 的 /props，也不要因为 models 那份
// meta 缺失/陈旧而把整个进程按一半容量跑（现场：服务开 256K，DSC 报 128K）。
func contextProbePaths(baseURL string) []string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, p := range []string{"/props", "/v1/props", "/v1/models", "/models"} {
		u := base + p
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// probeContextWindow 探测服务端的上下文窗口容量，返回 0 表示「服务端没说」。
//
// 字段按「谁最可信」排：llama.cpp 家族给 `meta.n_ctx`（真实运行时的 n_ctx，最准），
// vLLM 给 `max_model_len`；网关/代理（LiteLLM、各类 OpenAI 兼容层）常见的写法是
// `context_length`、`max_input_tokens`、`context_window`。多认几种是因为这条探测是
// 「配置不得超过服务容量」这条规则的唯一信息来源——云端没有探测值时只能靠 4xx 事后教，
// 那要白烧一次请求。
//
// 多个 model 条目时取**最大**的非零值：网关常把别名/小模型一起列出来，取第一条
// （data[0]）会拿到错的容量。
func probeContextWindow(baseURL string) int {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if base == "" {
		return 0
	}
	// ① 运行时属性先：/props 给的是**当前调度设置**，优先于 /v1/models 里可能陈旧的 meta。
	for _, p := range []string{"/props", "/v1/props"} {
		if v := probeWindowAt(base + p); v > 0 {
			return v
		}
	}
	// ② 模型表兜底：同一份数据的两个入口（带不带 /v1），各自内部取最大。
	best := 0
	for _, p := range []string{"/v1/models", "/models"} {
		if v := probeWindowAt(base + p); v > best {
			best = v
		}
	}
	return best
}

// probeWindowAt 从单个 URL 读容量（0 = 这条路径没给出可用值）。
func probeWindowAt(u string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	// /props 的响应可能带完整 chat_template（实测 230KB），只解码需要的字段。
	var payload struct {
		// OpenAI 风格：/v1/models、/models
		Data []struct {
			Meta struct {
				Nctx int `json:"n_ctx"`
			} `json:"meta"`
			MaxModelLen    int `json:"max_model_len"`
			ContextLength  int `json:"context_length"`
			MaxInputTokens int `json:"max_input_tokens"`
			ContextWindow  int `json:"context_window"`
			Nctx           int `json:"n_ctx"`
		} `json:"data"`
		// llama.cpp 风格：/props
		Nctx                     int `json:"n_ctx"`
		DefaultGenerationSetting struct {
			Nctx int `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0
	}
	best := 0
	for _, v := range []int{
		payload.Nctx,
		payload.DefaultGenerationSetting.Nctx,
	} {
		if v > best {
			best = v
		}
	}
	for _, d := range payload.Data {
		for _, v := range []int{
			d.Meta.Nctx, d.Nctx, d.MaxModelLen, d.ContextLength, d.MaxInputTokens, d.ContextWindow,
		} {
			if v > best {
				best = v
			}
		}
	}
	return best
}

// probeContextWindowWithRetry 反复探测：本地服务在「模型加载中」「正在生成占满 slot」
// 时会回 503 或直接超时，一次失败就退回 128K 等于让整个进程按一半容量工作——实测过
// 用户的服务开着 256K，DSC 却报 128K（他自己从界面上看出不对）。
//
// 只对本地/内网端点重试：云端探测失败基本就是「没有这项信息」，白等几秒没有意义。
func probeContextWindowWithRetry(baseURL string) (window int, attempts int) {
	if w := probeContextWindow(baseURL); w > 0 {
		return w, 1
	}
	if !looksLikeLocalEndpoint(baseURL) {
		return 0, 1
	}
	attempts = 1
	for _, delay := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second} {
		time.Sleep(delay)
		attempts++
		if w := probeContextWindow(baseURL); w > 0 {
			return w, attempts
		}
	}
	return 0, attempts
}

// looksLikeLocalEndpoint 判断 base_url 是否指向本机/内网（决定探测失败要不要重试）。
// 解析失败时按「本地」处理：宁可多等两秒，也不要因为一个怪 URL 少算一半容量。
func looksLikeLocalEndpoint(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return true
	}
	host := u.Hostname()
	if host == "localhost" || strings.HasSuffix(host, ".local") {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false // 域名：可能是云端，不重试
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// endpointForLog 只保留 scheme://host，供日志展示：base_url 里可能带 token/query，
// 原样打出来会把凭据写进日志。
func endpointForLog(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return "(unparsable base_url)"
	}
	return u.Scheme + "://" + u.Host
}

// resolveContextWindow 决定实际使用的上下文窗口，返回（窗口, 来源, 是否被服务端钳制）。
//
// **服务端能提供的容量是上限，配置只能往下调**：
//   - 配置写大了（云端常被宣传成 1M，或用户图省事在 128K 后面补个 0）时，按服务端实测
//     容量截断——否则每轮都要靠 provider 的 4xx 教一遍，白白烧掉一次请求与一段重试；
//   - 配置写小了（用 32K 省成本）时保留配置值：那是用户的刻意选择，不该被抬高；
//   - 探测不到（云端一律如此）时配置优先，再退回默认 128K。
//
// 三个来源的优先级因此是「配置与实测取小，再退默认」，而不是早先的「配置优先、实测只填空」。
func resolveContextWindow(configured, probed, def int) (window int, source string, clamped bool) {
	switch {
	case configured > 0:
		window, source = configured, "config"
	case probed > 0:
		window, source = probed, "probe"
	default:
		window, source = def, "default"
	}
	if probed > 0 && window > probed {
		window, source, clamped = probed, "probe-clamped", true
	}
	return window, source, clamped
}

func loadConfig(path string) (*core.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg core.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// getExecutableDir 獲取可執行文件所在目錄的絕對路徑
// resolveWorkspaceRoot 解析統一 workspace 根：
// 僅當配置提供絕對路徑時以其覆蓋（用戶顯式指定工作區）；
// 否則以显式设置的 DSC_WORKSPACE_ROOT 环境变量为根（插件侧 builtin_tools 的
// 回退读取同一变量，宿主与插件对「统一根」来源一致）；
// 再否則默認以啟動目錄 cwd 為根——在哪个目录启动 dsc，就以哪个目录为工作区
// （对齐 REX/Claude Code 的「以启动目录为工作区」直觉）。相對路徑配置不再
// 参与決定根（避免 ./workspace 把根推到子目錄）。
//
// 返回值一律正斜杆（POSIX 语义）：workspace 根是「路径传播的出入口」——它会被注入
// 各插件进程、拼进工具结果与沙箱提示、进而进入模型上下文；Windows 上 os.Getwd()
// 给出的是反斜杆路径，若原样透传，模型会照抄反斜杆去写路径，跨平台行为即漂移。
func resolveWorkspaceRoot(cwd, cfgRoot string) string {
	if filepath.IsAbs(cfgRoot) {
		return core.PClean(cfgRoot)
	}
	if env := os.Getenv("DSC_WORKSPACE_ROOT"); env != "" {
		return core.PClean(env)
	}
	if cwd == "" {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		}
	}
	return core.PClean(cwd)
}

// sandboxPolicyEnv 返回注入各插件进程的沙箱策略档名（DSC_SANDBOX_POLICY）：
// 与 Manager 读取 DSC_SANDBOX 的缺省解析一致，未配置时回退 workspace-write。
func sandboxPolicyEnv() string {
	switch core.ParseSandboxPolicy(os.Getenv("DSC_SANDBOX")) {
	case core.SandboxReadOnly:
		return "read-only"
	case core.SandboxFullAccess:
		return "full-access"
	default:
		return "workspace-write"
	}
}

func getExecutableDir() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	return core.PDir(exePath), nil
}

// statBinaryPath 根據 execDir 与可能的相對/絕對 binaryPath，返回用於 os.Stat 檢查的絕對路徑
func statBinaryPath(execDir, cfgPath string, defaultRel string) string {
	p := cfgPath
	if p == "" {
		p = core.PJoin(execDir, defaultRel)
	} else {
		if !filepath.IsAbs(p) {
			p = core.PJoin(execDir, p)
		}
	}
	return p
}

// loadBinaryPath 返回用於傳遞給插件管理器的路徑（通常是配置文件中的相對路徑或默認相對路徑）
func loadBinaryPath(cfgPath string, defaultRel string) string {
	if cfgPath != "" {
		return cfgPath
	}
	return defaultRel
}

// runOneTurn 以与 TUI 内部一致的 RunStream 方式运行一组输入（不渲染 TUI），
// 将流式帧直接输出到 stdout，完成后返回退出码（0=成功，1=失败）。
// 输入中的 @文件引用 与 TUI 一致解析为附件（图片→dsc-img 多模态，文本→dsc-txt 注入）。
func runOneTurn(agent core.Agent, ctx context.Context, input string) int {
	ch, err := agent.RunStream(ctx, input, tui.ResolveFileRefs(input))
	if err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		return 1
	}
	exitCode := 0
	for frame := range ch {
		switch frame.Status {
		case "streaming":
			// 模型文本增量，直接输出
			fmt.Print(frame.Output)
		case "tool":
			// 工具调用提示（如 [调用工具: shell]），原样输出
			fmt.Print(frame.Output)
		case "success":
			// 一轮完成；usage 仅提示到 stderr，不污染 stdout 结果
			if frame.Usage != nil && frame.Usage.TotalTokens > 0 {
				fmt.Fprintf(os.Stderr, "\n[已用 %d tokens]\n", frame.Usage.TotalTokens)
			}
		case "error":
			if frame.Error != "" {
				fmt.Fprintf(os.Stderr, "\n错误: %s\n", frame.Error)
			}
			exitCode = 1
		case "reasoning":
			fmt.Fprintf(os.Stderr, "\\n[REASONING]>%s", frame.Reasoning)
		}
	}
	return exitCode
}

// stdinIsRedirected 报告 stdin 是否为非终端（管道/文件重定向），
// 这是多轮 stdin 驱动的判定依据：只有重定向输入才触发多轮，避免终端手动单轮被阻塞。
func stdinIsRedirected() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice == 0
}

// runStdinLoop 从 stdin 逐行读取作为后续每一轮输入，逐轮调用 runOneTurn，
// 直到 EOF；会话事件溯源在 agent 内累积，故多轮天然共享上下文。
func runStdinLoop(agent core.Agent, ctx context.Context) int {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	exitCode := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fmt.Fprintf(os.Stderr, "\n>>> %s\n", line)
		if code := runOneTurn(agent, ctx, line); code != 0 {
			exitCode = code
		}
	}
	return exitCode
}

// runInputMode 以非 TUI 模式运行 agent（-input 启动时使用）：
// 首轮跑 -input 传入的文本；随后若 stdin 为重定向输入（管道/文件），
// 则继续逐行驱动多轮，直到 EOF。始终不进入 TUI 事件循环，故 ADMIN API / DEBUGGER
// 端点可在进程存活期间持续观察。
//
// singleShot（-headless）下跑完首轮即退出：headless 的契约是「单发、不留后台」，
// 而 stdin 多轮循环会**无限等下一个输入**——CI/管道里 stdin 常常是一个开着不关的
// 句柄（并非 EOF），于是任务早就跑完了，进程却永远不退出。这类挂起没有任何输出，
// 看起来和「模型卡住」一模一样。
func runInputMode(agent core.Agent, ctx context.Context, input string, singleShot bool) int {
	if code := runOneTurn(agent, ctx, input); code != 0 {
		return code
	}
	if !shouldReadStdin(singleShot, stdinIsRedirected()) {
		// -headless 单发，或终端手动单轮：跑完 -input 即退出
		return 0
	}
	return runStdinLoop(agent, ctx)
}

// shouldReadStdin 决定 -input 首轮之后是否继续按行读 stdin 驱动多轮。
//
// -headless 是「单发、不留后台」契约（见 help），永远不读——headless 场景的 stdin
// 常常是一个开着不关的管道句柄（不是 EOF），读下去就永远等不到输入，任务早已跑完
// 而进程不退出，看上去和模型卡死一模一样。其余情况仍按 stdin 是否被重定向判断。
func shouldReadStdin(singleShot, stdinRedirected bool) bool {
	return !singleShot && stdinRedirected
}

// ptcEnvEnabled 是否由环境变量 DSC_PTC 显式开启 PTC 呈现（判定与 agent 侧 ptcEnabled 一致）。
func ptcEnvEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSC_PTC"))) {
	case "1", "true", "on", "ptc", "yes":
		return true
	}
	return false
}

// historyInjectionCount 返回应下发给 agent 的 DSC_HISTORY_INJECTION 值。
//
// config 与 agent 共用 DSC 的三值语义，原值透传、不翻译：负值=不注入恢复历史、
// 0=未定义（按默认 = 不限制）、N>0=只注入恢复历史中最近 N 条。0 也照常下发——
// 两边对 0 的理解一致，没有需要转换的地方。
func historyInjectionCount(cfg *core.Config) (int, bool) {
	if cfg == nil {
		return 0, false
	}
	return cfg.HistoryInjection, true
}

func main() {
	// 捕获 panic：打印完整堆栈而非仅值，并以非零码退出——否则 recover 后 main
	// 落到 os.Exit(exitCode) 时 exitCode 仍为默认 0，CI 会把崩溃误判为成功。
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "panic: %v\n%s\n", r, debug.Stack())
			os.Exit(1)
		}
	}()

	// dsc help / -h / --help：打印用法后直接退出（不加载插件、不进 TUI）。
	// 必须放在 version/setup 之前——两者都会扫位置参数，`dsc help` 不该被它们先吃掉。
	if isHelpCommand(os.Args[1:]) {
		printHelp()
		os.Exit(0)
	}

	// 未知旗标/位置参数 fail-loud：此前一律被静默忽略，`dsc -h`、`dsc hepl` 这类
	// 拼写错误会直接启动 TUI，用户只看到「命令没按预期做」，比报错难查得多。
	if err := checkArgs(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	// dsc version：输出版本信息后直接退出（不加载插件、不进 TUI）。
	if isVersionCommand(os.Args[1:]) {
		printVersion()
		os.Exit(0)
	}

	// 獲取可執行文件所在目錄的絕對路徑
	execDir, err := getExecutableDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get executable directory: %v\n", err)
		os.Exit(1)
	}

	// 图像附件库目录（对齐 sessions/、memory/ 等可执行目录旧例）：未显式配置
	// DSC_ATTACHMENT_DIR 时注入 <ExecDir>/attachments，宿主与各插件进程
	//（buildEnv 继承宿主环境）路径天然一致。
	if os.Getenv("DSC_ATTACHMENT_DIR") == "" {
		os.Setenv("DSC_ATTACHMENT_DIR", core.PJoin(execDir, "attachments"))
	}

	// 临时目录（操作截图等 24 小时生命周期的工具产物，宿主启动时对 temp/
	// 内超时子目录统一清理，见 core/manager.go cleanupOldTempDirs）：未显式
	// 配置 DSC_TEMP_DIR 时注入 <ExecDir>/temp，插件进程继承后引用解析路径一致。
	//
	// 目录必须在这里就建好：模型习惯写 `cd /tmp && …`，而虚拟根表把 /tmp 映射到
	// 这个目录——它不存在时 cd 直接失败，`&&` 后面的命令一条都不跑，输出只剩一句
	// 找不到目录，看起来就像「这个环境不支持我的写法」。
	if os.Getenv("DSC_TEMP_DIR") == "" {
		os.Setenv("DSC_TEMP_DIR", core.PJoin(execDir, "temp"))
	}
	if dir := os.Getenv("DSC_TEMP_DIR"); dir != "" {
		_ = os.MkdirAll(dir, 0o755) // fail-soft：建不出来也不阻断启动
	}

	// 会话目录（agent 写会话日志、压缩后端插件写自己的状态文件）：
	// 未显式配置 DSC_SESSION_DIR 时注入 <ExecDir>/sessions，路径唯一来源是
	// session.Dir——宿主 Store、插件经 dsc.SessionDir() 读同一值，不得各自拼。
	if os.Getenv("DSC_SESSION_DIR") == "" {
		os.Setenv("DSC_SESSION_DIR", session.Dir(execDir))
	}

	// 记忆库目录（记忆库插件 写入 memory.db）：未显式配置
	// DSC_MEMORY_DIR 时注入 <ExecDir>/memory，插件经 dsc.MemoryDir() 读取同一值。
	if os.Getenv("DSC_MEMORY_DIR") == "" {
		os.Setenv("DSC_MEMORY_DIR", core.PJoin(execDir, core.MemoryDirName))
	}

	// 技能目录（插件的技能工具与 TUI 的 listSkills 都需引用）：
	// 未显式配置 DSC_SKILLS_DIR 时注入 <ExecDir>/skills，避免插件 fallback
	// "./skills"（CWD 相对）与 TUI 的 <exeDir>/skills 不一致。
	if os.Getenv("DSC_SKILLS_DIR") == "" {
		os.Setenv("DSC_SKILLS_DIR", core.PJoin(execDir, "skills"))
	}

	// dsc setup：交互式配置向导（不加载插件，直接读写 config.yaml）。
	// 检测规则：第一个非 flag 参数（- 开头之外）为 "setup" 时进入向导。
	if isSetupCommand(os.Args[1:]) {
		os.Exit(runSetup(bufio.NewScanner(os.Stdin), os.Stdout,
			core.ConfigPath,
			core.PJoin(execDir, "plugins")))
	}

	// 啟動目錄（cwd）：默認 workspace 根。用戶在哪个目录启动 dsc，
	// 就以哪个目录为工作区（对齐 REX/Claude Code）；獲取失敗時退化為可執行目錄。
	cwd, err := os.Getwd()
	if err != nil {
		cwd = execDir
	}

	// 解析啟動參數
	logToScreen := false
	logToFile := ""
	mode := "standard"      // 默認標準模式
	inputText := ""         // -input：一次性提示文本（自動化測試入口，不經 TUI）
	debuggerOpen := false   // -debugger：開放 /debugger 觀察路由（默認關閉，避免暴露會話隱私）
	adminAddr := ""         // -admin：管理 API 監聽地址（預設取環境變量 DSC_ADMIN_ADDR，缺省 127.0.0.1:9999）
	headless := false       // -headless：精简无头模式，专为 CI 单发（不开 ADMIN/热重载/cron，任务来自 -input）
	withUserGrant := false  // -with-user-grant：把需审批的动作视作已获用户授权（仅 -input/-headless 有效）
	hooksPath := ""         // -hooks：外部脚本钩子配置（hooks.json：严格 LUA / 原生可执行）
	var patchFiles []string // -patch：配置 overlay 文件列表（可重复，按顺序合并）

	// DSC_LOG_LEVEL：宿主运行日志级别（debug|info|warn|error，默认 info）。
	// 仅在 -log 启用（文件或屏幕）时有意义；无 -log 时默认静默（io.Discard）设计
	// 不变——无日志需求时零噪音；开启后即获得 DSH 等价的分级全链路能力
	//（llm request / tool execution / workflow / 插件生命周期 / 会话操作）。
	logLevel := hclog.Info
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSC_LOG_LEVEL"))) {
	case "debug":
		logLevel = hclog.Debug
	case "warn":
		logLevel = hclog.Warn
	case "error":
		logLevel = hclog.Error
	}

	for i, arg := range os.Args {
		if arg == "-log" {
			if i+1 < len(os.Args) {
				nextArg := os.Args[i+1]
				if strings.HasPrefix(nextArg, "-") {
					logToScreen = true
				} else {
					logToFile = nextArg
				}
			} else {
				logToScreen = true
			}
		} else if arg == "-mode" {
			// -mode 后跟模式名（来自 config/presets/<name>.yaml）。
			// 不在此硬编码允许列表——SwitchMode 后续读 preset 文件时
			// 会自然校验，用户新增 preset（如 thin.yaml）无需改此处的允许列表。
			// 错误的 mode 名会让 SwitchMode 报"failed to read preset config"。
			if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
				mode = os.Args[i+1]
			}
		} else if arg == "-input" {
			// -input 後跟提示文本作為參數值（以 - 開頭的視為缺失，避免吞掉後續選項）
			if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
				inputText = os.Args[i+1]
			}
		} else if arg == "-headless" {
			// 精简无头模式：单发任务、不开 ADMIN 端口 / 热重载 / cron，专为 CI 脚本
			headless = true
		} else if arg == "-with-user-grant" {
			// 无人值守入口的显式授权：没有问询通道时，审批门 fail-closed 会把所有写
			// 操作拒掉（装插件、写工作区外文件……）。用户用这个开关一次性表态，
			// 等价于他在 TUI 里对每次询问都点了 Allow。只对 -input/-headless 有意义。
			withUserGrant = true
		} else if arg == "-debugger" {
			// 顯式開放 /debugger 觀察路由（含完整會話歷史，屬敏感信息，默認不開放）
			debuggerOpen = true
		} else if arg == "-admin" {
			if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
				adminAddr = os.Args[i+1]
			}
		} else if arg == "-hooks" {
			if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
				hooksPath = os.Args[i+1]
			}
		} else if arg == "-patch" {
			// -patch <file.yaml>：加载 patch overlay 文件（可重复指定，按顺序合并）。
			// 对齐 DSH --patch overlay 机制（Go 风格单 - 选项）。
			// patch 文件含插件条目配置（如 MCP 客户端的 serverName/transport/command），
			// 合并到 config.yaml 后启动时自动应用（如自动连接 MCP 服务器）。
			if i+1 < len(os.Args) && !strings.HasPrefix(os.Args[i+1], "-") {
				patchFiles = append(patchFiles, os.Args[i+1])
			}
		}
	}

	// 未知 flag 响亮失败：此前静默忽略并落入 TUI（`dsc --version` 这类拼写
	// 错误在管道/嵌套调用下会一直阻塞），现直接给用法并以 2 退出。
	if err := validateStartupArgs(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "dsc: %v\n\n%s", err, startupUsage)
		os.Exit(2)
	}

	// -headless 精简无头模式校验：任务必须来自 -input 且非空白（对齐 harness headless：
	// 缺失/空白任务为用法错误，任何东西都不执行并退出 1）
	if headless {
		if strings.TrimSpace(inputText) == "" {
			fmt.Fprintln(os.Stderr, "headless mode requires -input <task>")
			os.Exit(1)
		}
	}

	// 交互式 TUI 需要终端：无 -input 且 stdin/stdout 任一不是终端时尽早失败。
	// 放在加载配置/插件之前，避免「先起插件与 cron，再卡死在 TUI」——嵌套调用
	//（如在 agent 的 shell 工具里再启动 dsc）与管道重定向都会命中这里。
	if strings.TrimSpace(inputText) == "" && !stdioIsTerminal() {
		fmt.Fprintln(os.Stderr, "dsc: interactive TUI requires a terminal; use -input \"<task>\" for a one-shot run, or `dsc version`")
		os.Exit(2)
	}

	logger := hclog.New(&hclog.LoggerOptions{
		Name:   "dsc-host",
		Level:  logLevel,
		Output: os.Stderr,
	})

	// 初始化 logger 与 coreLogger (根據 logToFile 与 logToScreen 調整)
	var coreLogger hclog.Logger
	var logOutput io.Writer

	if logToFile != "" {
		// 日志静默写到设定的path去
		f, err := os.OpenFile(logToFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			// 如果打開文件失敗，回退到屏幕
			logOutput = os.Stderr
			logger = hclog.New(&hclog.LoggerOptions{
				Name:   "dsc-host",
				Level:  logLevel,
				Output: logOutput,
			})
			coreLogger = hclog.New(&hclog.LoggerOptions{
				Name:   "core",
				Level:  logLevel,
				Output: logOutput,
			})
		} else {
			logOutput = f
			logger = hclog.New(&hclog.LoggerOptions{
				Name:   "dsc-host",
				Level:  logLevel,
				Output: logOutput,
			})
			coreLogger = hclog.New(&hclog.LoggerOptions{
				Name:   "core",
				Level:  logLevel,
				Output: logOutput,
			})
			// 確保在退出時關閉文件
			defer f.Close()
		}
	} else if logToScreen {
		// -log 无指定路径时似如今一样打印日志到屏幕
		logOutput = os.Stderr
		logger = hclog.New(&hclog.LoggerOptions{
			Name:   "dsc-host",
			Level:  logLevel,
			Output: logOutput,
		})
		coreLogger = hclog.New(&hclog.LoggerOptions{
			Name:   "core",
			Level:  logLevel,
			Output: logOutput,
		})
	} else {
		// 無參數時，日誌静默放棄，不作記錄
		logOutput = io.Discard
		logger = hclog.New(&hclog.LoggerOptions{
			Name:   "dsc-host",
			Level:  hclog.NoLevel,
			Output: logOutput,
		})
		coreLogger = hclog.New(&hclog.LoggerOptions{
			Name:   "core",
			Level:  hclog.NoLevel,
			Output: logOutput,
		})
	}

	// 统一把宿主日志与插件日志的扇出接到 ADMIN /logs SSE：`logOutput` 已锁定
	// 最终目的地（os.Stderr / 日志文件 / io.Discard），以其构建一个 LogFanout，
	// 让 logger 与 coreLogger 同时写原始目的地并广播给 /logs 订阅者。这样即便
	// 默认静默模式（io.Discard），也能按需经 ADMIN API 观察运行时日志。
	logFanout := core.NewLogFanout(logOutput)
	logger = hclog.New(&hclog.LoggerOptions{
		Name:   "dsc-host",
		Level:  logLevel,
		Output: logFanout,
	})
	coreLogger = hclog.New(&hclog.LoggerOptions{
		Name:   "core",
		Level:  logLevel,
		Output: logFanout,
	})

	logger.Info("starting dsc", "mode", mode)

	// Windows 专属兜底：把宿主纳入带 KILL_ON_JOB_CLOSE 的 Job Object（其余平台
	// no-op）。插件及其孙进程自动继承该 job；宿主正常退出、panic、os.Exit 或被
	// 强杀时，内核在句柄回收后一并终止，覆盖 client.Kill()/GracefulKill 来不及
	// 执行的场景（任务管理器强杀、崩溃）。须在任何插件拉起之前调用。
	if err := core.EnableProcessJob(); err != nil {
		logger.Warn("process job object unavailable, relying on graceful plugin shutdown", "error", err)
	} else if core.ProcessJobActive() {
		logger.Debug("process job object enabled (kill-on-job-close)")
	}

	// 加載 preset 配置文件
	presetPath := core.PJoin(execDir, "config", "presets", fmt.Sprintf("%s.yaml", mode))
	presetCfg, err := loadConfig(presetPath)
	if err != nil {
		// 如果 preset 配置文件不存在或加載失敗，回退到默認 config.yaml
		logger.Info("preset config not found or invalid, using default config", "presetPath", presetPath, "error", err)
		presetCfg = nil
	}

	// 從主配置 config/config.yaml 讀取工作空間根。
	// workspace_root 為統一工作空間根（對齊 DSH ctx.sandboxPolicy 的單一根來源），
	// 同時供 sandbox 判定與注入給各工具插件進程。默認以啟動目錄（cwd）為根——
	// 在哪个目录启动 dsc，就以哪个目录为工作区（对齐 REX/Claude Code）；
	// 僅當配置顯式提供絕對路徑時覆蓋（相對路徑配置不再参与決定根）。
	// 沙箱策略（read-only / workspace-write / full-access）由 TUI /sandbox 命令
	// 运行时切换，见 core.Manager.SetSandboxPolicy。
	mainCfg, err := loadConfig(core.ConfigPath)
	if err == nil && mainCfg != nil {
		core.WorkspaceRoot = resolveWorkspaceRoot(cwd, mainCfg.WorkspaceRoot)
	}
	warnIfWorkspaceIsInstallDir(logger, core.WorkspaceRoot)

	// -patch overlay 合并：加载所有 patch 文件并合并到 mainCfg（对齐 DSH --patch）。
	// patch 文件含插件条目配置（如 MCP 客户端），合并后启动时自动应用。
	if len(patchFiles) > 0 {
		overlays, perr := core.LoadPatchFiles(patchFiles)
		if perr != nil {
			fmt.Fprintf(os.Stderr, "load patch overlay failed: %v\n", perr)
			os.Exit(1)
		}
		if len(overlays) > 0 {
			for _, ov := range overlays {
				logger.Info("applying patch overlay", "source", ov.Source, "entries", len(ov.Entries))
			}
			if mainCfg == nil {
				mainCfg = &core.Config{}
			}
			mainCfg = core.ApplyPatchOverlays(mainCfg, overlays)
		}
	}
	// 打印统一用正斜杆（ToSlash），与模型 system prompt 中注入的工作区路径格式一致，
	// 避免 Windows 下日志显示反斜杆路径造成人类与模型所见不一致。
	logger.Info("workspace root", "root", filepath.ToSlash(core.WorkspaceRoot))

	// /settings history 持久化的历史注入条数（config.yaml history_injection）在启动时
	// 原值下发 agent 子进程：负值=不注入恢复历史、0=未定义（默认不限制）、N=最近 N 条。
	// config 与 agent 共用同一套 DSC 三值语义，不做转换——与 DSH 的「-1 不限制 /
	// 0 不注入」刻意不同：Go 的零值是未设置，把 0 当关闭会让漏写配置意外关掉历史。
	if n, ok := historyInjectionCount(mainCfg); ok {
		os.Setenv("DSC_HISTORY_INJECTION", strconv.Itoa(n))
		logger.Info("history injection from config", "count", n)
	}

	// 放大 go-core GRPCBroker 的连接超时（库默认 5 秒，见 EnvConnTimeout）：
	// 宿主与插件进程同时加载超大本地模型、传输通道被挤占时，接收方收到 ConnInfo
	// 后可能未能及时 Dial，一旦超过一次性窗口即被丢弃且无法重连。统一经环境变量
	// 下发，所有插件子进程（buildEnv 继承宿主环境）同样生效；外部已显式设置时尊重之。
	if os.Getenv("PLUGIN_BROKER_CONN_TIMEOUT") == "" {
		os.Setenv("PLUGIN_BROKER_CONN_TIMEOUT", "5m")
	}
	logger.Info("core broker conn timeout", "timeout", os.Getenv("PLUGIN_BROKER_CONN_TIMEOUT"))

	mgr := core.NewManager(&core.ManagerConfig{
		PluginDir:       core.PJoin(execDir, "plugins"),
		ExecDir:         execDir,
		Handshake:       core.Handshake,
		Logger:          logger,
		PluginLogger:    coreLogger,
		LogFanout:       logFanout,
		DebuggerEnabled: debuggerOpen,
		EnableHotReload: mainCfg != nil && mainCfg.HotReload,
		// PTC 呈现：-mode=ptc 或显式 DSC_PTC 开启（与 agent 侧 ptcEnabled 判定一致）
		PTC: mode == core.ModePTC || ptcEnvEnabled(),
	})
	defer mgr.Shutdown()
	// 安装退出信号处理：直接关闭终端等终止信号也能先逐只 Kill 插件子进程再退出，
	// 避免 defers 不执行时孤儿插件进程残留（Windows console 关闭事件亦一并兜底）。
	installShutdownSignals(mgr)
	// 通知 Manager 动态注入/卸载要写回的 config.yaml 路径，
	// 使运行期增删的插件在进程重启后依旧保留
	mgr.SetConfigPath(core.ConfigPath)
	// -with-user-grant：只对无人值守入口生效。TUI 有自己的审批 UI，在那里打开等于
	// 悄悄关掉所有确认，比不加还危险——所以显式拒绝而不是静默忽略。
	if withUserGrant {
		if headless || strings.TrimSpace(inputText) != "" {
			mgr.SetUserGrantAll(true)
			logger.Warn("user grant enabled for every approval-gated action",
				"flag", "-with-user-grant",
				"effect", "tools declaring ApprovalRequester run without asking; sandbox escalation is approved automatically")
		} else {
			logger.Warn("-with-user-grant ignored: it only applies to -input / -headless",
				"hint", "在 TUI 里请直接回答审批提示，或对当前会话使用 /approval")
		}
	}

	// 「MCP 是否启用」不在这里：那是插件自己的状态（由插件写自己的状态文件）。
	// 宿主不认识任何具体协议，插件不报工具，目录成本自然为 0。

	// 外部脚本钩子（-hooks hooks.json）：严格 LUA 脚本（go-lua 进程内解释）或
	// 原生可执行文件（直接 exec 不经 shell），在工具流水线 BeforeTool/AfterTool
	// 参与裁定。加载失败不致命（配置错误即无钩子），日志留痕便于排查。
	if hooksPath != "" {
		if err := mgr.LoadLuaHooks(hooksPath); err != nil {
			logger.Warn("lua hooks load failed; continuing without hooks", "path", hooksPath, "error", err.Error())
		}
	}

	// fail 記錄錯誤後先清理已加載的插件子進程再退出，避免 os.Exit 跳過 defer 導致殘留孤兒進程
	fail := func(format string, args ...interface{}) {
		logger.Error(fmt.Sprintf(format, args...))
		mgr.Shutdown()
		os.Exit(1)
	}

	// ===== 声明式加载：合并 config.yaml + preset，交给 Manager 按能力依赖拓扑加载 =====
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}

	// 从 config.yaml 收集启用的 LLM 条目与 agent 条目（沿用其 binary_path/env 声明）
	var llmEntries []core.PluginEntry
	var agentEntry *core.PluginEntry
	if mainCfg != nil {
		for i := range mainCfg.Plugins {
			e := mainCfg.Plugins[i]
			if !e.Enabled {
				continue
			}
			switch e.Type {
			case "llm":
				llmEntries = append(llmEntries, e)
			case "agent":
				if agentEntry == nil {
					agentEntry = &mainCfg.Plugins[i]
				}
			}
		}
	}

	// 若 config.yaml 未声明任何启用的 LLM，则按 环境变量 → DefaultLLM → 发现机制回退构造默认条目
	if len(llmEntries) == 0 {
		llmName := os.Getenv("LLM_PROVIDER")
		if llmName == "" && mainCfg != nil {
			llmName = mainCfg.DefaultLLM
		}
		if llmName == "" {
			// 发现机制：扫描 plugins/llm-* 目录取首个（与 setup.go 一致），
			// 不再硬编码 "openai"（对齐 AGENTS.md §8）。
			llmName = discoverFirstLLMPlugin()
		}
		if llmName == "" {
			fail("no LLM provider configured: set default_llm in config.yaml, LLM_PROVIDER env, or install a plugin under plugins/llm-*/")
		}
		llmEntries = append(llmEntries, core.PluginEntry{
			Name:       llmName,
			Type:       "llm",
			Enabled:    true,
			BinaryPath: loadBinaryPath("", "./plugins/llm-"+llmName+"/llm-"+llmName+ext),
		})
	}

	// 确定“活跃 LLM”用于探测上下文窗口与展示模型名：LLM_PROVIDER → DefaultLLM → 首个启用条目
	// （agent 的 primary LLM 由宿主按能力依赖解析，main 不再直接读 agent.depends_on.llm）
	activeLLMName := ""
	if v := os.Getenv("LLM_PROVIDER"); v != "" {
		activeLLMName = v
	} else if mainCfg != nil && mainCfg.DefaultLLM != "" {
		activeLLMName = mainCfg.DefaultLLM
	}
	if activeLLMName == "" && len(llmEntries) > 0 {
		activeLLMName = llmEntries[0].Name
	}

	activeLLMBinary := ""
	activeLLMEnv := map[string]string{}
	for _, e := range llmEntries {
		if e.Name == activeLLMName {
			activeLLMEnv = e.Env
			rel := e.BinaryPath
			if rel == "" {
				rel = "./plugins/llm-" + activeLLMName + "/llm-" + activeLLMName + ext
			}
			activeLLMBinary = statBinaryPath(execDir, rel, "./plugins/llm-"+activeLLMName+"/llm-"+activeLLMName+ext)
			break
		}
	}
	if activeLLMBinary == "" {
		activeLLMBinary = statBinaryPath(execDir, "", "./plugins/llm-"+activeLLMName+"/llm-"+activeLLMName+ext)
	}
	if _, err := os.Stat(activeLLMBinary); os.IsNotExist(err) {
		fail("LLM binary not found for provider %q at %q", activeLLMName, activeLLMBinary)
	}

	// 上下文窗口容量（token 数）：服务端实测容量是**上限**（探测 LLAMACPP 家族 /v1/models
	// 的 n_ctx / max_model_len），配置 context_window 只能往下调，不能凭空放大——云端把
	// 模型吹成 1M、或用户在 128K 后面补个 0，都会让「发出去的一定装得下」这条前提失效，
	// 结果每轮都靠 provider 的 4xx 教一遍。探测总是执行（OPENAI 组 BASE_URL 缺席回退
	// ANTHROPIC 组，同一 llama.cpp 端口 /v1/models 同源）：命中即作为上限，未命中（云端）
	// 才完全按配置/默认。该值同时是注入 LLM 插件的默认输出上限（见后）。
	probeWindow, probeAttempts := 0, 0
	probeBaseURL := ""
	// 上下文窗口探测同样按 provider 整组决定前缀（DSC_ 优先、旧名回退），不与终端
	// 同名变量混用。
	if baseURL := groupEnv(activeLLMEnv, "OPENAI", envGroupPreferDSC(activeLLMEnv, "OPENAI"), "BASE_URL"); baseURL != "" {
		probeBaseURL = baseURL
	} else if baseURL := groupEnv(activeLLMEnv, "ANTHROPIC", envGroupPreferDSC(activeLLMEnv, "ANTHROPIC"), "BASE_URL"); baseURL != "" {
		// LLM provider 插件 直连同一 LLAMACPP 端口（/v1/models 与 /v1/messages 同源），
		// OPENAI 组 BASE_URL 缺席时回退探测 anthropic 端点（两组均优先 DSC_ 前缀、旧名回退）。
		probeBaseURL = baseURL
	}
	if probeBaseURL != "" {
		probeWindow, probeAttempts = probeContextWindowWithRetry(probeBaseURL)
	}
	if probeWindow > 0 {
		logger.Info("context window probed from llm server", "window", probeWindow, "attempts", probeAttempts)
	} else if probeBaseURL != "" {
		// 失败必须说得出来：静默退回 128K 时，用户只能从界面上怀疑「容量怎么少了一半」。
		logger.Warn("context window probe returned nothing; falling back to config/default",
			"endpoint", endpointForLog(probeBaseURL), "attempts", probeAttempts,
			"hint", "服务端没给 n_ctx / max_model_len（云端常见）；本地服务在加载模型或 slot 占满时也会这样——已对本地端点重试。实际容量更大时请在 config.yaml 显式写 context_window")
	}
	configuredWindow := 0
	if mainCfg != nil && mainCfg.ContextWindow > 0 {
		configuredWindow = mainCfg.ContextWindow
	}
	contextWindow, windowSource, clamped := resolveContextWindow(configuredWindow, probeWindow, defaultContextWindow)
	if clamped {
		logger.Warn("configured context_window exceeds what the server reports; using the server value",
			"configured", configuredWindow, "server", probeWindow)
	}
	logger.Info("context window", "window", contextWindow, "source", windowSource)
	// 工作区根与默认会话标识：会话存档文件名就是「工作区路径 → 会话键」的编码，
	// 所以「历史没加载」最常见的两种原因都能在这一行看出来——①工作区根不是你以为的
	// 那个目录（例如从资源管理器启动 dsc.exe，cwd 变成程序目录）；②会话键与已有存档
	// 文件名不一致。用 -log 启动即可核对，无需猜。
	logger.Info("session identity", "workspace_root", core.WorkspaceRoot,
		"default_session", session.SessionKeyForProject(core.WorkspaceRoot),
		"session_dir", session.Dir(execDir))
	// 文本引用注入上限随上下文容量换算（TUI 与 -input 共用；窗口未知保持默认 1 MiB）
	tui.SetTextRefContextWindow(contextWindow)

	// 组装合并配置：LLM + agent（来自 config.yaml）+ tool/policy/dsc
	// （来自 preset；config.yaml 中启用的 tool/policy/dsc —— 含 install_dsc_plugin
	// 安装的 —— 亦并入，按名去重、preset 优先（同名取 preset），使模型安装的插件能跨重启生效）
	merged := assembleMerged(llmEntries, agentEntry, mainCfg, presetCfg, contextWindow, headless, inputText)

	// 模型名：TUI 展示与 request/header 的信封字段共用同一处解析；必须在插件进程
	// 拉起前注入 env（agent 写 header 时读 DSC_MODEL）。
	llmModelName := resolveActiveModelName(llmEntries, activeLLMName)
	injectModelName(merged, llmModelName)
	// 宿主自己的计量口径也要知道窗口与模型（用量锚点的信封比对依赖它们）。
	mgr.SetModelName(llmModelName)
	mgr.SetContextWindow(contextWindow)

	// compaction 选择同样 preset 优先（preset 是具体预设、主导行为）；config.yaml 写了
	// 不同的后端时明确提示，避免「改了 compaction 却不生效」的静默困惑。该字段现在真正
	// 决定由哪个插件接管 pre-step 压缩（见 Manager.dispatchEventToPlugins）。
	if mainCfg != nil && mainCfg.Compaction != "" && mainCfg.Compaction != merged.Compaction {
		logger.Info("compaction backend in config.yaml ignored (preset takes precedence)",
			"config", mainCfg.Compaction, "effective", merged.Compaction,
			"hint", "edit the preset's compaction field, or clear it there, to use the config value")
	}

	// 注入当前模式 + 工作根 + 沙箱档到所有插件进程（DSC_MODE）：LUA 宿主插件 据此限制
	// 「插件创造」仅在创造模式（creation）下允许。
	injectRuntimeEnv(merged, mode, core.WorkspaceRoot, sandboxPolicyEnv())

	// 有效上下文窗口下发所有插件（不止 agent）：压缩后端按它算触发阈值，缺省会退回
	// 128K，云端 1M 窗口下几步就压缩。
	injectContextWindow(merged, contextWindow)
	logger.Info("context window injected to all plugins", "window", contextWindow, "plugins", len(merged.Plugins))

	// 输出上限统一语义（不分本地/云端，行为一致）：向 LLM 插件注入 DSC_MAX_OUTPUT_TOKENS
	// =有效上下文窗口值。探测命中（本地 LLAMACPP）注入探测窗口值；探测不命中（云端）注入
	// 配置 context_window 值（含 128K 兜底默认）——窗口来源切换、注入行为不变，用户对
	// context_window 的调整在任何端点都同样生效。anthropic 兼容口把 max_tokens 视为
	// required，字段缺席时服务端自填保守默认（laamaafung server-chat.cpp 对缺席值填
	// 4096），「不携带=等模型自然结束」在其上退化为服务端默认截断；显式携带窗口值在
	// LLAMACPP 侧被服务端上下文自然钳制（生成至 EOS 或窗口满）。云端若模型输出上限低于
	// 注入值而被 400 拒绝，属用户可显式调整的范围：下调 context_window，或设插件 env
	// （DSC_ANTHROPIC_/DSC_OPENAI_MAX_OUTPUT_TOKENS=-1，旧名兼容；0 仍表示未设置）恢复不携带——可调整性优先于差异化默认。
	injectMaxOutputTokens(merged, contextWindow)
	logger.Info("max output tokens defaulted to effective context window", "max_tokens", contextWindow, "source", windowSource)

	// 声明式加载：Manager 内做依赖拓扑排序 + PENDING + 聚合 Tool 服务 + 一次性 RegisterServices。
	// 失败则自愈：把 config.yaml 与 preset 各自备份当前（坏）版、分别还原各自最近正常
	// 备份，再用还原后的配置重建插件集重试一次；仍失败才退出。
	mainConfigPath := core.ConfigPath
	pluginsDir := core.PJoin(execDir, "plugins")
	pluginsSnap := pluginsDir + "-backup"
	loadErr := mgr.LoadFromConfig(merged)
	if loadErr == nil {
		// 岗位在启动期定死：谁当值、还有谁也有这个能力。只有唯一候选时宿主直接把它
		// 放上岗位（用户启用一个后端即生效，不必再写一行配置）；多候选必须点名。
		if role := mgr.ResolveCompactionRole(); role.Holder != "" {
			logger.Info("compaction role resolved",
				"holder", role.Holder, "auto_selected", role.AutoSelected,
				"configured", role.Configured, "candidates", role.Candidates)
		}
		// 声明了需要宿主凭据的能力、却没有在 config.yaml 里显式授予时，插件运行时
		// 会静默失败（401/连不上）。宿主不按插件名自动放行凭据，所以这里必须把
		// 「该给哪个插件加哪一行」说出来。
		grants := make(map[string]map[string]string, len(merged.Plugins))
		for _, e := range merged.Plugins {
			grants[e.Name] = e.Env
		}
		for _, warn := range mgr.MissingCapabilityGrants(core.GrantMap(grants)) {
			logger.Warn(warn)
		}
	}
	if loadErr != nil {
		// 先锁定最近「正常」备份（避免把下面刚备份的坏版当成正常版回读），
		// 再把当前坏版各自备份留档，最后分别还原各自最近正常备份续启
		latestCfg, _ := core.LatestGoodBackup(mainConfigPath)
		latestPreset, _ := core.LatestGoodBackup(presetPath)
		badCfg, _ := core.BackupGoodFile(mainConfigPath, logger)
		badPreset, _ := core.BackupGoodFile(presetPath, logger)
		recovered := ""
		if latestCfg != "" {
			if err := core.RestoreGoodFile(mainConfigPath, latestCfg, logger); err == nil {
				recovered = latestCfg
			}
		}
		if latestPreset != "" {
			if err := core.RestoreGoodFile(presetPath, latestPreset, logger); err == nil && recovered == "" {
				recovered = latestPreset
			}
		}
		if recovered != "" {
			// 用还原后的 config.yaml / preset 重建插件集并重试
			if newMain, _ := loadConfig(mainConfigPath); newMain != nil {
				newPreset := presetCfg
				if p, _ := loadConfig(presetPath); p != nil {
					newPreset = p
				}
				merged = assemblePluginSet(newMain, newPreset, contextWindow, headless, inputText)
				// 对齐插件目录：先从「上次正常」快照回拷缺失/损坏（容错，跳过被运行进程锁住的）；
				// 回拷完成后，报告「未被还原后配置引用」的孤立插件目录——仅告警、不删除。
				// （孤立插件未启用过，无从判断其可用性，亦不能替用户保证将来不用，故先保留；
				// 日后若用户启用其却导致启动失败，再由本恢复机制兜底处理。）
				if err := core.RestorePluginsDir(pluginsSnap, pluginsDir, logger); err != nil {
					logger.Warn("plugins dir restore skipped (no snapshot?)", "err", err)
				}
				core.ReportOrphanPlugins(pluginsDir, core.RequiredPluginDirBases(merged), logger)
				injectRuntimeEnv(merged, mode, core.WorkspaceRoot, sandboxPolicyEnv())
				injectMaxOutputTokens(merged, contextWindow)
				logger.Warn("启动加载失败，已还原最近正常配置并重试（降级模式）",
					"cause", loadErr, "recovered", recovered, "badConfig", badCfg, "badPreset", badPreset)
				if err := mgr.LoadFromConfig(merged); err != nil {
					fail("failed to load plugins declaratively (even after config restore): %v", err)
				}
				loadErr = nil
			}
		}
		if loadErr != nil {
			fail("failed to load plugins declaratively: %v", loadErr)
		}
	}

	// 压缩是 agent 的强依赖（agent 元数据声明 requires/dsc/compaction）：它只跑循环与
	// 调度，不实现压缩，请求面的改写由当值后端完成。没有当值的后端时 agent 不会被
	// 激活，继续启动只会得到「有界面、没有循环」的假象——不如在这里就把缺什么说清楚。
	//
	// 这也是唯一的口径：不按插件名判断，只问「谁声明了 compression 能力并当值」。
	if problem := mgr.CompactionRoleProblem(); problem != "" {
		fail("compaction backend is required but not available: %s. Enable a plugin that declares Provides compaction in config.yaml (set `compaction: <name>` when several are enabled).", problem)
	}

	// 成功启动：把已生效的 config.yaml 与当前 preset 各自独立备份一次（配置自愈基础——
	// 之后若被改坏，可分别还原各自最近正常版本续启）
	if _, err := core.BackupGoodFile(mainConfigPath, logger); err != nil {
		logger.Warn("failed to backup config after successful start", "err", err)
	}
	if _, err := core.BackupGoodFile(presetPath, logger); err != nil {
		logger.Warn("failed to backup preset after successful start", "err", err)
	}
	// 维护 plugins 目录「上次正常」快照（二进制大，仅当有新内容才刷新）
	if done, err := core.BackupPluginsDir(pluginsDir, pluginsSnap, logger); err != nil {
		logger.Warn("failed to snapshot plugins dir", "err", err)
	} else if done {
		logger.Info("plugins dir snapshot refreshed", "snap", pluginsSnap)
	}

	// 后台监听/调度仅在常规模式启用；-headless 精简无头模式不开端口、不起常驻轮询，
	// 对齐 harness headless「进程只存活于单发、不留任何后台」的契约。
	if !headless {
		// 版本化二进制自动热重载（config.yaml hot_reload: true 时启用）：fsnotify + 周期扫描
		if err := mgr.StartHotReloadWatcher(); err != nil {
			logger.Warn("failed to start hot-reload watcher", "err", err)
		}

		// 启动 cron 定时任务调度器（失败仅告警，不阻塞主流程）
		if err := mgr.StartCron(); err != nil {
			logger.Warn("failed to start cron scheduler", "err", err)
		} else {
			logger.Info("cron scheduler started")
		}

		// 启动管理 API（管理能力）：
		// 管理能力是一种「可选能力」——只有需要它的插件（如 管理界面插件）
		// 才声明依赖。宿主按以下优先级决定是否启动管理 API：
		//   1. 用户显式 -admin <addr>：强制启动
		//   2. DSC_ADMIN_ADDR 环境变量：强制启动
		//   3. 配置中有插件声明 Requires: dsc/admin 能力依赖：自动启动
		//   4. DSC_NO_ADMIN=1：强制关闭（即使有插件需要也不启动，插件应优雅降级）
		//   5. 默认：关闭（TUI 终端用户通常不需要管理 API）
		needAdmin := false
		if adminAddr != "" {
			// 用户显式 -admin
			needAdmin = true
		} else if os.Getenv("DSC_ADMIN_ADDR") != "" {
			needAdmin = true
			adminAddr = os.Getenv("DSC_ADMIN_ADDR")
		} else if os.Getenv("DSC_NO_ADMIN") != "1" && os.Getenv("DSC_NO_ADMIN") != "true" {
			// 检查是否有插件声明了 requires/dsc/admin 能力依赖
			if mgr.HasPluginRequiringCapability("dsc", "admin") {
				needAdmin = true
				logger.Info("admin api auto-enabled (plugin requires admin capability)")
			}
		}

		if !needAdmin {
			logger.Info("admin api disabled (use -admin <addr> or DSC_ADMIN_ADDR to enable)")
		} else {
			if adminAddr == "" {
				adminAddr = "127.0.0.1:9999"
			}
			finalAddr := mgr.StartAdmin(adminAddr)
			if finalAddr != adminAddr {
				logger.Warn("admin api port auto-incremented due to conflict",
					"requested", adminAddr, "actual", finalAddr)
			}
			adminAddr = finalAddr
			logger.Info("admin api started", "addr", adminAddr)
			// 把实际监听地址注入到环境变量，供需要它的插件子进程读取
			os.Setenv("DSC_ADMIN_ADDR", adminAddr)
		}
	}

	// 获取 Agent 并运行（经事件包装：RunStream 回合完成时广播 agent/status，
	// 供 notify 等程序性插件订阅；对齐 DSH agent-loop 原生发 agent/status）
	agentName := mgr.GetMainAgentName()
	if agentName == "" {
		// 发现机制兜底：扫描 plugins/agent-* 目录取首个（与 assembleMerged 一致），
		// 不再硬编码具体的 agent 插件名（对齐 AGENTS.md §8）。
		agentName = "agent-" + discoverFirstAgentPlugin()
	}
	agent, ok := mgr.EventAgent()
	if !ok {
		fail("agent %s not found after loading", agentName)
	}

	ctx := context.Background()

	exitCode := 0
	if inputText != "" {
		// -input：一次性模式，不显示 TUI，完成后自然退出
		logger.Info("running in input mode", "input", inputText)
		exitCode = runInputMode(agent, ctx, inputText, headless)
		logger.Info("input mode finished", "exitCode", exitCode)

		// 回合完成音效宽限：notify 等程序性通知插件在 agent 回合完成时异步播放完成音效
		// （success/error，约 0.29s）。-input 单发回合结束后宿主随即回收插件子进程
		// （Windows 上为强杀，不跑 defer），可能导致音效被截断。已加载通知插件且非
		// headless（CI 单发无需报声）时，短暂宽限让音效播完再关闭；headless 保持快速退出。
		// 按能力探测（notify），任何声明该能力的插件均触发宽限，避免绑死
		// 具体插件名（对齐 AGENTS.md §8：宿主核心不得硬编码插件名）。
		if !headless && mgr.HasPluginProvidingCapability("notify") {
			const completionSoundGrace = 800 * time.Millisecond
			logger.Info("draining completion sound", "grace", completionSoundGrace.String())
			time.Sleep(completionSoundGrace)
		}
	} else {
		// TUI 启动回显：显示条数由 history_shows 控制（负值不显示、0 默认 20、N 最近 N），
		// 并受 history_injection 门控（注入关闭则不显示）；宿主从项目会话派生并截取
		// （见 core.Manager.LoadDisplayHistory）。
		histInj, _ := historyInjectionCount(mainCfg)
		histShows := 0
		if mainCfg != nil {
			histShows = mainCfg.HistoryShows
		}
		replay := mgr.LoadHistoryReplay(histInj, histShows)
		if replay != nil {
			logger.Info("tui history replay", "total", len(replay.Messages), "initial", replay.Initial,
				"history_injection", histInj, "history_shows", histShows)
		}
		// 退出提示（如 /config 切换了配置）：TUI 退出后屏幕已还原，由这里打印。
		note, err := tui.Run(agent, mgr, ctx, llmModelName, mode, contextWindow, replay)
		if note != "" {
			fmt.Println(note)
		}
		if err != nil {
			logger.Error("tui run failed", "error", err)
			exitCode = 1
		}
		logger.Info("tui exited")
	}

	// 完成完整的清理過程再退出
	logger.Info("shutting down...")
	mgr.Shutdown()
	os.Exit(exitCode)
}

// warnIfWorkspaceIsInstallDir 提示「工作区根就是安装目录」。
//
// 工作区根默认取启动目录，而会话键（`sessions/<键>.jsonl`）、记忆库、压缩后端状态
// 全都按它命名。于是从安装目录直接跑 `dsc.exe` 时，会把安装目录当成一个项目，
// 在 sessions/ 里留下 `E=--Programs--dsc-for-windows-amd64-xxxx` 这类文件——
// 看起来像「程序拿自己的路径当会话名」，其实是启动位置的问题。
//
// 不自动改：把根挪走会让「同一项目同一份会话」的前提失效（换目录=换项目）。
// 只提示，并给出两条明确出路。
func warnIfWorkspaceIsInstallDir(logger hclog.Logger, root string) {
	if logger == nil || root == "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	exeDir, err := core.PAbs(core.PDir(exe))
	if err != nil {
		return
	}
	absRoot, err := core.PAbs(root)
	if err != nil || absRoot != exeDir {
		return
	}
	logger.Warn("workspace root is the DSC installation directory",
		"workspace_root", root,
		"hint", "会话/记忆/压缩状态按工作区根命名，安装目录会被当成一个项目。"+
			"请到项目目录启动 dsc，或在 config.yaml 用 workspace_root 指定项目根")
}
