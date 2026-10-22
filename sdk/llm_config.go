package dsc

import (
	"os"
	"strings"
)

// LLMConfig 是 LLM 插件的通用配置面，把 apiKey / baseURL / model / maxTokens /
// visionEnabled 五个散落各 LLM 插件 main() 的 env 解析合一，并额外承载推理强度
// （reasoning effort）配置。provider 参数取 "anthropic" / "openai" / "ollama" 等，
// 对应 <PROVIDER>_API_KEY / <PROVIDER>_BASE_URL / <PROVIDER>_MODEL /
// <PROVIDER>_MAX_OUTPUT_TOKENS env；DSC_MAX_OUTPUT_TOKENS 与 DSC_NO_VISION 是跨
// provider 的宿主通用 env。
//
// 推理强度采用 DSC 统一命名 DSC_REASONING_EFFORT（取值 low/medium/high/max），
// 由 LoadLLMConfig 按 provider 映射为具体参数：anthropic 路径→思考 token 预算
// （ThinkingBudget，亦可被 DSC_ANTHROPIC_THINKING_BUDGET 直接覆盖）；openai 路径
// → reasoning_effort 字面量（OpenAIReasoningEffort，亦可被 DSC_OPENAI_REASONING_EFFORT
// 直接覆盖）。max 档位在 openai 路径钳到 high（DeepSeek/OpenAI 仅文档化
// low/medium/high）；若端点支持 max/xhigh 等，用 DSC_OPENAI_REASONING_EFFORT 传字面量。
//
// 解析顺序（先显式 provider env，后宿主通用 DSC_ env）对齐既有 llm-anthropic
// 与 llm-openai 的实现（maxTokensFromEnv 先读 ANTHROPIC_MAX_OUTPUT_TOKENS，
// 回退 DSC_MAX_OUTPUT_TOKENS），消除两份复制粘贴。
type LLMConfig struct {
	// APIKey 来自 <PROVIDER>_API_KEY；缺席返回空串（由调用方决定是否兜底）。
	APIKey string
	// BaseURL 来自 <PROVIDER>_BASE_URL；缺席返回空串。
	BaseURL string
	// Model 来自 <PROVIDER>_MODEL；缺席返回空串。
	Model string
	// MaxOutputTokens 来自 <PROVIDER>_MAX_OUTPUT_TOKENS，回退 DSC_MAX_OUTPUT_TOKENS；
	// 均缺席或非法返回 0（请求不携带 max_tokens，由模型自然结束）。
	MaxOutputTokens int64
	// VisionEnabled 按 DSC_NO_VISION 强制关闭；否则返回 true（由调用方按模型
	// 能力进一步判定，见 core.ModelSupportsImages）。
	VisionEnabled bool
	// ReasoningEffort 统一推理强度档位（DSC_REASONING_EFFORT），取值
	// low/medium/high/max；空串=未设，由插件自行决定默认行为。
	ReasoningEffort string
	// ThinkingBudget anthropic 路径思考 token 预算（DSC_ANTHROPIC_THINKING_BUDGET，
	// 回退旧名 ANTHROPIC_THINKING_BUDGET）；未设且 ReasoningEffort 已设时由档位映射；
	// 均为 0 时插件用默认 4096。
	ThinkingBudget int64
	// OpenAIReasoningEffort openai 路径 reasoning_effort 字面量（DSC_OPENAI_REASONING_EFFORT）；
	// 未设且 ReasoningEffort 已设时由档位映射（max→high）。非空即原样下发。
	OpenAIReasoningEffort string
}

// LoadLLMConfig 按 provider 名读取 LLM 通用配置。provider 不区分大小写。
//
// 用法（LLM 插件 main()）：
//
//	cfg := dsc.LoadLLMConfig("anthropic")
//	// 用 cfg.APIKey / cfg.BaseURL / cfg.Model / cfg.MaxOutputTokens / cfg.VisionEnabled
//
// 对齐 DSH packages/api/llm-* 各 provider 共用配置加载模式。
func LoadLLMConfig(provider string) LLMConfig {
	p := strings.ToUpper(strings.TrimSpace(provider))
	prefix := p + "_"
	// 整组决定前缀：三个键（API_KEY / BASE_URL / MODEL）任一以 DSC_ 前缀存在即整组
	// 采用 DSC_ 前缀读取，禁止 DSC_ 与非 DSC_ 变量混用（例如 DSC_ANTHROPIC_BASE_URL
	// 与旧 ANTHROPIC_MODEL 混搭导致半新半旧的不一致配置）。三者必须作为一个整体读取。
	useDSC := false
	for _, suf := range []string{"API_KEY", "BASE_URL", "MODEL"} {
		if os.Getenv("DSC_"+prefix+suf) != "" {
			useDSC = true
			break
		}
	}
	resolved := func(suffix string) string {
		if useDSC {
			return "DSC_" + prefix + suffix
		}
		return prefix + suffix
	}
	cfg := LLMConfig{
		APIKey:          strings.TrimSpace(os.Getenv(resolved("API_KEY"))),
		BaseURL:         strings.TrimSpace(os.Getenv(resolved("BASE_URL"))),
		Model:           strings.TrimSpace(os.Getenv(resolved("MODEL"))),
		MaxOutputTokens: EnvPositiveInt64(prefix + "MAX_OUTPUT_TOKENS"),
		VisionEnabled:   !envNoVision(),
	}
	// DSC_MAX_OUTPUT_TOKENS 兜底（provider 显式优先）
	if cfg.MaxOutputTokens == 0 {
		cfg.MaxOutputTokens = EnvPositiveInt64("DSC_MAX_OUTPUT_TOKENS")
	}
	// 推理强度：DSC 统一档位（DSC_REASONING_EFFORT）+ 各接口具体覆盖。
	// 优先级：接口专属变量 > 统一档位映射 > 0/空（插件用自身默认）。
	reasoningEffort := strings.TrimSpace(os.Getenv("DSC_REASONING_EFFORT"))
	cfg.ReasoningEffort = reasoningEffort
	if p == "ANTHROPIC" {
		tb := EnvPositiveInt64("DSC_ANTHROPIC_THINKING_BUDGET")
		if tb == 0 {
			tb = EnvPositiveInt64("ANTHROPIC_THINKING_BUDGET") // 旧名回退
		}
		if tb == 0 && reasoningEffort != "" {
			tb = anthropicThinkingBudgetForLevel(reasoningEffort)
		}
		cfg.ThinkingBudget = tb
	}
	if p == "OPENAI" {
		oai := strings.TrimSpace(os.Getenv("DSC_OPENAI_REASONING_EFFORT"))
		if oai == "" && reasoningEffort != "" {
			oai = openaiReasoningEffortForLevel(reasoningEffort)
		}
		cfg.OpenAIReasoningEffort = oai
	}
	return cfg
}

// anthropicThinkingBudgetForLevel 将 DSC 标准档位映射为 anthropic 路径思考 token 预算。
// 未知档位返回 0（交由插件用默认 4096）。max 与 high 同为 16384：统一两路处理，anthropic
// 思考预算与 openai reasoning_effort 在 max 档均回落 high（通常用 high 已足够，更高强度待
// 后续按需放开）；若端点支持更高预算，可用 DSC_ANTHROPIC_THINKING_BUDGET 直接覆盖。
func anthropicThinkingBudgetForLevel(level string) int64 {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "low":
		return 4096
	case "medium":
		return 8192
	case "high":
		return 16384
	case "max":
		return 16384
	default:
		return 0
	}
}

// openaiReasoningEffortForLevel 将 DSC 标准档位映射为 openai 路径 reasoning_effort 字面量。
// max 钳到 high（DeepSeek/OpenAI 文档化档位仅 low/medium/high）；若端点支持
// max/xhigh，由调用方设 DSC_OPENAI_REASONING_EFFORT 传字面量绕过本映射。
func openaiReasoningEffortForLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "max":
		return "high"
	default:
		return ""
	}
}

// envNoVision 解析 DSC_NO_VISION：1/true/on/yes 返回 true（强制关闭视觉）。
func envNoVision() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSC_NO_VISION"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}
