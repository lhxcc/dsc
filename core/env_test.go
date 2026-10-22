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
// DSC_<P>_API_KEY 这类 LLM 凭据）一律滤除；插件专属凭据只放行给登记的插件；
// config.yaml 为该插件显式声明的 env 不受过滤。
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

	// 插件专属放行：仅 tool-harness-webui 能读到 admin token（其 admin 代理转发需要），
	// 但它同样不应看到 LLM 凭据。
	webuiEnv := buildEnv("tool-harness-webui", nil, false)
	if !containsEnvKey(webuiEnv, "DSC_ADMIN_TOKEN") {
		t.Fatalf("tool-harness-webui 应能读取 DSC_ADMIN_TOKEN，got %v", webuiEnv)
	}
	if containsEnvKey(webuiEnv, "DSC_ANTHROPIC_API_KEY") {
		t.Fatalf("tool-harness-webui 不应读取 LLM 凭据，got %v", webuiEnv)
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
