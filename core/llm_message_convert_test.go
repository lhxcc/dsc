package core

import (
	"testing"

	"dsc/proto"
)

// 本文件覆盖「宿主 → LLM 插件」的消息转换保真。
//
// 回归背景：宿主有两条等价的 proto→core.Message 转换（router 用 protoMessagesToPlugin，
// 插件侧 gRPC 入口用 convertLLMChatRequest），其中只有后者抄了 ToolCallId。于是所有
// 走 router 的请求里 tool 消息都没有 tool_call_id —— OpenAI/DeepSeek 直接 422
// `messages[3]: missing field tool_call_id`（现场那份会话 472 条 tool 消息全缺），
// 而 Anthropic 插件按顺序配对，反倒看不出来。

// TestProtoMessagesToPluginKeepsToolCallID tool 消息的 tool_call_id 与 assistant 的
// tool_calls id 都必须原样透传——两者缺一，provider 端的工具调用就无法配对。
func TestProtoMessagesToPluginKeepsToolCallID(t *testing.T) {
	req := &proto.ChatRequest{Messages: []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolCalls: []*proto.ToolCall{
			{Id: "call_1", Name: "read", ArgumentsJson: `{"p":"a"}`},
			{Id: "call_2", Name: "write", ArgumentsJson: `{"p":"b"}`},
		}},
		{Role: "tool", Content: "result-1", ToolCallId: "call_1"},
		{Role: "tool", Content: "result-2", ToolCallId: "call_2"},
	}}

	msgs, _ := protoMessagesToPlugin(req)
	if len(msgs) != 5 {
		t.Fatalf("消息数 = %d, 期望 5", len(msgs))
	}
	if got := msgs[3].ToolCallID; got != "call_1" {
		t.Fatalf("tool 消息 ToolCallID = %q, 期望 call_1（缺了会让 OpenAI 422）", got)
	}
	if got := msgs[4].ToolCallID; got != "call_2" {
		t.Fatalf("tool 消息 ToolCallID = %q, 期望 call_2", got)
	}
	if len(msgs[2].ToolCalls) != 2 ||
		msgs[2].ToolCalls[0].ID != "call_1" || msgs[2].ToolCalls[1].ID != "call_2" {
		t.Fatalf("assistant tool_calls 丢失或错序: %+v", msgs[2].ToolCalls)
	}
}

// TestConvertLLMChatRequestDelegates 两条转换必须同源（曾经各写一份导致漂移）。
func TestConvertLLMChatRequestDelegates(t *testing.T) {
	req := &proto.ChatRequest{Messages: []*proto.Message{
		{Role: "tool", Content: "r", ToolCallId: "call_x"},
	}}
	a, _ := convertLLMChatRequest(req)
	b, _ := protoMessagesToPlugin(req)
	if a[0].ToolCallID != "call_x" || b[0].ToolCallID != "call_x" {
		t.Fatalf("tool_call_id 丢失: convert=%q plugin=%q", a[0].ToolCallID, b[0].ToolCallID)
	}
}
