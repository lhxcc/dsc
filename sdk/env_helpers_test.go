package dsc

import (
	"os"
	"testing"
)

// TestEnvWithFallback DSC_ 前缀优先、旧名回退、空白视同未设置。
func TestEnvWithFallback(t *testing.T) {
	t.Setenv("DSC_TEST_FB", "dsc-value")
	t.Setenv("TEST_FB", "legacy-value")
	if got := EnvWithFallback("DSC_TEST_FB", "TEST_FB", "def"); got != "dsc-value" {
		t.Fatalf("DSC_ 应优先: %q", got)
	}

	os.Unsetenv("DSC_TEST_FB")
	if got := EnvWithFallback("DSC_TEST_FB", "TEST_FB", "def"); got != "legacy-value" {
		t.Fatalf("应回退旧名: %q", got)
	}

	os.Unsetenv("TEST_FB")
	if got := EnvWithFallback("DSC_TEST_FB", "TEST_FB", "def"); got != "def" {
		t.Fatalf("都缺席应返回默认值: %q", got)
	}

	// 纯空白视同未设置（与其它 Env helper 一致）。
	t.Setenv("DSC_TEST_FB", "   ")
	if got := EnvWithFallback("DSC_TEST_FB", "TEST_FB", "def"); got != "def" {
		t.Fatalf("空白应视同未设置: %q", got)
	}

	// 无旧名可用时只看主名。
	t.Setenv("DSC_TEST_FB", "only-dsc")
	if got := EnvWithFallback("DSC_TEST_FB", "", "def"); got != "only-dsc" {
		t.Fatalf("无旧名时读主名: %q", got)
	}
}

func TestEnvStr(t *testing.T) {
	t.Setenv("DSC_TEST_STR", "  hello  ")
	if got := EnvStr("DSC_TEST_STR", "def"); got != "hello" {
		t.Errorf("EnvStr trimmed = %q, want %q", got, "hello")
	}
	t.Setenv("DSC_TEST_STR", "")
	if got := EnvStr("DSC_TEST_STR", "def"); got != "def" {
		t.Errorf("EnvStr absent = %q, want %q", got, "def")
	}
}

func TestEnvInt(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"42", 42},
		{"  -7  ", -7},
		{"not-a-number", 99},
		{"", 99},
	}
	for _, c := range cases {
		t.Setenv("DSC_TEST_INT", c.env)
		if got := EnvInt("DSC_TEST_INT", 99); got != c.want {
			t.Errorf("EnvInt(%q) = %d, want %d", c.env, got, c.want)
		}
	}
}

func TestEnvInt64(t *testing.T) {
	t.Setenv("DSC_TEST_I64", "12345678901")
	if got := EnvInt64("DSC_TEST_I64", 0); got != 12345678901 {
		t.Errorf("EnvInt64 = %d, want %d", got, 12345678901)
	}
	t.Setenv("DSC_TEST_I64", "bad")
	if got := EnvInt64("DSC_TEST_I64", 7); got != 7 {
		t.Errorf("EnvInt64 bad = %d, want %d", got, 7)
	}
}

func TestEnvBool(t *testing.T) {
	cases := []struct {
		env  string
		want bool
	}{
		{"1", true},
		{"true", true},
		{"TRUE", true},
		{"yes", true},
		{"on", true},
		{"0", false},
		{"false", false},
		{"no", false},
		{"off", false},
		{"", true}, // 缺席返回 def
	}
	for _, c := range cases {
		t.Setenv("DSC_TEST_BOOL", c.env)
		if got := EnvBool("DSC_TEST_BOOL", true); got != c.want {
			t.Errorf("EnvBool(%q) = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestEnvPositiveInt(t *testing.T) {
	cases := []struct {
		env  string
		want int
	}{
		{"42", 42},
		{"  100  ", 100},
		{"0", 0},
		{"-5", 0},
		{"bad", 0},
		{"", 0},
	}
	for _, c := range cases {
		t.Setenv("DSC_TEST_POS", c.env)
		if got := EnvPositiveInt("DSC_TEST_POS"); got != c.want {
			t.Errorf("EnvPositiveInt(%q) = %d, want %d", c.env, got, c.want)
		}
	}
}

func TestEnvPositiveInt64(t *testing.T) {
	cases := []struct {
		env  string
		want int64
	}{
		{"9999999999", 9999999999},
		{"0", 0},
		{"-1", 0},
		{"bad", 0},
		{"", 0},
	}
	for _, c := range cases {
		t.Setenv("DSC_TEST_POS64", c.env)
		if got := EnvPositiveInt64("DSC_TEST_POS64"); got != c.want {
			t.Errorf("EnvPositiveInt64(%q) = %d, want %d", c.env, got, c.want)
		}
	}
}

func TestLoadLLMConfig(t *testing.T) {
	// 显式 provider env
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_BASE_URL", "https://api.example.com")
	t.Setenv("ANTHROPIC_MODEL", "claude-test")
	t.Setenv("ANTHROPIC_MAX_OUTPUT_TOKENS", "8192")
	t.Setenv("DSC_NO_VISION", "")
	cfg := LoadLLMConfig("anthropic")
	if cfg.APIKey != "sk-test" {
		t.Errorf("APIKey = %q, want %q", cfg.APIKey, "sk-test")
	}
	if cfg.BaseURL != "https://api.example.com" {
		t.Errorf("BaseURL = %q, want %q", cfg.BaseURL, "https://api.example.com")
	}
	if cfg.Model != "claude-test" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-test")
	}
	if cfg.MaxOutputTokens != 8192 {
		t.Errorf("MaxOutputTokens = %d, want %d", cfg.MaxOutputTokens, 8192)
	}
	if !cfg.VisionEnabled {
		t.Error("VisionEnabled = false, want true")
	}

	// DSC_MAX_OUTPUT_TOKENS 兜底
	t.Setenv("ANTHROPIC_MAX_OUTPUT_TOKENS", "")
	t.Setenv("DSC_MAX_OUTPUT_TOKENS", "4096")
	cfg = LoadLLMConfig("anthropic")
	if cfg.MaxOutputTokens != 4096 {
		t.Errorf("MaxOutputTokens fallback = %d, want %d", cfg.MaxOutputTokens, 4096)
	}

	// DSC_NO_VISION=1 关闭视觉
	t.Setenv("DSC_NO_VISION", "1")
	cfg = LoadLLMConfig("anthropic")
	if cfg.VisionEnabled {
		t.Error("VisionEnabled = true, want false (DSC_NO_VISION=1)")
	}

	// OpenAI provider
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	cfg = LoadLLMConfig("openai")
	if cfg.APIKey != "sk-openai" {
		t.Errorf("OpenAI APIKey = %q, want %q", cfg.APIKey, "sk-openai")
	}
	if cfg.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("OpenAI BaseURL = %q, want %q", cfg.BaseURL, "https://api.openai.com/v1")
	}
}

// TestLoadLLMConfigMaxOutputTokensPrefix 验证单轮输出上限的 DSC_ 前缀优先/旧名回退：
// 此前只读旧名 <PROVIDER>_MAX_OUTPUT_TOKENS，写 DSC_ 名不生效（与文档自相矛盾）。
func TestLoadLLMConfigMaxOutputTokensPrefix(t *testing.T) {
	t.Setenv("DSC_OPENAI_MAX_OUTPUT_TOKENS", "65536")
	t.Setenv("OPENAI_MAX_OUTPUT_TOKENS", "8192")
	t.Setenv("DSC_MAX_OUTPUT_TOKENS", "131072")

	if cfg := LoadLLMConfig("openai"); cfg.MaxOutputTokens != 65536 {
		t.Fatalf("DSC_ 前缀应优先，got %d", cfg.MaxOutputTokens)
	}

	// 无 DSC_ 前缀 → 回退旧名。
	os.Unsetenv("DSC_OPENAI_MAX_OUTPUT_TOKENS")
	if cfg := LoadLLMConfig("openai"); cfg.MaxOutputTokens != 8192 {
		t.Fatalf("应回退旧名，got %d", cfg.MaxOutputTokens)
	}

	// 两者都缺席 → DSC_MAX_OUTPUT_TOKENS 兜底。
	os.Unsetenv("OPENAI_MAX_OUTPUT_TOKENS")
	if cfg := LoadLLMConfig("openai"); cfg.MaxOutputTokens != 131072 {
		t.Fatalf("应回退宿主通用 DSC_MAX_OUTPUT_TOKENS，got %d", cfg.MaxOutputTokens)
	}

	// 显式 -1（或任意负数）= 明确不携带，归一化为 -1，且不会被 DSC_MAX_OUTPUT_TOKENS
	// 兜底覆盖。
	for _, off := range []string{"-1", "-5"} {
		t.Setenv("DSC_OPENAI_MAX_OUTPUT_TOKENS", off)
		if cfg := LoadLLMConfig("openai"); cfg.MaxOutputTokens != -1 {
			t.Fatalf("显式 %q 应归一化为 -1（不携带），got %d", off, cfg.MaxOutputTokens)
		}
	}

	// 显式 0 仍表示「未设置」→ 继续走宿主兜底（0 保留 Go 零值语义）。
	t.Setenv("DSC_OPENAI_MAX_OUTPUT_TOKENS", "0")
	if cfg := LoadLLMConfig("openai"); cfg.MaxOutputTokens != 131072 {
		t.Fatalf("显式 0 应按未设置处理并兜底，got %d", cfg.MaxOutputTokens)
	}

	// 非法值按「未设置」处理 → 仍走宿主兜底，避免拼写错误静默关闭输出上限。
	t.Setenv("DSC_OPENAI_MAX_OUTPUT_TOKENS", "abc")
	if cfg := LoadLLMConfig("openai"); cfg.MaxOutputTokens != 131072 {
		t.Fatalf("非法值应按未设置处理并兜底，got %d", cfg.MaxOutputTokens)
	}

	// anthropic 同规则。
	t.Setenv("DSC_ANTHROPIC_MAX_OUTPUT_TOKENS", "65536")
	t.Setenv("ANTHROPIC_MAX_OUTPUT_TOKENS", "8192")
	if cfg := LoadLLMConfig("anthropic"); cfg.MaxOutputTokens != 65536 {
		t.Fatalf("anthropic 的 DSC_ 前缀应优先，got %d", cfg.MaxOutputTokens)
	}
}

// TestLoadLLMConfigDSCPrefixGroup 验证 anthropic/openai 的三个键（API_KEY/BASE_URL/
// MODEL）作为一个整体决定前缀：任一 DSC_ 前缀键存在即整组采用 DSC_ 前缀，禁止与
// 旧名混用。
func TestLoadLLMConfigDSCPrefixGroup(t *testing.T) {
	t.Setenv("DSC_NO_VISION", "")

	// 仅设置 DSC_ANTHROPIC_BASE_URL，未设 DSC_ANTHROPIC_MODEL（但旧 ANTHROPIC_MODEL 存在）：
	// 整组走 DSC_ 前缀，Model 必须读 DSC_ANTHROPIC_MODEL（空），不得回落旧 ANTHROPIC_MODEL。
	t.Setenv("DSC_ANTHROPIC_BASE_URL", "https://dsc.example.com")
	t.Setenv("ANTHROPIC_MODEL", "should-not-be-used")
	cfg := LoadLLMConfig("anthropic")
	if cfg.BaseURL != "https://dsc.example.com" {
		t.Errorf("BaseURL = %q, want DSC_ value", cfg.BaseURL)
	}
	if cfg.Model != "" {
		t.Errorf("Model = %q, want empty (group uses DSC_ prefix, no mixing)", cfg.Model)
	}
	t.Setenv("DSC_ANTHROPIC_MODEL", "dsc-model")
	if cfg = LoadLLMConfig("anthropic"); cfg.Model != "dsc-model" {
		t.Errorf("Model = %q, want dsc-model", cfg.Model)
	}

	// 「键被设置但值为空」也视为已配置 DSC_ 组：不得回退终端旧名——否则配置里
	// 显式留空的 key 会静默回落到终端历史遗留的 ANTHROPIC_API_KEY（实测事故：
	// 终端默认 key ****8888 被使用，导致 401）。
	t.Setenv("DSC_ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "terminal-key-should-not-be-used")
	if cfg = LoadLLMConfig("anthropic"); cfg.APIKey != "" {
		t.Errorf("DSC_ 键存在但为空时不得回退旧名，APIKey = %q", cfg.APIKey)
	}

	// 真正取消设置 DSC_ 前缀（os.Unsetenv，注意「设为空串」≠「未设置」）：整组回退旧名。
	os.Unsetenv("DSC_ANTHROPIC_BASE_URL")
	os.Unsetenv("DSC_ANTHROPIC_MODEL")
	os.Unsetenv("DSC_ANTHROPIC_API_KEY")
	t.Setenv("ANTHROPIC_BASE_URL", "https://old.example.com")
	t.Setenv("ANTHROPIC_MODEL", "old-model")
	cfg = LoadLLMConfig("anthropic")
	if cfg.BaseURL != "https://old.example.com" || cfg.Model != "old-model" {
		t.Errorf("回退旧名失败: BaseURL=%q Model=%q", cfg.BaseURL, cfg.Model)
	}
	if cfg.APIKey != "terminal-key-should-not-be-used" {
		t.Errorf("未设 DSC_ 时应回退旧名 APIKey，got %q", cfg.APIKey)
	}

	// OpenAI 组同样适用。
	t.Setenv("DSC_OPENAI_BASE_URL", "https://dsc.openai.example.com")
	t.Setenv("OPENAI_MODEL", "old-openai-model")
	cfg = LoadLLMConfig("openai")
	if cfg.BaseURL != "https://dsc.openai.example.com" {
		t.Errorf("OpenAI BaseURL = %q, want DSC_ value", cfg.BaseURL)
	}
	if cfg.Model != "" {
		t.Errorf("OpenAI Model = %q, want empty (no mixing)", cfg.Model)
	}
}

// TestLoadLLMConfigReasoningEffort 验证 DSC 统一推理强度 DSC_REASONING_EFFORT（6 档
// none/minimal/low/medium/high/max）按 provider 映射：anthropic→思考 token 预算
// （none=0 关闭/2048/4096/8192/16384/32768），openai→reasoning_effort 字面量 1:1；
// 接口专属变量优先于统一档位。
func TestLoadLLMConfigReasoningEffort(t *testing.T) {
	clearReasoningEnv := func() {
		for _, k := range []string{
			"DSC_REASONING_EFFORT", "DSC_ANTHROPIC_THINKING_BUDGET",
			"ANTHROPIC_THINKING_BUDGET", "DSC_OPENAI_REASONING_EFFORT",
		} {
			t.Setenv(k, "")
		}
	}

	// 1) 统一 high → anthropic 16384 / openai "high"
	clearReasoningEnv()
	t.Setenv("DSC_REASONING_EFFORT", "high")
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 16384 {
		t.Errorf("anthropic high ThinkingBudget = %d, want 16384", cfg.ThinkingBudget)
	}
	if cfg := LoadLLMConfig("openai"); cfg.OpenAIReasoningEffort != "high" {
		t.Errorf("openai high = %q, want high", cfg.OpenAIReasoningEffort)
	}

	// 2) 统一 max → anthropic 32768 / openai "max"（1:1 透传，不再钳到 high）
	clearReasoningEnv()
	t.Setenv("DSC_REASONING_EFFORT", "max")
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 32768 {
		t.Errorf("anthropic max ThinkingBudget = %d, want 32768", cfg.ThinkingBudget)
	}
	if cfg := LoadLLMConfig("openai"); cfg.OpenAIReasoningEffort != "max" {
		t.Errorf("openai max = %q, want max", cfg.OpenAIReasoningEffort)
	}

	// 2b) 统一 minimal → anthropic 2048 / openai "minimal"
	clearReasoningEnv()
	t.Setenv("DSC_REASONING_EFFORT", "minimal")
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 2048 {
		t.Errorf("anthropic minimal ThinkingBudget = %d, want 2048", cfg.ThinkingBudget)
	}
	if cfg := LoadLLMConfig("openai"); cfg.OpenAIReasoningEffort != "minimal" {
		t.Errorf("openai minimal = %q, want minimal", cfg.OpenAIReasoningEffort)
	}

	// 2d) 统一 none → anthropic 0（思考关闭）/ openai "none"
	clearReasoningEnv()
	t.Setenv("DSC_REASONING_EFFORT", "none")
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 0 || cfg.ReasoningEffort != "none" {
		t.Errorf("anthropic none: ThinkingBudget=%d ReasoningEffort=%q, want 0/none", cfg.ThinkingBudget, cfg.ReasoningEffort)
	}
	if cfg := LoadLLMConfig("openai"); cfg.OpenAIReasoningEffort != "none" {
		t.Errorf("openai none = %q, want none", cfg.OpenAIReasoningEffort)
	}

	// 3) 统一 low → anthropic 4096 / openai "low"
	clearReasoningEnv()
	t.Setenv("DSC_REASONING_EFFORT", "low")
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 4096 {
		t.Errorf("anthropic low ThinkingBudget = %d, want 4096", cfg.ThinkingBudget)
	}
	if cfg := LoadLLMConfig("openai"); cfg.OpenAIReasoningEffort != "low" {
		t.Errorf("openai low = %q, want low", cfg.OpenAIReasoningEffort)
	}

	// 4) 大小写不敏感：Medium → 8192
	clearReasoningEnv()
	t.Setenv("DSC_REASONING_EFFORT", "Medium")
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 8192 {
		t.Errorf("anthropic Medium ThinkingBudget = %d, want 8192", cfg.ThinkingBudget)
	}

	// 5) 接口专属覆盖统一档位：DSC_ANTHROPIC_THINKING_BUDGET 优先
	clearReasoningEnv()
	t.Setenv("DSC_REASONING_EFFORT", "high")
	t.Setenv("DSC_ANTHROPIC_THINKING_BUDGET", "8192")
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 8192 {
		t.Errorf("anthropic 专属预算应优先, ThinkingBudget = %d, want 8192", cfg.ThinkingBudget)
	}

	// 6) 旧名 ANTHROPIC_THINKING_BUDGET 回退（无 DSC_ 时）
	clearReasoningEnv()
	t.Setenv("ANTHROPIC_THINKING_BUDGET", "2048")
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 2048 {
		t.Errorf("anthropic 旧名回退, ThinkingBudget = %d, want 2048", cfg.ThinkingBudget)
	}

	// 7) DSC_OPENAI_REASONING_EFFORT 字面量绕过统一映射（端点支持 xhigh 等文档外档位时）
	clearReasoningEnv()
	t.Setenv("DSC_REASONING_EFFORT", "high")
	t.Setenv("DSC_OPENAI_REASONING_EFFORT", "xhigh")
	if cfg := LoadLLMConfig("openai"); cfg.OpenAIReasoningEffort != "xhigh" {
		t.Errorf("openai 专属字面量应优先, got %q, want xhigh", cfg.OpenAIReasoningEffort)
	}

	// 8) 未设 → 均为零值
	clearReasoningEnv()
	if cfg := LoadLLMConfig("anthropic"); cfg.ThinkingBudget != 0 || cfg.ReasoningEffort != "" {
		t.Errorf("anthropic 未设: ThinkingBudget=%d ReasoningEffort=%q", cfg.ThinkingBudget, cfg.ReasoningEffort)
	}
	if cfg := LoadLLMConfig("openai"); cfg.OpenAIReasoningEffort != "" || cfg.ReasoningEffort != "" {
		t.Errorf("openai 未设: OpenAIReasoningEffort=%q ReasoningEffort=%q", cfg.OpenAIReasoningEffort, cfg.ReasoningEffort)
	}
}
