package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dsc/proto/metadata"
)

// TestAdminMetadataExposesCapabilities 回归 /plugins/metadata 的观测面：能力边界
// （Provides 普通键 + requires/<type>/<cap> 编码）与服务声明必须可经管理 API 读到，
// 否则排障只能反查插件二进制。
func TestAdminMetadataExposesCapabilities(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	m.mu.Lock()
	m.coreMetadata["tool-x"] = &metadata.PluginInfo{
		Type:         "tool",
		Name:         "tool-x",
		Version:      "1.2.3",
		ApiVersion:   "1.0",
		Services:     []string{"tool", "hook"},
		Capabilities: map[string]string{"filesystem": "true", "requires/llm/llm": "true"},
	}
	m.mu.Unlock()

	e := m.adminEngine()
	req := httptest.NewRequest(http.MethodPost, "/plugins/metadata", strings.NewReader(`{"name":"tool-x"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Metadata struct {
			Capabilities map[string]string `json:"capabilities"`
			Services     []string          `json:"services"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if resp.Metadata.Capabilities["filesystem"] != "true" || resp.Metadata.Capabilities["requires/llm/llm"] != "true" {
		t.Fatalf("capabilities = %v", resp.Metadata.Capabilities)
	}
	if len(resp.Metadata.Services) != 2 {
		t.Fatalf("services = %v", resp.Metadata.Services)
	}
}

// TestAdminRoutesNoDuplicatedPrefix 回归管理 API 路由前缀：Group 已带前缀，
// 子路由不得再次拼接同名前缀（否则 /credentials/credentials/list 之类全部 404）。
func TestAdminRoutesNoDuplicatedPrefix(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	e := m.adminEngine()

	got := make(map[string]bool, len(e.Routes()))
	for _, r := range e.Routes() {
		got[r.Method()+" "+r.Path()] = true
	}

	want := []string{
		"GET /credentials/list", "GET /credentials/get",
		"POST /credentials/set", "POST /credentials/delete",
		"POST /feedback/add", "GET /feedback/list", "GET /feedback/stats",
		"POST /lsp/start", "POST /lsp/stop", "GET /lsp/diagnostics",
		"GET /operations/list", "POST /operations/<group>/<name>",
	}
	for _, route := range want {
		if !got[route] {
			t.Errorf("missing admin route %s", route)
		}
	}
	for route := range got {
		for _, dup := range []string{"/credentials/credentials/", "/feedback/feedback/", "/lsp/lsp/", "/operations/operations/"} {
			if strings.Contains(route, dup) {
				t.Errorf("duplicated prefix in admin route %s", route)
			}
		}
	}
}

// TestLSPToolWithoutManager 验证 LSPTool 在 nil manager 时安全返回。
func TestLSPToolWithoutManager(t *testing.T) {
	client := NewLSPClient("go", "/tmp", "test")
	if client == nil {
		t.Fatal("NewLSPClient returned nil")
	}
	tool := NewLSPTool(nil)
	out, err := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("LSPTool.Execute nil manager: %v", err)
	}
	if !strings.Contains(out, "No diagnostics available") {
		t.Fatalf("unexpected output: %q", out)
	}
}
