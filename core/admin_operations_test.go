package core

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"dsc/libs/vodka"
)

// 通用管理操作端点：任何声明了管理面的插件都经这里暴露，宿主不认识具体名字。
//
// 用真实路由树 + 真实 HTTP（而不是直接调 handler）：路径参数与鉴权中间件都在路由层，
// 跳过它们测出来的「通过」不算数。

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
	registerDemoOps(t, m)
	srv := newOperationAdmin(t, m)

	code, out := doOperation(t, srv, http.MethodGet, "/operations/list", "")
	if code != http.StatusOK {
		t.Fatalf("status = %d, body=%v", code, out)
	}
	ops, _ := out["operations"].([]any)
	if len(ops) != 2 {
		t.Fatalf("管理操作数 = %d, want 2：%v", len(ops), out)
	}
	byName := map[string]map[string]any{}
	for _, raw := range ops {
		op, _ := raw.(map[string]any)
		name, _ := op["name"].(string)
		byName[name] = op
	}
	alpha, ok := byName["alpha"]
	if !ok {
		t.Fatalf("目录里没有 alpha 操作：%v", byName)
	}
	if alpha["group"] != "demo" || alpha["tool"] != "demo_alpha" {
		t.Fatalf("分组/工具名不符：%v", alpha)
	}
	if alpha["schema"] == nil {
		t.Fatal("应带参数 schema（面板要据此生成表单）")
	}
	if visible, _ := alpha["model_visible"].(bool); !visible {
		t.Fatal("可见操作应标记 model_visible")
	}
	// 只给人用的操作也要在目录里，但标记不可见
	beta, ok := byName["beta"]
	if !ok {
		t.Fatalf("只给人用的操作也应出现在目录里：%v", byName)
	}
	if visible, _ := beta["model_visible"].(bool); visible {
		t.Fatal("隐藏操作不该标记 model_visible")
	}
}

// TestOperationsExecuteRunsManagementOp 执行端点调的就是那条管理操作。
func TestOperationsExecuteRunsManagementOp(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	registerDemoOps(t, m)
	srv := newOperationAdmin(t, m)

	code, out := doOperation(t, srv, http.MethodPost, "/operations/demo/alpha", `{}`)
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("status=%d body=%v", code, out)
	}
	if result, _ := out["result"].(string); result != "ok:demo_alpha" {
		t.Fatalf("执行结果不符：%v", out["result"])
	}
	if op, _ := out["operation"].(string); op != "demo/alpha" {
		t.Fatalf("回执应带操作标识：%v", out["operation"])
	}
}

// TestOperationsRejectsUnknownAndMalformed 未知操作 404、坏 body 400——调用方要能
// 区分「这里没有这条操作」与「参数不对」。
func TestOperationsRejectsUnknownAndMalformed(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	registerDemoOps(t, m)
	srv := newOperationAdmin(t, m)

	if code, _ := doOperation(t, srv, http.MethodPost, "/operations/demo/nope", `{}`); code != http.StatusNotFound {
		t.Fatalf("未知操作应 404，得到 %d", code)
	}
	if code, _ := doOperation(t, srv, http.MethodPost, "/operations/nogroup/x", `{}`); code != http.StatusNotFound {
		t.Fatalf("未知分组应 404，得到 %d", code)
	}
	if code, _ := doOperation(t, srv, http.MethodPost, "/operations/demo/alpha", `not-json`); code != http.StatusBadRequest {
		t.Fatalf("坏 body 应 400，得到 %d", code)
	}
	// 无 body 等价于空对象：无参操作是合法调用
	if code, _ := doOperation(t, srv, http.MethodPost, "/operations/demo/alpha", ""); code != http.StatusOK {
		t.Fatalf("空 body 应视为 {} 并执行，得到 %d", code)
	}
}

// TestOperationsRequireAdminToken 鉴权与其它 admin 路由一致：设了令牌就必须带上。
func TestOperationsRequireAdminToken(t *testing.T) {
	t.Setenv("DSC_ADMIN_TOKEN", "s3cret")

	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	registerDemoOps(t, m)
	srv := newOperationAdmin(t, m)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/operations/list", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("缺令牌应 401，得到 %d", resp.StatusCode)
	}

	if code, _ := doOperation(t, srv, http.MethodGet, "/operations/list", ""); code != http.StatusOK {
		t.Fatalf("带令牌应 200，得到 %d", code)
	}
}
