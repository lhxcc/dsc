package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// LSP 工具（对齐 DSH packages/lsp/lsp + lsp-stdio + tool-lsp）。
//
// DSH 的 LSP 是一个 Service + 工具，桥接外部 Language Server Protocol 服务器
// （如 gopls、typescript-language-server、pyright 等），把诊断信息（errors/warnings）、
// hover、定义跳转等暴露给模型。
//
// DSC 的适配：LSPClient 管理一个 stdio LSP 子进程，提供：
//   - Start：启动 LSP 服务器子进程并完成 initialize/initialized 握手
//   - Diagnostics：同步文档（didOpen/didChange）并获取诊断
//   - Stop：shutdown/exit 后停止子进程
//
// 当前实现支持 stdio 传输（LSP 标准传输之一）。

// LSPClient 管理一个 LSP 服务器子进程。
type LSPClient struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	serverID string
	language string
	rootPath string
	started  bool
	// diagnostics 缓存：file path → diagnostics
	diagCache map[string][]LSPDiagnostic

	// JSON-RPC 运行态（见 lsp_rpc.go）
	writeMu      sync.Mutex
	pending      map[int64]chan lspRPCResult
	nextID       atomic.Int64
	documents    map[string]int    // uri -> version
	documentText map[string]string // uri -> 最近一次同步的全文
	diagWait     map[string]chan struct{}
	done         chan struct{}
}

// LSPDiagnostic 一条诊断信息。
type LSPDiagnostic struct {
	Range    LSPRange `json:"range"`
	Severity int      `json:"severity"` // 1=error, 2=warning, 3=info, 4=hint
	Source   string   `json:"source"`
	Message  string   `json:"message"`
}

// LSPRange 诊断范围。
type LSPRange struct {
	Start LSPPosition `json:"start"`
	End   LSPPosition `json:"end"`
}

// LSPPosition 行列位置（0-based）。
type LSPPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

// LSPSeverity 诊断严重程度（对齐 LSP 协议 DiagnosticSeverity）。
const (
	LSPSeverityError   = 1
	LSPSeverityWarning = 2
	LSPSeverityInfo    = 3
	LSPSeverityHint    = 4
)

// NewLSPClient 创建 LSP 客户端。
// language 为编程语言（如 "go"、"typescript"、"python"），
// rootPath 为工作区根路径，serverID 为 LSP 服务器标识。
func NewLSPClient(language, rootPath, serverID string) *LSPClient {
	return &LSPClient{
		language:     language,
		rootPath:     rootPath,
		serverID:     serverID,
		diagCache:    make(map[string][]LSPDiagnostic),
		pending:      make(map[int64]chan lspRPCResult),
		documents:    make(map[string]int),
		documentText: make(map[string]string),
		diagWait:     make(map[string]chan struct{}),
	}
}

// Start 启动 LSP 服务器子进程并完成 initialize/initialized 握手。
// command 为 LSP 服务器可执行文件路径，args 为启动参数。
//
// 握手完成后，GetDiagnostics 会按文件内容发送 didOpen/didChange，并等待
// 服务端 publishDiagnostics 通知更新缓存（见 lsp_rpc.go）。
func (c *LSPClient) Start(ctx context.Context, command string, args ...string) error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return fmt.Errorf("LSP server already started")
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = c.rootPath
	// Windows 上隐藏 LSP 服务器子进程控制台，避免 TUI 中终端窗口闪烁
	ConfigureChildProcessAttrs(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("LSP stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("LSP stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("LSP stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("LSP start failed: %w", err)
	}

	c.cmd = cmd
	c.stdin = stdin
	c.stdout = stdout
	c.started = true
	c.pending = make(map[int64]chan lspRPCResult)
	c.documents = make(map[string]int)
	c.documentText = make(map[string]string)
	c.diagWait = make(map[string]chan struct{})
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()

	go c.readLoop(stdout)
	// stderr 只排空：语言服务器日志不能混入 JSON-RPC 协议流。
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go func() {
		_ = cmd.Wait()
		c.mu.Lock()
		c.started = false
		c.failPendingLocked(fmt.Errorf("LSP: server exited"))
		c.mu.Unlock()
		close(done)
	}()

	initCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := c.call(initCtx, "initialize", lspInitializeParams(c.rootPath)); err != nil {
		_ = c.Stop()
		return fmt.Errorf("LSP initialize failed: %w", err)
	}
	if err := c.notify("initialized", map[string]any{}); err != nil {
		_ = c.Stop()
		return fmt.Errorf("LSP initialized notification failed: %w", err)
	}
	return nil
}

// Stop 优雅停止 LSP 子进程：shutdown 请求 → exit 通知 → 关闭 stdin → Kill（兜底）。
// 发送 Kill 后等待最多 5 秒子进程退出，避免遗留僵尸进程。
func (c *LSPClient) Stop() error {
	c.mu.Lock()
	if !c.started || c.cmd == nil {
		c.mu.Unlock()
		return nil
	}
	stdin := c.stdin
	cmd := c.cmd
	done := c.done
	c.mu.Unlock()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, _ = c.call(shutdownCtx, "shutdown", nil)
	cancel()
	_ = c.notify("exit", nil)
	if stdin != nil {
		_ = stdin.Close()
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	if done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}

	c.mu.Lock()
	c.started = false
	c.cmd = nil
	c.stdin = nil
	c.stdout = nil
	c.diagCache = make(map[string][]LSPDiagnostic)
	c.pending = make(map[int64]chan lspRPCResult)
	c.documents = make(map[string]int)
	c.documentText = make(map[string]string)
	c.diagWait = make(map[string]chan struct{})
	c.mu.Unlock()
	return nil
}

// SetDiagnostics 设置文件的诊断信息（供 LSP 通知推送后缓存）。
func (c *LSPClient) SetDiagnostics(filePath string, diags []LSPDiagnostic) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.diagCache[filePath] = diags
}

// GetDiagnostics 获取文件的诊断信息：已连接 LSP 服务器时先同步文档
// （didOpen/didChange）并等待 publishDiagnostics 通知，再返回缓存；未启动时
// 直接返回外部 SetDiagnostics 写入的缓存。
func (c *LSPClient) GetDiagnostics(ctx context.Context, filePath string) ([]LSPDiagnostic, error) {
	p := PClean(strings.ReplaceAll(filePath, "\\", "/"))

	c.mu.Lock()
	started := c.started
	c.mu.Unlock()
	if started {
		uri := lspFileURI(p)
		wait := c.expectDiagnostics(uri)
		if err := c.syncDocument(ctx, p, uri); err != nil {
			c.cancelExpectDiagnostics(uri, wait)
			return nil, err
		}
		lspWaitDiagnostics(ctx, wait, 2*time.Second)
		// 超时/取消后撤销等待登记，避免迟到的 publishDiagnostics 关闭无人接收的通道。
		c.cancelExpectDiagnostics(uri, wait)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	diags := c.diagCache[p]
	return append([]LSPDiagnostic(nil), diags...), nil
}

// GetAllDiagnostics 获取所有文件的诊断信息。
func (c *LSPClient) GetAllDiagnostics(ctx context.Context) (map[string][]LSPDiagnostic, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := make(map[string][]LSPDiagnostic, len(c.diagCache))
	for k, v := range c.diagCache {
		out[k] = v
	}
	return out, nil
}

// FormatDiagnostics 格式化诊断信息为模型可读文本。
func FormatDiagnostics(filePath string, diags []LSPDiagnostic) string {
	if len(diags) == 0 {
		return fmt.Sprintf("No diagnostics for %s", filePath)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Diagnostics for %s (%d issues):\n", filePath, len(diags)))
	for _, d := range diags {
		severity := severityLabel(d.Severity)
		sb.WriteString(fmt.Sprintf("  [%s] Line %d:%d — %s",
			severity, d.Range.Start.Line+1, d.Range.Start.Character+1, d.Message))
		if d.Source != "" {
			sb.WriteString(fmt.Sprintf(" (%s)", d.Source))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// severityLabel 把数字严重程度转为人读标签。
func severityLabel(severity int) string {
	switch severity {
	case LSPSeverityError:
		return "ERROR"
	case LSPSeverityWarning:
		return "WARN"
	case LSPSeverityInfo:
		return "INFO"
	case LSPSeverityHint:
		return "HINT"
	default:
		return "?"
	}
}

// LSPTool 模型可调用的 LSP 诊断工具（对齐 DSH tool-lsp）。
// client 非 nil 时查询该客户端；client 为 nil 时聚合 Manager 所有已启动 LSPClient
// （由 admin API /lsp/start 注册的实例）的诊断缓存——支持多 LSP 服务器并存。
type LSPTool struct {
	client  *LSPClient
	manager *Manager // client 为 nil 时用此聚合所有 lspClients
}

// NewLSPTool 创建 LSP 诊断工具。client 非 nil 时绑定单实例；
// client 为 nil 时须配合 SetManager 注入聚合源。
func NewLSPTool(client *LSPClient) *LSPTool {
	return &LSPTool{client: client}
}

// SetManager 注入 Manager 供 client=nil 时聚合所有 LSPClient 诊断。
func (t *LSPTool) SetManager(m *Manager) {
	t.manager = m
}

func (t *LSPTool) Name() string { return "lsp_diagnostics" }

func (t *LSPTool) Description() string {
	return "Get language server diagnostics (errors, warnings) for a file or all files. " +
		"Requires an LSP server to be running. Pass a file path to get diagnostics for that file, " +
		"or omit to get all diagnostics across the workspace."
}

func (t *LSPTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
                "type": "object",
                "properties": {
                        "file_path": {"type": "string", "description": "File path to get diagnostics for. Omit for all files."}
                }
        }`)
}

func (t *LSPTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	// client 非 nil：单实例查询
	if t.client != nil {
		if p.FilePath != "" {
			diags, err := t.client.GetDiagnostics(ctx, p.FilePath)
			if err != nil {
				return "", err
			}
			return FormatDiagnostics(p.FilePath, diags), nil
		}
		allDiags, err := t.client.GetAllDiagnostics(ctx)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for filePath, diags := range allDiags {
			sb.WriteString(FormatDiagnostics(filePath, diags))
			sb.WriteString("\n")
		}
		if sb.Len() == 0 {
			return "No diagnostics available. Make sure the LSP server is running.", nil
		}
		return sb.String(), nil
	}

	// client 为 nil：聚合 Manager 所有已启动 LSPClient
	if t.manager == nil {
		return "No diagnostics available. No LSP server configured.", nil
	}
	allDiags, err := t.manager.lspDiagnosticsAll(ctx, p.FilePath)
	if err != nil {
		return "", err
	}
	if p.FilePath != "" {
		diags := allDiags[p.FilePath]
		return FormatDiagnostics(p.FilePath, diags), nil
	}
	var sb strings.Builder
	for filePath, diags := range allDiags {
		sb.WriteString(FormatDiagnostics(filePath, diags))
		sb.WriteString("\n")
	}
	if sb.Len() == 0 {
		return "No diagnostics available. Make sure the LSP server is running.", nil
	}
	return sb.String(), nil
}
