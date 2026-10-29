package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	// tools 按**公开名**索引（注册表键一致）：模型看到的名字必须带服务器前缀，
	// 否则两台服务器各有一个 search 时会互相覆盖。服务器原名在 rawName 里。
	entry, ok := client.tools["mcp__mockserver__search"]
	if !ok {
		t.Fatalf("tool 'mcp__mockserver__search' not found（现有 %v）", client.tools)
	}
	if entry.rawName != "search" {
		t.Errorf("rawName = %q, want search", entry.rawName)
	}
	if entry.publicName != "mcp__mockserver__search" {
		t.Errorf("publicName = %q", entry.publicName)
	}
	result, err := client.CallTool(context.Background(), "search", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if !strings.Contains(result, "result") {
		t.Errorf("CallTool result = %q", result)
	}
}
