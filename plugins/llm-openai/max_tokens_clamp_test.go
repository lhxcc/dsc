package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dsc "dsc-sdk"
	openai "github.com/sashabaranov/go-openai"
)

// 背景：宿主把 DSC_MAX_OUTPUT_TOKENS 注入为有效上下文窗口值（用户 context_window
// 设为 1048576），而部分端点对 max_tokens 有更低上限（DeepSeek 393216），服务端
// 直接 400「Invalid max_tokens value, the valid range of max_tokens is [1, 393216]」。
// 各端点上限不同、随模型变化，硬编码不可靠，故从拒绝报文里读上限并自适应。

func TestMaxTokensRejectionLimitParsesRange(t *testing.T) {
	err := &openai.APIError{
		HTTPStatusCode: http.StatusBadRequest,
		Message:        "Invalid max_tokens value, the valid range of max_tokens is [1, 393216]",
	}
	limit, ok := maxTokensRejectionLimit(err)
	if !ok || limit != 393216 {
		t.Fatalf("应解析出上限 393216，got limit=%d ok=%v", limit, ok)
	}

	// 流式路径的 RequestError 载体同样要能解析。
	streamErr := &openai.RequestError{
		HTTPStatusCode: http.StatusBadRequest,
		Body:           []byte(`{"error":{"message":"Invalid max_tokens value, the valid range of max_tokens is [1, 65536]"}}`),
	}
	if limit, ok := maxTokensRejectionLimit(streamErr); !ok || limit != 65536 {
		t.Fatalf("RequestError 应解析出 65536，got limit=%d ok=%v", limit, ok)
	}
}

func TestMaxTokensRejectionLimitWithoutRange(t *testing.T) {
	// 只提到 max_tokens、未给区间：返回 ok + limit=0（表示改发不带该字段的请求）。
	err := &openai.APIError{
		HTTPStatusCode: http.StatusBadRequest,
		Message:        "max_tokens must be positive",
	}
	if limit, ok := maxTokensRejectionLimit(err); !ok || limit != 0 {
		t.Fatalf("无区间时应返回 (0,true)，got limit=%d ok=%v", limit, ok)
	}
}

func TestMaxTokensRejectionLimitIgnoresOtherErrors(t *testing.T) {
	cases := []error{
		&openai.APIError{HTTPStatusCode: http.StatusBadRequest, Message: "invalid tool schema"},
		&openai.APIError{HTTPStatusCode: http.StatusUnauthorized, Message: "max_tokens invalid, range [1, 100]"},
		fmt.Errorf("connection reset"),
	}
	for _, err := range cases {
		if limit, ok := maxTokensRejectionLimit(err); ok {
			t.Fatalf("非 max_tokens 超范围错误不应触发重试：%v（limit=%d）", err, limit)
		}
	}
}

// TestResolveMaxTokensClampedByLearnedLimit 学习到上限后，后续请求直接钳制，
// 不再每次都先吃一次 400。
func TestResolveMaxTokensClampedByLearnedLimit(t *testing.T) {
	p := &OpenAIProvider{maxTokens: 1048576}
	if got := p.resolveMaxTokens(0, nil, nil); got != 1048576 {
		t.Fatalf("未知上限时按原值携带，got %d", got)
	}

	p.learnMaxTokensLimit(393216)
	if got := p.resolveMaxTokens(0, nil, nil); got != 393216 {
		t.Fatalf("已知上限时应钳制到 393216，got %d", got)
	}
	// 请求级参数（压缩净余值）同样受钳制。
	if got := p.resolveMaxTokens(500000, nil, nil); got != 393216 {
		t.Fatalf("请求级参数超上限也应钳制，got %d", got)
	}
	if got := p.resolveMaxTokens(20000, nil, nil); got != 20000 {
		t.Fatalf("未超上限不应被改动，got %d", got)
	}
}

// TestChatRetriesWithClampedMaxTokens 端到端：服务端先以 400 拒绝超范围 max_tokens，
// 插件按报文上限重试并成功，且第二次请求携带的就是钳制后的值。
func TestChatRetriesWithClampedMaxTokens(t *testing.T) {
	var calls, lastMaxTokens int
	rejected := false // 只对首个请求返回 400，之后正常响应
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if mt, ok := req["max_tokens"].(float64); ok {
			lastMaxTokens = int(mt)
		}
		if !rejected {
			rejected = true
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid max_tokens value, the valid range of max_tokens is [1, 393216]","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	clientCfg := openai.DefaultConfig("test-key")
	clientCfg.BaseURL = srv.URL
	p := &OpenAIProvider{
		client:    openai.NewClientWithConfig(clientCfg),
		model:     "m",
		maxTokens: 1048576, // 模拟 context_window: 1048576 的宿主注入
		fileCache: map[string]string{},
	}

	resp, err := p.Chat(context.Background(), nil, nil, 0)
	if err != nil {
		t.Fatalf("重试后应成功，got err=%v", err)
	}
	if calls != 2 {
		t.Fatalf("应恰好重试一次，实际请求 %d 次", calls)
	}
	if lastMaxTokens != 393216 {
		t.Fatalf("重试请求应携带钳制后的 393216，got %d", lastMaxTokens)
	}
	if resp.Content != "ok" {
		t.Fatalf("响应内容异常：%q", resp.Content)
	}

	// 上限已学习：下一次请求直接携带钳制值，不再触发 400。
	calls = 0
	if _, err := p.Chat(context.Background(), nil, nil, 0); err != nil {
		t.Fatalf("第二次调用失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("上限已学习后应一次成功，实际请求 %d 次", calls)
	}
	if lastMaxTokens != 393216 {
		t.Fatalf("后续请求应直接携带 393216，got %d", lastMaxTokens)
	}
}

// TestNoMaxTokensAfterRangeLessRejection 只报 max_tokens 非法但无区间时，
// 此后一律不携带该字段。
func TestNoMaxTokensAfterRangeLessRejection(t *testing.T) {
	p := &OpenAIProvider{maxTokens: 1048576}
	p.learnMaxTokensLimit(0)
	if got := p.resolveMaxTokens(0, nil, nil); got != 0 {
		t.Fatalf("应改为不携带 max_tokens，got %d", got)
	}
	if got := p.resolveMaxTokens(500000, nil, nil); got != 0 {
		t.Fatalf("请求级参数也不得携带，got %d", got)
	}
}

// TestResolveMaxTokensFillsFromWindowWhenUnset 未配置时按「窗口 − 本帧输入」现算。
//
// 现场：anthropic 兼容口把 max_tokens 当 required，缺席时服务端自填 4096 —— 长回答被
// 腰斩；而按窗口值硬填又会让 prompt+max_tokens 越过 n_ctx 直接 400。所以要按帧算。
func TestResolveMaxTokensFillsFromWindowWhenUnset(t *testing.T) {
	t.Setenv("DSC_CONTEXT_WINDOW", "10000")
	p := &OpenAIProvider{model: "m"}                                          // maxTokens = 0（未配置）
	msgs := []dsc.Message{{Role: "user", Content: strings.Repeat("u", 8000)}} // ~2k token

	got := p.resolveMaxTokens(0, msgs, nil)
	if got <= 0 {
		t.Fatalf("未配置时必须按窗口余量补一个值，got %d", got)
	}
	if prompt := dsc.EstimateMessageValuesTokens(msgs); prompt+got > 10000 {
		t.Fatalf("prompt(%d) + max_tokens(%d) 超过窗口 10000", prompt, got)
	}

	// 显式 -1：一律不携带（尊重）
	pNeg := &OpenAIProvider{model: "m", maxTokens: -1}
	if got := pNeg.resolveMaxTokens(0, msgs, nil); got != 0 {
		t.Fatalf("显式 -1 应不携带，got %d", got)
	}

	// 插件级默认比余量小：用默认值（探测上限的意义）
	pSmall := &OpenAIProvider{model: "m", maxTokens: 1024}
	if got := pSmall.resolveMaxTokens(0, msgs, nil); got != 1024 {
		t.Fatalf("默认值 1024 小于余量时应用默认值，got %d", got)
	}
	// 插件级默认比余量大：被余量夹住
	pBig := &OpenAIProvider{model: "m", maxTokens: 999999}
	if got := pBig.resolveMaxTokens(0, msgs, nil); got >= 999999 {
		t.Fatalf("超过余量时必须夹住，got %d", got)
	}
}
