package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"dsc-sdk"
)

// TestNoBearerHeaderWhenAPIKeyPresent 实证 SDK 的凭据链优先级：只要
// ANTHROPIC_API_KEY 存在，DefaultClientOptions 就在第一步 return，绝不会再追加
// Authorization: Bearer（来自 ANTHROPIC_AUTH_TOKEN）。
//
// 这正是宿主侧「DSC_ 组回写旧名 + 清除 AUTH_TOKEN」的依据：终端 .profile 常只导出
// ANTHROPIC_AUTH_TOKEN（给 Claude Code 用），若把它留给 SDK，请求会带 Bearer 头，
// DeepSeek /anthropic 按 Bearer 认证 → 报「api key ****8888 is invalid」。
func TestNoBearerHeaderWhenAPIKeyPresent(t *testing.T) {
	t.Setenv("ANTHROPIC_BASE_URL", "https://terminal.invalid/anthropic")
	t.Setenv("ANTHROPIC_API_KEY", "config-key-should-win")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-terminal-8888")

	cfg := dsc.LoadLLMConfig("anthropic")
	if cfg.APIKey != "config-key-should-win" {
		t.Fatalf("api key 应取 ANTHROPIC_API_KEY，got %q", cfg.APIKey)
	}

	var gotAuth, gotAPIKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"fake 401"}}`))
	}))
	defer srv.Close()

	client := anthropic.NewClient(
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(srv.URL),
	)
	_, _ = client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model("deepseek-flash"),
		MaxTokens: 64,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if gotAPIKey != "config-key-should-win" {
		t.Fatalf("X-Api-Key 应为配置值，got %q", gotAPIKey)
	}
	if gotAuth != "" {
		t.Fatalf("API_KEY 存在时不得发送 Authorization 头（否则 Bearer 会抢占认证），got %q", gotAuth)
	}
}

// TestExplicitAPIKeyWinsOverProcessEnv 守护凭据解析的优先级链：
// config.yaml 注入的 DSC_<P>_API_KEY（经 LoadLLMConfig 解析）必须一路胜到线上请求——
// vendored anthropic SDK 的 DefaultClientOptions 会读进程环境里的 ANTHROPIC_API_KEY，
// 而 NewClient 的显式 option 后应用获胜（本测试是这条链的活证明）。
//
// 历史事故：宿主→插件的 env 投递顺序被 fork 反转（os/exec 重复键取后者），终端里的
// 默认 key 覆盖了配置 key，DeepSeek 返回 401 "api key ****8888 is invalid"。
func TestExplicitAPIKeyWinsOverProcessEnv(t *testing.T) {
	// 旧式进程环境变量（模拟终端里的默认/残留配置，必须不生效）。
	t.Setenv("ANTHROPIC_API_KEY", "terminal-key-should-lose")
	t.Setenv("ANTHROPIC_BASE_URL", "https://terminal.invalid/anthropic")
	// config.yaml 注入（DSC_ 前缀整组读取）。
	t.Setenv("DSC_ANTHROPIC_API_KEY", "config-key-should-win")
	t.Setenv("DSC_ANTHROPIC_BASE_URL", "https://config.invalid/anthropic")
	t.Setenv("DSC_ANTHROPIC_MODEL", "deepseek-flash")

	cfg := dsc.LoadLLMConfig("anthropic")
	if cfg.APIKey != "config-key-should-win" {
		t.Fatalf("DSC_ 前缀应整组胜出，got apiKey=%q", cfg.APIKey)
	}
	if cfg.Model != "deepseek-flash" {
		t.Fatalf("模型名应取 DSC_ANTHROPIC_MODEL，got %q", cfg.Model)
	}

	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"fake 401"}}`))
	}))
	defer srv.Close()

	client := anthropic.NewClient(
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(srv.URL),
	)
	_, _ = client.Messages.New(context.Background(), anthropic.MessageNewParams{
		Model:     anthropic.Model(cfg.Model),
		MaxTokens: 64,
		Messages:  []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))},
	})
	if gotKey != "config-key-should-win" {
		t.Fatalf("SDK 实际发出的 x-api-key 应为配置值，got %q（进程 env 劫持）", gotKey)
	}
}
