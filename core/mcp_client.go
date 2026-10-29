package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/hashicorp/go-hclog"
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
//
// 传输是可替换的（见 mcp_transport.go）：streamable HTTP 或 stdio 子进程。协议流程
// （initialize → initialized → tools/list → tools/call）与传输无关，全在这里；会话
// 状态（HTTP 的 Mcp-Session-Id、stdio 的子进程）归各自的 transport 管。
type MCPClient struct {
	mu         sync.Mutex
	serverName string
	tools      map[string]*MCPTool
	manager    *Manager

	// transport 实际通道；logger 用于 stdio 的 stderr 与进程事件。
	transport mcpTransport
	logger    hclog.Logger

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

// NewMCPClient 创建 MCP 客户端（streamable HTTP）。serverName 仅允许
// [A-Za-z0-9_-]，最长 32 字符。endpoint 为 MCP 服务器的 HTTP URL
// （如 http://localhost:3000/mcp）。
func NewMCPClient(mgr *Manager, serverName, endpoint string) (*MCPClient, error) {
	c, err := newMCPClient(mgr, serverName, nil)
	if err != nil {
		return nil, err
	}
	c.transport = newHTTPTransport(endpoint)
	return c, nil
}

// NewMCPStdioClient 创建 MCP 客户端（stdio：把服务器作为子进程跑）。
//
// 与 HTTP 版的差别只在 transport；协议流程完全相同。子进程的环境**不是**整份继承
// 宿主环境，而是白名单基线 + 调用方显式给的 env（见 mcpStdioEnv）——MCP 服务器是
// 第三方代码，不该顺手拿到宿主的 API key。
func NewMCPStdioClient(mgr *Manager, serverName string, spec MCPStdioSpec, logger hclog.Logger) (*MCPClient, error) {
	c, err := newMCPClient(mgr, serverName, logger)
	if err != nil {
		return nil, err
	}
	tr, terr := newStdioTransport(spec, logger)
	if terr != nil {
		return nil, terr
	}
	c.transport = tr
	return c, nil
}

func newMCPClient(mgr *Manager, serverName string, logger hclog.Logger) (*MCPClient, error) {
	if !isMCPServerNameValid(serverName) {
		return nil, fmt.Errorf("invalid MCP server name %q: use [A-Za-z0-9_-] only, max 32 chars", serverName)
	}
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	return &MCPClient{
		serverName: serverName,
		tools:      make(map[string]*MCPTool),
		manager:    mgr,
		logger:     logger,
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
	// 重置传输状态：HTTP 重连必须拿新会话（旧 id 在新连接上一定 404）；stdio 若子进程
	// 已退出则重启一个。
	if c.transport == nil {
		return fmt.Errorf("MCP client %q has no transport", c.serverName)
	}
	if err := c.transport.Reset(c.logger); err != nil {
		return fmt.Errorf("MCP transport reset failed: %w", err)
	}

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
		if ht, ok := c.transport.(*httpTransport); ok {
			ht.setProtocolVersion(v) // stdio 没有 header，不需要记
		}
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
	if c.manager != nil {
		for _, t := range c.tools {
			c.manager.toolRegistry.Unregister(t.publicName)
		}
	}
	c.tools = make(map[string]*MCPTool)
	tr := c.transport
	c.mu.Unlock()

	// 传输关闭放在锁外：stdio 的 Close 要等子进程退出（最多 stdioShutdownGrace），
	// 期间不该挡住别人读工具表。
	if tr != nil {
		tr.Close() // stdio：关 stdin 回收子进程；http：无操作
	}
}

// TransportKind 返回传输类型（http / stdio）。
func (c *MCPClient) TransportKind() string {
	if c.transport == nil {
		return ""
	}
	return c.transport.Kind()
}

// Describe 返回一行传输描述（HTTP 是 URL，stdio 是命令行）。
func (c *MCPClient) Describe() string {
	if c.transport == nil {
		return ""
	}
	return c.transport.Describe()
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

// doJSON 发送一次 JSON-RPC 往返，具体通道由 transport 决定（HTTP / stdio）。
//
// 会话过期只可能来自 HTTP（404）；stdio 的「会话过期」表现为子进程退出，由 transport
// 直接报错。
func (c *MCPClient) doJSON(ctx context.Context, req map[string]any) (map[string]any, error) {
	if c.transport == nil {
		return nil, fmt.Errorf("MCP client %q has no transport", c.serverName)
	}
	return c.transport.RoundTrip(ctx, req)
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
