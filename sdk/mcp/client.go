// Package mcp 是一个不依赖宿主的 MCP 客户端：协议流程（initialize → initialized →
// tools/list → tools/call，另有 resources 与 prompts 两类只读能力）加两种传输
// （streamable HTTP、stdio 子进程）。
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

// listPageLimit 列表接口最多翻多少页。
//
// nextCursor 由服务器给，理论上有限；但没有上限的循环在「buggy 服务器一直回同样的
// cursor」时会永远转下去（客户端卡死，看上去像服务器掉线）。这里两道保险：页数上限 +
// cursor 重复检测。
const listPageLimit = 100

// Tool 服务器声明的工具（尚未命名、尚未注册）。
type Tool struct {
	// Name 服务器侧的原始名（tools/call 用它）。
	Name string
	// Description 服务器给的说明。
	Description string
	// Schema 输入参数的 JSON Schema（原始字节，可能为空对象）。
	Schema json.RawMessage
}

// Resource 服务器暴露的一份可读取内容（resources/read 用 URI 取）。
type Resource struct {
	// URI 服务器侧标识（file:///、repo://、mem:// 等，服务器自定义）。
	URI string
	// Name 人读的名字（多数服务器给，缺省时调用方回落到 URI）。
	Name string
	// Description 说明，可能为空。
	Description string
	// MimeType 内容类型（text/plain、application/json 等），可能为空。
	MimeType string
}

// PromptArg prompt 的一个参数声明。
type PromptArg struct {
	Name        string
	Description string
	Required    bool
}

// Prompt 服务器暴露的提示词模板（prompts/get 用名字取，可带参数）。
type Prompt struct {
	Name        string
	Description string
	Arguments   []PromptArg
}

// ServerCapabilities initialize 结果里的能力声明。
//
// Declared 表示服务器到底有没有给 capabilities 字段：**省略**与**明说没有**是两回事。
// 现实里大量服务器把 capabilities 写成 {} 或干脆不给，但 tools/list 照样能用，
// 所以「没声明」不能当成「没有」；只有明说了没有 tools 时才容忍 tools/list 报找不到方法。
type ServerCapabilities struct {
	Declared  bool
	Tools     bool
	Resources bool
	Prompts   bool
}

// Client 一个 MCP 服务器会话。
//
// 生命周期：New* → Connect（握手 + 发现工具/资源/提示词）→ 调用 → Disconnect。
// Connect 可重复调用：HTTP 会重新握手拿新会话，stdio 会在子进程退出后重启它。
type Client struct {
	serverName string
	transport  Transport
	logger     Logger

	mu        sync.Mutex
	caps      ServerCapabilities
	tools     []Tool
	resources []Resource
	prompts   []Prompt
	nextID    int64
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

// Capabilities 握手时服务器声明的能力（Connect 之后有效）。
func (c *Client) Capabilities() ServerCapabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.caps
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

// Resources 已发现的资源（按 URI 排序，结果稳定）。
func (c *Client) Resources() []Resource {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Resource, len(c.resources))
	copy(out, c.resources)
	return out
}

// ResourceCount 已发现的资源数。
func (c *Client) ResourceCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.resources)
}

// Prompts 已发现的提示词（按名排序，结果稳定）。
func (c *Client) Prompts() []Prompt {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Prompt, len(c.prompts))
	copy(out, c.prompts)
	return out
}

// PromptCount 已发现的提示词数。
func (c *Client) PromptCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.prompts)
}

// Connect 握手并发现工具/资源/提示词。
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
	caps := parseCapabilities(result["capabilities"])
	c.mu.Lock()
	c.caps = caps
	c.mu.Unlock()

	// initialized 是通知，服务器不回应。
	_, _ = c.roundTrip(ctx, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})

	if err := c.discoverTools(ctx, caps); err != nil {
		return err
	}
	// 资源与提示词是可选能力：只有服务器**明说**支持才去问。
	// 反过来的代价更高——对一台只有 tools 的服务器调 resources/list，多数会回
	// -32601「method not found」，日志里天天一串错误噪音，还会让人误判插件是坏的。
	if caps.Resources {
		if err := c.discoverResources(ctx); err != nil {
			// 声明了却拿不到：工具照常可用，只是没有资源可读。降级而不是整个连接失败。
			c.logger.Info("mcp resources/list failed; continuing without resources",
				"server", c.serverName, "error", err.Error())
		}
	}
	if caps.Prompts {
		if err := c.discoverPrompts(ctx); err != nil {
			c.logger.Info("mcp prompts/list failed; continuing without prompts",
				"server", c.serverName, "error", err.Error())
		}
	}
	return nil
}

// parseCapabilities 解析 initialize 的 capabilities 字段。
func parseCapabilities(v any) ServerCapabilities {
	m, ok := v.(map[string]any)
	if !ok {
		return ServerCapabilities{}
	}
	_, hasTools := m["tools"]
	_, hasResources := m["resources"]
	_, hasPrompts := m["prompts"]
	return ServerCapabilities{
		Declared:  true,
		Tools:     hasTools,
		Resources: hasResources,
		Prompts:   hasPrompts,
	}
}

// discoverTools 拉取 tools/list（自动翻页）并替换本地工具集。
func (c *Client) discoverTools(ctx context.Context, caps ServerCapabilities) error {
	items, err := c.listAll(ctx, "tools/list", "tools")
	if err != nil {
		// 服务器**明说**没有 tools 时才容忍失败（对 prompts/resources-only 服务器，
		// tools/list 报 -32601 是正常现象）；未声明或明说有 tools 时失败仍须报错，
		// 否则「目录空了」会被当成「这台服务器没有工具」而静默下去。
		if caps.Declared && !caps.Tools {
			c.logger.Info("mcp tools/list unsupported by server; continuing without tools",
				"server", c.serverName, "error", err.Error())
			c.mu.Lock()
			c.tools = nil
			c.mu.Unlock()
			return nil
		}
		return fmt.Errorf("mcp tools/list failed: %w", err)
	}

	tools := make([]Tool, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		name, _ := item["name"].(string)
		if name == "" || seen[name] {
			// 分页交界处重复给同一条、或 cursor 把服务器绕回同一页，都会造成重名；
			// 登记同名工具会被注册表拒掉，这里先去重（保留先出现的那条）。
			continue
		}
		seen[name] = true
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

// discoverResources 拉取 resources/list（自动翻页）。
func (c *Client) discoverResources(ctx context.Context) error {
	items, err := c.listAll(ctx, "resources/list", "resources")
	if err != nil {
		return err
	}
	out := make([]Resource, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		uri, _ := item["uri"].(string)
		if uri == "" || seen[uri] {
			continue
		}
		seen[uri] = true
		name, _ := item["name"].(string)
		if name == "" {
			name = uri
		}
		desc, _ := item["description"].(string)
		mime, _ := item["mimeType"].(string)
		out = append(out, Resource{URI: uri, Name: name, Description: desc, MimeType: mime})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })

	c.mu.Lock()
	c.resources = out
	c.mu.Unlock()
	return nil
}

// discoverPrompts 拉取 prompts/list（自动翻页）。
func (c *Client) discoverPrompts(ctx context.Context) error {
	items, err := c.listAll(ctx, "prompts/list", "prompts")
	if err != nil {
		return err
	}
	out := make([]Prompt, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		name, _ := item["name"].(string)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		desc, _ := item["description"].(string)
		var args []PromptArg
		if rawArgs, ok := item["arguments"].([]any); ok {
			for _, rawArg := range rawArgs {
				arg, ok := rawArg.(map[string]any)
				if !ok {
					continue
				}
				argName, _ := arg["name"].(string)
				if argName == "" {
					continue
				}
				argDesc, _ := arg["description"].(string)
				required, _ := arg["required"].(bool)
				args = append(args, PromptArg{Name: argName, Description: argDesc, Required: required})
			}
		}
		out = append(out, Prompt{Name: name, Description: desc, Arguments: args})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	c.mu.Lock()
	c.prompts = out
	c.mu.Unlock()
	return nil
}

// listAll 取一个分页列表接口的全部条目（tools/list、resources/list、prompts/list）。
//
// MCP 的列表响应形如 {"<key>": [...], "nextCursor": "..."}：把 nextCursor 原样作为下一页
// 的 cursor 再问，直到不再返回。不翻页的代价很隐蔽——服务器只回第一页，调用方以为
// 「这台服务器就这么多工具」，缺的那部分永远看不见。
func (c *Client) listAll(ctx context.Context, method, key string) ([]map[string]any, error) {
	var out []map[string]any
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < listPageLimit; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		resp, err := c.roundTrip(ctx, map[string]any{
			"jsonrpc": "2.0",
			"id":      c.nextRequestID(),
			"method":  method,
			"params":  params,
		})
		if err != nil {
			return nil, err
		}
		if errMsg, ok := resp["error"]; ok {
			return nil, fmt.Errorf("mcp error: %v", errMsg)
		}
		result, ok := resp["result"].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("no result")
		}
		if items, ok := result[key].([]any); ok {
			for _, raw := range items {
				if item, ok := raw.(map[string]any); ok {
					out = append(out, item)
				}
			}
		}
		next, _ := result["nextCursor"].(string)
		if next == "" {
			return out, nil
		}
		if seen[next] {
			// 服务器把同一个 cursor 又给了回来：再问就是死循环，停下并把已拿到的交出去。
			c.logger.Info("mcp pagination cursor repeated; stopping early",
				"server", c.serverName, "method", method, "pages", page+1)
			return out, nil
		}
		seen[next] = true
		cursor = next
	}
	c.logger.Info("mcp pagination hit page limit; stopping early",
		"server", c.serverName, "method", method, "limit", listPageLimit, "items", len(out))
	return out, nil
}

// callMethod 发一个请求，遇到「会话过期」时重新握手再试一次。
//
// 「服务端重启/会话超时」不该报成调用失败：重连一次就能继续，用户看不到区别。
func (c *Client) callMethod(ctx context.Context, req map[string]any) (map[string]any, error) {
	resp, err := c.roundTrip(ctx, req)
	if errors.Is(err, ErrSessionExpired) {
		if cerr := c.Connect(ctx); cerr != nil {
			return nil, fmt.Errorf("session expired and reconnect failed: %w", cerr)
		}
		req["id"] = c.nextRequestID()
		resp, err = c.roundTrip(ctx, req)
	}
	return resp, err
}

// CallTool 调用服务器上的工具（按服务器侧原始名）。
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	resp, err := c.callMethod(ctx, map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": json.RawMessage(args),
		},
	})
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

// ReadResource 读取一份资源的内容（resources/read）。
//
// 返回文本；二进制块（base64 blob）不做解码——把几 MB 的 base64 塞进对话既没用也贵，
// 只给一句「这是二进制，多大」让人自己决定要不要别的路子。
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return "", fmt.Errorf("resource uri is required")
	}
	resp, err := c.callMethod(ctx, map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "resources/read",
		"params":  map[string]any{"uri": uri},
	})
	if err != nil {
		return "", fmt.Errorf("mcp resources/read %q failed: %w", uri, err)
	}
	if errMsg, ok := resp["error"]; ok {
		return "", fmt.Errorf("mcp error: %v", errMsg)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("mcp resources/read: no result")
	}
	contents, ok := result["contents"].([]any)
	if !ok {
		b, _ := json.Marshal(result)
		return string(b), nil
	}
	var sb strings.Builder
	for _, item := range contents {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := block["text"].(string); ok {
			sb.WriteString(text)
			if !strings.HasSuffix(text, "\n") {
				sb.WriteString("\n")
			}
			continue
		}
		if blob, ok := block["blob"].(string); ok {
			mime, _ := block["mimeType"].(string)
			if mime == "" {
				mime = "application/octet-stream"
			}
			fmt.Fprintf(&sb, "[binary resource omitted: %s, %d base64 chars]\n", mime, len(blob))
		}
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// GetPrompt 取一条提示词（prompts/get），把消息渲染成纯文本。
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("prompt name is required")
	}
	params := map[string]any{"name": name}
	if len(args) > 0 {
		params["arguments"] = args
	}
	resp, err := c.callMethod(ctx, map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextRequestID(),
		"method":  "prompts/get",
		"params":  params,
	})
	if err != nil {
		return "", fmt.Errorf("mcp prompts/get %q failed: %w", name, err)
	}
	if errMsg, ok := resp["error"]; ok {
		return "", fmt.Errorf("mcp error: %v", errMsg)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("mcp prompts/get: no result")
	}
	desc, _ := result["description"].(string)
	var sb strings.Builder
	if desc != "" {
		sb.WriteString(desc)
		sb.WriteString("\n\n")
	}
	messages, ok := result["messages"].([]any)
	if !ok {
		b, _ := json.Marshal(result)
		return string(b), nil
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		text := renderPromptContent(msg["content"])
		if text == "" {
			continue
		}
		if role == "" {
			role = "user"
		}
		fmt.Fprintf(&sb, "[%s]\n%s\n\n", role, text)
	}
	return strings.TrimRight(sb.String(), "\n"), nil
}

// renderPromptContent 把 prompt 消息里的 content 拆成文本。
//
// 规范允许 content 是单个块或块数组，块类型不止 text（image/audio/resource）。
// 非文本块保留「是什么」而不是静默丢弃——丢掉的恰恰可能是提示词的关键部分。
func renderPromptContent(v any) string {
	switch content := v.(type) {
	case string:
		return content
	case map[string]any:
		return renderPromptBlock(content)
	case []any:
		parts := make([]string, 0, len(content))
		for _, item := range content {
			if block, ok := item.(map[string]any); ok {
				if s := renderPromptBlock(block); s != "" {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func renderPromptBlock(block map[string]any) string {
	if text, ok := block["text"].(string); ok {
		return text
	}
	kind, _ := block["type"].(string)
	if kind == "" {
		return ""
	}
	return fmt.Sprintf("[%s block omitted]", kind)
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
