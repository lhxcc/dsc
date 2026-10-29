package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// fakeNotifyingMCP 一台会在运行期改工具表并推 notifications/tools/list_changed 的服务器。
//
// POST 走请求/响应，GET 是通知流（streamable HTTP 的标准做法）。工具 toggle 被调用时
// 把 beta 换成 gamma，然后推一条 list_changed——插件必须自己看到这次变化（不用重连），
// 并清掉已消失工具的加载记录。
type fakeNotifyingMCP struct {
	mu    sync.Mutex
	tools []string
	notif chan string
}

func (f *fakeNotifyingMCP) handler(t *testing.T) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("writer is not a flusher")
				return
			}
			flusher.Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case method := <-f.notif:
					fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":%q}\n\n", method)
					flusher.Flush()
				}
			}
		}

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

		f.mu.Lock()
		defer f.mu.Unlock()
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		switch req.Method {
		case "initialize":
			resp["result"] = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			}
		case "tools/list":
			items := make([]any, 0, len(f.tools))
			for _, name := range f.tools {
				items = append(items, map[string]any{
					"name": name, "description": name, "inputSchema": map[string]any{"type": "object"},
				})
			}
			resp["result"] = map[string]any{"tools": items}
		case "tools/call":
			if name, _ := params["name"].(string); name == "toggle" {
				f.tools = []string{"alpha", "gamma"} // beta 消失，gamma 出现
				select {
				case f.notif <- "notifications/tools/list_changed":
				default:
				}
			}
			resp["result"] = map[string]any{"content": []any{map[string]any{"type": "text", "text": "toggled"}}}
		default:
			resp["error"] = map[string]any{"code": -32601, "message": "not found"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

// TestCatalogNotificationUpdatesTools 服务器运行期换工具：不重连也要看到新的、丢掉旧的，
// 且「已加载」里不能再留着已经不存在的工具。
func TestCatalogNotificationUpdatesTools(t *testing.T) {
	fake := &fakeNotifyingMCP{tools: []string{"alpha", "beta", "toggle"}, notif: make(chan string, 4)}
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()

	a := newTestApp(t)
	a.setEnabled(true)
	if _, err := a.execConnect(map[string]any{"server_name": "live", "endpoint": srv.URL + "/mcp"}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	// 必须先断开再让 httptest.Server 关：通知流是长连接，Server.Close 会等它结束。
	defer a.execDisconnect(map[string]any{})
	if _, err := a.execLoad(map[string]any{"tools": []any{"mcp__live__beta"}}); err != nil {
		t.Fatalf("load: %v", err)
	}

	// 触发服务器换工具
	if _, err := a.execList(map[string]any{}); err != nil {
		t.Fatalf("list: %v", err)
	}
	a.mu.Lock()
	conn := a.clients["live"]
	a.mu.Unlock()
	if _, err := conn.client.CallTool(context.Background(), "toggle", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("call toggle: %v", err)
	}

	// 通知到达 → 客户端刷新目录 → 插件清掉失效加载记录
	deadline := time.Now().Add(5 * time.Second)
	for {
		names := map[string]bool{}
		for _, tool := range a.currentTools() {
			names[tool.Name] = true
		}
		if names["mcp__live__gamma"] && !names["mcp__live__beta"] {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("目录未随通知更新：现有 %v", names)
		}
		time.Sleep(20 * time.Millisecond)
	}

	a.mu.Lock()
	_, betaStillLoaded := a.loaded["mcp__live__beta"]
	a.mu.Unlock()
	if betaStillLoaded {
		t.Fatal("beta 已从服务器消失，加载记录应被清掉")
	}
}
