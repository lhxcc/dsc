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
		"POST /mcp/connect", "POST /mcp/disconnect", "GET /mcp/list",
	}
	for _, route := range want {
		if !got[route] {
			t.Errorf("missing admin route %s", route)
		}
	}
	for route := range got {
		for _, dup := range []string{"/credentials/credentials/", "/feedback/feedback/", "/lsp/lsp/", "/mcp/mcp/"} {
			if strings.Contains(route, dup) {
				t.Errorf("duplicated prefix in admin route %s", route)
			}
		}
	}
}

// TestLSPAdminRoutesUnauthenticated 验证 LSPTool 在 nil manager 时安全返回。
func TestLSPAdminRoutesUnauthenticated(t *testing.T) {
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

// TestMCPClientValidation 验证 MCP server name 校验。
func TestMCPClientValidation(t *testing.T) {
	invalid := []string{"", "has space", "has/slash", strings.Repeat("a", 33), "has.dot"}
	for _, name := range invalid {
		if isMCPServerNameValid(name) {
			t.Errorf("server name %q should be invalid", name)
		}
	}
	valid := []string{"go", "python-lsp", "server_1", "MyServer", strings.Repeat("a", 32)}
	for _, name := range valid {
		if !isMCPServerNameValid(name) {
			t.Errorf("server name %q should be valid", name)
		}
	}
}

// TestMCPMockServerConnect 使用模拟 MCP HTTP 服务器验证 Connect + discoverTools + CallTool。
func TestMCPMockServerConnect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		method, _ := req["method"].(string)
		id := req["id"]
		resp := map[string]any{"jsonrpc": "2.0", "id": id}
		switch method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "mock", "version": "1.0"},
			}
		case "tools/list":
			resp["result"] = map[string]any{
				"tools": []map[string]any{
					{"name": "search", "description": "Search docs", "inputSchema": map[string]any{"type": "object"}},
					{"name": "fetch", "description": "Fetch URL", "inputSchema": map[string]any{"type": "object"}},
				},
			}
		case "tools/call":
			resp["result"] = map[string]any{"content": []map[string]any{{"type": "text", "text": "result"}}}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	client, err := NewMCPClient(nil, "mockserver", srv.URL)
	if err != nil {
		t.Fatalf("NewMCPClient: %v", err)
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if len(client.tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(client.tools))
	}
	if _, ok := client.tools["search"]; !ok {
		t.Fatal("tool 'search' not found")
	}
	if client.tools["search"].publicName != "mcp__mockserver__search" {
		t.Errorf("publicName = %q", client.tools["search"].publicName)
	}
	result, err := client.CallTool(context.Background(), "search", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(result, "result") {
		t.Errorf("CallTool result = %q", result)
	}
}
