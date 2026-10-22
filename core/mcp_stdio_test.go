package core

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestMain 兼任 MCP stdio 测试服务器：子进程以环境变量识别后进入 helper 模式，
// 其余情况正常跑测试。
func TestMain(m *testing.M) {
	if os.Getenv("DSC_MCP_STDIO_HELPER") == "1" {
		runMCPStdioHelper()
		return
	}
	if os.Getenv("DSC_LSP_HELPER") == "1" {
		runLSPHelper()
		return
	}
	os.Exit(m.Run())
}

// runMCPStdioHelper 最小 MCP stdio 服务器：逐行读 JSON-RPC，支持 initialize、
// tools/list（一个 echo 工具）与 tools/call。
func runMCPStdioHelper() {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			continue
		}
		id, hasID := req["id"]
		if !hasID {
			continue // 通知（notifications/initialized）无需响应
		}
		method, _ := req["method"].(string)
		resp := map[string]any{"jsonrpc": "2.0", "id": id}
		switch method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"serverInfo":      map[string]any{"name": "helper", "version": "1.0"},
			}
		case "tools/list":
			resp["result"] = map[string]any{"tools": []any{map[string]any{
				"name":        "echo",
				"description": "echo text",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			params, _ := req["params"].(map[string]any)
			name, _ := params["name"].(string)
			if name != "echo" {
				resp["error"] = map[string]any{"code": -32601, "message": "unknown tool"}
				break
			}
			resp["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "stdio-ok"}}}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		_ = enc.Encode(resp)
	}
}

// TestMCPStdioConnectDiscoverAndCall 回归 stdio 传输：拉起子进程 → initialize →
// tools/list 注册到 ToolRegistry → tools/call → Disconnect 注销工具并回收进程。
func TestMCPStdioConnectDiscoverAndCall(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	client, err := NewMCPStdioClient(m, "stdio", os.Args[0], nil,
		[]string{"DSC_MCP_STDIO_HELPER=1"}, "")
	if err != nil {
		t.Fatalf("NewMCPStdioClient: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := client.ToolCount(); got != 1 {
		t.Fatalf("ToolCount = %d, want 1", got)
	}

	tool, ok := m.toolRegistry.Get("mcp__stdio__echo")
	if !ok {
		t.Fatal("stdio MCP tool not registered")
	}
	out, err := tool.Execute(ctx, json.RawMessage(`{"text":"hi"}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if out != "stdio-ok" {
		t.Fatalf("output = %q, want stdio-ok", out)
	}

	client.Disconnect()
	if _, ok := m.toolRegistry.Get("mcp__stdio__echo"); ok {
		t.Fatal("tool should be unregistered after disconnect")
	}
}

// TestMCPStdioMissingCommand 校验参数缺失与启动失败都返回明确错误（不遗留进程）。
func TestMCPStdioMissingCommand(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	if _, err := NewMCPStdioClient(m, "stdio", "", nil, nil, ""); err == nil {
		t.Fatal("empty command should fail")
	}
	client, err := NewMCPStdioClient(m, "stdio", "/nonexistent/dsc-mcp-server", nil, nil, "")
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err == nil {
		client.Disconnect()
		t.Fatal("connect with nonexistent command should fail")
	}
}
