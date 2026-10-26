package main

import (
	"testing"

	"dsc/proto"
	"dsc/session"
)

// TestSeedUsageFromHistory 重启恢复时用「上次成功请求」的 prompt 用量作锚点：
// 首发 Project 应退回该实测值，而不是全量派生估算（后者会把压缩后端遮蔽、
// 模型看不到的历史也算进「已用」，标题虚高）。
func TestSeedUsageFromHistory(t *testing.T) {
	a := &ReactLoopAgent{contextWindow: 1048576}
	s := session.New()
	// 较早的一次成功请求
	s.Append(session.LLMAttempt, &session.LLMAttemptData{Turn: 1, Step: 1,
		Usage: &proto.Usage{PromptTokens: 30000, TotalTokens: 30500}}, nil)
	// 最近一次成功请求（应取它）
	s.Append(session.LLMAttempt, &session.LLMAttemptData{Turn: 75, Step: 21,
		Usage: &proto.Usage{PromptTokens: 40347, TotalTokens: 40864}, FinishReason: "stop"}, nil)
	// 之后一次失败（无 prompt 用量，不应覆盖锚点）
	s.Append(session.LLMAttempt, &session.LLMAttemptData{Turn: 76, Step: 3,
		Error: "context_window_exceeded"}, nil)

	a.seedUsageFromHistory(s)

	if a.lastPromptTokens != 40347 {
		t.Fatalf("lastPromptTokens = %d, want 40347", a.lastPromptTokens)
	}
	if got := a.tokenMeter().Project(913292); got != 40347 {
		t.Fatalf("Project(全量估算 913292) = %d, want 锚点实测 40347（不应退回全量估算）", got)
	}
}
