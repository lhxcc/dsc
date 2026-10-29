package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestMCPStdioRegistersToolsWithManager 连接 stdio 服务器后，工具必须真的进
// ToolRegistry（mcp__<server>__<tool>），并且断开时被注销。
//
// 单测 transport 只能证明「能收发」；工具注册走的是 mcpToolAdapter → toolRegistry，
// 漏了这一步的表现是「连接成功、工具数也对，但模型看不见任何一个」。
func TestMCPStdioRegistersToolsWithManager(t *testing.T) {
	mgr := NewManager(&ManagerConfig{})
	c, err := NewMCPStdioClient(mgr, "helper", stdioHelperSpec(t), nil)
	if err != nil {
		t.Fatalf("new stdio client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	for _, want := range []string{"mcp__helper__echo", "mcp__helper__big"} {
		if _, ok := mgr.toolRegistry.Get(want); !ok {
			t.Fatalf("工具 %q 未注册到 ToolRegistry（现有 %v）", want, mgr.toolRegistry.List())
		}
	}

	c.Disconnect()
	for _, gone := range []string{"mcp__helper__echo", "mcp__helper__big"} {
		if _, ok := mgr.toolRegistry.Get(gone); ok {
			t.Fatalf("断开后 %q 仍留在 ToolRegistry", gone)
		}
	}
}

// TestMCPStdioThroughConnectTool 走模型真实会走的那条路：mcp_connect 工具的入参 →
// 连接 → 注册 → mcp_list 看得见 → mcp_disconnect 拔掉。
//
// 单测 transport 与单测 Manager 各管一段，这条把它们接起来——工具层解析错一个字段
// （比如把 args 当字符串）就只会在「模型调不通」时暴露。
func TestMCPStdioThroughConnectTool(t *testing.T) {
	mgr := NewManager(&ManagerConfig{})
	spec := stdioHelperSpec(t)
	argsJSON, _ := json.Marshal(map[string]any{
		"server_name": "helper",
		"transport":   "stdio",
		"command":     spec.Command,
		"args":        spec.Args,
		"env":         spec.Env,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	connect := &mcpConnectTool{m: mgr}
	out, err := connect.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("mcp_connect: %v", err)
	}
	if !strings.Contains(out, "4 tools registered") {
		t.Fatalf("mcp_connect 回执不符: %s", out)
	}
	if _, ok := mgr.toolRegistry.Get("mcp__helper__echo"); !ok {
		t.Fatalf("工具未注册: %v", mgr.toolRegistry.List())
	}

	// 审批文案必须说明「会拉起本机进程」：stdio 与 http 的风险不同
	if reason := connect.ApprovalReason(string(argsJSON)); !strings.Contains(reason, "Spawns") {
		t.Fatalf("stdio 的审批文案应说明会拉起进程，got %q", reason)
	}

	list := &mcpListTool{m: mgr}
	out, err = list.Execute(ctx, json.RawMessage(`{"server":"helper"}`))
	if err != nil {
		t.Fatalf("mcp_list: %v", err)
	}
	if !strings.Contains(out, "mcp__helper__echo") {
		t.Fatalf("mcp_list 看不到工具: %s", out)
	}

	disconnect := &mcpDisconnectTool{m: mgr}
	if _, err := disconnect.Execute(ctx, json.RawMessage(`{"server_name":"helper"}`)); err != nil {
		t.Fatalf("mcp_disconnect: %v", err)
	}
	if _, ok := mgr.toolRegistry.Get("mcp__helper__echo"); ok {
		t.Fatal("断开后工具仍在注册表")
	}
}
