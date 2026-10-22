package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	dsc "dsc-sdk"
	openai "github.com/sashabaranov/go-openai"
)

// TestExplicitAPIKeyWinsOverProcessEnv 与 llm-anthropic 的同名测试对齐，守护
// openai 路径的凭据优先级：config.yaml 注入的 DSC_OPENAI_API_KEY（经
// LoadLLMConfig 整组解析）必须一路胜到线上请求，不被进程环境里的旧名
// OPENAI_API_KEY 覆盖。
//
// 历史事故：宿主→插件的 env 投递顺序被 fork 反转（os/exec 重复键取后者），
// 终端默认 key 覆盖配置 key，DeepSeek 返回 401。openai 插件与 anthropic 插件
// 共用同一条投递链，故同样中招。
func TestExplicitAPIKeyWinsOverProcessEnv(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "terminal-key-should-lose")
	t.Setenv("OPENAI_BASE_URL", "https://terminal.invalid/v1")
	t.Setenv("DSC_OPENAI_API_KEY", "config-key-should-win")
	t.Setenv("DSC_OPENAI_BASE_URL", "https://config.invalid/v1")
	t.Setenv("DSC_OPENAI_MODEL", "deepseek-flash")

	cfg := dsc.LoadLLMConfig("openai")
	if cfg.APIKey != "config-key-should-win" {
		t.Fatalf("DSC_ 前缀应整组胜出，got apiKey=%q", cfg.APIKey)
	}
	if cfg.Model != "deepseek-flash" {
		t.Fatalf("模型名应取 DSC_OPENAI_MODEL，got %q", cfg.Model)
	}

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmpl-test","object":"chat.completion","created":0,"model":"deepseek-flash","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	// 与 main() 相同的构造方式（DefaultConfig 之后显式覆盖 BaseURL）。
	clientCfg := openai.DefaultConfig(cfg.APIKey)
	clientCfg.BaseURL = srv.URL
	client := openai.NewClientWithConfig(clientCfg)

	if _, err := client.CreateChatCompletion(context.Background(), openai.ChatCompletionRequest{
		Model:    cfg.Model,
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
	}); err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if gotAuth != "Bearer config-key-should-win" {
		t.Fatalf("实际发出的 Authorization 应为配置 key，got %q（进程 env 劫持）", gotAuth)
	}
}
