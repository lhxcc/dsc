package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fakeTransport 一个进程内的 JSON-RPC 服务器：按方法名查表，返回预置的响应。
//
// 用它而不是起子进程，是因为这里要验的是**客户端协议层**的行为（翻页、能力降级、
// 内容块渲染），不是进程管道——进程管道已有 stdio 那组测试覆盖。
type fakeTransport struct {
	// handler 收到 method + params，返回 result（nil 表示返回 JSON-RPC error）。
	handler func(method string, params map[string]any) (any, *rpcError)
	calls   []string
}

type rpcError struct {
	code    int
	message string
}

func (f *fakeTransport) RoundTrip(_ context.Context, req map[string]any) (map[string]any, error) {
	method, _ := req["method"].(string)
	params, _ := req["params"].(map[string]any)
	f.calls = append(f.calls, method)
	if method == "notifications/initialized" {
		return map[string]any{}, nil
	}
	result, rerr := f.handler(method, params)
	if rerr != nil {
		return map[string]any{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"error":   map[string]any{"code": rerr.code, "message": rerr.message},
		}, nil
	}
	return map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result}, nil
}

func (f *fakeTransport) Reset(Logger) error { return nil }
func (f *fakeTransport) Close()             {}
func (f *fakeTransport) Kind() string       { return "fake" }
func (f *fakeTransport) Describe() string   { return "fake" }

// TestToolsListPagination 分页必须翻到底：只取第一页会静默少列工具。
func TestToolsListPagination(t *testing.T) {
	pages := [][]any{
		{
			map[string]any{"name": "a", "description": "first page", "inputSchema": map[string]any{"type": "object"}},
			map[string]any{"name": "b", "description": "first page"},
		},
		{
			map[string]any{"name": "c", "description": "second page"},
		},
		{
			map[string]any{"name": "d", "description": "third page"},
		},
	}
	tr := &fakeTransport{}
	tr.handler = func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "initialize":
			return map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
			}, nil
		case "tools/list":
			idx := 0
			if cursor, ok := params["cursor"].(string); ok {
				fmt.Sscanf(cursor, "page-%d", &idx)
			}
			result := map[string]any{"tools": pages[idx]}
			if idx+1 < len(pages) {
				result["nextCursor"] = fmt.Sprintf("page-%d", idx+1)
			}
			return result, nil
		}
		return map[string]any{}, nil
	}

	c := NewClient("paged", tr, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if got := c.ToolCount(); got != 4 {
		t.Fatalf("工具数 = %d, 期望 4（分页未翻到底）", got)
	}
	names := make([]string, 0, 4)
	for _, tool := range c.Tools() {
		names = append(names, tool.Name)
	}
	if strings.Join(names, ",") != "a,b,c,d" {
		t.Fatalf("工具名 = %v, 期望 a,b,c,d（且按名排序）", names)
	}
}

// TestToolsListPaginationStopsOnRepeatedCursor 服务器把同一个 cursor 反复给回来时不能死循环。
func TestToolsListPaginationStopsOnRepeatedCursor(t *testing.T) {
	tr := &fakeTransport{}
	tr.handler = func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "initialize":
			return map[string]any{"capabilities": map[string]any{"tools": map[string]any{}}}, nil
		case "tools/list":
			return map[string]any{
				"tools":      []any{map[string]any{"name": "loop"}},
				"nextCursor": "same",
			}, nil
		}
		return map[string]any{}, nil
	}

	c := NewClient("looping", tr, nil)
	done := make(chan error, 1)
	go func() { done <- c.Connect(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
	case <-context.Background().Done():
	}
	if got := c.ToolCount(); got != 1 {
		t.Fatalf("工具数 = %d, 期望 1（重复 cursor 应立即停下）", got)
	}
	// 只应问两次：第一页 + 一次带 cursor 的重试
	listCalls := 0
	for _, m := range tr.calls {
		if m == "tools/list" {
			listCalls++
		}
	}
	if listCalls > 2 {
		t.Fatalf("tools/list 被调用 %d 次（重复 cursor 未及时停止）", listCalls)
	}
}

// TestResourcesAndPrompts 资源与提示词的发现、读取、渲染。
func TestResourcesAndPrompts(t *testing.T) {
	tr := &fakeTransport{}
	tr.handler = func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "initialize":
			return map[string]any{"capabilities": map[string]any{
				"tools":     map[string]any{},
				"resources": map[string]any{"subscribe": false},
				"prompts":   map[string]any{},
			}}, nil
		case "tools/list":
			return map[string]any{"tools": []any{map[string]any{"name": "t1"}}}, nil
		case "resources/list":
			// 两页：验证 resources 也走同一套翻页
			if params["cursor"] == "r2" {
				return map[string]any{"resources": []any{
					map[string]any{"uri": "mem://second", "name": "second"},
				}}, nil
			}
			return map[string]any{
				"resources": []any{
					map[string]any{"uri": "mem://first", "name": "first", "description": "one", "mimeType": "text/plain"},
					map[string]any{"uri": "mem://bin", "name": "bin", "mimeType": "image/png"},
				},
				"nextCursor": "r2",
			}, nil
		case "prompts/list":
			return map[string]any{"prompts": []any{
				map[string]any{
					"name": "summarize", "description": "summarize text",
					"arguments": []any{
						map[string]any{"name": "text", "description": "what to summarize", "required": true},
						map[string]any{"name": "style", "required": false},
					},
				},
			}}, nil
		case "resources/read":
			uri, _ := params["uri"].(string)
			if uri == "mem://bin" {
				return map[string]any{"contents": []any{map[string]any{
					"uri": uri, "mimeType": "image/png", "blob": strings.Repeat("A", 2048),
				}}}, nil
			}
			return map[string]any{"contents": []any{map[string]any{
				"uri": uri, "mimeType": "text/plain", "text": "hello resource",
			}}}, nil
		case "prompts/get":
			name, _ := params["name"].(string)
			if name != "summarize" {
				return nil, &rpcError{code: -32602, message: "unknown prompt"}
			}
			return map[string]any{
				"description": "summarize text",
				"messages": []any{
					map[string]any{"role": "user", "content": map[string]any{"type": "text", "text": "summarize: hello"}},
					map[string]any{"role": "assistant", "content": []any{
						map[string]any{"type": "text", "text": "ok"},
						map[string]any{"type": "image", "data": "…", "mimeType": "image/png"},
					}},
				},
			}, nil
		}
		return map[string]any{}, nil
	}

	c := NewClient("full", tr, nil)
	ctx := context.Background()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}

	caps := c.Capabilities()
	if !caps.Declared || !caps.Tools || !caps.Resources || !caps.Prompts {
		t.Fatalf("能力解析错误：%+v", caps)
	}

	if got := c.ResourceCount(); got != 3 {
		t.Fatalf("资源数 = %d, 期望 3（含第二页）", got)
	}
	resources := c.Resources()
	if resources[0].URI != "mem://bin" || resources[0].MimeType != "image/png" {
		t.Fatalf("资源排序/字段错误：%+v", resources[0])
	}

	text, err := c.ReadResource(ctx, "mem://first")
	if err != nil || text != "hello resource" {
		t.Fatalf("ReadResource = %q, %v", text, err)
	}
	blob, err := c.ReadResource(ctx, "mem://bin")
	if err != nil {
		t.Fatalf("ReadResource(bin): %v", err)
	}
	if !strings.Contains(blob, "binary resource omitted") || !strings.Contains(blob, "image/png") {
		t.Fatalf("二进制资源应给出省略说明，得到 %q", blob)
	}

	if got := c.PromptCount(); got != 1 {
		t.Fatalf("提示词数 = %d, 期望 1", got)
	}
	prompt := c.Prompts()[0]
	if prompt.Name != "summarize" || len(prompt.Arguments) != 2 || !prompt.Arguments[0].Required {
		t.Fatalf("提示词解析错误：%+v", prompt)
	}

	rendered, err := c.GetPrompt(ctx, "summarize", map[string]string{"text": "hello"})
	if err != nil {
		t.Fatalf("GetPrompt: %v", err)
	}
	for _, want := range []string{"summarize text", "[user]", "summarize: hello", "[assistant]", "ok", "[image block omitted]"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("GetPrompt 输出缺少 %q：\n%s", want, rendered)
		}
	}

	if _, err := c.GetPrompt(ctx, "nope", nil); err == nil {
		t.Fatal("未知 prompt 应返回错误")
	}
}

// TestToolsOnlyServerSkipsResourcesAndPrompts 没有声明就不问：少噪音、少一轮握手往返。
func TestToolsOnlyServerSkipsResourcesAndPrompts(t *testing.T) {
	tr := &fakeTransport{}
	tr.handler = func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "initialize":
			return map[string]any{"capabilities": map[string]any{"tools": map[string]any{}}}, nil
		case "tools/list":
			return map[string]any{"tools": []any{map[string]any{"name": "only"}}}, nil
		default:
			return nil, &rpcError{code: -32601, message: "method not found"}
		}
	}

	c := NewClient("tools-only", tr, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	for _, method := range tr.calls {
		if method == "resources/list" || method == "prompts/list" {
			t.Fatalf("未声明该能力的服务器不应被问 %s（调用序列 %v）", method, tr.calls)
		}
	}
}

// TestPromptsOnlyServerToleratesMissingTools prompts-only 服务器不能因为 tools/list 报
// -32601 就整个连接失败；反过来，声明了 tools 的服务器失败仍须报错。
func TestPromptsOnlyServerToleratesMissingTools(t *testing.T) {
	tr := &fakeTransport{}
	tr.handler = func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "initialize":
			return map[string]any{"capabilities": map[string]any{"prompts": map[string]any{}}}, nil
		case "tools/list":
			return nil, &rpcError{code: -32601, message: "method not found"}
		case "prompts/list":
			return map[string]any{"prompts": []any{map[string]any{"name": "p"}}}, nil
		}
		return map[string]any{}, nil
	}
	c := NewClient("prompts-only", tr, nil)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("connect 应容忍 tools/list 不支持：%v", err)
	}
	if c.ToolCount() != 0 || c.PromptCount() != 1 {
		t.Fatalf("tools=%d prompts=%d, 期望 0/1", c.ToolCount(), c.PromptCount())
	}

	// 反面：明说有 tools 却失败 → 必须报错，否则「目录空了」会被当成「没有工具」
	tr2 := &fakeTransport{}
	tr2.handler = func(method string, params map[string]any) (any, *rpcError) {
		switch method {
		case "initialize":
			return map[string]any{"capabilities": map[string]any{"tools": map[string]any{}}}, nil
		case "tools/list":
			return nil, &rpcError{code: -32603, message: "boom"}
		}
		return map[string]any{}, nil
	}
	if err := NewClient("broken", tr2, nil).Connect(context.Background()); err == nil {
		t.Fatal("声明了 tools 却 tools/list 失败时必须报错")
	}
}

// TestParseCapabilities 省略 capabilities 与声明为空是两回事。
func TestParseCapabilities(t *testing.T) {
	if got := parseCapabilities(nil); got.Declared || got.Tools {
		t.Fatalf("缺失 capabilities 应视为未声明：%+v", got)
	}
	if got := parseCapabilities(map[string]any{}); !got.Declared || got.Tools {
		t.Fatalf("空 capabilities 应已声明且无 tools：%+v", got)
	}
	got := parseCapabilities(map[string]any{"resources": map[string]any{}})
	if !got.Declared || !got.Resources || got.Tools {
		t.Fatalf("resources-only 解析错误：%+v", got)
	}
	_ = json.RawMessage(nil)
}
