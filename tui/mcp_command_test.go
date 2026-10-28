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
// newMCPTestModel 建一个带临时 config.yaml 的 TUI 模型：/mcp on|off 会写回配置，
// 测试绝不能碰到仓库里真实的那份。
func newMCPTestModel(t *testing.T) (*Model, string) {
	t.Helper()
	m := newRenderCacheModel(t)
	m.manager = core.NewManager(&core.ManagerConfig{})
	path := core.PJoin(t.TempDir(), "config.yaml")
	m.manager.SetConfigPath(path)
	return m, path
}

func TestMCPStatusCommandDefaultsOff(t *testing.T) {
	m, _ := newMCPTestModel(t)

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
	m, path := newMCPTestModel(t)

	m.runSlashCommand("/mcp on")
	if !m.manager.MCPToolsEnabled() {
		t.Fatal("/mcp on 应打开开关")
	}
	if out := strings.Join(m.lines, "\n"); !strings.Contains(out, "已开启") {
		t.Fatalf("应提示已开启：%s", out)
	}

	// 已写回配置：重启后仍启用
	cfg, err := core.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if cfg.MCP == nil || !cfg.MCP.Enabled {
		t.Fatalf("on 应写回 mcp.enabled=true，实际 %+v", cfg.MCP)
	}

	m.runSlashCommand("/mcp off")
	if m.manager.MCPToolsEnabled() {
		t.Fatal("/mcp off 应关闭开关")
	}
	cfg, _ = core.LoadConfig(path)
	if cfg.MCP == nil || cfg.MCP.Enabled {
		t.Fatalf("off 应写回 mcp.enabled=false，实际 %+v", cfg.MCP)
	}
	// 状态里要写出写到哪个文件（否则用户不知道改的是哪份配置）
	if out := strings.Join(m.lines, "\n"); !strings.Contains(out, "已写入配置") {
		t.Fatalf("应提示写回路径：%s", out)
	}
}

// TestMCPConnectAndListViaSlash /mcp connect + /mcp list 走的是宿主工具，
// 与模型调用同一实现（这里验证链路，不重复验证工具语义）。
func TestMCPConnectAndListViaSlash(t *testing.T) {
	srv := fakeMCPForTUI(t, "alpha", "beta")
	m, _ := newMCPTestModel(t)
	m.manager.SetMCPToolsEnabled(true) // 开关只由人改：连接不会自动打开

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
	m, _ := newMCPTestModel(t)
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
	m, _ := newMCPTestModel(t)
	m.manager.SetMCPToolsEnabled(true)
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

// TestMCPConnectRequiresEnable 开关关闭时 /mcp connect 不静默帮用户打开：开关是要写进
// 配置文件的部署决定，必须是人明确按下的 /mcp on。
func TestMCPConnectRequiresEnable(t *testing.T) {
	srv := fakeMCPForTUI(t, "alpha")
	m, path := newMCPTestModel(t)

	if cmd := m.runMCPAdmin("connect", []string{"demo", srv.URL}); cmd != nil {
		t.Fatal("未启用时不应真的去连接")
	}
	if out := strings.Join(m.lines, "\n"); !strings.Contains(out, "/mcp on") {
		t.Fatalf("应提示先启用：%s", out)
	}
	if m.manager.MCPToolsEnabled() {
		t.Fatal("不应自动打开开关")
	}
	if cfg, err := core.LoadConfig(path); err == nil && cfg.MCP != nil && cfg.MCP.Enabled {
		t.Fatal("不应把开关写进配置")
	}

	// 启用后同一命令照常工作
	m.runSlashCommand("/mcp on")
	if cmd := m.runMCPAdmin("connect", []string{"demo", srv.URL}); cmd == nil {
		t.Fatal("启用后 connect 应执行")
	}
}
