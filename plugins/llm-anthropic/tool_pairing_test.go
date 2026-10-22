package main

import (
	"testing"

	"dsc/core"
	"github.com/anthropics/anthropic-sdk-go"
)

// msgToolUseIDs / msgResultIDs 测试辅助：取一条消息里的 tool_use / tool_result id。
func msgToolUseIDs(m anthropic.MessageParam) []string { return toolUseIDs(m.Content) }

func msgResultIDs(m anthropic.MessageParam) []string {
	var ids []string
	for _, b := range m.Content {
		if b.OfToolResult != nil {
			ids = append(ids, b.OfToolResult.ToolUseID)
		}
	}
	return ids
}

func resultIsError(m anthropic.MessageParam, id string) bool {
	for _, b := range m.Content {
		if b.OfToolResult != nil && b.OfToolResult.ToolUseID == id {
			return b.OfToolResult.IsError.Value
		}
	}
	return false
}

// TestRepairToolPairingMergesParallelResults 并行工具调用的多条 tool 消息必须合并为
// 一条 user 消息（Anthropic 要求 result 聚合在紧跟 assistant 的那条消息里）。
func TestRepairToolPairingMergesParallelResults(t *testing.T) {
	p := &AnthropicProvider{}
	params, _ := p.buildMessageParams([]core.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_00", Name: "a"}, {ID: "call_01", Name: "b"}}},
		{Role: "tool", ToolCallID: "call_00", Content: "ra"},
		{Role: "tool", ToolCallID: "call_01", Content: "rb"},
	}, nil, 0)

	msgs := params.Messages
	if len(msgs) != 3 {
		t.Fatalf("应合并为 3 条消息（user/assistant/user），实际 %d 条", len(msgs))
	}
	if msgs[1].Role != anthropic.MessageParamRoleAssistant || len(msgToolUseIDs(msgs[1])) != 2 {
		t.Fatalf("第 2 条应为含两个 tool_use 的 assistant，got %v", msgToolUseIDs(msgs[1]))
	}
	got := msgResultIDs(msgs[2])
	if len(got) != 2 || got[0] != "call_00" || got[1] != "call_01" {
		t.Fatalf("两条 tool_result 应在同一条 user 消息里，got %v", got)
	}
}

// TestRepairToolPairingFillsMissingResult 只有部分结果时，缺失的一方补合成 result
// （is_error=true），否则整个请求会被 400 拒绝。
func TestRepairToolPairingFillsMissingResult(t *testing.T) {
	p := &AnthropicProvider{}
	params, _ := p.buildMessageParams([]core.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_00", Name: "a"}, {ID: "call_01", Name: "b"}}},
		{Role: "tool", ToolCallID: "call_00", Content: "ra"},
	}, nil, 0)

	msgs := params.Messages
	if len(msgs) != 3 {
		t.Fatalf("应为 3 条消息，实际 %d", len(msgs))
	}
	ids := msgResultIDs(msgs[2])
	if len(ids) != 2 {
		t.Fatalf("应补齐两个 result，got %v", ids)
	}
	if resultIsError(msgs[2], "call_00") {
		t.Fatal("已有结果的一方不应标记为 error")
	}
	if !resultIsError(msgs[2], "call_01") {
		t.Fatal("缺失结果的一方应补 is_error=true 的合成 result")
	}
}

// TestRepairToolPairingResultPrecedesNextUserTurn 工具结果后紧跟新一轮用户输入时，
// result 必须排在该 user 消息最前（不能只发文本）。
func TestRepairToolPairingResultPrecedesNextUserTurn(t *testing.T) {
	p := &AnthropicProvider{}
	params, _ := p.buildMessageParams([]core.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_00", Name: "a"}}},
		{Role: "user", Content: "继续"},
	}, nil, 0)

	msgs := params.Messages
	if len(msgs) != 3 {
		t.Fatalf("应为 3 条消息，实际 %d", len(msgs))
	}
	last := msgs[2]
	if ids := msgResultIDs(last); len(ids) != 1 || ids[0] != "call_00" {
		t.Fatalf("新用户输入的消息必须以合成 result 开头，got %v", ids)
	}
	if last.Content[0].OfToolResult == nil {
		t.Fatal("result 必须是该消息的第一个块")
	}
}

// TestRepairToolPairingTrailingToolUse 历史以 assistant(tool_use) 结尾时补一条 result，
// 否则该 tool_use 永远没有紧随的 tool_result。
func TestRepairToolPairingTrailingToolUse(t *testing.T) {
	p := &AnthropicProvider{}
	params, _ := p.buildMessageParams([]core.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "调用工具", ToolCalls: []core.ToolCall{{ID: "call_00", Name: "a"}}},
	}, nil, 0)

	msgs := params.Messages
	if len(msgs) != 3 {
		t.Fatalf("末尾应补一条 result 消息，实际 %d 条", len(msgs))
	}
	if ids := msgResultIDs(msgs[2]); len(ids) != 1 || ids[0] != "call_00" {
		t.Fatalf("末尾 result 不对: %v", ids)
	}
}

// TestRepairToolPairingDropsOrphanResult 没有对应 tool_use 的孤儿 result 必须剔除，
// 否则 Anthropic 同样报 400（unexpected tool_use_id）。
func TestRepairToolPairingDropsOrphanResult(t *testing.T) {
	msgs := repairToolPairing([]anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("hi")),
		anthropic.NewUserMessage(anthropic.NewToolResultBlock("ghost", "orphan", false)),
		anthropic.NewUserMessage(anthropic.NewTextBlock("继续")),
	})
	if len(msgs) != 2 {
		t.Fatalf("孤儿 result 消息应被丢弃，实际 %d 条", len(msgs))
	}
	for _, m := range msgs {
		if ids := msgResultIDs(m); len(ids) != 0 {
			t.Fatalf("不应残留孤儿 result: %v", ids)
		}
	}
}

// TestRepairToolPairingKeepsValidHistory 合法历史（单工具调用 + 对应结果）不应被改动。
func TestRepairToolPairingKeepsValidHistory(t *testing.T) {
	p := &AnthropicProvider{}
	params, _ := p.buildMessageParams([]core.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []core.ToolCall{{ID: "call_00", Name: "a"}}},
		{Role: "tool", ToolCallID: "call_00", Content: "ra"},
		{Role: "assistant", Content: "done"},
	}, nil, 0)

	msgs := params.Messages
	if len(msgs) != 4 {
		t.Fatalf("合法历史应保持 4 条，实际 %d 条", len(msgs))
	}
	if ids := msgResultIDs(msgs[2]); len(ids) != 1 || ids[0] != "call_00" {
		t.Fatalf("result 不应被改动: %v", ids)
	}
	if msgs[3].Role != anthropic.MessageParamRoleAssistant {
		t.Fatal("末条 assistant 不应被追加内容")
	}
}
