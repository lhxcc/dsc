package core

import "testing"

// TestToolCallReadOnly 并行准入的唯一依据：工具自声明的能力标签。
// 未声明能力一律不可并行（fail-closed）；writes-* 优先于 read-only（保守）。
func TestToolCallReadOnly(t *testing.T) {
	tests := []struct {
		name string
		caps []string
		args string
		want bool
	}{
		{name: "显式只读", caps: []string{CapabilityReadOnly}, args: `{}`, want: true},
		{name: "read-only-when 命中", caps: []string{"writes-path:path", "read-only-when:command=view"}, args: `{"command":"view","path":"/a"}`, want: true},
		{name: "read-only-when 未命中", caps: []string{"writes-path:path", "read-only-when:command=view"}, args: `{"command":"create","path":"/a"}`, want: false},
		{name: "writes-path 命中即写", caps: []string{"writes-path:path"}, args: `{"path":"/a"}`, want: false},
		{name: "writes-path 参数缺失按写", caps: []string{"writes-path:path"}, args: `{}`, want: false},
		{name: "writes-unknown 不可并行", caps: []string{CapabilityWritesUnknown}, args: `{"command":"ls"}`, want: false},
		{name: "read-only 与 writes-unknown 并存按写", caps: []string{CapabilityReadOnly, CapabilityWritesUnknown}, args: `{}`, want: false},
		{name: "未声明能力 fail-closed", caps: nil, args: `{}`, want: false},
		{name: "未知 token 不算声明", caps: []string{"requires-never-approval"}, args: `{}`, want: false},
		{name: "参数非法且只有 read-only", caps: []string{CapabilityReadOnly}, args: `not-json`, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ToolCallReadOnly(tt.caps, tt.args); got != tt.want {
				t.Fatalf("ToolCallReadOnly(%v, %q) = %v, want %v", tt.caps, tt.args, got, tt.want)
			}
		})
	}
}

// TestAllToolsProtoPropagatesCapabilities 字段保真：工具能力必须随工具目录下发到
// agent——agent 分属独立进程，只能据此判定只读与并行资格，丢失即静默退化为全串行。
func TestAllToolsProtoPropagatesCapabilities(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	_ = m.toolRegistry.Register(&RemoteTool{name: "reader", capabilities: []string{CapabilityReadOnly}})
	_ = m.toolRegistry.Register(&RemoteTool{name: "writer", capabilities: []string{"writes-path:path"}})

	byName := map[string][]string{}
	for _, tool := range m.AllToolsProto() {
		byName[tool.Name] = tool.Capabilities
	}
	if len(byName["reader"]) != 1 || byName["reader"][0] != CapabilityReadOnly {
		t.Fatalf("reader capabilities = %v, want [%s]", byName["reader"], CapabilityReadOnly)
	}
	if len(byName["writer"]) != 1 || byName["writer"][0] != "writes-path:path" {
		t.Fatalf("writer capabilities = %v, want [writes-path:path]", byName["writer"])
	}
}
