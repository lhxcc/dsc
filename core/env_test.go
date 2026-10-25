package core

import (
	"strings"
	"testing"
)

// TestPluginEnvPassThrough 验证插件 env = 宿主环境 + 插件自定义 env
// （互通服务 ID 改经 SetInterconnect 握手传入，不经 env）。
func TestPluginEnvPassThrough(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	env := m.coreEnv(PluginEntry{Env: map[string]string{"A": "1"}})
	if !containsEnv(env, "A", "1") {
		t.Fatalf("custom env should pass through, got %v", env)
	}
	if containsEnvKey(env, "DSC_LLM_SERVICE_ID") || containsEnvKey(env, "DSC_NOTIFY_SERVICE_ID") {
		t.Fatalf("interconnect ids must not be injected via env, got %v", env)
	}
}

func containsEnv(env []string, key, val string) bool {
	for _, kv := range env {
		if kv == key+"="+val {
			return true
		}
	}
	return false
}

func containsEnvKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// TestBuildEnvStripsSecretsForNonLLM 验证非 LLM 插件：宿主环境里的凭据（含
// DSC_<P>_API_KEY 这类 LLM 凭据）一律滤除；插件凭据只能经 config.yaml 的 `env:`
// 块显式授予（核心不维护任何「按插件名放行」的白名单）。
func TestBuildEnvStripsSecretsForNonLLM(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-secret")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-secret")
	t.Setenv("SOME_PROVIDER_SECRET", "abc")
	t.Setenv("DSC_ANTHROPIC_API_KEY", "sk-llm-secret") // LLM 凭据同样属于凭据，不得外泄
	t.Setenv("DSC_OLLAMA_API_KEY", "sk-ollama-secret")
	t.Setenv("DSC_ADMIN_TOKEN", "admin-token")
	t.Setenv("PLAIN_VAR", "keep")

	env := buildEnv("tool-filesystem", nil, false)
	for _, k := range []string{
		"OPENAI_API_KEY", "ANTHROPIC_AUTH_TOKEN", "SOME_PROVIDER_SECRET",
		"DSC_ANTHROPIC_API_KEY", "DSC_OLLAMA_API_KEY", "DSC_ADMIN_TOKEN",
	} {
		if containsEnvKey(env, k) {
			t.Fatalf("非 LLM 插件不得继承宿主凭据 %s，got %v", k, env)
		}
	}
	if !containsEnvKey(env, "PLAIN_VAR") {
		t.Fatalf("非机密宿主变量应保留，got %v", env)
	}

	// 凭据只能经配置显式授予：即使某个插件「需要」admin token，核心也不按名字放行
	// （核心不认识插件名，这类白名单会随插件增减腐化）。
	strictEnv := buildEnv("any-plugin", nil, false)
	if containsEnvKey(strictEnv, "DSC_ADMIN_TOKEN") {
		t.Fatalf("未在 config.yaml 声明 env 时不得继承 admin token，got %v", strictEnv)
	}
	// 显式授予后可用，且仍不夹带 LLM 凭据。
	granted := buildEnv("any-plugin", map[string]string{"DSC_ADMIN_TOKEN": "admin-token"}, false)
	if !containsEnvKey(granted, "DSC_ADMIN_TOKEN") {
		t.Fatalf("config.yaml 显式授予的凭据应注入，got %v", granted)
	}
	if containsEnvKey(granted, "DSC_ANTHROPIC_API_KEY") {
		t.Fatalf("显式授予单个凭据不应连带 LLM 凭据，got %v", granted)
	}

	// 显式注入（config.yaml 的 env: 块）不受过滤——那是「给这个插件的配置」。
	explicit := buildEnv("tool-ssh", map[string]string{"TOOL_SSH_PASSWORD": "pw"}, false)
	if !containsEnv(explicit, "TOOL_SSH_PASSWORD", "pw") {
		t.Fatalf("config.yaml 显式注入的插件凭据必须保留，got %v", explicit)
	}

	// LLM 插件：凭据全保留（含宿主继承的）。
	envLLM := buildEnv("llm-anthropic", nil, true)
	if !containsEnvKey(envLLM, "OPENAI_API_KEY") || !containsEnvKey(envLLM, "DSC_ANTHROPIC_API_KEY") {
		t.Fatalf("LLM 插件应保留凭据，got %v", envLLM)
	}
}
