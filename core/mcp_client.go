package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	dscmcp "dsc-sdk/mcp"
	"github.com/hashicorp/go-hclog"
)

// MCPStdioSpec 拉起 stdio 服务器所需的全部信息（命令行、参数、环境、工作目录）。
//
// 与 SDK 的 mcp.StdioSpec 同形：这里是宿主的对外入参（配置与工具参数都汇成它），
// 传下去时再转一次——保持宿主 API 稳定，SDK 可以自行演进。
type MCPStdioSpec struct {
	Command string
	Args    []string
	Env     map[string]string
	CWD     string
}

// MCPTool 一个经 MCP 桥接的工具（宿主视角：公开名 + 服务器侧原名）。
type MCPTool struct {
	Name        string          // 公开名：mcp__<server>__<raw>
	Description string          // 服务器给的说明
	Schema      json.RawMessage // 输入参数 schema
	publicName  string          // 同 Name（内部别名，与注册表键对应）
	rawName     string          // 服务器侧原名（tools/call 用）
	serverName  string          // 所属服务器
}

// MCPClient 宿主侧的 MCP 客户端封装：连接、命名、注册、曝光。
//
// 协议与传输在 dsc-sdk/mcp（不依赖宿主的纯客户端）；这一层只做宿主的事——给工具起
// 公开名、装进注册表、决定它们此刻对模型可见与否、断开时注销。这样拆的理由很直接：
// MCP 迟早要变成插件，插件拿得到 SDK、拿不到 core。
type MCPClient struct {
	client  *dscmcp.Client
	manager *Manager
	logger  hclog.Logger

	tools map[string]*MCPTool // 按公开名索引（与注册表键一致）
}

// NewMCPClient 创建 streamable HTTP 客户端。
func NewMCPClient(mgr *Manager, serverName, endpoint string) (*MCPClient, error) {
	c, err := newMCPClient(mgr, serverName, nil)
	if err != nil {
		return nil, err
	}
	c.client = dscmcp.NewHTTPClient(serverName, endpoint, c.logger)
	return c, nil
}

// NewMCPStdioClient 创建 stdio 客户端（把服务器作为子进程拉起）。
//
// 子进程环境是白名单基线 + spec.Env（见 SDK 的 mcp.StdioSpec）：MCP 服务器是第三方
// 代码，不该顺手继承宿主的 API key。
func NewMCPStdioClient(mgr *Manager, serverName string, spec MCPStdioSpec, logger hclog.Logger) (*MCPClient, error) {
	c, err := newMCPClient(mgr, serverName, logger)
	if err != nil {
		return nil, err
	}
	sc, serr := dscmcp.NewStdioClient(serverName, dscmcp.StdioSpec{
		Command: spec.Command,
		Args:    spec.Args,
		Env:     spec.Env,
		CWD:     spec.CWD,
	}, c.logger)
	if serr != nil {
		return nil, serr
	}
	c.client = sc
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
		tools:   make(map[string]*MCPTool),
		manager: mgr,
		logger:  logger,
	}, nil
}

// ServerName 返回服务器名。
func (c *MCPClient) ServerName() string {
	if c.client == nil {
		return ""
	}
	return c.client.ServerName()
}

// TransportKind 返回传输类型（http / stdio）。
func (c *MCPClient) TransportKind() string {
	if c.client == nil {
		return ""
	}
	return c.client.Kind()
}

// Describe 返回一行传输描述（HTTP 是 URL，stdio 是命令行）。
func (c *MCPClient) Describe() string {
	if c.client == nil {
		return ""
	}
	return c.client.Describe()
}

// Tools 返回已发现的工具（按公开名排序，结果稳定）。
func (c *MCPClient) Tools() []*MCPTool {
	out := make([]*MCPTool, 0, len(c.tools))
	for _, t := range c.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolCount 返回已发现的工具数。
func (c *MCPClient) ToolCount() int { return len(c.tools) }

// Connect 握手、发现工具，并注册进宿主的工具注册表。
//
// 重复调用即重新握手：HTTP 换新会话、stdio 在子进程退出后重启；注册按名字覆盖，幂等。
func (c *MCPClient) Connect(ctx context.Context) error {
	if c.client == nil {
		return fmt.Errorf("MCP client has no transport")
	}
	if err := c.client.Connect(ctx); err != nil {
		return err
	}
	c.registerTools()
	return nil
}

// registerTools 把 SDK 层的工具转成宿主工具并注册。
//
// 公开名 = mcp__<server>__<raw>：模型看到的名字必须带服务器前缀，否则两台服务器各有
// 一个 "list" 时会互相覆盖。
func (c *MCPClient) registerTools() {
	serverName := c.client.ServerName()
	tools := make(map[string]*MCPTool, len(c.client.Tools()))
	for _, t := range c.client.Tools() {
		publicName := mcpPublicName(serverName, t.Name)
		mt := &MCPTool{
			Name:        publicName,
			Description: t.Description,
			Schema:      t.Schema,
			publicName:  publicName,
			rawName:     t.Name,
			serverName:  serverName,
		}
		tools[publicName] = mt
		if c.manager == nil {
			continue
		}
		// 用 mcpToolAdapter 而不是 RemoteTool：RemoteTool 经 gRPC 转发到工具插件，
		// 而 MCP 工具直接走客户端的 JSON-RPC。
		c.manager.toolRegistry.Register(&mcpToolAdapter{
			name:        publicName,
			description: t.Description,
			schema:      t.Schema,
			client:      c,
			rawName:     t.Name,
		})
	}
	c.tools = tools
}

// CallTool 调用服务器上的工具（按服务器侧原名）。
func (c *MCPClient) CallTool(ctx context.Context, rawName string, args json.RawMessage) (string, error) {
	if c.client == nil {
		return "", fmt.Errorf("MCP tool %q: client has no transport", rawName)
	}
	return c.client.CallTool(ctx, rawName, args)
}

// Disconnect 注销工具并关闭传输（stdio 回收子进程）。
func (c *MCPClient) Disconnect() {
	if c.manager != nil {
		for _, t := range c.tools {
			c.manager.toolRegistry.Unregister(t.publicName)
		}
	}
	c.tools = make(map[string]*MCPTool)
	if c.client != nil {
		c.client.Disconnect()
	}
}

// mcpToolAdapter 把 MCP 工具接到宿主注册表。
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

// IsHidden 实现 HiddenTool：MCP 工具注册后默认**不进**模型目录，要 mcp_load 明确
// 加载才出现（一台服务器可能有几百个工具，全塞进目录每个请求都要付钱）。关掉总闸时
// 连已加载的也一起藏——off 的语义是「这个话题现在不占我任何预算」。
//
// 隐藏不等于不可用：它仍在注册表里，后台任务、子代理、按名调用照常工作。
func (t *mcpToolAdapter) IsHidden() bool {
	if t.client == nil || t.client.manager == nil {
		return false // 未绑定宿主（单测直连）时不做曝光裁剪
	}
	m := t.client.manager
	if !m.MCPToolsEnabled() {
		return true
	}
	return !m.mcpToolLoaded(t.name)
}

func (t *mcpToolAdapter) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if t.client == nil {
		return "", fmt.Errorf("MCP tool %q: client not bound (server disconnected?)", t.name)
	}
	return t.client.CallTool(ctx, t.rawName, args)
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

// isMCPServerNameValid 服务器名只允许 [A-Za-z0-9_-] 且 ≤32 字符（它会进工具名与日志）。
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
