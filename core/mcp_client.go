package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// MCP 客户端（对齐 DSH/Cordis 的 mcp-client 插件）。
//
// DSH 的 MCP 客户端是一个 Cordis 插件，连接外部 MCP 服务器并把其工具注册为
// mcp__<serverName>__<rawName> 命名的模型工具。每个插件实例连接一个 MCP 服务器；
// 多个实例可连接多个 MCP 服务器。
//
// DSC 的适配：DSC 的 MCP 客户端作为宿主内置工具注册器——连接外部 MCP 服务器的
// stdio 或 HTTP 传输，发现其工具列表，注册到 ToolRegistry。工具命名约定对齐 DSH：
// mcp__<serverName>__<rawName>。
//
// 当前实现支持 HTTP/SSE 传输（MCP 标准传输之一）。stdio 传输需要进程管理，
// 后续按需扩展。

// MCPClient 连接一个外部 MCP 服务器，发现并注册其工具。
type MCPClient struct {
	mu         sync.Mutex
	serverName string
	endpoint   string
	httpClient *http.Client
	tools      map[string]*MCPTool // rawName → tool
	manager    *Manager

	// sessionID 是 initialize 响应头 Mcp-Session-Id 带回的会话标识。
	//
	// streamable HTTP 传输把「一次连接」建模成服务端会话：initialize 的响应用
	// `Mcp-Session-Id` 头下发标识，**之后每个请求都必须原样回带**，否则服务端直接
	// 400（"Bad Request: Mcp-Session-Id header is required"）。此前这里完全没保存
	// 这个头，于是 initialize 成功、紧接着的 tools/list 就 400——连接看起来「连不上」，
	// 实际是第二次请求被拒，一个工具也注册不上。
	sessionID string
	// protocolVersion 是 initialize 协商出的版本，后续请求按规范放在
	// MCP-Protocol-Version 头里（缺失时部分服务端直接拒绝）。
	protocolVersion string
	// nextID 下一次请求的 JSON-RPC id（单调递增的小整数）。
	//
	// 不能用 time.Now().UnixNano()：那串值约 1.79e18，超过 IEEE-754 的安全整数
	// 上限 2^53。服务端是 JS（官方 SDK 就是），解析后数值被舍入，严格校验会直接判
	// 「Invalid JSON-RPC message」并回 400——实测同一个 tools/call，小整数 200、
	// UnixNano 400、字符串 200。id 只用于配对响应，小整数足够且无精度问题。
	nextID int64
}

// MCPTool 一个经 MCP 桥接的工具。
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	// publicName 模型可见的工具名：mcp__<serverName>__<rawName>
	publicName string
	// rawName MCP 服务器上的原始工具名
	rawName string
	// serverName MCP 服务器名
	serverName string
}

// NewMCPClient 创建 MCP 客户端。serverName 仅允许 [A-Za-z0-9_-]，最长 32 字符。
// endpoint 为 MCP 服务器的 HTTP URL（如 http://localhost:3000/mcp）。
func NewMCPClient(mgr *Manager, serverName, endpoint string) (*MCPClient, error) {
	if !isMCPServerNameValid(serverName) {
		return nil, fmt.Errorf("invalid MCP server name %q: use [A-Za-z0-9_-] only, max 32 chars", serverName)
	}
	return &MCPClient{
		serverName: serverName,
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		tools:      make(map[string]*MCPTool),
		manager:    mgr,
	}, nil
}

// nextRequestID 取下一个请求 id（小整数，单调递增，永不超出 2^53）。
func (c *MCPClient) nextRequestID() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// Connect 连接 MCP 服务器并发现工具。重复调用即重新握手（工具会被重复注册，
// 注册表按名字覆盖，幂等）。
func (c *MCPClient) Connect(ctx context.Context) error {
	// 清空上一次的会话：重连必须拿新会话，旧 id 在新连接上一定 404
	c.mu.Lock()
	c.sessionID = ""
	c.mu.Unlock()

	// MCP 协议：发送 initialize 请求
	initReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "dsc-mcp-client",
				"version": "1.0.0",
			},
		},
	}

	resp, err := c.doJSON(ctx, initReq)
	if err != nil {
		return fmt.Errorf("MCP initialize failed: %w", err)
	}
	// 验证响应中有 capabilities；记下协商出的协议版本，后续请求要带
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return fmt.Errorf("MCP initialize: no result in response")
	}
	if v, ok := result["protocolVersion"].(string); ok && v != "" {
		c.mu.Lock()
		c.protocolVersion = v
		c.mu.Unlock()
	}

	// 发送 initialized 通知
	notif := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	_, _ = c.doJSON(ctx, notif) // 通知不需响应

	// 发现工具
	return c.discoverTools(ctx)
}

// discoverTools 列出 MCP 服务器的工具并注册到 ToolRegistry。
func (c *MCPClient) discoverTools(ctx context.Context) error {
	listReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "tools/list",
		"params":  map[string]any{},
	}

	resp, err := c.doJSON(ctx, listReq)
	if err != nil {
		return fmt.Errorf("MCP tools/list failed: %w", err)
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		return fmt.Errorf("MCP tools/list: no result")
	}

	toolsRaw, ok := result["tools"].([]any)
	if !ok {
		return fmt.Errorf("MCP tools/list: no tools array")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, t := range toolsRaw {
		tool, ok := t.(map[string]any)
		if !ok {
			continue
		}
		rawName, _ := tool["name"].(string)
		if rawName == "" {
			continue
		}
		desc, _ := tool["description"].(string)
		schema, _ := tool["inputSchema"].(map[string]any)
		schemaJSON, _ := json.Marshal(schema)

		publicName := mcpPublicName(c.serverName, rawName)
		mcpTool := &MCPTool{
			Name:        publicName,
			Description: desc,
			Schema:      schemaJSON,
			publicName:  publicName,
			rawName:     rawName,
			serverName:  c.serverName,
		}
		c.tools[rawName] = mcpTool

		// 注册到 ToolRegistry：用 mcpToolAdapter 而非 RemoteTool——
		// RemoteTool.Execute 需要 proto.ToolServiceClient（gRPC 转发到工具插件进程），
		// 而 MCP 工具不经 gRPC，需直接调用 MCPClient.CallTool（JSON-RPC over HTTP）。
		// 此前用 RemoteTool{client:nil} 会在模型调用时 panic（nil 解引用）。
		if c.manager != nil {
			c.manager.toolRegistry.Register(&mcpToolAdapter{
				name:        publicName,
				description: desc,
				schema:      json.RawMessage(schemaJSON),
				client:      c,
				rawName:     rawName,
			})
		}
	}

	return nil
}

// CallTool 调用 MCP 服务器上的工具。
func (c *MCPClient) CallTool(ctx context.Context, rawName string, args json.RawMessage) (string, error) {
	callReq := map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      rawName,
			"arguments": json.RawMessage(args),
		},
	}

	resp, err := c.doJSON(ctx, callReq)
	if errors.Is(err, errMCPSessionExpired) {
		// 服务端重启或会话超时：重新握手一次再试，别把「会话过期」报成工具失败
		if cerr := c.Connect(ctx); cerr != nil {
			return "", fmt.Errorf("MCP tools/call %q: session expired and reconnect failed: %w", rawName, cerr)
		}
		resp, err = c.doJSON(ctx, callReq)
	}
	if err != nil {
		return "", fmt.Errorf("MCP tools/call %q failed: %w", rawName, err)
	}

	if errMsg, ok := resp["error"]; ok {
		return "", fmt.Errorf("MCP error: %v", errMsg)
	}

	result, ok := resp["result"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("MCP tools/call: no result")
	}

	// MCP 返回 content 数组（文本块）
	content, ok := result["content"].([]any)
	if !ok {
		// 尝试直接返回 result 的 JSON
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

// Disconnect 断开连接并注销所有工具。
func (c *MCPClient) Disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.manager != nil {
		for _, t := range c.tools {
			c.manager.toolRegistry.Unregister(t.publicName)
		}
	}
	c.tools = make(map[string]*MCPTool)
}

// ServerName 返回 MCP 服务器名。
func (c *MCPClient) ServerName() string {
	return c.serverName
}

// Tools 返回已发现的工具列表（副本）。
func (c *MCPClient) Tools() []*MCPTool {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*MCPTool, 0, len(c.tools))
	for _, t := range c.tools {
		out = append(out, t)
	}
	return out
}

// doJSON 发送 JSON-RPC 请求并解析响应。
//
// HTTP 头设置兼容新旧两种 MCP HTTP 传输：
//   - 旧版 JSON-RPC over HTTP（Content-Type: application/json）
//   - 新版 streamable-http（MCP 规范 2025-03-26+，要求 Accept 同时含
//     application/json 和 text/event-stream）
//
// 新版服务器经 Accept 头检测客户端能力——缺 text/event-stream 会返回
// HTTP 406 "Not Acceptable"。设上两个类型后，旧版服务器忽略 Accept、
// 新版服务器选择其中一种响应（简单请求通常返回 application/json）。
// errMCPSessionExpired 表示服务端不再认识当前会话（streamable HTTP 的 404）。
// 调用方据此重新 initialize，而不是把「会话过期」当成工具调用失败。
var errMCPSessionExpired = fmt.Errorf("mcp: session expired")

func (c *MCPClient) doJSON(ctx context.Context, req map[string]any) (map[string]any, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// MCP streamable-http 传输要求客户端声明接受 application/json 与
	// text/event-stream 两种响应格式（MCP 规范 2025-03-26 §2.1）。
	// 旧版 JSON-RPC 服务器忽略 Accept 头，不受影响。
	httpReq.Header.Set("Accept", "application/json, text/event-stream")

	c.mu.Lock()
	sid, ver := c.sessionID, c.protocolVersion
	c.mu.Unlock()
	if sid != "" {
		httpReq.Header.Set("Mcp-Session-Id", sid)
	}
	if ver != "" {
		httpReq.Header.Set("MCP-Protocol-Version", ver)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 服务端可能在任意响应上重新下发会话标识（initialize 一定有），有就更新。
	if newSID := resp.Header.Get("Mcp-Session-Id"); newSID != "" && newSID != sid {
		c.mu.Lock()
		c.sessionID = newSID
		c.mu.Unlock()
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

// mcpPublicName 生成模型可见的工具名：mcp__<serverName>__<rawName>
// 对齐 DSH 的命名约定（参见 dsh/packages/mcp/mcp-client/src/tools.ts）。
func mcpPublicName(serverName, rawName string) string {
	name := "mcp__" + serverName + "__" + rawName
	// 规范化：非 [A-Za-z0-9_-] 字符替换为 _（对齐 DeepSeek 函数名约束）
	name = mcpNormalizeName(name)
	// 截断到 64 字符（对齐 DeepSeek 函数名约束）
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// mcpNormalizeName 把非 [A-Za-z0-9_-] 字符替换为 _。
func mcpNormalizeName(s string) string {
	b := []byte(s)
	for i := range b {
		c := b[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	return string(b)
}

// isMCPServerNameValid 校验 MCP 服务器名：仅 [A-Za-z0-9_-]，最长 32 字符。
func isMCPServerNameValid(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// mcpToolAdapter 把 MCP 工具桥接为 ToolDefinition。
// 模型经它调用工具时，直接经 MCPClient.CallTool（JSON-RPC over HTTP）执行，
// 而非经 RemoteTool 的 gRPC 转发路径——后者要求 proto.ToolServiceClient，
// 而 MCP 工具不经 gRPC，沿用 RemoteTool 会在 Execute 时 nil 解引用 panic。
type mcpToolAdapter struct {
	name        string
	description string
	schema      json.RawMessage
	client      *MCPClient
	rawName     string
}

func (t *mcpToolAdapter) Name() string                      { return t.name }
func (t *mcpToolAdapter) Description() string               { return t.description }
func (t *mcpToolAdapter) ParametersSchema() json.RawMessage { return t.schema }

func (t *mcpToolAdapter) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.client == nil {
		return "", fmt.Errorf("MCP tool %q: client not bound (server disconnected?)", t.name)
	}
	return t.client.CallTool(ctx, t.rawName, args)
}
