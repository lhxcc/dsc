package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// LSP JSON-RPC 2.0 over stdio：消息以 `Content-Length: N\r\n\r\n<body>` 分帧。
// 本文件负责分帧、请求/响应关联、服务端通知（publishDiagnostics）与文档同步，
// 把 lsp.go 从「只起进程 + 缓存」升级为完整握手客户端。

// lspRPCResult 一次 LSP 请求的结果或失败。
type lspRPCResult struct {
	result json.RawMessage
	err    error
}

// lspFrameContentLength 从消息头读取 Content-Length；非法帧返回错误。
func lspReadMessage(r *bufio.Reader) ([]byte, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "content-length") {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("LSP: invalid Content-Length %q", v)
			}
			length = n
		}
	}
	if length < 0 {
		return nil, fmt.Errorf("LSP: missing Content-Length header")
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// lspWriteMessage 以 LSP 帧格式写入一条 JSON 消息（写操作串行化）。
func (c *LSPClient) lspWriteMessage(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var frame bytes.Buffer
	fmt.Fprintf(&frame, "Content-Length: %d\r\n\r\n", len(b))
	frame.Write(b)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.stdin == nil {
		return fmt.Errorf("LSP: server not running")
	}
	_, err = c.stdin.Write(frame.Bytes())
	return err
}

// call 发送带 id 的请求并等待响应。
func (c *LSPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan lspRPCResult, 1)

	c.mu.Lock()
	if !c.started {
		c.mu.Unlock()
		return nil, fmt.Errorf("LSP: server not running")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := c.lspWriteMessage(req); err != nil {
		c.dropPending(id)
		return nil, err
	}
	select {
	case r := <-ch:
		return r.result, r.err
	case <-ctx.Done():
		c.dropPending(id)
		return nil, ctx.Err()
	}
}

// notify 发送无 id 的通知。
func (c *LSPClient) notify(method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		req["params"] = params
	}
	return c.lspWriteMessage(req)
}

// dropPending 移除已取消的请求登记。
func (c *LSPClient) dropPending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// failPendingLocked 在进程退出时让在途请求立即失败；需已持有 c.mu。
func (c *LSPClient) failPendingLocked(err error) {
	for id, ch := range c.pending {
		select {
		case ch <- lspRPCResult{err: err}:
		default:
		}
		delete(c.pending, id)
	}
}

// readLoop 读取并分发服务端消息：响应按 id 投递；通知（publishDiagnostics 等）
// 更新缓存；服务端请求（configuration/registerCapability 等）回一个空结果，
// 避免真实语言服务器因等待客户端响应而阻塞。
func (c *LSPClient) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		raw, err := lspReadMessage(br)
		if err != nil {
			c.mu.Lock()
			c.failPendingLocked(fmt.Errorf("LSP: read: %w", err))
			c.mu.Unlock()
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 && string(msg.ID) != "null" {
				c.respondServerRequest(msg.ID, msg.Method)
			} else {
				c.handleServerNotification(msg.Method, msg.Params)
			}
			continue
		}
		if len(msg.ID) == 0 || string(msg.ID) == "null" {
			continue
		}
		id, ok := parseJSONRPCID(msg.ID) // 整数/浮点兼容：LSP 服务端回显的 id 可能是任一形态
		if !ok {
			continue
		}
		c.mu.Lock()
		ch := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()
		if ch == nil {
			continue
		}
		if msg.Error != nil {
			ch <- lspRPCResult{err: fmt.Errorf("LSP error %d: %s", msg.Error.Code, msg.Error.Message)}
			continue
		}
		ch <- lspRPCResult{result: msg.Result}
	}
}

// respondServerRequest 回应服务端发起的请求（本项目不需要这些能力，回空结果）。
func (c *LSPClient) respondServerRequest(id json.RawMessage, method string) {
	var result any
	switch method {
	case "workspace/configuration":
		result = []any{}
	case "workspace/workspaceFolders":
		result = []any{}
	case "client/registerCapability", "client/unregisterCapability",
		"window/workDoneProgress/create", "window/showMessageRequest":
		result = nil
	default:
		result = nil
	}
	_ = c.lspWriteMessage(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

// handleServerNotification 处理服务端通知：当前只关心 publishDiagnostics。
func (c *LSPClient) handleServerNotification(method string, params json.RawMessage) {
	if method != "textDocument/publishDiagnostics" {
		return
	}
	var p struct {
		URI         string          `json:"uri"`
		Diagnostics []LSPDiagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
		return
	}
	filePath := lspPathFromURI(p.URI)
	if filePath == "" {
		return
	}
	c.mu.Lock()
	c.diagCache[filePath] = p.Diagnostics
	wait := c.diagWait[p.URI]
	delete(c.diagWait, p.URI)
	c.mu.Unlock()
	if wait != nil {
		close(wait)
	}
}

// lspInitializeParams 构造 initialize 请求参数（workspace 根 + 诊断订阅能力）。
func lspInitializeParams(rootPath string) map[string]any {
	rootURI := lspFileURI(rootPath)
	return map[string]any{
		"processId": os.Getpid(),
		"rootPath":  rootPath,
		"rootUri":   rootURI,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"synchronization":    map[string]any{"dynamicRegistration": false},
				"publishDiagnostics": map[string]any{"relatedInformation": true},
				"hover":              map[string]any{"contentFormat": []any{"markdown", "plaintext"}},
				"definition":         map[string]any{"linkSupport": true},
			},
			"workspace": map[string]any{
				"configuration":    true,
				"workspaceFolders": true,
			},
		},
		"workspaceFolders": []any{map[string]any{
			"uri":  rootURI,
			"name": path.Base(strings.ReplaceAll(rootPath, "\\", "/")),
		}},
	}
}

// syncDocument 确保语言服务器已打开该文件；内容变化时发送全量 didChange。
func (c *LSPClient) syncDocument(ctx context.Context, filePath, uri string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("LSP: read %s: %w", filePath, err)
	}
	text := string(data)

	c.mu.Lock()
	version, opened := c.documents[uri]
	prev := c.documentText[uri]
	if !opened {
		c.documents[uri] = 1
		c.documentText[uri] = text
	} else if prev != text {
		version++
		c.documents[uri] = version
		c.documentText[uri] = text
	}
	c.mu.Unlock()

	if !opened {
		return c.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri":        uri,
				"languageId": c.language,
				"version":    1,
				"text":       text,
			},
		})
	}
	if prev != text {
		return c.notify("textDocument/didChange", map[string]any{
			"textDocument":   map[string]any{"uri": uri, "version": version},
			"contentChanges": []any{map[string]any{"text": text}},
		})
	}
	return nil
}

// expectDiagnostics 注册一次诊断等待；服务端 publishDiagnostics 到达时关闭。
func (c *LSPClient) expectDiagnostics(uri string) chan struct{} {
	ch := make(chan struct{})
	c.mu.Lock()
	c.diagWait[uri] = ch
	c.mu.Unlock()
	return ch
}

// cancelExpectDiagnostics 撤销等待（发送 didOpen/didChange 失败时）。
func (c *LSPClient) cancelExpectDiagnostics(uri string, ch chan struct{}) {
	c.mu.Lock()
	if c.diagWait[uri] == ch {
		delete(c.diagWait, uri)
	}
	c.mu.Unlock()
}

// lspWaitDiagnostics 等待诊断通知，超时/取消返回（调用方读缓存兜底）。
func lspWaitDiagnostics(ctx context.Context, ch chan struct{}, timeout time.Duration) {
	if ch == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ch:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// parseJSONRPCID 解析 JSON-RPC 响应 id（兼容整数与浮点表示）。
//
// LSP 服务端回显的 id 可能是 3 或 3.0；两者都要认，否则响应会因「认不出 id」被丢弃、
// 调用方一直等到超时。
func parseJSONRPCID(raw json.RawMessage) (int64, bool) {
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

// lspFileURI 把文件系统路径转成 file:// URI（统一正斜杆；Windows 盘符前补 "/"）。
func lspFileURI(p string) string {
	q := strings.ReplaceAll(p, `\\`, "/")
	q = strings.ReplaceAll(q, "\\", "/")
	q = PClean(q)
	if q == "" {
		return ""
	}
	if !strings.HasPrefix(q, "/") {
		q = "/" + q
	}
	return "file://" + (&url.URL{Path: q}).EscapedPath()
}

// lspPathFromURI 把 file:// URI 还原为本地路径（解码 %XX；Windows 去掉盘符前导 "/"）。
func lspPathFromURI(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	p, err := url.PathUnescape(u.Path)
	if err != nil {
		return ""
	}
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return PClean(p)
}
