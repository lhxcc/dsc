package main

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"dsc/core"
)

// TestAnthropicEffortForLevel 验证统一档位 → output_config.effort 映射。
func TestAnthropicEffortForLevel(t *testing.T) {
	cases := []struct {
		in   string
		want anthropic.OutputConfigEffort // 空串表示不携带 effort（none/空）
	}{
		{"", ""},
		{"none", ""},
		{"NONE", ""}, // 大小写不敏感
		{"minimal", anthropic.OutputConfigEffortLow},
		{"low", anthropic.OutputConfigEffortLow},
		{"medium", anthropic.OutputConfigEffortMedium},
		{"high", anthropic.OutputConfigEffortHigh},
		{"max", anthropic.OutputConfigEffortMax},
	}
	for _, c := range cases {
		if got := anthropicEffortForLevel(c.in); got != c.want {
			t.Errorf("anthropicEffortForLevel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestBuildMessageParamsEmitsOutputConfigEffort 验证 thinking 开启且设了档位时，
// 请求同时携带 thinking（budget）与 output_config.effort；DeepSeek 忽略 budget、
// 吃 effort，二者并存互不冲突。
func TestBuildMessageParamsEmitsOutputConfigEffort(t *testing.T) {
	msgs := []core.Message{{Role: "user", Content: "hi"}}

	// high → effort=high
	p := &AnthropicProvider{thinking: true, thinkingBudget: 16384, reasoningEffort: "high"}
	params, _ := p.buildMessageParams(msgs, nil, 0)
	if params.OutputConfig.Effort != anthropic.OutputConfigEffortHigh {
		t.Errorf("high: OutputConfig.Effort = %q, want high", params.OutputConfig.Effort)
	}

	// minimal → effort 降级为 low（Anthropic/DeepSeek effort 枚举无 minimal）
	p2 := &AnthropicProvider{thinking: true, thinkingBudget: 2048, reasoningEffort: "minimal"}
	params2, _ := p2.buildMessageParams(msgs, nil, 0)
	if params2.OutputConfig.Effort != anthropic.OutputConfigEffortLow {
		t.Errorf("minimal: OutputConfig.Effort = %q, want low (degraded)", params2.OutputConfig.Effort)
	}

	// none → 上游已关闭 thinking，不携带 effort
	p3 := &AnthropicProvider{thinking: false, thinkingBudget: 4096, reasoningEffort: "none"}
	params3, _ := p3.buildMessageParams(msgs, nil, 0)
	if params3.OutputConfig.Effort != "" {
		t.Errorf("none: OutputConfig.Effort = %q, want empty (thinking disabled)", params3.OutputConfig.Effort)
	}

	// 未设档位 → 不携带 effort（向后兼容）
	p4 := &AnthropicProvider{thinking: true, thinkingBudget: 4096, reasoningEffort: ""}
	params4, _ := p4.buildMessageParams(msgs, nil, 0)
	if params4.OutputConfig.Effort != "" {
		t.Errorf("empty: OutputConfig.Effort = %q, want empty", params4.OutputConfig.Effort)
	}
}
