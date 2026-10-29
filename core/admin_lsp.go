package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

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
	group.Post("/lsp/start", m.handleLSPStart)
	group.Post("/lsp/stop", m.handleLSPStop)
	group.Get("/lsp/diagnostics", m.handleLSPDiagnostics)
}

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
