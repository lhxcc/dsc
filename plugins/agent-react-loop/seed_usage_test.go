package main

import (
	"testing"

	"dsc/proto"
	"dsc/session"
)

// TestSeedUsageFromHistory 重启恢复时用「上次成功请求」的 prompt 用量作锚点：
// 它被记进 meter（MeasuredPromptTokens 可读，供展示兜底与「锚点是否已超窗」判断），
// 但**不得**直接充当投影。
//
// 曾经这里断言 Project 退回该实测值（把全量派生估算「压」下去，避免标題虚高）。
// 那在小窗口读超长会话时是灾难：滑動窗口每步裁剪 → 拿不到增量基线 → Project 恒等于
// 这个旧值，界面上的「已用」从首帧起就不动（实测 33K/3.29%，而 provider 实测 730K）。
// 现在投影退回估算，并由调用方按**裁剪后真正发出的表面**计算；压缩后的大小由宿主
// 记录的「实际表面」接管，不会虚高。
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
	// 实测值仍可作为展示兜底读到
	if got := a.tokenMeter().MeasuredPromptTokens(); got != 40347 {
		t.Fatalf("MeasuredPromptTokens = %d, want 40347", got)
	}
	// 但没有增量基线时不能拿它当投影——投影必须描述「即将发出的这一发」
	if got := a.tokenMeter().Project(913292); got != 913292 {
		t.Fatalf("Project(913292) = %d, want 913292（无基线时应退回估算，不是旧实测值）", got)
	}
}
