package session

import (
	"testing"

	"dsc/proto"
)

// LastUsageAnchor 是「重启后用量锚点」的**唯一实现**（宿主 core 与 agent 插件经 sdk
// 都调这一份）。它把 llm/attempt（用量）与 request/header（信封）按 Turn/Step 配对，
// 因此测试必须同时钉住：
//   - 只认成功结算且带 prompt 用量的 attempt；
//   - 取**最后一条**满足条件的，而不是第一条；
//   - 同一步重试多条 header 时取**最近那条**（那次尝试实际用的信封）；
//   - header 缺失（本改动前的旧日志）→ ToolsHash 为空 → Comparable 恒 false。
func TestLastUsageAnchor(t *testing.T) {
	if _, ok := LastUsageAnchor(nil); ok {
		t.Fatal("nil session 应返回 false")
	}

	s := New()
	// 第一轮：header + 成功 attempt（应被更晚的覆盖）
	s.Append(RequestHeader, &RequestHeaderData{
		Turn: 1, Step: 1, ToolsHash: "old-hash", Model: "m1", ContextWindow: 131072, SentEstimate: 900,
	}, nil)
	s.Append(LLMAttempt, &LLMAttemptData{
		Turn: 1, Step: 1, Usage: &proto.Usage{PromptTokens: 1000, TotalTokens: 1100},
	}, nil)
	// 第二轮：失败 attempt（即便带 usage 也不能当锚点）
	s.Append(RequestHeader, &RequestHeaderData{
		Turn: 2, Step: 1, ToolsHash: "h2", Model: "m2", ContextWindow: 1048576, SentEstimate: 30000,
	}, nil)
	s.Append(LLMAttempt, &LLMAttemptData{
		Turn: 2, Step: 1, Error: "rate limited", Usage: &proto.Usage{PromptTokens: 99999},
	}, nil)
	// 第三轮：同一步两条 header（第一次尝试失败后重试，信封以最近一条为准）
	s.Append(RequestHeader, &RequestHeaderData{
		Turn: 3, Step: 1, ToolsHash: "stale", Model: "m3", ContextWindow: 1048576, SentEstimate: 1,
	}, nil)
	s.Append(RequestHeader, &RequestHeaderData{
		Turn: 3, Step: 1, ToolsHash: "fresh", Model: "m3", ContextWindow: 1048576, SentEstimate: 37790,
	}, nil)
	s.Append(LLMAttempt, &LLMAttemptData{
		Turn: 3, Step: 1, Usage: &proto.Usage{PromptTokens: 40347, TotalTokens: 40349},
	}, nil)

	a, ok := LastUsageAnchor(s)
	if !ok {
		t.Fatal("存在成功且带用量的调用，应返回 true")
	}
	if a.PromptTokens != 40347 {
		t.Fatalf("应取最后一条成功调用的 prompt tokens=40347，got %d", a.PromptTokens)
	}
	if a.ToolsHash != "fresh" || a.SentEstimate != 37790 || a.Model != "m3" {
		t.Fatalf("应配对同一步**最近**的 header，got %+v", a)
	}
	if !a.Comparable("fresh", "m3", 1048576) {
		t.Fatal("同信封应判定可比")
	}
	if a.Comparable("fresh", "m4", 1048576) {
		t.Fatal("换模型后不应判定可比")
	}
	if a.Comparable("other", "m3", 1048576) {
		t.Fatal("换工具目录后不应判定可比")
	}

	// 只有失败调用时不得给出锚点
	only := New()
	only.Append(RequestHeader, &RequestHeaderData{Turn: 1, Step: 1, ToolsHash: "h"}, nil)
	only.Append(LLMAttempt, &LLMAttemptData{Turn: 1, Step: 1, Error: "boom", Usage: &proto.Usage{PromptTokens: 500}}, nil)
	if _, ok := LastUsageAnchor(only); ok {
		t.Fatal("失败调用不应作为锚点")
	}

	// 旧日志：只有 attempt、没有 header → 锚点存在但不可比（保守方向）
	legacy := New()
	legacy.Append(LLMAttempt, &LLMAttemptData{
		Turn: 1, Step: 1, Usage: &proto.Usage{PromptTokens: 4096},
	}, nil)
	la, ok := LastUsageAnchor(legacy)
	if !ok {
		t.Fatal("旧日志仍应给出用量")
	}
	if la.Comparable("any", "any", 0) {
		t.Fatal("没有 header 的旧日志必须判定不可比")
	}
}
