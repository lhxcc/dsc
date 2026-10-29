// Package mcp 是一个不依赖宿主的 MCP 客户端：协议流程（initialize → initialized →
// tools/list → tools/call）加两种传输（streamable HTTP、stdio 子进程）。
//
// 它**不知道** DSC 的存在：不认识工具注册表、不认识审批与曝光、不决定工具叫什么
// 名字。哪些工具进模型目录、叫什么名字、要不要审批，都是调用方的事。这样同一份客户端
// 既能被宿主内建使用，也能被任意插件整块拿去——MCP 从核心搬到插件时不必重写协议，
// 也不会有第二份实现漂移。
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// defaultProtocolVersion 握手时请求的协议版本（服务端可回落到自己支持的版本）。
const defaultProtocolVersion = "2024-11-05"

// Tool 服务器声明的工具（尚未命名、尚未注册）。
type Tool struct {
	// Name 服务器侧的原始名（tools/call 用它）。
	Name string
	// Description 服务器给的说明。
	Description string
	// Schema 输入参数的 JSON Schema（原始字节，可能为空对象）。
	Schema json.RawMessage
}

// Client 一个 MCP 服务器会话。
//
// 生命周期：New* → Connect（握手 + 发现工具）→ Tools/CallTool → Disconnect。
// Connect 可重复调用：HTTP 会重新握手拿新会话，stdio 会在子进程退出后重启它。
type Client struct {
	serverName string
	transport  Transport
	logger     Logger

	mu     sync.Mutex
	tools  []Tool
	nextID int64
}

// NewHTTPClient 连接一个 streamable HTTP 端点的客户端。
func NewHTTPClient(serverName, endpoint string, logger Logger) *Client {
	return NewClient(serverName, newHTTPTransport(endpoint), logger)
}

// NewStdioClient 拉起一个本地子进程作为服务器。
//
// 子进程的环境是**白名单基线 + spec.Env**（见 StdioSpec）：MCP 服务器是第三方代码，
// 不该顺手继承调用方进程的全部环境变量。
func NewStdioClient(serverName string, spec StdioSpec, logger Logger) (*Client, error) {
	tr, err := newStdioTransport(spec, logger)
	if err != nil {
		return nil, err
	}
	return NewClient(serverName, tr, logger), nil
}

// NewClient 用给定传输构造客户端（便于测试与自定义通道）。
func NewClient(serverName string, transport Transport, logger Logger) *Client {
	if logger == nil {
		logger = nopLogger{}
	}
	return &Client{serverName: serverName, transport: transport, logger: logger}
}

// ServerName 服务器名（调用方用它组工具名前缀）。
func (c *Client) ServerName() string { return c.serverName }

// Kind 传输类型：http / stdio。
func (c *Client) Kind() string {
	if c.transport == nil {
		return ""
	}
	return c.transport.Kind()
}

// Describe 一行传输描述（HTTP 是 URL，stdio 是命令行），用于日志与状态展示。
func (c *Client) Describe() string {
	if c.transport == nil {
		return ""
	}
	return c.transport.Describe()
}

// Tools 已发现的工具（按名排序，结果稳定）。
func (c *Client) Tools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Tool, len(c.tools))
	copy(out, c.tools)
	return out
}

// ToolCount 已发现的工具数。
func (c *Client) ToolCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tools)
}

// Connect 握手并发现工具。
func (c *Client) Connect(ctx context.Context) error {
	if c.transport == nil {
		return fmt.Errorf("mcp client %q has no transport", c.serverName)
	}
	if err := c.transport.Reset(c.logger); err != nil {
		return fmt.Errorf("mcp transport reset failed: %w", err)
	}

	resp, err := c.roundTrip(ctx, map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": defaultProtocolVersion,
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "dsc-mcp-client",
				"version": "1.0.0",
			},
		},
	})
	if err != nil {
		return fmt.Errorf("mcp initialize failed: %w", err)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return fmt.Errorf("mcp initialize: no result in response")
	}
	// 协商出的协议版本要带在后续 HTTP 请求头上（stdio 无 header，不需要）。
	if v, ok := result["protocolVersion"].(string); ok && v != "" {
		if ht, ok := c.transport.(*httpTransport); ok {
			ht.setProtocolVersion(v)
		}
	}

	// initialized 是通知，服务器不回应。
	_, _ = c.roundTrip(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	return c.discoverTools(ctx)
}

// discoverTools 拉取 tools/list 并替换本地工具集。
func (c *Client) discoverTools(ctx context.Context) error {
	resp, err := c.roundTrip(ctx, map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	if err != nil {
		return fmt.Errorf("mcp tools/list failed: %w", err)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return fmt.Errorf("mcp tools/list: no result")
	}
	toolsRaw, ok := result["tools"].([]any)
	if !ok {
		return fmt.Errorf("mcp tools/list: no tools array")
	}

	tools := make([]Tool, 0, len(toolsRaw))
	for _, raw := range toolsRaw {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := item["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := item["description"].(string)
		schema, _ := item["inputSchema"].(map[string]any)
		schemaJSON, _ := json.Marshal(schema)
		tools = append(tools, Tool{Name: name, Description: desc, Schema: schemaJSON})
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	c.mu.Lock()
	c.tools = tools
	c.mu.Unlock()
	return nil
}

// CallTool 调用服务器上的工具（按服务器侧原始名）。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	callReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": json.RawMessage(args),
		},
	}

	resp, err := c.roundTrip(ctx, callReq)
	if errors.Is(err, ErrSessionExpired) {
		// 服务端重启或会话超时：重新握手一次再试，「会话过期」不该报成工具失败。
		if cerr := c.Connect(ctx); cerr != nil {
			return "", fmt.Errorf("mcp tools/call %q: session expired and reconnect failed: %w", name, cerr)
		}
		callReq["id"] = c.nextRequestID()
		resp, err = c.roundTrip(ctx, callReq)
	}
	if err != nil {
		return "", fmt.Errorf("mcp tools/call %q failed: %w", name, err)
	}
	if errMsg, ok := resp["error"]; ok {
		return "", fmt.Errorf("mcp error: %v", errMsg)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("mcp tools/call: no result")
	}
	// content 是文本块数组；没有 content 时原样返回 result（部分服务器只给结构化数据）。
	content, ok := result["content"].([]any)
	if !ok {
		b, _ := json.Marshal(result)
		return string(b), nil
	}
	var sb strings.Builder
	for _, item := range content {
		if block, ok := item.(map[string]any); ok {
			if text, ok := block["text"].(string); ok {
				sb.WriteString(text)
			}
		}
	}
	return sb.String(), nil
}

// Disconnect 关闭传输（stdio 回收子进程）。可重复调用。
func (c *Client) Disconnect() {
	if c.transport != nil {
		c.transport.Close()
	}
}

func (c *Client) roundTrip(ctx context.Context, req map[string]any) (map[string]any, error) {
	return c.transport.RoundTrip(ctx, req)
}

// nextRequestID 小整数、单调递增（永不越过 2^53，服务端多为 JS）。
func (c *Client) nextRequestID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}
