package session

import (
	"testing"

	"dsc/proto"
)

// LastPromptUsage 是「重启后用量锚点」的**唯一实现**（宿主 core 与 agent 插件经 sdk
// 都调这一份）。本测试钉住三条判定，避免将来有人在自己那侧又写一遍倒查循环：
//   - 只认成功结算（Error 为空）：失败调用没有可用的 prompt 规模；
//   - 只认带 prompt 用量的（Usage 非 nil 且 PromptTokens > 0）；
//   - 取**最后一条**满足条件的，而不是第一条/最大值。
func TestLastPromptUsage(t *testing.T) {
	if _, _, ok := LastPromptUsage(nil); ok {
		t.Fatal("nil session 应返回 false")
	}

	s := New()
	// 更早的一次成功调用（应被更晚的覆盖）
	s.Append(LLMAttempt, &LLMAttemptData{
		Turn: 1, Step: 1,
		Usage: &proto.Usage{PromptTokens: 1000, TotalTokens: 1100},
	}, nil)
	// 失败调用：即便带了 usage 也不能当锚点
	s.Append(LLMAttempt, &LLMAttemptData{
		Turn: 1, Step: 2, Error: "rate limited", Code: "rate_limited",
		Usage: &proto.Usage{PromptTokens: 99999},
	}, nil)
	// 成功但 provider 未报用量：跳过
	s.Append(LLMAttempt, &LLMAttemptData{Turn: 1, Step: 3, FinishReason: "stop"}, nil)
	// 最新一条成功的（锚点应取这条）
	s.Append(LLMAttempt, &LLMAttemptData{
		Turn: 2, Step: 1,
		Usage: &proto.Usage{PromptTokens: 40347, CompletionTokens: 2, TotalTokens: 40349},
	}, nil)

	got, usage, ok := LastPromptUsage(s)
	if !ok {
		t.Fatal("存在成功且带用量的调用，应返回 true")
	}
	if got != 40347 {
		t.Fatalf("应取最后一条成功调用的 prompt tokens=40347，got %d", got)
	}
	if usage == nil || usage.PromptTokens != 40347 {
		t.Fatalf("应同时返回该次完整 usage，got %+v", usage)
	}

	// 只有失败调用时不得给出锚点
	only := New()
	only.Append(LLMAttempt, &LLMAttemptData{Turn: 1, Step: 1, Error: "boom", Usage: &proto.Usage{PromptTokens: 500}}, nil)
	if _, _, ok := LastPromptUsage(only); ok {
		t.Fatal("失败调用不应作为锚点")
	}
}
