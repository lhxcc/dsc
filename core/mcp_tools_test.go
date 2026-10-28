package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
	m.SetMCPToolsEnabled(true) // 注入开关默认关闭，见 TestMCPToolsDisabledByDefault

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
	m.SetMCPToolsEnabled(true)
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
	m.SetMCPToolsEnabled(true)

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
	m.SetMCPToolsEnabled(true)

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

// TestMCPToolsDisabledByDefault 开关是**总闸**：默认关闭时除 MCP 之外一个相关工具都
// 不出现——连入口 mcp_connect 也不注入，目录成本恰为 0。日常会话根本不用 MCP，没有
// 理由为「可能用到」常態付费。
func TestMCPToolsDisabledByDefault(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	if m.MCPToolsEnabled() {
		t.Fatal("MCP 注入开关应默认关闭")
	}
	mustConnectFake(t, m, "demo", "alpha", "beta")
	m.setMCPToolsLoaded([]string{"mcp__demo__alpha"}, true)

	for _, tool := range m.AllToolsProto() {
		name := tool.GetName()
		if isMCPToolName(name) || isMCPManagementTool(name) {
			t.Fatalf("关闭状态下不应出现 MCP 工具：%s", name)
		}
	}

	// 打开后：入口 + 管理工具 + 已加载的那一个；未加载的仍然不出现
	m.SetMCPToolsEnabled(true)
	names := toolNamesOf(m.AllToolsProto())
	for _, want := range []string{mcpConnectToolName, mcpListToolName, mcpLoadToolName, mcpUnloadToolName, mcpDisconnectToolName, "mcp__demo__alpha"} {
		if !names[want] {
			t.Fatalf("打开后应出现 %s（目录=%v）", want, names)
		}
	}
	if names["mcp__demo__beta"] {
		t.Fatal("未加载的工具不应因打开开关而出现")
	}

	// 关闭不清加载记录：再打开仍是同一批
	m.SetMCPToolsEnabled(false)
	m.SetMCPToolsEnabled(true)
	if !toolNamesOf(m.AllToolsProto())["mcp__demo__alpha"] {
		t.Fatal("关闭/打开不应丢失加载记录")
	}
}

// TestMCPConnectDoesNotToggleSwitch 开关只由人改：连接动作不得改变它（关着时它根本
// 不可见；能看见就说明已经开着）。模型侧没有任何工具能开关 MCP 注入。
func TestMCPConnectDoesNotToggleSwitch(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	m.SetMCPToolsEnabled(true)
	srv := fakeMCPServer(t, "alpha")
	if _, err := (&mcpConnectTool{m: m}).Execute(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"server_name":"demo","endpoint":%q}`, srv.URL))); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !m.MCPToolsEnabled() {
		t.Fatal("连接不应关掉开关")
	}
	// 关掉后再直接调一次工具（模拟被别的路径调用）：开关不应被打开
	m.SetMCPToolsEnabled(false)
	if _, err := (&mcpConnectTool{m: m}).Execute(context.Background(),
		json.RawMessage(fmt.Sprintf(`{"server_name":"demo2","endpoint":%q}`, srv.URL))); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if m.MCPToolsEnabled() {
		t.Fatal("连接工具不得打开开关（开关只由人经 /mcp on 改）")
	}
}

// TestPersistMCPEnabledWritesConfig /mcp on|off 要把开关写回 config.yaml，且只动 mcp
// 段——注释、其它字段原样保留（这是「配置是用户手写的」这一前提的要求）。
func TestPersistMCPEnabledWritesConfig(t *testing.T) {
	dir := t.TempDir()
	path := PJoin(dir, "config.yaml")
	original := "# 用户手写的注释\nworkspace_root: /tmp/ws\npersona: 保持简洁\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	m.SetConfigPath(path)

	if err := m.SetMCPToolsEnabledPersist(true); err != nil {
		t.Fatalf("persist on: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.MCP == nil || !cfg.MCP.Enabled {
		t.Fatalf("配置应写入 mcp.enabled=true，实际 %+v", cfg.MCP)
	}
	if cfg.WorkspaceRoot != "/tmp/ws" || cfg.Persona != "保持简洁" {
		t.Fatalf("不应破坏其它字段：%+v", cfg)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# 用户手写的注释") {
		t.Fatalf("注释应保留：%s", data)
	}

	// 关闭写回 false（显式表达「关」，而不是删掉键让人猜）
	if err := m.SetMCPToolsEnabledPersist(false); err != nil {
		t.Fatalf("persist off: %v", err)
	}
	cfg, _ = LoadConfig(path)
	if cfg.MCP == nil || cfg.MCP.Enabled {
		t.Fatalf("配置应写入 mcp.enabled=false，实际 %+v", cfg.MCP)
	}
	// 幂等：重复写不产生第二个 mcp 段
	if err := m.SetMCPToolsEnabledPersist(false); err != nil {
		t.Fatalf("persist off again: %v", err)
	}
	data, _ = os.ReadFile(path)
	if strings.Count(string(data), "mcp:") != 1 {
		t.Fatalf("bogus 重复段：%s", data)
	}
}

// TestMissingMCPConfigMeansDisabled 配置里没有 mcp 段即视为未启用（默认关闭的语义
// 必须由配置本身表达，而不是靠某个内存默认值）。
func TestMissingMCPConfigMeansDisabled(t *testing.T) {
	path := PJoin(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("workspace_root: /tmp/ws\n"), 0644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MCP != nil {
		t.Fatalf("无 mcp 段应解析为 nil（未启用），实际 %+v", cfg.MCP)
	}
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	if m.MCPToolsEnabled() {
		t.Fatal("未配置时开关应为关闭")
	}
}

// TestMCPDisconnectUnregistersEverything 断开必须三步一起做：删登记、注销工具、清曝光。
// 只删登记会留下一批调不通的幽灵工具；只注销工具会留下空壳，重连被判「已连接」。
func TestMCPDisconnectUnregistersEverything(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	mustConnectFake(t, m, "demo", "alpha", "beta")
	m.SetMCPToolsEnabled(true)
	m.setMCPToolsLoaded([]string{"mcp__demo__alpha"}, true)

	out, err := (&mcpDisconnectTool{m: m}).Execute(context.Background(), json.RawMessage(`{"server_name":"demo"}`))
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if !strings.Contains(out, "2 tool(s) unregistered") {
		t.Fatalf("out = %s", out)
	}
	if m.GetMCPClient("demo") != nil {
		t.Fatal("登记应被删除")
	}
	if _, ok := m.toolRegistry.Get("mcp__demo__alpha"); ok {
		t.Fatal("工具应从注册表注销")
	}
	if m.mcpToolLoaded("mcp__demo__alpha") {
		t.Fatal("曝光记录应被清掉")
	}
	// 断开不关开关：其它服务器/后续连接仍按开关注入
	if !m.MCPToolsEnabled() {
		t.Fatal("断开不应关闭注入开关")
	}
	// 未连接的服务器报错要说清现状
	if _, err := (&mcpDisconnectTool{m: m}).Execute(context.Background(), json.RawMessage(`{"server_name":"nope"}`)); err == nil {
		t.Fatal("断开未连接的服务器应报错")
	}
}

// TestMCPDisconnectAll 空 server_name = 全部断开。
func TestMCPDisconnectAll(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	mustConnectFake(t, m, "one", "a")
	mustConnectFake(t, m, "two", "b")
	out, err := (&mcpDisconnectTool{m: m}).Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("disconnect all: %v", err)
	}
	if !strings.Contains(out, "2 server(s)") {
		t.Fatalf("out = %s", out)
	}
	if len(m.MCPStatus().Servers) != 0 {
		t.Fatal("应全部断开")
	}
}
