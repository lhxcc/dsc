package dsc

import (
	"os"
	"strconv"
	"strings"
)

// LLMConfig 是 LLM 插件的通用配置面，把 apiKey / baseURL / model / maxTokens /
// visionEnabled 五个散落各 LLM 插件 main() 的 env 解析合一，并额外承载推理强度
// （reasoning effort）配置。provider 参数取 "anthropic" / "openai" / "ollama" 等，
// 对应 DSC_<PROVIDER>_API_KEY / _BASE_URL / _MODEL / _MAX_OUTPUT_TOKENS（前三个
// 整组决定前缀，旧名 <PROVIDER>_* 回退）；DSC_MAX_OUTPUT_TOKENS 与 DSC_NO_VISION
// 是跨 provider 的宿主通用 env。
//
// 推理强度采用 DSC 统一命名 DSC_REASONING_EFFORT，取值对齐 OpenAI 官方档位（统一 6 档：
// none / minimal / low / medium / high / max；xhigh 等文档外档位仍可用
// DSC_OPENAI_REASONING_EFFORT 直接直传，不经此统一映射）。由 LoadLLMConfig 按 provider
// 映射为具体参数：anthropic 路径→思考 token 预算（ThinkingBudget，none 关闭思考，
// 其余 2048/4096/8192/16384/32768，亦可被 DSC_ANTHROPIC_THINKING_BUDGET 直接覆盖）；
// openai 路径→ reasoning_effort 字面量（OpenAIReasoningEffort，6 档 1:1 透传，亦可被
// DSC_OPENAI_REASONING_EFFORT 直接覆盖为端点支持的字面量）。
//
// 解析顺序（先 provider 专属 env，后宿主通用 DSC_ env）对齐既有 llm-anthropic
// 与 llm-openai 的实现（先读 DSC_<P>_MAX_OUTPUT_TOKENS / 旧名 <P>_MAX_OUTPUT_TOKENS，
// 再回退 DSC_MAX_OUTPUT_TOKENS），消除两份复制粘贴。
type LLMConfig struct {
	// APIKey 来自 <PROVIDER>_API_KEY；缺席返回空串（由调用方决定是否兜底）。
	APIKey string
	// BaseURL 来自 <PROVIDER>_BASE_URL；缺席返回空串。
	BaseURL string
	// Model 来自 <PROVIDER>_MODEL；缺席返回空串。
	Model string
	// MaxOutputTokens 三态：0=未设置（已由 DSC_MAX_OUTPUT_TOKENS 兜底，仍为 0 表示
	// 宿主也未注入 → 不携带）；-1=明确不携带 max_tokens；>0=携带该值。
	// 来源 DSC_<PROVIDER>_MAX_OUTPUT_TOKENS（旧名 <PROVIDER>_MAX_OUTPUT_TOKENS 回退）。
	// 用 -1 而非 0 表达「明确关闭」，因为 0 是 Go 零值、保留「未设置」语义。
	MaxOutputTokens int64
	// VisionEnabled 按 DSC_NO_VISION 强制关闭；否则返回 true（由调用方按模型
	// 能力进一步判定，见 core.ModelSupportsImages）。
	VisionEnabled bool
	// ReasoningEffort 统一推理强度档位（DSC_REASONING_EFFORT），取值
	// none/minimal/low/medium/high/max；空串=未设，由插件自行决定默认行为。
	// 其中 none 表示关闭推理（anthropic 路径据此关闭 thinking；openai 路径下发
	// reasoning_effort=none）。xhigh 等文档外档位不在统一枚举内，用
	// DSC_OPENAI_REASONING_EFFORT 直传即可。
	// anthropic 路径：档位同时驱动 thinking.budget_tokens（真实 Anthropic 用）与
	// output_config.effort（DeepSeek /anthropic 口用，因其忽略 budget_tokens）。
	ReasoningEffort string
	// ThinkingBudget anthropic 路径思考 token 预算（DSC_ANTHROPIC_THINKING_BUDGET，
	// 回退旧名 ANTHROPIC_THINKING_BUDGET）；未设且 ReasoningEffort 已设时由档位映射；
	// 均为 0 时插件用默认 4096。
	ThinkingBudget int64
	// OpenAIReasoningEffort openai 路径 reasoning_effort 字面量（DSC_OPENAI_REASONING_EFFORT）；
	// 未设且 ReasoningEffort 已设时由档位映射（none/minimal/low/medium/high/max 1:1 透传）。
	// 非空即原样下发。
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
	// 整组决定前缀：三个键（API_KEY / BASE_URL / MODEL）任一以 DSC_ 前缀「被设置」即整组
	// 采用 DSC_ 前缀读取，禁止 DSC_ 与非 DSC_ 变量混用（例如 DSC_ANTHROPIC_BASE_URL
	// 与旧 ANTHROPIC_MODEL 混搭导致半新半旧的不一致配置）。三者必须作为一个整体读取。
	//
	// 用 LookupEnv 判断「键存在」而非「值非空」：配置里显式写 `DSC_ANTHROPIC_API_KEY: ""`
	// 表达的是「用 DSC_ 组、该键留空」，此时绝不能把控制权交回终端的旧名同义变量
	//（终端里常有历史遗留的 ANTHROPIC_API_KEY 默认值，回退过去会静默用错凭据）。
	useDSC := false
	for _, suf := range []string{"API_KEY", "BASE_URL", "MODEL"} {
		if _, ok := os.LookupEnv("DSC_" + prefix + suf); ok {
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
	// 单轮输出上限：DSC_<P>_MAX_OUTPUT_TOKENS 优先、旧名 <P>_MAX_OUTPUT_TOKENS 回退。
	//
	// 三态语义（用 -1 哨兵把「未设置」与「明确不携带」分开，不依赖 Go 零值判断）：
	//   0  = 未设置（含显式写 0）→ 继续走 DSC_MAX_OUTPUT_TOKENS 兜底
	//   -1 = 明确不携带 max_tokens（写 -1 或任意负数均归一化为 -1）
	//   >0 = 携带该值
	// 消费者只需 `>0 才携带`，负值天然被忽略。若想关闭宿主注入的默认上限，必须写 -1
	// （写 0 仍表示未设置，会被兜底覆盖——这是刻意的：0 保留 Go 零值语义）。
	maxOut, maxOutSet := int64(0), false
	if v, ok := EnvLookupSet("DSC_"+prefix+"MAX_OUTPUT_TOKENS", prefix+"MAX_OUTPUT_TOKENS"); ok {
		if n, parseErr := strconv.ParseInt(v, 10, 64); parseErr == nil {
			switch {
			case n > 0:
				maxOut, maxOutSet = n, true
			case n < 0:
				maxOut, maxOutSet = -1, true
			}
			// n == 0 与非法值都保持「未设置」语义 → 由兜底处理
		}
	}
	cfg := LLMConfig{
		APIKey:  strings.TrimSpace(os.Getenv(resolved("API_KEY"))),
		BaseURL: strings.TrimSpace(os.Getenv(resolved("BASE_URL"))),
		Model:   strings.TrimSpace(os.Getenv(resolved("MODEL"))),
		// MaxOutputTokens 见上方三态说明。
		MaxOutputTokens: maxOut,
		VisionEnabled:   !envNoVision(),
	}
	// DSC_MAX_OUTPUT_TOKENS 兜底（仅「未设置」时生效；非法值按未设置处理）
	if !maxOutSet {
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
// none 返回 0（思考关闭，由插件据 cfg.ReasoningEffort=="none" 判定）；minimal/low/medium/
// high/max 对应 2048/4096/8192/16384/32768。未知档位返回 0（交由插件用默认
// 4096）。max 恢复 32768 以与 openai 路径的 max 档对应；若端点支持更高预算，可用
// DSC_ANTHROPIC_THINKING_BUDGET 直接覆盖。
func anthropicThinkingBudgetForLevel(level string) int64 {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none":
		return 0
	case "minimal":
		return 2048
	case "low":
		return 4096
	case "medium":
		return 8192
	case "high":
		return 16384
	case "max":
		return 32768
	default:
		return 0
	}
}

// openaiReasoningEffortForLevel 将 DSC 标准档位映射为 openai 路径 reasoning_effort 字面量。
// 统一 6 档（none/minimal/low/medium/high/max）与此处 1:1 透传；xhigh 等文档外档位不在
// 统一枚举内，由调用方设 DSC_OPENAI_REASONING_EFFORT 直接覆盖。
func openaiReasoningEffortForLevel(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none", "minimal", "low", "medium", "high", "max":
		return strings.ToLower(strings.TrimSpace(level))
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
