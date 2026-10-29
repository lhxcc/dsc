package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"dsc/libs/vodka"
)

// registerLSPAdminRoutes 注册 /lsp/* admin 端点。
// 对齐 DSH packages/lsp/lsp + lsp-stdio + tool-lsp 的能力暴露面：
//   - POST /lsp/start   启动 LSP 服务器子进程并注册 LSPTool 到 ToolRegistry
//   - POST /lsp/stop    停止 LSP 服务器子进程
//   - GET  /lsp/diagnostics  获取诊断信息（按 file_path 可选过滤）
//
// LSPClient 经 stdio 与外部 LSP 服务器（gopls、typescript-language-server 等）通信，
// 诊断信息由外部组件经 SetDiagnostics 推送缓存（完整 LSP 协议握手留待后续）。
// LSPTool 注册到 ToolRegistry 后模型可调用 lsp_diagnostics 工具查询诊断。
func (m *Manager) registerLSPAdminRoutes(group *vodka.RouteGroup) {
	group.Post("/start", m.handleLSPStart)
	group.Post("/stop", m.handleLSPStop)
	group.Get("/diagnostics", m.handleLSPDiagnostics)
}

// registerMCPAdminRoutes 注册 /mcp/* admin 端点。
// 对齐 DSH packages/mcp/mcp-client 的能力暴露面：
//   - POST /mcp/connect     连接 MCP 服务器并发现注册其工具（mcp__<server>__<rawName>）
//   - POST /mcp/disconnect   断开 MCP 服务器并注销其工具
//   - GET  /mcp/list         列出已连接的 MCP 服务器及其工具
//
// MCPClient 经 JSON-RPC over HTTP 与外部 MCP 服务器通信，discoverTools 自动把
// 服务器工具注册到 ToolRegistry，模型可经 mcp__<server>__<rawName> 名调用。
func (m *Manager) registerMCPAdminRoutes(group *vodka.RouteGroup) {
	group.Post("/connect", m.handleMCPConnect)
	group.Post("/disconnect", m.handleMCPDisconnect)
	group.Get("/list", m.handleMCPList)
}

// ---- LSP handlers ----

// ensureLSPClients 懒初始化 lspClients map（线程安全，需持 m.mu）。
func (m *Manager) ensureLSPClients() {
	if m.lspClients == nil {
		m.lspClients = make(map[string]*LSPClient)
	}
}

// ensureLSPToolRegistered 首次启动 LSP 服务器时把 LSPTool 注册到 ToolRegistry。
// LSPTool.client=nil，由 SetManager 注入聚合源——Execute 时遍历所有已启动 LSPClient。
func (m *Manager) ensureLSPToolRegistered() {
	if m.lspToolRegistered {
		return
	}
	if m.toolRegistry == nil {
		return
	}
	tool := NewLSPTool(nil)
	tool.SetManager(m)
	m.toolRegistry.Register(tool)
	m.lspToolRegistered = true
}

// lspDiagnosticsAll 聚合所有已启动 LSPClient 的诊断（按文件路径合并）。
// 供 LSPTool.Execute 在 client=nil 时调用。
func (m *Manager) lspDiagnosticsAll(ctx context.Context, filePath string) (map[string][]LSPDiagnostic, error) {
	m.mu.Lock()
	clients := make([]*LSPClient, 0, len(m.lspClients))
	for _, c := range m.lspClients {
		clients = append(clients, c)
	}
	m.mu.Unlock()

	out := make(map[string][]LSPDiagnostic)
	for _, c := range clients {
		if filePath != "" {
			diags, err := c.GetDiagnostics(ctx, filePath)
			if err == nil && len(diags) > 0 {
				out[filePath] = append(out[filePath], diags...)
			}
			continue
		}
		all, err := c.GetAllDiagnostics(ctx)
		if err != nil {
			continue
		}
		for f, diags := range all {
			out[f] = append(out[f], diags...)
		}
	}
	return out, nil
}

func (m *Manager) handleLSPStart(c *vodka.Context) error {
	var req struct {
		ServerID string   `json:"server_id"` // 如 "go"、"typescript"
		Language string   `json:"language"`  // 如 "go"、"typescript"、"python"
		Command  string   `json:"command"`   // LSP 服务器可执行文件路径
		Args     []string `json:"args"`      // 启动参数
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		return vodka.NewHTTPError(http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	if req.ServerID == "" || req.Command == "" {
		return vodka.NewHTTPError(http.StatusBadRequest, "server_id and command are required")
	}
	if req.Language == "" {
		req.Language = req.ServerID
	}

	rootPath := ""
	if m.config != nil {
		rootPath = m.config.ExecDir
	}
	if ws := WorkspaceRoot; ws != "" {
		rootPath = ws
	}

	m.mu.Lock()
	m.ensureLSPClients()
	if existing, ok := m.lspClients[req.ServerID]; ok && existing.started {
		m.mu.Unlock()
		return vodka.NewHTTPError(http.StatusConflict, fmt.Sprintf("LSP server %q already started", req.ServerID))
	}
	client := NewLSPClient(req.Language, rootPath, req.ServerID)
	m.lspClients[req.ServerID] = client
	m.mu.Unlock()

	// 首次启动时注册 LSPTool 到 ToolRegistry（无状态工具，注册一次）
	m.mu.Lock()
	m.ensureLSPToolRegistered()
	m.mu.Unlock()

	ctx := context.Background()
	if err := client.Start(ctx, req.Command, req.Args...); err != nil {
		m.mu.Lock()
		delete(m.lspClients, req.ServerID)
		m.mu.Unlock()
		return vodka.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(map[string]any{"ok": true, "server_id": req.ServerID, "language": req.Language})
}

func (m *Manager) handleLSPStop(c *vodka.Context) error {
	var req struct {
		ServerID string `json:"server_id"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		return vodka.NewHTTPError(http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	if req.ServerID == "" {
		return vodka.NewHTTPError(http.StatusBadRequest, "server_id is required")
	}

	m.mu.Lock()
	client, ok := m.lspClients[req.ServerID]
	if !ok {
		m.mu.Unlock()
		return vodka.NewHTTPError(http.StatusNotFound, fmt.Sprintf("LSP server %q not found", req.ServerID))
	}
	delete(m.lspClients, req.ServerID)
	m.mu.Unlock()

	if err := client.Stop(); err != nil {
		return vodka.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(map[string]any{"ok": true, "server_id": req.ServerID})
}

func (m *Manager) handleLSPDiagnostics(c *vodka.Context) error {
	filePath := c.QueryParam("file_path")
	ctx := context.Background()

	all, err := m.lspDiagnosticsAll(ctx, filePath)
	if err != nil {
		return vodka.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	if filePath != "" {
		diags := all[filePath]
		return c.JSON(map[string]any{"file_path": filePath, "diagnostics": diags, "count": len(diags)})
	}
	totalCount := 0
	for _, diags := range all {
		totalCount += len(diags)
	}
	return c.JSON(map[string]any{"files": all, "file_count": len(all), "total_diagnostics": totalCount})
}

// ---- MCP handlers ----

// ensureMCPClients 懒初始化 mcpClients map（线程安全，需持 m.mu）。
func (m *Manager) ensureMCPClients() {
	if m.mcpClients == nil {
		m.mcpClients = make(map[string]*MCPClient)
	}
}

func (m *Manager) handleMCPConnect(c *vodka.Context) error {
	// 请求体与 mcp_connect 工具同形：http 用 endpoint，stdio 用 command/args/env/cwd。
	// 解码成 map 再走同一份解析，避免管理 API 只认 endpoint 而工具认 command。
	var raw map[string]any
	if err := json.NewDecoder(c.Request.Body).Decode(&raw); err != nil {
		return vodka.NewHTTPError(http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	spec, err := MCPSpecFromMap(raw)
	if err != nil {
		return vodka.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if spec.ServerName == "" {
		return vodka.NewHTTPError(http.StatusBadRequest, "server_name is required")
	}
	if !isMCPServerNameValid(spec.ServerName) {
		return vodka.NewHTTPError(http.StatusBadRequest, "invalid server_name: use [A-Za-z0-9_-] only, max 32 chars")
	}
	if _, err := spec.TransportKind(); err != nil {
		return vodka.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	m.mu.Lock()
	m.ensureMCPClients()
	_, exists := m.mcpClients[spec.ServerName]
	m.mu.Unlock()
	if exists {
		return vodka.NewHTTPError(http.StatusConflict, fmt.Sprintf("MCP server %q already connected", spec.ServerName))
	}

	toolCount, err := m.ConnectMCPServerSpec(context.Background(), spec)
	if err != nil {
		return vodka.NewHTTPError(http.StatusBadGateway, err.Error())
	}

	return c.JSON(map[string]any{"ok": true, "server_name": spec.ServerName, "tools_discovered": toolCount})
}

func (m *Manager) handleMCPDisconnect(c *vodka.Context) error {
	var req struct {
		ServerName string `json:"server_name"`
	}
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil {
		return vodka.NewHTTPError(http.StatusBadRequest, "invalid JSON: "+err.Error())
	}
	if req.ServerName == "" {
		return vodka.NewHTTPError(http.StatusBadRequest, "server_name is required")
	}

	if m.GetMCPClient(req.ServerName) == nil {
		return vodka.NewHTTPError(http.StatusNotFound, fmt.Sprintf("MCP server %q not found", req.ServerName))
	}

	removed, err := m.DisconnectMCPServer(req.ServerName)
	if err != nil {
		return vodka.NewHTTPError(http.StatusInternalServerError, err.Error())
	}
	return c.JSON(map[string]any{"ok": true, "server_name": req.ServerName, "tools_removed": removed})
}

// DisconnectMCPServer 断开并清理一台 MCP 服务器：删登记、注销工具、清曝光记录。
// 返回注销的工具数。
//
// 与 ConnectMCPServer 对称，是唯一的断开实现（管理 API、模型侧 mcp_disconnect、
// 以及将来的其它入口都走它）。三步必须一起做：只删登记会让工具留在注册表里，成为
// 一批永远调不通的幽灵；只注销工具会让 mcpClients 里留下一个空壳，重复 connect 会被
// 判「已连接」而永远连不上。
func (m *Manager) DisconnectMCPServer(serverName string) (int, error) {
	m.mu.Lock()
	m.ensureMCPClients()
	client, ok := m.mcpClients[serverName]
	if !ok {
		m.mu.Unlock()
		return 0, fmt.Errorf("MCP server %q is not connected", serverName)
	}
	delete(m.mcpClients, serverName)
	m.mcpServerCount.Add(-1)
	m.mu.Unlock()

	// 先取数量再断开：Disconnect 会清空工具表，之后读就是 0（管理 API 的
	// tools_removed 与模型侧的回执都靠这个数）。
	count := client.ToolCount()
	client.Disconnect() // 内部已按 c.tools 逐个 Unregister

	// 曝光记录一并清掉：否则重新连上同名服务器时，同一批工具会「凭空」直接可见，
	// 而模型上一轮明明是卸载过的。
	m.forgetMCPTools(serverName)
	return count, nil
}

func (m *Manager) handleMCPList(c *vodka.Context) error {
	m.mu.Lock()
	m.ensureMCPClients()
	servers := make([]map[string]any, 0, len(m.mcpClients))
	for name, client := range m.mcpClients {
		tools := make([]map[string]any, 0, len(client.tools))
		for _, t := range client.tools {
			tools = append(tools, map[string]any{
				"name":        t.publicName,
				"raw_name":    t.rawName,
				"description": t.Description,
			})
		}
		servers = append(servers, map[string]any{
			"server_name": name,
			"endpoint":    client.Describe(),
			"transport":   client.TransportKind(),
			"tool_count":  len(client.tools),
			"tools":       tools,
		})
	}
	m.mu.Unlock()
	return c.JSON(map[string]any{"servers": servers, "count": len(servers)})
}

// ---- LSPTool.Execute 聚合逻辑 ----
//
// LSPTool.client 非 nil 时查询单实例；client 为 nil 时经 Manager.lspDiagnosticsAll
// 聚合所有已启动 LSPClient 的诊断缓存（由 admin API /lsp/start 注册的实例）。
// SetManager 在 ensureLSPToolRegistered 中注入，无需修改 LSPTool 自身签名。

// ConnectMCPServer 连接一个 MCP 服务器、发现其工具并登记到 Manager。
// 返回发现的工具数。
//
// 这是「连接」的唯一实现：管理 API（/mcp/connect）、启动时的配置自动连接、
// 以及模型侧的 mcp_connect 工具都走它。三条入口各写一份会分叉——比如某一条
// 忘了把客户端登记进 mcpClients，断开时就会留下一批无人管的工具注册。
// MCPConnectSpec 描述一次连接请求：HTTP 用 Endpoint，stdio 用 Command/Args/Env/CWD。
//
// Transport 留空按 Endpoint 非空判为 http（兼容只传 endpoint 的旧调用）。
type MCPConnectSpec struct {
	ServerName string
	Transport  string // "http" | "stdio"（空 = 按字段推断）
	Endpoint   string
	Command    string
	Args       []string
	Env        map[string]string
	CWD        string
}

// transport 归一化传输类型：空值按「有 endpoint 就是 http，有 command 就是 stdio」推断。
// TransportKind 归一化传输类型（供配置解析、管理 API 与工具共用）。
func (s MCPConnectSpec) TransportKind() (string, error) {
	switch t := strings.ToLower(strings.TrimSpace(s.Transport)); t {
	case "", "http", "streamable-http", "streamable_http":
		if strings.TrimSpace(s.Endpoint) != "" {
			return "http", nil
		}
		if strings.TrimSpace(s.Command) != "" {
			return "stdio", nil
		}
		return "", fmt.Errorf("either endpoint (http) or command (stdio) is required")
	case "stdio":
		if strings.TrimSpace(s.Command) == "" {
			return "", fmt.Errorf("stdio transport requires command")
		}
		return "stdio", nil
	default:
		return "", fmt.Errorf("unsupported MCP transport %q: use http or stdio", s.Transport)
	}
}

// ConnectMCPServer 连接一个 HTTP(streamable) MCP 服务器（兼容旧签名）。
func (m *Manager) ConnectMCPServer(ctx context.Context, serverName, endpoint string) (int, error) {
	return m.ConnectMCPServerSpec(ctx, MCPConnectSpec{ServerName: serverName, Endpoint: endpoint})
}

// ConnectMCPServerSpec 连接一个 MCP 服务器（HTTP 或 stdio）、发现其工具并登记到
// Manager。返回发现的工具数。
//
// 这是「连接」的唯一实现：管理 API（/mcp/connect）、启动时的配置自动连接、以及模型侧
// 的 mcp_connect 工具都走它。三条入口各写一份会分叉——比如某一条忘了把客户端登记进
// mcpClients，断开时就会留下一批无人管的工具注册。
func (m *Manager) ConnectMCPServerSpec(ctx context.Context, spec MCPConnectSpec) (int, error) {
	if !isMCPServerNameValid(spec.ServerName) {
		return 0, fmt.Errorf("invalid MCP server name %q: use [A-Za-z0-9_-] only, max 32 chars", spec.ServerName)
	}
	kind, err := spec.TransportKind()
	if err != nil {
		return 0, err
	}
	var client *MCPClient
	switch kind {
	case "http":
		client, err = NewMCPClient(m, spec.ServerName, strings.TrimSpace(spec.Endpoint))
	case "stdio":
		client, err = NewMCPStdioClient(m, spec.ServerName, MCPStdioSpec{
			Command: strings.TrimSpace(spec.Command),
			Args:    spec.Args,
			Env:     spec.Env,
			CWD:     strings.TrimSpace(spec.CWD),
		}, m.logger)
	}
	if err != nil {
		return 0, err
	}
	if err := client.Connect(ctx); err != nil {
		// 连不上就别把子进程/客户端留在原地：stdio 已经拉起了进程，不关会变成孤儿。
		client.Disconnect()
		return 0, fmt.Errorf("MCP connect failed: %w", err)
	}
	m.RegisterMCPClient(spec.ServerName, client)
	m.logger.Info("mcp server connected", "server", spec.ServerName, "transport", kind,
		"target", client.Describe(), "tools", client.ToolCount())
	return client.ToolCount(), nil
}

// RegisterMCPClient 注册一个已连接的 MCPClient 到 Manager.mcpClients。
// 供 main.go 的 autoConnectMCPFromConfig（-patch 注入的 MCP 配置自动连接）
// 与 admin API /mcp/connect 共用同一注册路径。
func (m *Manager) RegisterMCPClient(serverName string, client *MCPClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureMCPClients()
	if _, existed := m.mcpClients[serverName]; !existed {
		m.mcpServerCount.Add(1)
	}
	m.mcpClients[serverName] = client
}

// GetMCPClient 取得已注册的 MCP 客户端（供测试与运行时查询）。
func (m *Manager) GetMCPClient(serverName string) *MCPClient {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureMCPClients()
	return m.mcpClients[serverName]
}

// ToolCount 返回 MCP 客户端已发现的工具数量（供日志输出）。
func (c *MCPClient) ToolCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.tools)
}
