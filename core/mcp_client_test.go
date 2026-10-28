package core

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMCPPublicName(t *testing.T) {
	cases := []struct {
		serverName, rawName, want string
	}{
		{"myserver", "get_weather", "mcp__myserver__get_weather"},
		{"fs", "read_file", "mcp__fs__read_file"},
		{"db", "query", "mcp__db__query"},
	}
	for _, c := range cases {
		if got := mcpPublicName(c.serverName, c.rawName); got != c.want {
			t.Errorf("mcpPublicName(%q, %q) = %q, want %q", c.serverName, c.rawName, got, c.want)
		}
	}
}

func TestMCPPublicNameNormalization(t *testing.T) {
	// 含非法字符的 rawName 应被规范化
	got := mcpPublicName("srv", "tool.name")
	if got != "mcp__srv__tool_name" {
		t.Errorf("mcpPublicName with dot = %q, want mcp__srv__tool_name", got)
	}
}

func TestMCPPublicNameTruncation(t *testing.T) {
	// 超长名应截断到 64 字符
	longName := "mcp__srv__" + string(make([]byte, 100))
	for i := range longName {
		if longName[i] == 0 {
			longName = longName[:i] + "a" + longName[i+1:]
		}
	}
	longName = "mcp__srv__" + repeatStr("a", 100)
	got := mcpPublicName("srv", repeatStr("a", 100))
	if len(got) > 64 {
		t.Errorf("mcpPublicName should be truncated to 64 chars, got %d", len(got))
	}
}

func TestMCPServerNameValid(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
	}{
		{"myserver", true},
		{"fs-tools", true},
		{"db_v2", true},
		{"", false},
		{"a", true},
		{repeatStr("a", 33), false}, // too long
		{"bad name", false},         // space
		{"bad.name", false},         // dot
	}
	for _, c := range cases {
		if got := isMCPServerNameValid(c.name); got != c.valid {
			t.Errorf("isMCPServerNameValid(%q) = %v, want %v", c.name, got, c.valid)
		}
	}
}

func TestMCPNormalizeName(t *testing.T) {
	cases := []struct {
		input, want string
	}{
		{"hello", "hello"},
		{"hello_world", "hello_world"},
		{"hello.world", "hello_world"},
		{"hello world", "hello_world"},
		{"hello/world", "hello_world"},
	}
	for _, c := range cases {
		if got := mcpNormalizeName(c.input); got != c.want {
			t.Errorf("mcpNormalizeName(%q) = %q, want %q", c.input, got, c.want)
		}
	}
}

func repeatStr(s string, n int) string {
	result := ""
	for i := 0; i < n; i++ {
		result += s
	}
	return result
}

// TestMCPClientKeepsSessionAndUsesSafeID 客户端必须做对 streamable HTTP 的两件事，
// 否则「连不上」：
//
//  1. initialize 响应头 Mcp-Session-Id 必须保存，并在**之后每个请求**上回带。
//     缺了它，服务端（官方 SDK）直接 400 "Bad Request: Mcp-Session-Id header is
//     required"——现场就是这样：initialize 成功、tools/list 立刻 400，一个 MCP 工具
//     都注册不上，模型只好去 shell 里另起一个 dsc。
//  2. JSON-RPC id 必须是小整数。原先用 time.Now().UnixNano()（≈1.79e18），超过
//     IEEE-754 安全整数 2^53，JS 服务端解析后判 "Invalid JSON-RPC message" 回 400；
//     实测同一请求：小整数 200、UnixNano 400、字符串 200。
//
// 这个假服务端照着官方 SDK 的行为校验这两点，所以一旦退回旧实现就会红。
func TestMCPClientKeepsSessionAndUsesSafeID(t *testing.T) {
	const sessionID = "sess-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		// 请求体形态校验：id 必须能解析成不超过 2^53 的整数（模拟 JS 服务端的严格校验）
		if len(req.ID) > 0 && string(req.ID) != "null" {
			raw := strings.Trim(string(req.ID), `"`)
			n, err := strconv.ParseFloat(raw, 64)
			if err != nil || n > 9007199254740992 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32700,"message":"Parse error: Invalid JSON-RPC message"},"id":null}`))
				return
			}
		}

		if req.Method == "initialize" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", sessionID)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"fake","version":"1"}}}`))
			return
		}
		// 其余请求：会话头与协议版本头必须齐
		if r.Header.Get("Mcp-Session-Id") != sessionID {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32000,"message":"Bad Request: Mcp-Session-Id header is required"},"id":null}`))
			return
		}
		if r.Header.Get("Mcp-Protocol-Version") == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32600,"message":"missing MCP-Protocol-Version"},"id":null}`))
			return
		}
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"ping","description":"ping tool","inputSchema":{"type":"object"}}]}}`))
		case "tools/call":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":3,"result":{"content":[{"type":"text","text":"pong"}]}}`))
		default:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer srv.Close()

	c, err := NewMCPClient(nil, "fake", srv.URL)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	tools := c.Tools()
	if len(tools) != 1 || tools[0].Name != "mcp__fake__ping" {
		t.Fatalf("工具发现结果 = %+v", tools)
	}
	out, err := c.CallTool(ctx, "ping", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "pong" {
		t.Fatalf("CallTool 返回 %q, 期望 pong", out)
	}
}
