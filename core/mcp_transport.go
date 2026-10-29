package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
)

// MCP 传输层：一次 JSON-RPC 往返背后的实际通道。
//
// 两种实现：
//   - httpTransport：streamable HTTP（MCP 规范 2025-03-26+，响应可为 JSON 或 SSE），
//     会话状态在 Mcp-Session-Id 头里；
//   - stdioTransport：把 MCP 服务器作为**子进程**跑，JSON-RPC 逐行写 stdin / 读 stdout，
//     会话状态就是那个进程本身（对齐 DSH mcp-client 的 StdioClientTransport）。
//
// 为什么要抽象这一层：MCPClient 的 initialize / tools-list / tools-call 与传输无关，
// 会话过期重连、工具注册、错误包装都在客户端；把「怎么把一行 JSON 送过去再取回一行」
// 隔开之后，两种传输各自的怪癖（HTTP 的 header 与 SSE、stdio 的管道与进程退出）不会
// 渗进协议逻辑。
type mcpTransport interface {
	// RoundTrip 发送一个 JSON-RPC 消息并返回响应；无 id 的通知返回空 map。
	RoundTrip(ctx context.Context, req map[string]any) (map[string]any, error)
	// Reset 丢弃会话状态并准备下一次握手（HTTP 清会话 id；stdio 在子进程已退出时重启）。
	Reset(logger hclog.Logger) error
	// Close 释放通道（HTTP 无操作；stdio 关 stdin 并回收子进程）。
	Close()
	// Kind 传输类型标识：http / stdio。
	Kind() string
	// Describe 一行人类可读描述（状态行、日志、管理 API 用）。
	Describe() string
}

// ============================================================
// streamable HTTP
// ============================================================

// errMCPSessionExpired 表示服务端不再认识当前会话（streamable HTTP 的 404）。
// 调用方据此重新 initialize，而不是把「会话过期」当成工具调用失败。
var errMCPSessionExpired = fmt.Errorf("mcp: session expired")

type httpTransport struct {
	endpoint   string
	httpClient *http.Client

	mu              sync.Mutex
	sessionID       string
	protocolVersion string
}

func newHTTPTransport(endpoint string) *httpTransport {
	return &httpTransport{
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (t *httpTransport) Kind() string     { return "http" }
func (t *httpTransport) Describe() string { return t.endpoint }
func (t *httpTransport) Close()           {}

// Reset 清掉会话：重连必须拿新会话，旧 id 在新连接上一定 404。
func (t *httpTransport) Reset(_ hclog.Logger) error {
	t.mu.Lock()
	t.sessionID = ""
	t.mu.Unlock()
	return nil
}

// RoundTrip 发送一次 JSON-RPC 请求。
//
// HTTP 头设置兼容新旧两种 MCP HTTP 传输：
//   - 旧版 JSON-RPC over HTTP（Content-Type: application/json）
//   - 新版 streamable-http（MCP 规范 2025-03-26+，要求 Accept 同时含
//     application/json 和 text/event-stream）
//
// 新版服务器经 Accept 头检测客户端能力——缺 text/event-stream 会返回 HTTP 406
// "Not Acceptable"。设上两个类型后，旧版服务器忽略 Accept、新版服务器选择其中一种
// 响应（简单请求通常返回 application/json）。
func (t *httpTransport) RoundTrip(ctx context.Context, req map[string]any) (map[string]any, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", t.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")

	t.mu.Lock()
	sid, ver := t.sessionID, t.protocolVersion
	t.mu.Unlock()
	if sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	if ver != "" {
		httpReq.Header.Set("MCP-Protocol-Version", ver)
	}

	resp, err := t.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 服务端可能在任意响应上重新下发会话标识（initialize 一定有），有就更新。
	if newSID := resp.Header.Get("Mcp-Session-Id"); newSID != "" && newSID != sid {
		t.mu.Lock()
		t.sessionID = newSID
		t.mu.Unlock()
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	switch {
	case resp.StatusCode == http.StatusOK:
	// 正常响应，按内容类型解析（JSON 或 SSE）
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		// 通知（如 notifications/initialized）按规范回 202/204 且无正文
		return map[string]any{}, nil
	case resp.StatusCode == http.StatusNotFound && sid != "":
		// 会话已失效：交给调用方重新 initialize
		return nil, errMCPSessionExpired
	default:
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	// streamable-http 服务器可能以 text/event-stream（SSE）响应：
	// 每行 "data: <json>\n\n" 格式。
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/event-stream") {
		return ParseSSEResponse(respBody)
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("invalid JSON response: %w", err)
	}
	return result, nil
}

// setProtocolVersion 记住 initialize 协商出的版本（后续请求要带）。
func (t *httpTransport) setProtocolVersion(v string) {
	if v == "" {
		return
	}
	t.mu.Lock()
	t.protocolVersion = v
	t.mu.Unlock()
}

// ParseSSEResponse 从 SSE 响应体提取首个 data: 行的 JSON 负载。
// MCP streamable-http 传输的 SSE 格式：每行 "data: <json>\n\n"，
// 客户端取首个事件 data 字段解析为 JSON-RPC 响应。
func ParseSSEResponse(body []byte) (map[string]any, error) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data: ") {
			jsonData := strings.TrimPrefix(line, "data: ")
			var result map[string]any
			if err := json.Unmarshal([]byte(jsonData), &result); err != nil {
				return nil, fmt.Errorf("invalid SSE data JSON: %w", err)
			}
			return result, nil
		}
	}
	return nil, fmt.Errorf("no data: line found in SSE response")
}

// ============================================================
// stdio（子进程）
// ============================================================

// MCPStdioSpec 是启动一个 stdio MCP 服务器所需的全部信息。
//
// Command/Args 是命令行，Env 只会追加到**白名单基线**上（见 mcpStdioEnv）——
// MCP 服务器是第三方代码，不能把宿主的整份环境（含各家 API key）灌给它。
type MCPStdioSpec struct {
	Command string
	Args    []string
	Env     map[string]string
	CWD     string
}

// stdioShutdownGrace 关 stdin 之后等子进程自行退出的时间，超时才 Kill。
const stdioShutdownGrace = 3 * time.Second

// mcpStdioBaseEnv 传给 MCP 子进程的环境变量白名单（大小写不敏感匹配）。
//
// 只给「能启动进程、能找到自己的运行时」所必需的那些：PATH 及其 Windows 扩展、
// 系统根、家目录、临时目录。**不给**任何 DSC_* / *_API_KEY —— 需要密钥的服务器
// 应由用户在配置里显式写进 env（那是明确的授权动作，不是隐式继承）。
var mcpStdioBaseEnv = []string{
	"PATH", "PATHEXT", "SystemRoot", "SYSTEMDRIVE", "COMSPEC", "WINDIR",
	"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA",
	"TEMP", "TMP", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"SHELL", "TERM", "NUMBER_OF_PROCESSORS", "PROCESSOR_ARCHITECTURE",
	"PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432",
	"NODE_PATH", "NODE_OPTIONS", "PYTHONPATH", "PYTHONIOENCODING",
	"JAVA_HOME", "GOROOT", "GOPATH", "CARGO_HOME", "RUSTUP_HOME",
}

// mcpStdioEnv 组装子进程环境：白名单基线 + 调用方显式给的 env（后者覆盖前者）。
//
// 排序输出：同一份配置在不同机器上环境块字节一致，便于比对与排查（也避免
// Windows 上「环境块顺序不同 → 进程行为不同」这种难查的差异）。
func mcpStdioEnv(extra map[string]string) []string {
	env := map[string]string{}
	for _, key := range mcpStdioBaseEnv {
		if v, ok := os.LookupEnv(key); ok {
			env[strings.ToUpper(key)] = key + "=" + v
		}
	}
	for k, v := range extra {
		if k == "" {
			continue
		}
		env[strings.ToUpper(k)] = k + "=" + v
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		out = append(out, kv)
	}
	sort.Strings(out)
	return out
}

type stdioTransport struct {
	spec   MCPStdioSpec
	logger hclog.Logger

	// mu 串行化往返，并保护下面这些字段的快照（stdin/lines/done/cmd）。
	// **进程退出路径不取它**：等待 goroutine 若为拿锁而阻塞，会让正在等待响应的
	// RoundTrip 永远等不到 done 关闭（自锁）。退出信息走 exitMu。
	mu    sync.Mutex
	stdin io.WriteCloser
	lines chan []byte
	done  chan struct{}
	cmd   *exec.Cmd

	exitMu  sync.Mutex
	closed  bool
	doneErr error
}

func newStdioTransport(spec MCPStdioSpec, logger hclog.Logger) (*stdioTransport, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return nil, fmt.Errorf("stdio MCP server needs a command")
	}
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	t := &stdioTransport{
		spec:   spec,
		logger: logger,
		lines:  make(chan []byte, 32),
		done:   make(chan struct{}),
	}
	if err := t.start(); err != nil {
		return nil, err
	}
	return t, nil
}

// start 拉起子进程并接上三条管道。
//
// stderr 必须持续消费：管道写满会把服务器卡死在 write 上（看起来像「工具调用无响应」）。
// 这里逐行转进宿主日志，前缀带服务器名，便于对上是谁在报错。
func (t *stdioTransport) start() error {
	cmd := exec.Command(t.spec.Command, t.spec.Args...)
	// Windows：隐藏子进程控制台窗口（否则每拉一个 MCP 服务器都弹一个黑框）。
	// 与 lsp / lua 的 spawn 站点同一防护网（proc_attr_regression_test 会检查）。
	ConfigureChildProcessAttrs(cmd)
	cmd.Dir = t.spec.CWD
	cmd.Env = mcpStdioEnv(t.spec.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdio MCP server stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdio MCP server stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stdio MCP server stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start stdio MCP server %q: %w", t.describeCmd(), err)
	}

	lines, done := t.lines, t.done
	t.mu.Lock()
	t.cmd, t.stdin = cmd, stdin
	t.mu.Unlock()

	t.logger.Info("mcp stdio server started", "command", t.describeCmd(), "pid", cmd.Process.Pid)

	go t.readStdout(stdout, lines, done)
	go t.drainStderr(stderr)
	go func() {
		err := cmd.Wait()
		t.exitMu.Lock()
		if !t.closed {
			// 进程自己退出（崩溃 / 主动结束）：让等待中的往返立刻失败，而不是挂住
			t.doneErr = fmt.Errorf("stdio MCP server %q exited: %v", t.describeCmd(), err)
		}
		t.exitMu.Unlock()
		close(done)
	}()
	return nil
}

// readStdout 逐行读响应。用 ReadBytes 而不是 bufio.Scanner：工具结果可能上兆，
// Scanner 的 token 上限会把它截断成「invalid JSON」。
func (t *stdioTransport) readStdout(r io.Reader, lines chan []byte, done chan struct{}) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			select {
			case lines <- line:
			case <-done:
				return
			}
		}
		if err != nil {
			return // EOF 或读错误：由 wait goroutine 统一收口
		}
	}
}

func (t *stdioTransport) drainStderr(r io.Reader) {
	br := bufio.NewReaderSize(r, 16*1024)
	for {
		line, err := br.ReadBytes('\n')
		if s := strings.TrimSpace(string(line)); s != "" {
			t.logger.Debug("mcp stdio stderr", "server", t.spec.Command, "line", s)
		}
		if err != nil {
			return
		}
	}
}

func (t *stdioTransport) Kind() string { return "stdio" }

func (t *stdioTransport) Describe() string { return "stdio: " + t.describeCmd() }

func (t *stdioTransport) describeCmd() string {
	parts := append([]string{t.spec.Command}, t.spec.Args...)
	return strings.Join(parts, " ")
}

// Reset：stdio 没有「会话」，只有「进程」。进程还活着就什么都不做；已经退出就重启
// 一个（对齐 HTTP 侧 404 重连的语义：下一次调用前恢复可用状态）。
func (t *stdioTransport) Reset(_ hclog.Logger) error {
	select {
	case <-t.done:
	default:
		return nil // 还活着
	}
	t.mu.Lock()
	t.lines = make(chan []byte, 32)
	t.done = make(chan struct{})
	t.stdin = nil
	t.cmd = nil
	t.mu.Unlock()
	t.exitMu.Lock()
	t.closed = false
	t.doneErr = nil
	t.exitMu.Unlock()
	return t.start()
}

// Close 关 stdin 让服务器正常退出，超时才强杀。
func (t *stdioTransport) Close() {
	t.exitMu.Lock()
	if t.closed {
		t.exitMu.Unlock()
		return
	}
	t.closed = true
	t.exitMu.Unlock()

	t.mu.Lock()
	stdin, cmd, done := t.stdin, t.cmd, t.done
	t.mu.Unlock()

	if stdin != nil {
		_ = stdin.Close() // 多数 MCP 服务器见到 stdin EOF 就退出
	}
	if cmd == nil || cmd.Process == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(stdioShutdownGrace):
		_ = cmd.Process.Kill()
		<-done
	}
}

// RoundTrip 写一行 JSON-RPC，读回匹配 id 的那一行。
//
// 读循环里遇到的三类消息都要正确处理，否则会「调用无响应」：
//   - 响应（有 id、无 method）：id 匹配就返回，不匹配说明是更早某次调用的迟到响应，
//     丢弃（我们的 id 单调递增，不会重复）；
//   - 服务器→客户端请求（有 id、有 method）：按规范回 -32601（我们不声明 sampling /
//     roots 能力），不回会让服务器一直等；
//   - 通知（无 id）：忽略。
func (t *stdioTransport) RoundTrip(ctx context.Context, req map[string]any) (map[string]any, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	stdin, lines, done := t.stdin, t.lines, t.done
	select {
	case <-done:
		return nil, t.exitErr()
	default:
	}
	if stdin == nil {
		return nil, fmt.Errorf("stdio MCP server is not running")
	}

	payload, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := stdin.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("write to stdio MCP server: %w", err)
	}

	// 通知没有 id，服务器不会回应（MCP 规定 notifications/initialized 无响应）。
	wantID, hasID := req["id"]
	if !hasID {
		return map[string]any{}, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
			return nil, t.exitErr()
		case line := <-lines:
			var msg map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(line), &msg); err != nil {
				t.logger.Debug("mcp stdio non-JSON line", "line", string(line))
				continue
			}
			if _, isRequest := msg["method"]; isRequest {
				if rid, hasID := msg["id"]; hasID {
					t.replyMethodNotFound(stdin, rid, msg)
				}
				continue
			}
			if mid, ok := msg["id"]; ok && mcpIDEqual(mid, wantID) {
				return msg, nil
			}
			// 迟到/无关响应：丢弃
		}
	}
}

// exitErr 取进程退出原因（不取 t.mu，避免与 RoundTrip 互锁）。
func (t *stdioTransport) exitErr() error {
	t.exitMu.Lock()
	defer t.exitMu.Unlock()
	if t.doneErr != nil {
		return t.doneErr
	}
	return fmt.Errorf("stdio MCP server %q is not running", t.describeCmd())
}

// replyMethodNotFound 回应服务器发起的请求：我们不让服务器反向调用客户端能力
// （sampling / roots / elicitation 都没声明），明确回 -32601 比静默挂起好。
func (t *stdioTransport) replyMethodNotFound(stdin io.Writer, id any, msg map[string]any) {
	method, _ := msg["method"].(string)
	resp := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error": map[string]any{
			"code":    -32601,
			"message": "method not supported by dsc client: " + method,
		},
	}
	payload, err := json.Marshal(resp)
	if err != nil || stdin == nil {
		return
	}
	_, _ = stdin.Write(append(payload, '\n'))
}

// mcpIDEqual 比较 JSON-RPC id。我们发的是整数，服务端回 JSON 数字（解析成 float64），
// 也可能是字符串（少数实现），所以统一按文本比较。
func mcpIDEqual(a, b any) bool {
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}
