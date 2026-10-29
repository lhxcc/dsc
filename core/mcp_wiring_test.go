package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 客户端协议与 stdio 传输已经搬进 dsc-sdk/mcp（那里有自己的 helper 进程测试，一条
// 外部命令都不依赖）。这一组只钉住宿主这一薄层：**命名、注册、注销**。
//
// 为什么不在这里再起一个 stdio 服务器：注册逻辑与传输无关，用 HTTP 假服务器就能覆盖；
// 而 stdio 的进程管道、2 MiB 结果、-32601 回覆、env 白名单都已在 SDK 里测过。两处
// 各测各的职责，比在一处重复造第二个 helper 服务器更省。

// TestMCPRegistersToolsWithManager 连接后工具必须真的进 ToolRegistry
// （mcp__<server>__<tool>），断开时被注销。
//
// 漏掉这一步的表现是「连接成功、工具数也对，但模型一个也看不到」。
func TestMCPRegistersToolsWithManager(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	mustConnectFake(t, m, "helper", "alpha", "beta")

	for _, want := range []string{"mcp__helper__alpha", "mcp__helper__beta"} {
		if _, ok := m.toolRegistry.Get(want); !ok {
			t.Fatalf("工具 %q 未注册（现有 %v）", want, m.toolRegistry.List())
		}
	}

	if _, err := m.DisconnectMCPServer("helper"); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	for _, gone := range []string{"mcp__helper__alpha", "mcp__helper__beta"} {
		if _, ok := m.toolRegistry.Get(gone); ok {
			t.Fatalf("断开后 %q 仍留在注册表", gone)
		}
	}
}

// TestMCPThroughConnectTool 走模型真实会走的那条路：mcp_connect 入参 → 连接 → 注册 →
// mcp_list 看得见 → mcp_disconnect 拔掉。
//
// 工具层解析错一个字段（比如把 args 当字符串），就只会在「模型调不通」时暴露。
func TestMCPThroughConnectTool(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	m.SetMCPToolsEnabled(true)
	srv := fakeMCPServer(t, "alpha", "beta")
	argsJSON, _ := json.Marshal(map[string]any{"server_name": "demo", "endpoint": srv.URL})

	ctx := context.Background()
	out, err := (&mcpConnectTool{m: m}).Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("mcp_connect: %v", err)
	}
	if !strings.Contains(out, "2 tools") {
		t.Fatalf("回执不符：%s", out)
	}
	if _, ok := m.toolRegistry.Get("mcp__demo__alpha"); !ok {
		t.Fatalf("工具未注册：%v", m.toolRegistry.List())
	}

	list, err := (&mcpListTool{m: m}).Execute(ctx, json.RawMessage(`{"server":"demo"}`))
	if err != nil {
		t.Fatalf("mcp_list: %v", err)
	}
	if !strings.Contains(list, "mcp__demo__alpha") {
		t.Fatalf("mcp_list 看不到工具：%s", list)
	}

	if _, err := (&mcpDisconnectTool{m: m}).Execute(ctx, json.RawMessage(`{"server_name":"demo"}`)); err != nil {
		t.Fatalf("mcp_disconnect: %v", err)
	}
	if _, ok := m.toolRegistry.Get("mcp__demo__alpha"); ok {
		t.Fatal("断开后工具仍在注册表")
	}
}
