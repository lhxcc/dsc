package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dsc/proto"
)

// fakeMCPServer 起一个最小可用的 MCP streamable-HTTP 服务端：initialize / tools/list
// / tools/call。工具名与描述可控，用于验证「注册 ≠ 可见」这条链。
func fakeMCPServer(t *testing.T, toolNames ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
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
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-test")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{},
					"serverInfo":      map[string]any{"name": "fake", "version": "1"},
				},
			})
			return
		}
		switch req.Method {
		case "tools/list":
			tools := make([]map[string]any, 0, len(toolNames))
			for _, name := range toolNames {
				tools = append(tools, map[string]any{
					"name":        name,
					"description": "Tool " + name + " does something useful with a description long enough to cost tokens.",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"query": map[string]any{"type": "string", "description": "query text"},
							"limit": map[string]any{"type": "integer", "description": "max results"},
						},
					},
				})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": tools},
			})
		case "tools/call":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "called"}}},
			})
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func mustConnectFake(t *testing.T, m *Manager, serverName string, toolNames ...string) {
	t.Helper()
	srv := fakeMCPServer(t, toolNames...)
	if _, err := m.ConnectMCPServer(context.Background(), serverName, srv.URL); err != nil {
		t.Fatalf("connect %s: %v", serverName, err)
	}
}

// TestMCPToolsHiddenUntilLoaded 注册 ≠ 可见：连上服务器后，它的工具不得进入模型目录，
// 直到 mcp_load 显式加载。
//
// 这是「按需曝光」的核心不变量。工具目录每个请求都发：实测一个 229 工具的服务器
// 约 6.97 万 tokens（1M 窗口 7%、128K 模型 54%）。连接时就把它们全塞进去，等于让
// 每次请求都替一堆用不上的工具付费。
func TestMCPToolsHiddenUntilLoaded(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	mustConnectFake(t, m, "demo", "alpha", "beta")

	names := toolNamesOf(m.AllToolsProto())
	for _, hidden := range []string{"mcp__demo__alpha", "mcp__demo__beta"} {
		if names[hidden] {
			t.Fatalf("%s 未加载却出现在模型目录里（目录=%v）", hidden, names)
		}
	}
	// 注册表里必须有——「不可见」不等于「不可调用」（子代理、后台任务仍能调）
	if _, ok := m.toolRegistry.Get("mcp__demo__alpha"); !ok {
		t.Fatal("工具应已注册到注册表")
	}
	// 管理工具此时必须可见（否则模型无从发现这些工具）
	for _, want := range []string{mcpListToolName, mcpLoadToolName, mcpUnloadToolName, mcpConnectToolName} {
		if !names[want] {
			t.Fatalf("有 MCP 工具时 %s 应可见（目录=%v）", want, names)
		}
	}

	// 加载一个 → 只它出现
	if changed := m.setMCPToolsLoaded([]string{"mcp__demo__alpha"}, true); len(changed) != 1 {
		t.Fatalf("changed = %v", changed)
	}
	names = toolNamesOf(m.AllToolsProto())
	if !names["mcp__demo__alpha"] {
		t.Fatal("加载后 alpha 应可见")
	}
	if names["mcp__demo__beta"] {
		t.Fatal("beta 未加载，不应可见")
	}

	// 卸载 → 又消失
	m.setMCPToolsLoaded([]string{"mcp__demo__alpha"}, false)
	names = toolNamesOf(m.AllToolsProto())
	if names["mcp__demo__alpha"] {
		t.Fatal("卸载后 alpha 不应可见")
	}
}

// TestMCPManagementToolsHiddenWithoutServer 没有连接任何服务器时，三个运行期管理
// 工具不占目录（它们一个也用不上），但 mcp_connect 必须留着——那是把「没有」变成
// 「有」的唯一入口。
func TestMCPManagementToolsHiddenWithoutServer(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	names := toolNamesOf(m.AllToolsProto())
	for _, hidden := range []string{mcpListToolName, mcpLoadToolName, mcpUnloadToolName} {
		if names[hidden] {
			t.Fatalf("未连接任何服务器时 %s 不应出现在目录里", hidden)
		}
	}
	if !names[mcpConnectToolName] {
		t.Fatal("mcp_connect 必须始终可见（否则模型没有办法连 MCP）")
	}
}

// TestMCPLoadUnloadReportCost 加载/卸载要报告目录成本，模型才有依据「用完即弃」。
func TestMCPLoadUnloadReportCost(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	mustConnectFake(t, m, "demo", "alpha", "beta", "gamma")

	out, err := (&mcpLoadTool{m: m}).Execute(context.Background(), json.RawMessage(`{"server":"demo"}`))
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if !strings.Contains(out, "Loaded 3 tool(s)") || !strings.Contains(out, "tokens per request") {
		t.Fatalf("加载结果应报告数量与成本：%s", out)
	}
	if cost := m.loadedMCPCost(); cost <= 0 {
		t.Fatalf("已加载成本应为正数，实际 %d", cost)
	}

	out, err = (&mcpUnloadTool{m: m}).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("unload all: %v", err)
	}
	if !strings.Contains(out, "Unloaded 3 tool(s)") {
		t.Fatalf("无参数卸载应清空全部：%s", out)
	}
	if len(m.loadedMCPTools()) != 0 {
		t.Fatalf("卸载后应为空，实际 %v", m.loadedMCPTools())
	}
}

// TestMCPLoadAcceptsBareNameAndRejectsAmbiguous 裸名唯一时可用；两台服务器有同名工具
// 时必须报歧义并列出候选，不能猜一个。
func TestMCPLoadAcceptsBareNameAndRejectsAmbiguous(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	mustConnectFake(t, m, "one", "search")
	mustConnectFake(t, m, "two", "search")

	out, err := (&mcpLoadTool{m: m}).Execute(context.Background(), json.RawMessage(`{"tools":["mcp__one__search"]}`))
	if err != nil {
		t.Fatalf("full name load: %v", err)
	}
	if !strings.Contains(out, "mcp__one__search") {
		t.Fatalf("out = %s", out)
	}

	if _, err := (&mcpLoadTool{m: m}).Execute(context.Background(), json.RawMessage(`{"tools":["search"]}`)); err == nil {
		t.Fatal("同名工具应报歧义")
	} else if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("错误应说明歧义并列出候选：%v", err)
	}
}

// TestMCPConnectValidatesArguments 连接前先把参数挡掉：非法服务器名、非 http 端点。
// 前者会生成不合法的工具名，后者会在 doJSON 里报一个难以理解的 URL 错误。
func TestMCPConnectValidatesArguments(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	tool := &mcpConnectTool{m: m}

	cases := []struct{ args, want string }{
		{`{"server_name":"bad name","endpoint":"http://127.0.0.1:1/mcp"}`, "invalid server_name"},
		{`{"server_name":"ok","endpoint":"ftp://x/mcp"}`, "http(s) URL"},
		{`{"server_name":"","endpoint":"http://x/mcp"}`, "required"},
	}
	for _, c := range cases {
		if _, err := tool.Execute(context.Background(), json.RawMessage(c.args)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s → %v, 期望含 %q", c.args, err, c.want)
		}
	}
}

// TestMCPConnectIsIdempotent 重复连接同名服务器不报冲突，而是说明「已连接」并指向
// mcp_list——模型重复调用是常见行为，报冲突会让它以为连接失败。
func TestMCPConnectIsIdempotent(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	srv := fakeMCPServer(t, "alpha")
	tool := &mcpConnectTool{m: m}
	args := json.RawMessage(fmt.Sprintf(`{"server_name":"demo","endpoint":%q}`, srv.URL))

	if _, err := tool.Execute(context.Background(), args); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	out, err := tool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("second connect 不应报错: %v", err)
	}
	if !strings.Contains(out, "already connected") || !strings.Contains(out, "mcp_list") {
		t.Fatalf("out = %s", out)
	}
}

// TestMCPDisconnectForgetsExposure 断开服务器要清掉曝光记录，否则重连同名服务器时
// 同一批工具会「凭空」直接可见——而模型上一轮明明卸载过。
func TestMCPDisconnectForgetsExposure(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	mustConnectFake(t, m, "demo", "alpha")
	m.setMCPToolsLoaded([]string{"mcp__demo__alpha"}, true)
	if !m.mcpToolLoaded("mcp__demo__alpha") {
		t.Fatal("夹具无效：应已加载")
	}
	m.forgetMCPTools("demo")
	if m.mcpToolLoaded("mcp__demo__alpha") {
		t.Fatal("断开后曝光记录应被清掉")
	}
}

// toolNamesOf 把目录转成集合。
func toolNamesOf(tools []*proto.Tool) map[string]bool {
	out := make(map[string]bool, len(tools))
	for _, t := range tools {
		out[t.GetName()] = true
	}
	return out
}
