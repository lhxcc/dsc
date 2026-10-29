package core

import (
	"testing"
)

// TestMCPSpecFromMap 配置与工具共用同一份解析：两种传输的字段必须都能认出来。
func TestMCPSpecFromMap(t *testing.T) {
	httpSpec, err := MCPSpecFromMap(map[string]any{
		"server_name": "a", "endpoint": "http://127.0.0.1:9/mcp",
	})
	if err != nil {
		t.Fatalf("http spec: %v", err)
	}
	if kind, err := httpSpec.TransportKind(); err != nil || kind != "http" {
		t.Fatalf("kind = %q err = %v, want http", kind, err)
	}

	stdioSpec, err := MCPSpecFromMap(map[string]any{
		"server_name": "b", "transport": "stdio",
		"command": "node", "args": []any{"s.mjs", "mcp"},
		"env": map[string]any{"TOKEN": "t"}, "cwd": "/tmp",
	})
	if err != nil {
		t.Fatalf("stdio spec: %v", err)
	}
	if kind, err := stdioSpec.TransportKind(); err != nil || kind != "stdio" {
		t.Fatalf("kind = %q err = %v, want stdio", kind, err)
	}
	if len(stdioSpec.Args) != 2 || stdioSpec.Args[1] != "mcp" || stdioSpec.Env["TOKEN"] != "t" {
		t.Fatalf("stdio 字段解析不符: %+v", stdioSpec)
	}

	// 字段类型错误要说清楚，而不是静默丢掉
	if _, err := MCPSpecFromMap(map[string]any{"server_name": "c", "args": "not-an-array"}); err == nil {
		t.Fatal("args 非数组应报错")
	}
	// 什么都不给：推断不出传输类型（解析本身不报错，归一化时报）
	empty, err := MCPSpecFromMap(map[string]any{"server_name": "d"})
	if err != nil {
		t.Fatalf("解析不应因缺字段报错: %v", err)
	}
	if _, kerr := empty.TransportKind(); kerr == nil {
		t.Fatal("既无 endpoint 也无 command 时 TransportKind 应报错")
	}
	// 显式写了 stdio 却没 command：同样是错
	if _, kerr := (MCPConnectSpec{ServerName: "e", Transport: "stdio"}).TransportKind(); kerr == nil {
		t.Fatal("stdio 缺 command 应报错")
	}
}
