package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeMCP 一个最小 MCP 服务器（initialize / tools/list / tools/call）。
//
// 测试不引任何外部命令或仓库外的服务器：HTTP 假服务器就够覆盖插件这一层的装配逻辑
// （连接、命名、曝光、加载、断开），而协议与 stdio 管道在 dsc-sdk/mcp 里用自己的
// helper 进程序测过。
func fakeMCP(t *testing.T, tools ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		var resp map[string]any
		if req.ID != nil {
			resp = map[string]any{"jsonrpc": "2.0", "id": req.ID}
		} else {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}}
		case "tools/list":
			items := make([]map[string]any, 0, len(tools))
			for _, name := range tools {
				items = append(items, map[string]any{
					"name": name, "description": name + " tool",
					"inputSchema": map[string]any{"type": "object"},
				})
			}
			resp["result"] = map[string]any{"tools": items}
		case "tools/call":
			resp["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": "called"}}}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTestApp 一个只跑内存状态的 app（状态文件落在 t.TempDir，不碰用户工作区）。
func newTestApp(t *testing.T) *app {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(t.TempDir(), "dsc"), 0o755); err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Setenv("DSC_MCP_STATE", filepath.Join(t.TempDir(), "mcp.state.json"))
	return newApp()
}

// TestConfigShapes 两种配置来源都要认：插件自己的 DSC_MCP_SERVERS，以及宿主转发的
// DSC_PLUGIN_CONFIG（旧部署的 config.mcp 借位写法，含单服务器形状）。
func TestConfigShapes(t *testing.T) {
	t.Setenv("DSC_MCP_SERVERS", `[{"name":"a","endpoint":"http://x/mcp"}]`)
	if specs := specsFromEnv(); len(specs) != 1 || specs[0].Name != "a" {
		t.Fatalf("DSC_MCP_SERVERS 解析失败：%+v", specs)
	}

	t.Setenv("DSC_MCP_SERVERS", "")
	// 旧形状：一条条目一个服务器，名字字段叫 server_name。
	t.Setenv("DSC_PLUGIN_CONFIG", `{"mcp":{"server_name":"legacy","endpoint":"http://y/mcp"}}`)
	specs := specsFromEnv()
	if len(specs) != 1 || specs[0].Name != "legacy" || specs[0].Endpoint != "http://y/mcp" {
		t.Fatalf("旧形状解析失败：%+v", specs)
	}

	// 新形状：servers 数组
	t.Setenv("DSC_PLUGIN_CONFIG", `{"mcp":{"servers":[{"name":"n1","command":"node","args":["s.mjs"]}]}}`)
	specs = specsFromEnv()
	if len(specs) != 1 || specs[0].Command != "node" || len(specs[0].Args) != 1 {
		t.Fatalf("servers 数组解析失败：%+v", specs)
	}

	// 没有 mcp 段：什么都不连（不能瞎猜）
	t.Setenv("DSC_PLUGIN_CONFIG", `{"other":{}}`)
	if specs := specsFromEnv(); len(specs) != 0 {
		t.Fatalf("无关配置不该产生服务器：%+v", specs)
	}
}

// TestConnectExposesHiddenTools 连接后：管理操作进目录，服务器工具**注册但隐藏**。
func TestConnectExposesHiddenTools(t *testing.T) {
	a := newTestApp(t)
	srv := fakeMCP(t, "alpha", "beta")

	if _, err := a.execConnect(map[string]any{"server_name": "demo", "endpoint": srv.URL}); err == nil {
		t.Fatal("总闸关闭时不应允许连接")
	}
	a.setEnabled(true)

	out, err := a.execConnect(map[string]any{"server_name": "demo", "endpoint": srv.URL})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if !strings.Contains(out, "2 tools") {
		t.Fatalf("connect 回执不符：%s", out)
	}

	tools := map[string]toolView{}
	for _, tool := range a.currentTools() {
		tools[tool.Name] = toolView{hidden: tool.Hidden, management: tool.Management}
	}
	if _, ok := tools["mcp_connect"]; !ok {
		t.Fatal("开启后应出现管理操作")
	}
	if tools["mcp_connect"].management != "mcp" {
		t.Fatalf("管理操作应声明归属，got %q", tools["mcp_connect"].management)
	}
	for _, name := range []string{"mcp__demo__alpha", "mcp__demo__beta"} {
		v, ok := tools[name]
		if !ok {
			t.Fatalf("服务器工具未上报：%s（现有 %v）", name, tools)
		}
		if !v.hidden {
			t.Fatalf("%s 未加载时应隐藏", name)
		}
	}

	// 加载后只影响曝光，不影响注册
	if _, err := a.execLoad(map[string]any{"tools": []any{"mcp__demo__alpha"}}); err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, tool := range a.currentTools() {
		switch tool.Name {
		case "mcp__demo__alpha":
			if tool.Hidden {
				t.Fatal("加载后应对模型可见")
			}
		case "mcp__demo__beta":
			if !tool.Hidden {
				t.Fatal("未加载的仍应隐藏")
			}
		}
	}
}

// TestListLoadUnloadDisconnect 全链路：列 → 裸名加载 → 卸载 → 断开清空。
func TestListLoadUnloadDisconnect(t *testing.T) {
	a := newTestApp(t)
	a.setEnabled(true)
	srv := fakeMCP(t, "alpha", "beta")
	if _, err := a.execConnect(map[string]any{"server_name": "demo", "endpoint": srv.URL}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	out, err := a.execList(map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "mcp__demo__alpha") || !strings.Contains(out, "mcp__demo__beta") {
		t.Fatalf("list 缺工具：%s", out)
	}

	// 裸名（唯一）应可解析
	if _, err := a.execLoad(map[string]any{"tools": []any{"alpha"}}); err != nil {
		t.Fatalf("裸名 load: %v", err)
	}
	out, _ = a.execList(map[string]any{})
	if !strings.Contains(out, "mcp__demo__alpha [loaded]") {
		t.Fatalf("加载标记缺失：%s", out)
	}

	if out, err := a.execUnload(map[string]any{"server": "demo"}); err != nil || !strings.Contains(out, "1 tool") {
		t.Fatalf("unload: %v %s", err, out)
	}

	out, err = a.execDisconnect(map[string]any{"server_name": "demo"})
	if err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if !strings.Contains(out, "unregistered") {
		t.Fatalf("disconnect 回执不符：%s", out)
	}
	for _, tool := range a.currentTools() {
		if strings.HasPrefix(tool.Name, "mcp__demo__") {
			t.Fatalf("断开后仍上报工具：%s", tool.Name)
		}
	}
}

// TestStatusAndToggle /mcp 的开关与状态文本，并且开关要落盘（重启后保持）。
func TestStatusAndToggle(t *testing.T) {
	a := newTestApp(t)
	if a.isEnabled() {
		t.Fatal("默认应关闭")
	}
	text, err := a.runCommand(nil, "mcp", "on")
	if err != nil {
		t.Fatalf("on: %v", err)
	}
	if !strings.Contains(text, "已开启") {
		t.Fatalf("on 后状态不符：%s", text)
	}
	if loadEnabled(a.statePath) != true {
		t.Fatal("开关应落盘")
	}

	if text, _ := a.runCommand(nil, "mcp", ""); !strings.Contains(text, "服务器：未连接") {
		t.Fatalf("无参应显示状态：%s", text)
	}
	if _, err := a.runCommand(nil, "mcp", "bogus"); err == nil {
		t.Fatal("未知子命令应报错")
	}
}

// TestPublicNameRules 公开名规则：前缀、非法字符替换、截断。
func TestPublicNameRules(t *testing.T) {
	if got := publicName("srv", "tools/list"); got != "mcp__srv__tools_list" {
		t.Fatalf("非法字符应替换：%q", got)
	}
	if got := publicName("srv", strings.Repeat("a", 100)); len(got) != 64 {
		t.Fatalf("应截断到 64，得到 %d", len(got))
	}
	if serverOf("mcp__srv__x") != "srv" {
		t.Fatalf("serverOf 解析错：%q", serverOf("mcp__srv__x"))
	}
}

type toolView struct {
	hidden     bool
	management string
}
