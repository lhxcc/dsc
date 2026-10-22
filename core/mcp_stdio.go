package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MCP stdio 传输：与 MCP 服务器子进程经 stdin/stdout 交换「一行一个 JSON-RPC
// 消息」（MCP 规范 stdio transport）。请求按 id 关联响应，服务端通知（无 id）
// 直接忽略；stderr 只排空、不混入协议流。
//
// 进程生命周期与 MCPClient 对齐：Connect 时启动、Disconnect 时关闭 stdin 并
// Kill（超时兜底），避免宿主退出后留下孤儿 MCP 进程。

// mcpStdioSpec 启动 stdio MCP 服务器所需参数（Connect 时拉起进程）。
type mcpStdioSpec struct {
	Command string
	Args    []string
	Env     []string // KEY=VALUE 追加到宿主环境
	WorkDir string
}

// mcpRPCResult 一次请求的响应或失败。
type mcpRPCResult struct {
	result map[string]any
	err    error
}

// mcpStdioTransport 管理 MCP stdio 子进程与 JSON-RPC 收发。
type mcpStdioTransport struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[int64]chan mcpRPCResult
	closed    bool
	closedErr error

	nextID    atomic.Int64
	done      chan struct{}
	closeOnce sync.Once
}

// startMCPStdioTransport 启动 MCP 服务器子进程并开始读取响应。
func startMCPStdioTransport(spec mcpStdioSpec) (*mcpStdioTransport, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return nil, fmt.Errorf("MCP stdio: command is required")
	}
	cmd := exec.Command(spec.Command, spec.Args...)
	if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	// Windows 下隐藏 MCP 服务器控制台窗口（与其他子进程一致）。
	ConfigureChildProcessAttrs(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("MCP stdio: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("MCP stdio: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("MCP stdio: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("MCP stdio: start %q: %w", spec.Command, err)
	}

	t := &mcpStdioTransport{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		pending: make(map[int64]chan mcpRPCResult),
		done:    make(chan struct{}),
	}
	go t.readLoop()
	go func() { _, _ = io.Copy(io.Discard, stderr) }()
	go func() {
		_ = cmd.Wait()
		t.failPending(fmt.Errorf("MCP stdio server exited"))
		close(t.done)
	}()
	return t, nil
}

// Call 发送带 id 的 JSON-RPC 请求并等待响应。
func (t *mcpStdioTransport) Call(ctx context.Context, req map[string]any) (map[string]any, error) {
	id := t.nextID.Add(1)
	req["id"] = id
	ch := make(chan mcpRPCResult, 1)

	t.mu.Lock()
	if t.closed {
		err := t.closedErr
		t.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("MCP stdio transport closed")
		}
		return nil, err
	}
	t.pending[id] = ch
	t.mu.Unlock()

	if err := t.writeLine(req); err != nil {
		t.dropPending(id)
		return nil, err
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		// 与 HTTP doJSON 保持同一形状：返回完整 JSON-RPC 信封（含 result）。
		return map[string]any{"result": r.result}, nil
	case <-ctx.Done():
		t.dropPending(id)
		return nil, ctx.Err()
	}
}

// Notify 发送无 id 的 JSON-RPC 通知（不等待响应）。
func (t *mcpStdioTransport) Notify(req map[string]any) error {
	delete(req, "id")
	return t.writeLine(req)
}

// Close 关闭 stdin 并终止子进程（2 秒内未退出则强杀），等待回收。
func (t *mcpStdioTransport) Close() error {
	t.closeOnce.Do(func() {
		_ = t.stdin.Close()
		if t.cmd.Process != nil {
			_ = t.cmd.Process.Kill()
		}
		select {
		case <-t.done:
		case <-time.After(2 * time.Second):
		}
	})
	return nil
}

// writeLine 以一行 JSON 写入 stdin（写操作串行化）。
func (t *mcpStdioTransport) writeLine(req map[string]any) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("MCP stdio write: %w", err)
	}
	return nil
}

// readLoop 逐行读取 stdout，按 id 投递响应；无 id 的服务端通知忽略。
func (t *mcpStdioTransport) readLoop() {
	sc := bufio.NewScanner(t.stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 容忍大工具结果
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Result map[string]any  `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue // 非协议行（服务端日志等）忽略
		}
		if len(msg.ID) == 0 || string(msg.ID) == "null" {
			continue // 服务端通知，无响应
		}
		id, ok := mcpParseID(msg.ID)
		if !ok {
			continue
		}
		t.mu.Lock()
		ch := t.pending[id]
		delete(t.pending, id)
		t.mu.Unlock()
		if ch == nil {
			continue
		}
		if msg.Error != nil {
			ch <- mcpRPCResult{err: fmt.Errorf("MCP error %d: %s", msg.Error.Code, msg.Error.Message)}
			continue
		}
		ch <- mcpRPCResult{result: msg.Result}
	}
	t.failPending(fmt.Errorf("MCP stdio server closed stdout"))
}

// dropPending 移除已取消/写失败的请求登记。
func (t *mcpStdioTransport) dropPending(id int64) {
	t.mu.Lock()
	delete(t.pending, id)
	t.mu.Unlock()
}

// failPending 进程退出时让所有在途请求立即失败，避免调用方等待到超时。
func (t *mcpStdioTransport) failPending(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.closedErr = err
	pending := t.pending
	t.pending = make(map[int64]chan mcpRPCResult)
	t.mu.Unlock()
	for _, ch := range pending {
		select {
		case ch <- mcpRPCResult{err: err}:
		default:
		}
	}
}

// mcpParseID 解析 JSON-RPC id（兼容整数与浮点表示）。
func mcpParseID(raw json.RawMessage) (int64, bool) {
	var i int64
	if err := json.Unmarshal(raw, &i); err == nil {
		return i, true
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return int64(f), true
	}
	return 0, false
}
