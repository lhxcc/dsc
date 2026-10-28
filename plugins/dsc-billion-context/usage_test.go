package main

import (
	"strings"
	"testing"

	dsc "dsc-sdk"
	"dsc/proto"
)

// TestUsageTokensPrefersReportedPromptUsage 压缩决策必须按「模型真正收到多少」判压力。
//
// 宿主给的 ev.TokenCount 是「消息 + 工具目录」的完整估算，但那估算的是 agent 的原始
// 历史；压缩只改写视图、不回写历史，所以它只增不减：越过阈值后压缩永远压不下来，
// 插件就会每轮提示压缩。上一轮 provider 上报的 prompt 用量随压缩回落，才是正确口径。
func TestUsageTokensPrefersReportedPromptUsage(t *testing.T) {
	msgs := []*proto.Message{{Role: "user", Content: strings.Repeat("x", 4000)}} // ~1000 token

	// provider 上报了真实 prompt 用量：以它为准，忽略原始历史估算
	ev := dsc.AgentPreStepEvent{TokenCount: 500_000, LastUsageTokens: 20_000}
	if got := usageTokens(ev, msgs); got != 20_000 {
		t.Fatalf("应优先用 provider 上报的 prompt 用量 20000，实际 %d", got)
	}

	// provider 不报用量（本地端点常见）：退回宿主的原始历史估算，行为不变
	ev = dsc.AgentPreStepEvent{TokenCount: 500_000}
	if got := usageTokens(ev, msgs); got != 500_000 {
		t.Fatalf("无上报用量时应退回宿主估算 500000，实际 %d", got)
	}

	// 两者都没有：本地估算兜底，不能为 0（否则使用率恒为 0，永不提示）
	ev = dsc.AgentPreStepEvent{}
	if got := usageTokens(ev, msgs); got <= 0 {
		t.Fatalf("无任何来源时应用本地估算，实际 %d", got)
	}
}

// TestUsageTokensFallbackIncludesToolDirectory 本地兜底必须与「请求大小」同口径：
// 消息 + 工具目录。
//
// 工具目录每次请求都带、且不随会话增长消失（十几到几十个工具约 10k token）。漏掉它
// 就是系统性低估压力——128K 窗口下 10k 相当于 7.8%，够让阈值判定晚整整一步。
func TestUsageTokensFallbackIncludesToolDirectory(t *testing.T) {
	msgs := []*proto.Message{{Role: "user", Content: strings.Repeat("x", 4000)}}
	msgOnly := dsc.EstimateProtoMessagesTokens(msgs)

	ev := dsc.AgentPreStepEvent{ToolTokens: 12_345}
	if got := usageTokens(ev, msgs); got != msgOnly+12_345 {
		t.Fatalf("兜底应含工具目录：want %d（消息 %d + 工具 12345），got %d",
			msgOnly+12_345, msgOnly, got)
	}
}
