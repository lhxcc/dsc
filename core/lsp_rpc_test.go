package core

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain 兼任 LSP 测试服务器：同一个测试二进制带 DSC_LSP_HELPER=1 启动时进入
// helper 模式（省掉额外编译产物，测的也正是真实子进程管道），其余情况正常跑测试。
func TestMain(m *testing.M) {
	if os.Getenv("DSC_LSP_HELPER") == "1" {
		runLSPHelper()
		return
	}
	os.Exit(m.Run())
}

// runLSPHelper 最小 LSP 服务器（stdio，Content-Length 分帧）：
//   - initialize → 返回 caps
//   - initialized/didOpen/didChange → 推送一条 publishDiagnostics
//   - shutdown → null；exit → 退出
func runLSPHelper() {
	in := bufio.NewReader(os.Stdin)
	for {
		raw, err := lspReadMessage(in)
		if err != nil {
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}
		switch msg.Method {
		case "initialize":
			lspHelperWrite(map[string]any{
				"jsonrpc": "2.0", "id": msg.ID,
				"result": map[string]any{
					"capabilities": map[string]any{"textDocumentSync": 1},
					"serverInfo":   map[string]any{"name": "lsp-helper", "version": "1.0"},
				},
			})
		case "shutdown":
			lspHelperWrite(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": nil})
		case "exit":
			return
		case "textDocument/didOpen", "textDocument/didChange":
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			_ = json.Unmarshal(msg.Params, &p)
			lspHelperWrite(map[string]any{
				"jsonrpc": "2.0",
				"method":  "textDocument/publishDiagnostics",
				"params": map[string]any{
					"uri": p.TextDocument.URI,
					"diagnostics": []any{map[string]any{
						"range": map[string]any{
							"start": map[string]any{"line": 0, "character": 0},
							"end":   map[string]any{"line": 0, "character": 1},
						},
						"severity": 2,
						"source":   "lsp-helper",
						"message":  "helper diagnostic",
					}},
				},
			})
		}
	}
}

// lspHelperWrite 以 LSP 帧格式写一条消息到 stdout。
func lspHelperWrite(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	fmt.Printf("Content-Length: %d\r\n\r\n%s", len(b), b)
}

// TestLSPInitializeAndDiagnostics 回归完整握手：启动子进程 → initialize/initialized →
// GetDiagnostics 触发 didOpen 并收到 publishDiagnostics → Stop 优雅退出。
func TestLSPInitializeAndDiagnostics(t *testing.T) {
	dir := t.TempDir()
	file := PJoin(dir, "main.go")
	if err := os.WriteFile(file, []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DSC_LSP_HELPER", "1")
	c := NewLSPClient("go", dir, "fake")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := c.Start(ctx, os.Args[0]); err != nil {
		t.Fatalf("Start: %v", err)
	}

	diags, err := c.GetDiagnostics(ctx, file)
	if err != nil {
		t.Fatalf("GetDiagnostics: %v", err)
	}
	if len(diags) != 1 || diags[0].Message != "helper diagnostic" || diags[0].Severity != LSPSeverityWarning {
		t.Fatalf("diagnostics = %+v", diags)
	}

	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestLSPDiagnosticsWithoutServer 校验未启动时仍读取外部 SetDiagnostics 的缓存。
func TestLSPDiagnosticsWithoutServer(t *testing.T) {
	c := NewLSPClient("go", t.TempDir(), "manual")
	c.SetDiagnostics("/tmp/x.go", []LSPDiagnostic{{Message: "manual", Severity: LSPSeverityError}})
	diags, err := c.GetDiagnostics(context.Background(), "/tmp/x.go")
	if err != nil || len(diags) != 1 || diags[0].Message != "manual" {
		t.Fatalf("diags = %+v, err = %v", diags, err)
	}
}
