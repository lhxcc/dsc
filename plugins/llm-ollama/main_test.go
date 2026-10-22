package main

import (
	"os"
	"testing"
)

// TestOllamaThinkingEnabled 守护思考开关的前缀语义：DSC_OLLAMA_THINKING 优先，
// 旧名 OLLAMA_THINKING 回退；0/false/off/no 关闭，未设置默认开启。
func TestOllamaThinkingEnabled(t *testing.T) {
	os.Unsetenv("DSC_OLLAMA_THINKING")
	os.Unsetenv("OLLAMA_THINKING")
	if !ollamaThinkingEnabled() {
		t.Fatal("未设置时应默认开启思考")
	}

	for _, off := range []string{"0", "false", "OFF", "no"} {
		t.Setenv("DSC_OLLAMA_THINKING", off)
		if ollamaThinkingEnabled() {
			t.Fatalf("%q 应关闭思考", off)
		}
	}

	// DSC_ 前缀优先。
	t.Setenv("DSC_OLLAMA_THINKING", "1")
	t.Setenv("OLLAMA_THINKING", "0")
	if !ollamaThinkingEnabled() {
		t.Fatal("DSC_OLLAMA_THINKING 应优先于旧名")
	}

	// 无 DSC_ 时回退旧名。
	os.Unsetenv("DSC_OLLAMA_THINKING")
	t.Setenv("OLLAMA_THINKING", "0")
	if ollamaThinkingEnabled() {
		t.Fatal("无 DSC_ 时应回退旧名 OLLAMA_THINKING=0 关闭")
	}
}

func TestResolveOllamaConfigDSCPrefixGroup(t *testing.T) {
	// 保存并清理相关环境变量，避免污染其它用例
	keys := []string{"DSC_OLLAMA_HOST", "DSC_OLLAMA_MODEL", "OLLAMA_HOST", "OLLAMA_MODEL", "OLLAMA_THINKING"}
	saved := map[string]string{}
	for _, k := range keys {
		saved[k] = os.Getenv(k)
		os.Unsetenv(k)
	}
	defer func() {
		for _, k := range keys {
			if saved[k] == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, saved[k])
			}
		}
	}()

	// 1) DSC_ 前缀任一存在 → 整组采用 DSC_（不混用旧名）
	os.Setenv("DSC_OLLAMA_HOST", "http://dsc-host:11434")
	model, host := resolveOllamaConfig()
	if host != "http://dsc-host:11434" {
		t.Fatalf("DSC_ 组应返回 DSC_OLLAMA_HOST: %q", host)
	}
	if model != "llama3.1:latest" {
		t.Fatalf("DSC_ 组无 MODEL 时应回默认: %q", model)
	}

	// 2) DSC_ 组 MODEL 也存在 → 读取 DSC_ 值，不回退旧名
	os.Setenv("DSC_OLLAMA_MODEL", "qwen2.5:14b")
	// 同时设置一个旧名，验证不会被混用
	os.Setenv("OLLAMA_MODEL", "legacy-model-should-be-ignored")
	// 终端里残留的旧名 HOST 同样不得劫持 config 注入的 DSC_OLLAMA_HOST
	//（env 投递链修复前，子进程里的同名变量会被宿主值覆盖，连到错误服务）。
	os.Setenv("OLLAMA_HOST", "http://terminal-legacy:11434")
	model, host = resolveOllamaConfig()
	if model != "qwen2.5:14b" {
		t.Fatalf("DSC_ 组应优先读 DSC_OLLAMA_MODEL: %q", model)
	}
	if host != "http://dsc-host:11434" {
		t.Fatalf("整组 DSC_ 不应回退旧名 HOST: %q", host)
	}

	// 3) 无 DSC_ 前缀 → 回退旧名
	os.Unsetenv("DSC_OLLAMA_HOST")
	os.Unsetenv("DSC_OLLAMA_MODEL")
	os.Setenv("OLLAMA_HOST", "http://legacy:11434")
	os.Setenv("OLLAMA_MODEL", "llama2")
	model, host = resolveOllamaConfig()
	if host != "http://legacy:11434" {
		t.Fatalf("无 DSC_ 时应回退 OLLAMA_HOST: %q", host)
	}
	if model != "llama2" {
		t.Fatalf("无 DSC_ 时应回退 OLLAMA_MODEL: %q", model)
	}

	// 4) 完全无配置 → 默认值
	os.Unsetenv("OLLAMA_HOST")
	os.Unsetenv("OLLAMA_MODEL")
	model, host = resolveOllamaConfig()
	if host != "http://127.0.0.1:11434" {
		t.Fatalf("默认 HOST 应为本机 ollama: %q", host)
	}
	if model != "llama3.1:latest" {
		t.Fatalf("默认 MODEL 应为 llama3.1:latest: %q", model)
	}
}
