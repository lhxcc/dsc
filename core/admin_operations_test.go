package core

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"dsc/libs/vodka"
)

// newOperationAdmin 起一个只挂通用操作端点的管理面（真实路由树 + 真实 HTTP）。
//
// 用真实 router 而不是直接调 handler：路径参数（{group}/{name}）与鉴权中间件都在
// 路由层，跳过它们测出来的「通过」不算数。
func newOperationAdmin(t *testing.T, m *Manager) *httptest.Server {
	t.Helper()
	e := vodka.New()
	g := e.Group("/operations")
	g.Use(adminAuth)
	m.registerOperationAdminRoutes(g)
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)
	return srv
}

func doOperation(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = bytes.NewBufferString(body)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token := os.Getenv("DSC_ADMIN_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// TestOperationsListExposesCatalog 目录端点给出「有哪些管理操作、各自要什么参数、
// 现在对模型可见吗」——呈现层据此渲染，不需要认识任何子系统名字。
func TestOperationsListExposesCatalog(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	srv := newOperationAdmin(t, m)

	code, out := doOperation(t, srv, http.MethodGet, "/operations/list", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%v", code, out)
	}
	ops, _ := out["operations"].([]any)
	if len(ops) != 5 {
		t.Fatalf("管理操作数 = %d, want 5：%v", len(ops), out)
	}
	var sawConnect bool
	for _, raw := range ops {
		op, _ := raw.(map[string]any)
		if op["group"] != "mcp" {
			t.Fatalf("分组不符：%v", op)
		}
		if op["name"] == "connect" {
			sawConnect = true
			if op["tool"] != mcpConnectToolName {
				t.Fatalf("connect 应指向 %s，得到 %v", mcpConnectToolName, op["tool"])
			}
			if op["schema"] == nil {
				t.Fatal("应带参数 schema（面板要据此生成表单）")
			}
			// 默认关闭：对模型不可见，但仍要出现在目录里（否则没法显示「怎么打开」）
			if visible, _ := op["model_visible"].(bool); visible {
				t.Fatal("MCP 默认关闭时不应标记 model_visible")
			}
		}
	}
	if !sawConnect {
		t.Fatal("目录里没有 connect 操作")
	}
}

// TestOperationsExecuteRunsManagementOp 执行端点走的是同一批管理操作：
// connect → list → disconnect 全链路过这里，而不是专属的 /mcp/* 路由。
func TestOperationsExecuteRunsManagementOp(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	m.SetMCPToolsEnabled(true)
	srv := newOperationAdmin(t, m)
	fake := fakeMCPServer(t, "alpha", "beta")

	body, _ := json.Marshal(map[string]any{"server_name": "demo", "endpoint": fake.URL})
	code, out := doOperation(t, srv, http.MethodPost, "/operations/mcp/connect", string(body))
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("connect: status=%d body=%v", code, out)
	}
	if result, _ := out["result"].(string); !strings.Contains(result, "2 tools") {
		t.Fatalf("connect 回执不符：%v", out["result"])
	}

	code, out = doOperation(t, srv, http.MethodPost, "/operations/mcp/list", `{"server":"demo"}`)
	if code != http.StatusOK {
		t.Fatalf("list: status=%d body=%v", code, out)
	}
	if result, _ := out["result"].(string); !strings.Contains(result, "mcp__demo__alpha") {
		t.Fatalf("list 看不到工具：%v", out["result"])
	}

	code, out = doOperation(t, srv, http.MethodPost, "/operations/mcp/disconnect", `{"server_name":"demo"}`)
	if code != http.StatusOK {
		t.Fatalf("disconnect: status=%d body=%v", code, out)
	}
	if _, ok := m.toolRegistry.Get("mcp__demo__alpha"); ok {
		t.Fatal("断开后工具仍在注册表")
	}
}

// TestOperationsRejectsUnknownAndMalformed 未知操作 404、坏 body 400——调用方要能
// 区分「这里没有这条操作」与「参数不对」。
func TestOperationsRejectsUnknownAndMalformed(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	srv := newOperationAdmin(t, m)

	if code, _ := doOperation(t, srv, http.MethodPost, "/operations/mcp/nope", `{}`); code != http.StatusNotFound {
		t.Fatalf("未知操作应 404，得到 %d", code)
	}
	if code, _ := doOperation(t, srv, http.MethodPost, "/operations/nogroup/x", `{}`); code != http.StatusNotFound {
		t.Fatalf("未知分组应 404，得到 %d", code)
	}
	if code, _ := doOperation(t, srv, http.MethodPost, "/operations/mcp/list", `not-json`); code != http.StatusBadRequest {
		t.Fatalf("坏 body 应 400，得到 %d", code)
	}
	// 无 body 等价于空对象：list 是合法调用（只是没有服务器）
	if code, _ := doOperation(t, srv, http.MethodPost, "/operations/mcp/list", ""); code != http.StatusOK {
		t.Fatalf("空 body 应视为 {} 并执行，得到 %d", code)
	}
}

// TestOperationsRequireAdminToken 鉴权与其它 admin 路由一致：设了令牌就必须带上。
func TestOperationsRequireAdminToken(t *testing.T) {
	t.Setenv("DSC_ADMIN_TOKEN", "s3cret")

	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	srv := newOperationAdmin(t, m)

	// 不带令牌
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/operations/list", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺令牌应 401，得到 %d", resp.StatusCode)
	}

	// 带令牌
	if code, _ := doOperation(t, srv, http.MethodGet, "/operations/list", ""); code != http.StatusOK {
		t.Fatalf("带令牌应 200，得到 %d", code)
	}
}
