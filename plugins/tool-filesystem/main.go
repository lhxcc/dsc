package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"dsc-sdk"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Session 表示一個持久的 shell 會話
type Session struct {
	SessionID string
	Cwd       string
	Runner    *interp.Runner
	StdoutBuf *syncedBuilder
	StderrBuf *syncedBuilder
	// stdoutSignal / stderrSignal 活动信号 writer：包在对应缓冲外（interp 经
	// StdIO 写入它们），每次输出写入即向活跃续命执行域上报活动。随 session
	// 持久存在，回调在每次命令执行时换绑（见 execSessionCommand）。
	stdoutSignal *activityWriter
	stderrSignal *activityWriter
	mu           sync.Mutex
	// lastUsed 最近一次执行命令的时间（LRU 淘汰依据）。持久会话是默认行为，
	// 池子里必然会有多个；淘汰必须挑「最久没用过的」，而不是 map 里随便一个
	// ——后者会把正在用的会话踢掉，模型下一轮进来 cwd 莫名丢失。
	lastUsed time.Time
}

// syncedBuilder 是線程安全的輸出累加緩衝：Runner 經 activityWriter 向其寫入
// （interp 可能在後台並發寫 stdout/stderr），讀寫並發安全。
type syncedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncedBuilder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncedBuilder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *syncedBuilder) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

func (s *syncedBuilder) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Reset()
}

// SessionManager 管理所有持久的 shell 會話
type SessionManager struct {
	sessions map[string]*Session
	mu       sync.RWMutex
}

var globalSessionManager = &SessionManager{
	sessions: make(map[string]*Session),
}

// maxSessions 上限：防持久 shell 会话 map 无限增长。
const maxSessions = 64

// activityWriter 活动信号 writer：包装会话输出缓冲，每次写入（输出到达）即向
// 当前调用的活跃续命执行域上报活动（dsc.TouchActivity）。超时预算与看门狗由
// timeout-policy 插件经宿主裁决安装（tool/execute 槽），本工具只供给活动信号——
// 输出到达即活动，不感知预算与策略。回调随每次命令执行换绑；未换绑
// （无进行中的命令）时 no-op。
type activityWriter struct {
	mu      sync.Mutex
	w       io.Writer
	onWrite func([]byte)
}

func (a *activityWriter) Write(p []byte) (int, error) {
	a.mu.Lock()
	f, w := a.onWrite, a.w
	a.mu.Unlock()
	if f != nil {
		f(p)
	}
	return w.Write(p)
}

// bind 把输出回调换绑到本次调用的 ctx；run 结束后以 bind(nil) 解绑。
//
// 回调做两件事，走的是同一条 ctx：
//   - TouchActivity：告诉宿主的活跃续命看门狗「还在动」；
//   - AppendOutput：把这段字节登记进跨进程活动表，宿主转后台之后 job_output 就能
//     读到「最近在说什么」。命令跑完仍以完整输出为权威结果，这里只是尾部快照。
func (a *activityWriter) bind(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if ctx == nil {
		a.onWrite = nil
		return
	}
	a.onWrite = func(p []byte) {
		dsc.TouchActivity(ctx)
		// 尾部快照同样要给人看：外部程序按系统码页（GBK）写出、我们按 UTF-8 收，
		// 未还原的字节到了宿主只会变成 U+FFFD。这里按块尽力还原（完整输出在落定时
		// 还会用整段/逐行两级解码再净化一次，块边界切开的字符只影响这一眼的观感）。
		if !utf8.Valid(p) {
			p = []byte(ensureUTF8(string(p)))
		}
		dsc.AppendOutput(ctx, p)
	}
}

// main 以公共 SDK（dsc-sdk）声明式启动：SDK 自动提供 ToolService /
// PluginMetadata / PluginHookService 与 go-core 组装（重写自旧的
// ToolServiceServer/MetadataServer/ToolMetadataGRPCPlugin 样板）。
func main() {
	// 定義 shell 工具描述
	baseDescription := "Execute a shell command or script using mvdan/sh interpreter internally (POSIX shell standard). The shell persists across calls by default: cwd, exported variables and shell functions carry over, so `cd` once instead of repeating it in every command. Pass `workdir` to move the shell into a directory (applies to new and existing sessions alike). Pass `session_id: \"new\"` only when you need a clean shell with a fresh cwd and environment; an explicit non-empty `session_id` gives you a separate persistent shell, e.g. one per task."

	var pathAdvice string
	var extraAdvice string

	if runtime.GOOS == "windows" {
		pathAdvice = "CRITICAL: In Windows environments, terminal path styles vary (e.g., Git Bash uses '/mnt/d/...', while this terminal supports 'D:/...'). A bare POSIX path like '/docs' resolves to the current drive root (e.g. 'D:/docs'), matching Linux real-root semantics — the session workspace is reached via the '/workspace' prefix or the real path shown by 'pwd'. You MUST first run the 'pwd' command to obtain the current directory path format before performing any path operations. Always wrap paths in quotes to ensure safe usage."
		extraAdvice = "Note: This interpreter does not support PowerShell (PWSH), CMD, or other Windows-specific shell command interpreters. Please use standard Unix/Linux POSIX shell commands only (e.g., ls, find, cd, grep)."
	} else {
		pathAdvice = "When working with paths, it is mandatory to first use the 'pwd' command to get the current directory path format, and always wrap paths in quotes to ensure safe usage."
	}

	description := baseDescription
	if pathAdvice != "" {
		description += " " + pathAdvice
	}
	if extraAdvice != "" {
		description += " " + extraAdvice
	}
	// 输出去向的建议：只说明后果，不做拦截。
	//
	// job_output 能看到的运行期输出，全部来自这条命令**连着我们**的 stdout/stderr。
	// 一旦 `> log 2>&1`，字节直接进文件，job_output 只能看到"(no output yet)"直到
	// 命令结束——而这恰好是「想盯进度」和「想留日志」两个目标冲突的地方。要不要
	// 重定向只有模型自己知道（它知道后面还有没有别的步骤要读那个文件），所以这里
	// 只把代价讲清楚，然后明确说重定向也完全可以。
	description += " If you plan to watch its progress with job_output while it runs, keep stdout and stderr attached: " +
		"redirecting them (`> log 2>&1`) sends the output straight to the file and makes the running job look silent."
	if runtime.GOOS != "windows" {
		// tee 是外部程序：Windows 上默认没有，写上去等于让模型去撞墙。
		description += " Use `2>&1 | tee log` when you want the file and a live view together."
	}
	description += " When the log file is what you actually need, redirect anyway — that is a legitimate choice."

	// 定義 shell 工具
	schema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "command": {
                                "type": "string",
                                "description": "The shell command or script to execute."
                        },
                        "description": {
                                "type": "string",
                                "description": "Clear, concise description of what this command does in active voice, 5-10 words (shown in the UI). Examples: \"ls\" → \"List files in current directory\"; \"git status\" → \"Show working tree status\"."
                        },
                        "workdir": {
                                "type": "string",
                                "description": "Working directory for this command. Defaults to the session workspace; a relative path is resolved against it."
                        },
                        "session_id": {
                                "type": "string",
                                "description": "Shell to use. Omit to reuse the conversation's persistent shell (default: cwd and exported variables carry over). Use 'new' for a clean shell; any other name keeps a separate persistent shell of its own."
                        },
                        "sandbox_permissions": {
                                "type": "string",
                                "enum": ["workspace-write", "danger-full-access"],
                                "description": "Optional sandbox escalation, ONLY for retrying a call that was denied by the sandbox. Do NOT set it on normal or read-only calls: requesting a mode that is not strictly wider than the current one is ignored and the call simply runs under the current mode. To genuinely widen (read-only → workspace-write/danger-full-access; workspace-write → danger-full-access), retry the exact command once with this field plus 'justification' to request one-call user approval."
                        },
                        "justification": {
                                "type": "string",
                                "description": "Required together with 'sandbox_permissions': a one-sentence reason shown to the user in the approval prompt."
                        },
                        "run_in_background": {
                                "type": "boolean",
                                "description": "Run the command in the background and return a job id immediately. Track output with job_output, stop with job_kill. No timeout applies. Omit for foreground execution (default)."
                        }
                },
                "required": ["command", "description"]
        }`)
	handler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var params struct {
			Command   string `json:"command"`
			WorkDir   string `json:"workdir"`
			Cwd       string `json:"cwd"` // 向后兼容旧参数名
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(args, &params); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if strings.TrimSpace(params.Command) == "" {
			return "", fmt.Errorf("command is required")
		}

		// 會話鍵：默認持久。
		//
		// 語義（反轉自早期實現：那時不傳 id 就每次新 shell，模型必須反復 cd）：
		//   - 不傳 session_id → 綁到**調用方會話**（同一對話復用同一個 shell），
		//     宿主沒給身份時退化為進程內單一默認會話；
		//   - session_id: "new" → 顯式要一個全新 shell（cwd/env 都乾淨）；
		//   - 其它值 → 按該名字取用（模型自建的分組，例如按任務隔離）。
		sessionID := sessionKeyFor(ctx, params.SessionID)

		// 獲取或創建 session
		// workdir 优先于 cwd（对齐 DSH 参数名）；cwd 保留向后兼容
		workdir := params.WorkDir
		if workdir == "" {
			workdir = params.Cwd
		}
		session, err := getOrCreateSession(sessionID, workdir)
		if err != nil {
			return "", fmt.Errorf("failed to create or get session: %w", err)
		}

		// 執行命令到 session
		touchSession(session)
		output, exitCode, err := execSessionCommand(ctx, session, params.Command)
		if err != nil {
			return "", fmt.Errorf("failed to execute command in session: %w", err)
		}

		return formatShellResult(params.Command, output, exitCode), nil
	}

	sdk := dsc.New(dsc.Config{
		Name:    "filesystem",
		Version: "1.0.0",
		Type:    dsc.TypeTool,
		Provides: map[string]string{
			// 提供 "filesystem" 能力：含 shell 工具，可执行命令与读写文件
			"filesystem": "true",
		},
	})
	// shell 可能长时间阻塞（起服务、等端口、跑测试）：声明自动后台能力，由宿主
	// 按「后台登记 + 前台宽限期」执行——宽限内跑完是普通工具结果，超时转后台并
	// 返回 job id，完成时照常通知。不这样的话一条长命令会占死整个 step，
	// 期间任何插话/完成通知都只能等它跑完。
	sdk.Tool(dsc.Tool{
		Name:         "shell",
		Description:  description,
		Schema:       schema,
		Handler:      handler,
		ViewFn:       shellView,
		Capabilities: []string{dsc.CapabilityAutoBackground},
	})
	sdk.Serve()
}

// exitCodeMarkRe 匹配结果里追加的退出码标记（[exit_code: N]，见 formatShellResult）。
var exitCodeMarkRe = regexp.MustCompile(`\[exit_code\s*:\s*(-?\d+)\]`)

// mapWorkspacePath 把模型书写的路径按虚拟根表映射为真实路径（实现在 core，SDK 转发）。
// shell 侧只保留 AST 字面量重写这一个接入点——本插件不再持有任何虚拟根知识。
func mapWorkspacePath(p string) string {
	return dsc.MapWorkspacePath(p)
}

// mapWorkspacePaths 遍历 shell AST，把纯字面量词中的虚拟根前缀重写为真实路径：
// /workspace（跨平台）、/tmp 与 /mnt/<drive>（仅 Windows）——规则全在 core 的虚拟根
// 表里，这里只负责「在解释执行前改写字面量」。只改写词首的路径形状词（裸词 / 单双引号
// 内无变量展开），变量展开/命令替换等复杂词不改，避免误伤。cd /workspace、cd /tmp、
// ls -la /workspace/x、cat "/workspace/a b" 都被覆盖——包括 cd 内建（改 runner.Dir，
// 不经文件操作的 res()），这正是当年 /tmp 在 cd 下失效、而文件操作下正常的根因。
func mapWorkspacePaths(node syntax.Node) {
	mapWorkspacePathsFor(node, runtime.GOOS)
}

// mapWorkspacePathsFor 是 mapWorkspacePaths 的 goos 注入变体（便于跨平台测试）；
// 生产路径使用真实 runtime.GOOS。
func mapWorkspacePathsFor(node syntax.Node, goos string) {
	syntax.Walk(node, func(n syntax.Node) bool {
		switch v := n.(type) {
		case *syntax.Lit:
			v.Value = mapShellLitFor(v.Value, goos)
		case *syntax.SglQuoted:
			// SglQuoted 自身即内容（无独立 Lit 子节点），改写后不再下钻，
			// 否则 Walk 会继续进入其子节点把已改写的值再映射一次（见 DblQuoted 注释）。
			v.Value = mapShellLitFor(v.Value, goos)
			return false
		case *syntax.DblQuoted:
			// 改写各 Lit 子部分后返回 false 阻止 Walk 下钻：否则子 Lit 节点会被
			// *syntax.Lit 分支再映射一次。映射表本身「首次命中即返回」，对已映射
			// 的结果再跑一遍也只会落到「不匹配任何前缀」，但双重遍历没有意义。
			for _, part := range v.Parts {
				if lit, ok := part.(*syntax.Lit); ok {
					lit.Value = mapShellLitFor(lit.Value, goos)
				}
			}
			return false
		}
		return true
	})
}

// mapShellLitFor 用虚拟根表映射一个字面量（goos 显式注入便于跨平台测试）。
//
// 表内已按「具体前缀在前」排好序，且**首次命中即返回**，因此不存在旧实现
// 「先 /tmp 后 /workspace」那种两段串联的顺序陷阱（工作区根落在 /tmp/... 下时
// 会被二次改写）。
func mapShellLitFor(p, goos string) string {
	return dsc.MapWorkspacePathFor(p, dsc.WorkspaceRoot(), dsc.TempDir(), goos)
}

// shellView 为 shell 工具声明结构化视图：标题 Shell + 退出码徽标（0 绿 / 非 0 红）
// + 命令输出正文（去掉追加的 [exit_code: N] 标记，避免与徽标重复）。
func shellView(_ context.Context, _ json.RawMessage, result string) (json.RawMessage, error) {
	body := strings.TrimSpace(result)
	exitCode := int64(0)
	if m := exitCodeMarkRe.FindStringSubmatch(body); len(m) == 2 {
		if n, err := strconv.ParseInt(m[1], 10, 32); err == nil {
			exitCode = n
		}
		body = exitCodeMarkRe.ReplaceAllString(body, "")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		body = "(no output)"
	}
	tone := "green"
	if exitCode != 0 {
		tone = "red"
	}
	return dsc.PlainView("Shell", &dsc.ViewBadge{Text: fmt.Sprintf("exit %d", exitCode), Tone: tone}, body), nil
}

// formatShellResult 根據命令輸出與退出碼組裝最終返回文本：
// - 非 0 退出碼：無論是否有輸出，都追加 [exit_code: N]。
// - 0 退出碼且有輸出：直接返回輸出，不附加 [exit_code: 0]。
// - 無輸出：顯式寫出 (no output)。
//
// 最後一條是刻意的：只回 [exit_code: 0] 時，模型分不清「命令沒東西可說」與
// 「工具沒抓到輸出」，往往把乾淨工作區的 `git status` 當成工具壞了、反复重跑。
// 明確寫出無輸出，配合退出碼即可唯一確定「跑成功了，只是沒有任何輸出」。
// formatShellResult 根據命令輸出與退出碼組裝最終返回文本：
// - 非 0 退出碼：無論是否有輸出，都追加 [exit_code: N]。
// - 0 退出碼且有輸出：直接返回輸出，不附加 [exit_code: 0]。
// - 無輸出：顯式寫出 (no output)。
//
// 最後一條是刻意的：只回 [exit_code: 0] 時，模型分不清「命令沒東西可說」與
// 「工具沒抓到輸出」，往往把乾淨工作區的 `git status` 當成工具壞了、反复重跑。
// 明確寫出無輸出，配合退出碼即可唯一確定「跑成功了，只是沒有任何輸出」。
//
// command 用于诊断「命令链在 && 处中断」：`cd <不存在目录> && python3 << 'PY' …`
// 每条后续命令都不会执行，输出只剩一句 cd 失败——模型极易误判成「这个环境不支持
// heredoc / 多行脚本」，然后换着写法反复重试同一个错误。
func formatShellResult(command, output string, exitCode int32) string {
	// 进度条式输出先归一（ 重绘只保留最终帧，CRLF 归一为 LF）：这些中间帧模型
	// 永远用不上，却可能占掉结果的绝大部分（实测一条 git 结果 2415 字符里 2112 是
	// 中间帧）。渲染侧同样会归一，这里做在出口上让「模型看到＝人眼看到」。
	// 唯一实现见 core.CollapseCarriageReturns（经 sdk 转发）。
	output = dsc.CollapseCarriageReturns(output)
	empty := strings.TrimSpace(output) == ""
	if exitCode != 0 {
		var b strings.Builder
		if empty {
			fmt.Fprintf(&b, "\n[exit_code: %d]\n(no output)\n", exitCode)
		} else {
			b.WriteString(output)
			fmt.Fprintf(&b, "\n[exit_code: %d]\n", exitCode)
		}
		if strings.Contains(command, "&&") {
			b.WriteString("[note: the command chain stopped at a failing step; commands after && were not run]\n")
		}
		return b.String()
	}
	if empty {
		return "\n[exit_code: 0]\n(no output)\n"
	}
	return output
}

// shellHandlers 返回与路径归一相关的基础 handler（生产 runner 与测试 runner 共用）：
//   - ExecHandler：内建式常用 POSIX 工具（mkdir/ls/cat/touch/rm/cp/mv/grep/head/tail/wc）
//     进程内实现，不依赖外部 PATH；未命中的命令仍回退默认 PATH 外部执行。
//   - OpenHandler：重定向/输入输出也走同一套路径归一（res）。默认 handler 只按 Dir
//     拼相对路径，于是 `echo x > /tmp/a`、`2>/dev/null` 会绕过内建命令的路径映射
//     （Windows 上 /tmp 落到当前盘根）。
func shellHandlers() []interp.RunnerOption {
	return []interp.RunnerOption{
		interp.ExecHandler(shellExecHandler),
		interp.OpenHandler(func(ctx context.Context, path string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
			if path == "" {
				return os.OpenFile(path, flag, perm)
			}
			return os.OpenFile(res(interp.HandlerCtx(ctx), path), flag, perm)
		}),
	}
}

// defaultSessionKey 拿不到调用方身份时使用的兜底会话键（進程內单例）。
const defaultSessionKey = "default"

// shellSessionSeq 为显式 "new" 的会话名提供单调序号（见 sessionKeyFor）。
var shellSessionSeq uint64

// sessionKeyFor 计算本次调用使用的 shell 会话键。
//
// 语义：**默认持久**。不传 session_id 时绑定到调用方会话，同一对话复用同一个
// shell（cwd、export 的变量、函数都留着），模型不必每条命令都 `cd` 一次。
// 想要干净环境必须显式 `session_id: "new"`。
func sessionKeyFor(ctx context.Context, requested string) string {
	switch k := strings.TrimSpace(requested); {
	case k == "new":
		// 显式新建：纳秒 + 自增序号。只用纳秒不够——同一纳秒内连续两次调用
		// 会拿到同一个键，等于 "new" 又复用了刚才那个 shell（Windows 时钟粒度
		// 下实测能撞上）。
		return fmt.Sprintf("shell-%d-%d", time.Now().UnixNano(), atomic.AddUint64(&shellSessionSeq, 1))
	case k != "":
		return k
	}
	if sid := dsc.CallSessionID(ctx); sid != "" {
		// 前缀避免与模型自建的 id 撞名（模型可能正好用 "default" 之类）
		return "conv:" + sid
	}
	return defaultSessionKey
}

// resolveSessionWorkdir 把 workdir 参数归一为可用的绝对目录。
//
// 空值 → 统一工作空间根（启动 dsc 的目录），而不是插件进程的可执行目录；显式值
// 先过 `/workspace` 前缀映射（与命令 AST 的路径映射同一套语义），相对路径锚到
// 工作空间根——与 shell 内相对路径的语义一致。
func resolveSessionWorkdir(w string) string {
	if w == "" {
		if ws := dsc.WorkspaceRoot(); ws != "" {
			return ws
		}
		if cwd, err := os.Getwd(); err == nil {
			return cwd
		}
		return os.TempDir()
	}
	mapped := dsc.MapModelPath(w) // 设备名 + 虚拟根，与命令里的路径同一套表
	if !filepath.IsAbs(mapped) {
		if ws := dsc.WorkspaceRoot(); ws != "" {
			mapped = dsc.PJoin(ws, mapped)
		}
	}
	return mapped
}

// touchSession 记录最近使用时间（LRU 淘汰依据）。
func touchSession(s *Session) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// lastUse 读取最近使用时间（加锁，避免与 touchSession 竞态）。
func (s *Session) lastUse() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastUsed
}

// getOrCreateSession 獲取或創建 session
func getOrCreateSession(sessionID, cwd string) (*Session, error) {
	dir := resolveSessionWorkdir(cwd)

	globalSessionManager.mu.RLock()
	session, exists := globalSessionManager.sessions[sessionID]
	globalSessionManager.mu.RUnlock()

	if exists {
		// 復用時也要遵守 workdir：此前是直接 return，模型按描述傳 workdir 會被
		// 靜默忽略（命令照樣跑在舊目錄），而且沒有任何提示。
		if strings.TrimSpace(cwd) != "" {
			session.mu.Lock()
			session.Runner.Dir = dir
			session.Cwd = dir
			session.mu.Unlock()
		}
		touchSession(session)
		return session, nil
	}

	// 創建新 session runner
	initialEnv := expand.ListEnviron(os.Environ()...)

	stdoutBuf := &syncedBuilder{}
	stderrBuf := &syncedBuilder{}
	// 活动信号 writer 包在缓冲外：输出写入即向活跃续命执行域上报活动
	//（dsc.TouchActivity）；执行域由 timeout-policy 插件经宿主裁决安装。
	stdoutSignal := &activityWriter{w: stdoutBuf}
	stderrSignal := &activityWriter{w: stderrBuf}

	runnerOpts := []interp.RunnerOption{
		interp.Env(initialEnv),
		interp.Dir(dir),
		interp.StdIO(nil, stdoutSignal, stderrSignal),
	}
	runnerOpts = append(runnerOpts, shellHandlers()...)

	runner, err := interp.New(runnerOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to create shell runner: %w", err)
	}

	session = &Session{
		SessionID:    sessionID,
		Cwd:          dir,
		lastUsed:     time.Now(),
		Runner:       runner,
		StdoutBuf:    stdoutBuf,
		StderrBuf:    stderrBuf,
		stdoutSignal: stdoutSignal,
		stderrSignal: stderrSignal,
	}

	globalSessionManager.mu.Lock()
	// 达上限时淘汰**最久未用**的会话。以前是 map 里随便取一个删——可能踢掉正在用的
	// 那个，模型下一轮带同一 id 进来拿到全新 shell，cwd/变量「莫名失忆」且无从解释。
	if len(globalSessionManager.sessions) >= maxSessions {
		oldestKey := ""
		var oldest time.Time
		for k, v := range globalSessionManager.sessions {
			if lu := v.lastUse(); oldestKey == "" || lu.Before(oldest) {
				oldestKey, oldest = k, lu
			}
		}
		if oldestKey != "" {
			delete(globalSessionManager.sessions, oldestKey)
		}
	}
	globalSessionManager.sessions[sessionID] = session
	globalSessionManager.mu.Unlock()

	return session, nil
}

// execSessionCommand 在 session 中執行命令，返回輸出和退出碼。
// ctx 用于传播调用方取消与活跃续命执行域（若有）：命令輸出每次寫入都經
// activityWriter 上報活動（dsc.TouchActivity），看門狗與預算由 timeout-policy
// 插件經宿主裁決安裝——持續無輸出達預算才取消，避免誤殺仍在產出的長編譯/測試。
func execSessionCommand(ctx context.Context, session *Session, command string) (string, int32, error) {
	session.mu.Lock()
	session.StdoutBuf.Reset()
	session.StderrBuf.Reset()
	session.mu.Unlock()

	// 解析命令為 shell 語法樹
	parser := syntax.NewParser()
	file, err := parser.Parse(strings.NewReader(command+"\n"), "")
	if err != nil {
		// 如果解析失敗，嘗試作為單個命令執行
		command = "echo 'Syntax error in command: " + strings.ReplaceAll(err.Error(), "'", "''") + "'"
		file, err = parser.Parse(strings.NewReader(command+"\n"), "")
		if err != nil {
			return "", 0, fmt.Errorf("failed to parse command: %w", err)
		}
	}
	// 把 /workspace 虚拟根前缀映射为真实工作区根，使模型初期 `cd /workspace`、
	// `ls /workspace/x` 等探索不再报 no such file or directory（对齐 sandbox 语义）。
	mapWorkspacePaths(file)

	// 活动信号换绑到本次调用 ctx：输出到达 → dsc.TouchActivity（活跃续命）。
	// 看门狗与预算由 timeout-policy 插件经宿主裁决安装（tool/execute 槽），
	// 本工具只供给活动信号；ctx 未携带执行域时 TouchActivity 为 no-op。
	session.stdoutSignal.bind(ctx)
	session.stderrSignal.bind(ctx)
	defer func() {
		session.stdoutSignal.bind(nil)
		session.stderrSignal.bind(nil)
	}()

	err = session.Runner.Run(ctx, file)

	exitCode := int32(0)
	if err != nil {
		if exitErr, ok := err.(interp.ExitStatus); ok {
			exitCode = int32(exitErr)
		} else {
			exitCode = 1
		}
	}

	session.mu.Lock()
	output := ensureUTF8(session.StdoutBuf.String() + session.StderrBuf.String())
	session.mu.Unlock()

	// 活跃续命看门狗触发：上抛取消错误，由工具流水线桥替换为裁决文案
	//（timeout-policy 的 message）。其余路径保持既有语义：解释器错误计入
	// 退出码、结果照常返回（非零退出不是 Go 错误），用户中断不在此上抛。
	if err != nil && dsc.IdleDeadlineExceeded(ctx) {
		return output, exitCode, err
	}
	return output, exitCode, nil
}
