package core

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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

// TestMCPStdioWithREA 对真实 MCP 服务器（rea-agents）跑一遍 stdio 全链路。
//
// 默认跳过：它依赖仓库外的 O:\Agents\rea-agents 与 node。要跑：
//
//	DSC_REA_MCP_E2E=1 go test ./core/ -run TestMCPStdioWithREA -v
//
// 这个用例的价值在于「真实服务器」——它的工具数是三位数（229），响应体大、有
// stderr 日志、有需要 cwd 的相对路径解析，mock helper 覆盖不到这些。
func TestMCPStdioWithREA(t *testing.T) {
	if os.Getenv("DSC_REA_MCP_E2E") != "1" {
		t.Skip("set DSC_REA_MCP_E2E=1 to run against O:/Agents/rea-agents")
	}
	dir := os.Getenv("DSC_REA_MCP_DIR")
	if dir == "" {
		dir = filepath.Join("O:", string(os.PathSeparator), "Agents", "rea-agents")
	}
	entry := filepath.Join(dir, "scripts", "rea.mjs")
	if _, err := os.Stat(entry); err != nil {
		t.Skipf("rea-agents not found at %s: %v", dir, err)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH: %v", err)
	}

	c, err := NewMCPStdioClient(nil, "rea", MCPStdioSpec{
		Command: node,
		Args:    []string{entry, "mcp"},
		CWD:     dir,
	}, nil)
	if err != nil {
		t.Fatalf("new stdio client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Disconnect()

	tools := c.Tools()
	if len(tools) < 100 {
		t.Fatalf("工具数 = %d，期望三位数（rea 的目录约 229）", len(tools))
	}
	t.Logf("discovered %d tools from rea", len(tools))

	// 至少有一个工具带真实 inputSchema（空 schema 会让模型无法构造参数）
	withSchema := 0
	for _, tool := range tools {
		if len(tool.Schema) > 2 {
			withSchema++
		}
	}
	if withSchema == 0 {
		t.Fatal("所有工具都没有 inputSchema")
	}

	// 调一个不需要前置状态的只读工具：分析设置快照
	out, err := c.CallTool(ctx, "inspect_analysis_settings", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool inspect_analysis_settings: %v", err)
	}
	if !strings.Contains(out, "revision") && !strings.Contains(out, "settings") {
		t.Fatalf("返回内容不像分析设置快照: %.200s", out)
	}
	t.Logf("inspect_analysis_settings -> %d bytes", len(out))
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
