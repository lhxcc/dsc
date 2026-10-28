package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dsc/core"
)

// fakeMCPForTUI 极简 MCP 服务端（initialize / tools/list / tools/call），
// 只为验证斜杆命令链路通到宿主。
func fakeMCPForTUI(t *testing.T, toolNames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		body := make([]byte, 0)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-tui")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{},
					"serverInfo": map[string]any{"name": "fake", "version": "1"}},
			})
			return
		}
		switch req.Method {
		case "tools/list":
			tools := make([]map[string]any, 0, len(toolNames))
			for _, name := range toolNames {
				tools = append(tools, map[string]any{
					"name": name, "description": "tool " + name,
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": tools}})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "ok"}}}})
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMCPStatusCommandDefaultsOff /mcp 默认展示「关闭」：日常会话不该为 MCP 付目录成本，
// 这一点必须在人看得见的地方写清楚，否则用户不知道模型为什么看不到 MCP 工具。
func TestMCPStatusCommandDefaultsOff(t *testing.T) {
	m := newRenderCacheModel(t)
	m.manager = core.NewManager(&core.ManagerConfig{})

	if handled, _ := m.runSlashCommand("/mcp"); !handled {
		t.Fatal("/mcp 应被处理")
	}
	out := strings.Join(m.lines, "\n")
	if !strings.Contains(out, "已关闭") {
		t.Fatalf("默认应显示关闭：%s", out)
	}
	if m.manager.MCPToolsEnabled() {
		t.Fatal("默认不应开启注入")
	}
}

// TestMCPOnOffCommand /mcp on|off 直接改宿主状态（不经工具、不需审批）。
func TestMCPOnOffCommand(t *testing.T) {
	m := newRenderCacheModel(t)
	m.manager = core.NewManager(&core.ManagerConfig{})

	m.runSlashCommand("/mcp on")
	if !m.manager.MCPToolsEnabled() {
		t.Fatal("/mcp on 应打开开关")
	}
	if out := strings.Join(m.lines, "\n"); !strings.Contains(out, "已开启") {
		t.Fatalf("应提示已开启：%s", out)
	}

	m.runSlashCommand("/mcp off")
	if m.manager.MCPToolsEnabled() {
		t.Fatal("/mcp off 应关闭开关")
	}
}

// TestMCPConnectAndListViaSlash /mcp connect + /mcp list 走的是宿主工具，
// 与模型调用同一实现（这里验证链路，不重复验证工具语义）。
func TestMCPConnectAndListViaSlash(t *testing.T) {
	srv := fakeMCPForTUI(t, "alpha", "beta")
	m := newRenderCacheModel(t)
	m.manager = core.NewManager(&core.ManagerConfig{})

	cmd := m.runMCPAdmin("connect", []string{"demo", srv.URL})
	if cmd == nil {
		t.Fatal("connect 应返回待执行命令")
	}
	msg, ok := cmd().(mcpAdminMsg)
	if !ok || msg.err != nil {
		t.Fatalf("connect 失败: %+v", msg)
	}
	if !strings.Contains(msg.text, "2 tools") {
		t.Fatalf("connect 结果 = %s", msg.text)
	}

	// 人工连接也应打开开关（与模型侧一致：连上就是要用）
	if !m.manager.MCPToolsEnabled() {
		t.Fatal("connect 后应自动打开注入")
	}

	listCmd := m.runMCPAdmin("list", nil)
	listMsg := listCmd().(mcpAdminMsg)
	if listMsg.err != nil {
		t.Fatalf("list 失败: %v", listMsg.err)
	}
	for _, want := range []string{"mcp__demo__alpha", "mcp__demo__beta"} {
		if !strings.Contains(listMsg.text, want) {
			t.Fatalf("list 应包含 %s：%s", want, listMsg.text)
		}
	}
}

// TestMCPUnknownSubShowsUsage 未知子命令给用法，不能静默什么都不做。
func TestMCPUnknownSubShowsUsage(t *testing.T) {
	m := newRenderCacheModel(t)
	m.manager = core.NewManager(&core.ManagerConfig{})
	m.runSlashCommand("/mcp bogus")
	out := strings.Join(m.lines, "\n")
	if !strings.Contains(out, "/mcp connect") {
		t.Fatalf("应给出用法：%s", out)
	}
}

// TestMCPStatusShowsLoadedCost 状态里要显示已加载工具的目录成本——/mcp on 之后这些
// 工具会随每个请求发出去，用户得知道自己在付多少。
func TestMCPStatusShowsLoadedCost(t *testing.T) {
	srv := fakeMCPForTUI(t, "alpha", "beta")
	m := newRenderCacheModel(t)
	m.manager = core.NewManager(&core.ManagerConfig{})
	if msg := m.runMCPAdmin("connect", []string{"demo", srv.URL})().(mcpAdminMsg); msg.err != nil {
		t.Fatalf("connect: %v", msg.err)
	}
	m.runMCPAdmin("load", []string{"mcp__demo__alpha"})()

	m.lines = nil
	m.runSlashCommand("/mcp")
	out := strings.Join(m.lines, "\n")
	if !strings.Contains(out, "tokens 每请求") || !strings.Contains(out, "mcp__demo__alpha") {
		t.Fatalf("状态应显示已加载工具与成本：%s", out)
	}
}
