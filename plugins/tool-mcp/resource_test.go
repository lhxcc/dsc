package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeRichMCP 一台同时提供 tools（分两页）、resources、prompts 的 MCP 服务器。
//
// 三个能力一起给，是为了验证插件这一层的装配规则：
//   - tools 分页要翻到底（只取第一页会静默少列工具）；
//   - resources / prompts 工具**只在服务器声明该能力时**才出现在目录里；
//   - 读取/取模板按 uri / name 定位到正确的服务器。
type richMCPOptions struct {
	tools     bool
	resources bool
	prompts   bool
	pages     bool // tools/list 是否分页
}

func fakeRichMCP(t *testing.T, opt richMCPOptions) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     any             `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var params map[string]any
		_ = json.Unmarshal(req.Params, &params)

		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			caps := map[string]any{}
			if opt.tools {
				caps["tools"] = map[string]any{}
			}
			if opt.resources {
				caps["resources"] = map[string]any{}
			}
			if opt.prompts {
				caps["prompts"] = map[string]any{}
			}
			resp["result"] = map[string]any{"protocolVersion": "2024-11-05", "capabilities": caps}
		case "tools/list":
			page := "1"
			if c, ok := params["cursor"].(string); ok {
				page = c
			}
			if !opt.pages || page == "1" {
				items := []any{
					map[string]any{"name": "alpha", "description": "alpha tool", "inputSchema": map[string]any{"type": "object"}},
					map[string]any{"name": "beta", "description": "beta tool", "inputSchema": map[string]any{"type": "object"}},
				}
				result := map[string]any{"tools": items}
				if opt.pages {
					result["nextCursor"] = "2"
				}
				resp["result"] = result
			} else {
				resp["result"] = map[string]any{"tools": []any{
					map[string]any{"name": "gamma", "description": "gamma tool", "inputSchema": map[string]any{"type": "object"}},
				}}
			}
		case "resources/list":
			resp["result"] = map[string]any{"resources": []any{
				map[string]any{"uri": "mem://doc", "name": "doc", "description": "a doc", "mimeType": "text/plain"},
				map[string]any{"uri": "mem://blob", "name": "blob", "mimeType": "application/octet-stream"},
			}}
		case "resources/read":
			uri, _ := params["uri"].(string)
			if uri == "mem://blob" {
				resp["result"] = map[string]any{"contents": []any{map[string]any{
					"uri": uri, "mimeType": "application/octet-stream", "blob": "AAAA",
				}}}
				break
			}
			resp["result"] = map[string]any{"contents": []any{map[string]any{
				"uri": uri, "mimeType": "text/plain", "text": "content of " + uri,
			}}}
		case "prompts/list":
			resp["result"] = map[string]any{"prompts": []any{
				map[string]any{"name": "summarize", "description": "summarize", "arguments": []any{
					map[string]any{"name": "text", "required": true},
				}},
			}}
		case "prompts/get":
			resp["result"] = map[string]any{
				"messages": []any{
					map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "please summarize"}},
				},
			}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestToolsPaginationThroughPlugin 插件侧也走分页：第一页 2 个 + 第二页 1 个 = 3 个。
func TestToolsPaginationThroughPlugin(t *testing.T) {
	a := newTestApp(t)
	a.setEnabled(true)
	srv := fakeRichMCP(t, richMCPOptions{tools: true, pages: true})
	if _, err := a.execConnect(map[string]any{"server_name": "paged", "endpoint": srv.URL}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	out, err := a.execList(map[string]any{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, want := range []string{"mcp__paged__alpha", "mcp__paged__beta", "mcp__paged__gamma"} {
		if !strings.Contains(out, want) {
			t.Fatalf("分页未翻到底，缺 %s：\n%s", want, out)
		}
	}
}

// TestResourceAndPromptToolsGatedByCapability 能力决定工具是否出现：只有 tools 的服务器
// 不该多出资源/提示词工具（目录成本为 0），声明了的才出现且立即可见。
func TestResourceAndPromptToolsGatedByCapability(t *testing.T) {
	plain := newTestApp(t)
	plain.setEnabled(true)
	if _, err := plain.execConnect(map[string]any{"server_name": "plain", "endpoint": fakeRichMCP(t, richMCPOptions{tools: true}).URL}); err != nil {
		t.Fatalf("connect plain: %v", err)
	}
	for _, tool := range plain.currentTools() {
		if strings.HasPrefix(tool.Name, "mcp_list_resources") || strings.HasPrefix(tool.Name, "mcp_get_prompt") {
			t.Fatalf("只有 tools 的服务器不该出现 %s", tool.Name)
		}
	}

	rich := newTestApp(t)
	rich.setEnabled(true)
	if _, err := rich.execConnect(map[string]any{"server_name": "rich", "endpoint": fakeRichMCP(t, richMCPOptions{tools: true, resources: true, prompts: true}).URL}); err != nil {
		t.Fatalf("connect rich: %v", err)
	}
	seen := map[string]bool{}
	for _, tool := range rich.currentTools() {
		seen[tool.Name] = true
		switch tool.Name {
		case "mcp_list_resources", "mcp_read_resource", "mcp_list_prompts", "mcp_get_prompt":
			if tool.Hidden {
				t.Fatalf("%s 在能力可用时应可见（它不走 mcp_load 曝光策略）", tool.Name)
			}
		}
	}
	for _, want := range []string{"mcp_list_resources", "mcp_read_resource", "mcp_list_prompts", "mcp_get_prompt"} {
		if !seen[want] {
			t.Fatalf("声明了能力却缺少 %s（现有 %v）", want, seen)
		}
	}
}

// TestResourceReadAndPromptGet 读资源、取提示词，含二进制资源的省略说明与错误路径。
func TestResourceReadAndPromptGet(t *testing.T) {
	a := newTestApp(t)
	a.setEnabled(true)
	if _, err := a.execConnect(map[string]any{"server_name": "rich", "endpoint": fakeRichMCP(t, richMCPOptions{tools: true, resources: true, prompts: true}).URL}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	out, err := a.execListResources(map[string]any{})
	if err != nil {
		t.Fatalf("list_resources: %v", err)
	}
	if !strings.Contains(out, "mem://doc") || !strings.Contains(out, "text/plain") {
		t.Fatalf("资源列表不符：%s", out)
	}

	text, err := a.execReadResource(context.Background(), map[string]any{"uri": "mem://doc"})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if text != "content of mem://doc" {
		t.Fatalf("读到的内容不符：%q", text)
	}

	blob, err := a.execReadResource(context.Background(), map[string]any{"uri": "mem://blob"})
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !strings.Contains(blob, "binary resource omitted") {
		t.Fatalf("二进制资源应给出省略说明：%q", blob)
	}

	if _, err := a.execReadResource(context.Background(), map[string]any{"uri": "mem://nope"}); err == nil {
		t.Fatal("不存在的资源应报错")
	}
	if _, err := a.execReadResource(context.Background(), map[string]any{}); err == nil {
		t.Fatal("缺 uri 应报错")
	}

	prompts, err := a.execListPrompts(map[string]any{})
	if err != nil {
		t.Fatalf("list_prompts: %v", err)
	}
	if !strings.Contains(prompts, "summarize") || !strings.Contains(prompts, "text*") {
		t.Fatalf("提示词列表不符（必填参数应带 *）：%s", prompts)
	}

	rendered, err := a.execGetPrompt(context.Background(), map[string]any{"name": "summarize", "arguments": map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatalf("get_prompt: %v", err)
	}
	if !strings.Contains(rendered, "[user]") || !strings.Contains(rendered, "please summarize") {
		t.Fatalf("提示词渲染不符：%s", rendered)
	}
}

// TestResourceAmbiguousServer 同名资源出现在多台服务器时必须要求点名，而不是随便挑一台。
func TestResourceAmbiguousServer(t *testing.T) {
	a := newTestApp(t)
	a.setEnabled(true)
	opt := richMCPOptions{tools: true, resources: true}
	for _, name := range []string{"one", "two"} {
		if _, err := a.execConnect(map[string]any{"server_name": name, "endpoint": fakeRichMCP(t, opt).URL}); err != nil {
			t.Fatalf("connect %s: %v", name, err)
		}
	}
	if _, err := a.execReadResource(context.Background(), map[string]any{"uri": "mem://doc"}); err == nil ||
		!strings.Contains(err.Error(), "several servers") {
		t.Fatalf("歧义资源应要求 server=，得到 %v", err)
	}
	text, err := a.execReadResource(context.Background(), map[string]any{"uri": "mem://doc", "server": "two"})
	if err != nil || text != "content of mem://doc" {
		t.Fatalf("点名服务器后应可读：%q, %v", text, err)
	}
	if _, err := a.execReadResource(context.Background(), map[string]any{"uri": "mem://doc", "server": "nope"}); err == nil {
		t.Fatal("未连接的服务器应报错")
	}
}

// TestResourceCommandEntry /mcp resources / prompts 走同一套实现。
func TestResourceCommandEntry(t *testing.T) {
	a := newTestApp(t)
	a.setEnabled(true)
	if _, err := a.execConnect(map[string]any{"server_name": "rich", "endpoint": fakeRichMCP(t, richMCPOptions{tools: true, resources: true, prompts: true}).URL}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if out, err := a.runCommand(nil, "mcp", "resources"); err != nil || !strings.Contains(out, "mem://doc") {
		t.Fatalf("/mcp resources: %v %s", err, out)
	}
	if out, err := a.runCommand(nil, "mcp", "read mem://doc"); err != nil || !strings.Contains(out, "content of") {
		t.Fatalf("/mcp read: %v %s", err, out)
	}
	if out, err := a.runCommand(nil, "mcp", "prompts"); err != nil || !strings.Contains(out, "summarize") {
		t.Fatalf("/mcp prompts: %v %s", err, out)
	}
	if out, err := a.runCommand(nil, "mcp", "prompt summarize text=hi"); err != nil || !strings.Contains(out, "please summarize") {
		t.Fatalf("/mcp prompt: %v %s", err, out)
	}
	// 状态行应带上资源/提示词计数
	if out, _ := a.runCommand(nil, "mcp", "status"); !strings.Contains(out, "个资源") || !strings.Contains(out, "个提示词") {
		t.Fatalf("status 缺少资源/提示词计数：%s", out)
	}
}
