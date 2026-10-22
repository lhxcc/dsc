package core

import (
	"testing"

	"dsc/proto"
)

// assistantToolCalls 构造带 tool_calls 的 assistant 消息。
func assistantToolCalls(content string, ids ...string) *proto.Message {
	m := &proto.Message{Role: "assistant", Content: content}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, &proto.ToolCall{Id: id, Name: "tool-" + id, ArgumentsJson: "{}"})
	}
	return m
}

// toolMsg 构造 tool 结果消息。
func toolMsg(id, content string) *proto.Message {
	return &proto.Message{Role: "tool", Content: content, ToolCallId: id}
}

func TestSnapRetainStart(t *testing.T) {
	// 0 system / 1 user / 2 assistant(tool a,b,c) / 3 tool a / 4 notice(user) / 5 tool b / 6 tool c / 7 user
	build := func() []*proto.Message {
		return []*proto.Message{
			{Role: "system", Content: "s"},
			{Role: "user", Content: "u1"},
			assistantToolCalls("", "a", "b", "c"),
			toolMsg("a", "ra"),
			{Role: "user", Content: "notice"},
			toolMsg("b", "rb"),
			toolMsg("c", "rc"),
			{Role: "user", Content: "u2"},
		}
	}
	tests := []struct {
		name      string
		retainIdx int
		base      int
		want      int
		wantOK    bool
	}{
		{name: "非工具切点原样返回", retainIdx: 7, base: 1, want: 7, wantOK: true},
		{name: "切在首个工具结果上", retainIdx: 3, base: 1, want: 2, wantOK: true},
		{name: "切在连续工具中间", retainIdx: 6, base: 1, want: 2, wantOK: true},
		{name: "notice 之后切在工具上", retainIdx: 5, base: 1, want: 2, wantOK: true},
		{name: "吸附结果等于 base 视为无安全切点", retainIdx: 3, base: 2, want: 0, wantOK: false},
		{name: "retainIdx 超出长度", retainIdx: 99, base: 1, want: 0, wantOK: false},
		{name: "retainIdx 不大于 base", retainIdx: 1, base: 1, want: 0, wantOK: false},
		{name: "尾部为空等价全压缩", retainIdx: 8, base: 1, want: 8, wantOK: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := SnapRetainStart(build(), tt.retainIdx, tt.base)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Fatalf("SnapRetainStart() = (%d, %v), want (%d, %v)", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestSnapRetainStartInvalidHistory(t *testing.T) {
	cases := map[string][]*proto.Message{
		"工具消息缺 id": {
			{Role: "user", Content: "u"},
			assistantToolCalls("", "a"),
			{Role: "tool", Content: "r"},
		},
		"最近 assistant 不拥有该调用": {
			{Role: "user", Content: "u"},
			assistantToolCalls("", "a"),
			toolMsg("b", "rb"), // 归属错误
		},
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			if idx, ok := SnapRetainStart(msgs, len(msgs)-1, 1); ok {
				t.Fatalf("SnapRetainStart() = (%d, true), want (_, false)", idx)
			}
		})
	}
}

func TestValidateToolPairing(t *testing.T) {
	valid := []*proto.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
		assistantToolCalls("", "a", "b"),
		toolMsg("a", "ra"),
		{Role: "user", Content: "notice"}, // advisory 上下文允许夹在工具块内
		toolMsg("b", "rb"),
	}
	if err := ValidateToolPairing(valid); err != nil {
		t.Fatalf("valid history reported error: %v", err)
	}

	cases := map[string][]*proto.Message{
		"tool 消息缺 id": {
			{Role: "user", Content: "u"},
			assistantToolCalls("", "a"),
			{Role: "tool", Content: "r"},
		},
		"孤立的 tool 消息": {
			{Role: "user", Content: "u"},
			toolMsg("a", "ra"),
		},
		"assistant tool_call 无结果": {
			{Role: "user", Content: "u"},
			assistantToolCalls("", "a", "b"),
			toolMsg("a", "ra"),
		},
		"assistant tool_call 缺 id": {
			{Role: "user", Content: "u"},
			{Role: "assistant", ToolCalls: []*proto.ToolCall{{Name: "shell"}}},
			{Role: "tool", Content: "r"},
		},
		"重复声明已回答的 id": {
			{Role: "user", Content: "u"},
			assistantToolCalls("", "a"),
			toolMsg("a", "ra"),
			assistantToolCalls("", "a"),
			toolMsg("a", "ra2"),
		},
	}
	for name, msgs := range cases {
		t.Run(name, func(t *testing.T) {
			if err := ValidateToolPairing(msgs); err == nil {
				t.Fatal("expected validation error, got nil")
			}
		})
	}
}

func TestRepairToolPairing(t *testing.T) {
	orphan := toolMsg("missing", "orphan")
	answered := assistantToolCalls("", "a", "b")
	emptyAssistant := assistantToolCalls("", "b") // 只有无结果的调用，修复后应整体丢弃
	msgs := []*proto.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
		answered,
		toolMsg("a", "ra"),
		emptyAssistant,
		orphan,
	}

	fixed, n := RepairToolPairing(msgs)
	if n == 0 {
		t.Fatal("expected repairs, got 0")
	}
	if err := ValidateToolPairing(fixed); err != nil {
		t.Fatalf("repaired history still invalid: %v", err)
	}
	// 原有消息不得被就地修改。
	if len(answered.GetToolCalls()) != 2 {
		t.Fatalf("input mutated: assistant tool_calls = %d", len(answered.GetToolCalls()))
	}
	// 合法 tool 结果保留，孤儿被丢弃。
	var toolIDs []string
	for _, m := range fixed {
		if m.GetRole() == "tool" {
			toolIDs = append(toolIDs, m.GetToolCallId())
		}
	}
	if len(toolIDs) != 1 || toolIDs[0] != "a" {
		t.Fatalf("tool messages after repair = %v, want [a]", toolIDs)
	}

	// 幂等：再次调用不产生任何修复。
	again, n := RepairToolPairing(fixed)
	if n != 0 {
		t.Fatalf("second repair = %d, want 0", n)
	}
	if len(again) != len(fixed) {
		t.Fatalf("second repair changed length: %d -> %d", len(fixed), len(again))
	}
}

func TestRepairToolPairingNoopOnValidHistory(t *testing.T) {
	msgs := []*proto.Message{
		{Role: "system", Content: "s"},
		{Role: "user", Content: "u"},
		assistantToolCalls("thinking", "a"),
		toolMsg("a", "ra"),
		{Role: "assistant", Content: "done"},
	}
	fixed, n := RepairToolPairing(msgs)
	if n != 0 {
		t.Fatalf("repair count = %d, want 0", n)
	}
	if len(fixed) != len(msgs) {
		t.Fatalf("valid history changed length: %d -> %d", len(msgs), len(fixed))
	}
}
