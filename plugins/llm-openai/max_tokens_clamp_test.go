package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

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
	if got := p.resolveMaxTokens(0); got != 1048576 {
		t.Fatalf("未知上限时按原值携带，got %d", got)
	}

	p.learnMaxTokensLimit(393216)
	if got := p.resolveMaxTokens(0); got != 393216 {
		t.Fatalf("已知上限时应钳制到 393216，got %d", got)
	}
	// 请求级参数（压缩净余值）同样受钳制。
	if got := p.resolveMaxTokens(500000); got != 393216 {
		t.Fatalf("请求级参数超上限也应钳制，got %d", got)
	}
	if got := p.resolveMaxTokens(20000); got != 20000 {
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
	if got := p.resolveMaxTokens(0); got != 0 {
		t.Fatalf("应改为不携带 max_tokens，got %d", got)
	}
	if got := p.resolveMaxTokens(500000); got != 0 {
		t.Fatalf("请求级参数也不得携带，got %d", got)
	}
}

// TestResolveMaxTokensThreeStates 插件只按三态把宿主给的值发出去，不自己算窗口余量。
//
// 语义：>0 = 宿主算好的本帧预算（直接发，再夹服务端认过的上限）；0 = 宿主没给（用户
// 在 env 里显式配置了），用插件默认；<0 = 明确不携带。窗口余量的计算在宿主
// （core.outputBudgetFor）——插件各算一套会让同一份窗口策略散落在 N 个 provider 里。
func TestResolveMaxTokensThreeStates(t *testing.T) {
	p := &OpenAIProvider{model: "m", maxTokens: 8192}

	if got := p.resolveMaxTokens(4096); got != 4096 {
		t.Fatalf("宿主给了预算就照发，got %d", got)
	}
	if got := p.resolveMaxTokens(0); got != 8192 {
		t.Fatalf("宿主没给时用插件默认值，got %d", got)
	}
	if got := p.resolveMaxTokens(-1); got != -1 {
		t.Fatalf("-1 = 无限，必须原样携带，got %d", got)
	}

	// 显式 0：禁用该字段（不携带）
	pZero := &OpenAIProvider{model: "m", maxTokens: 0}
	if got := pZero.resolveMaxTokens(0); got != 0 {
		t.Fatalf("0 = 禁用（不携带），got %d", got)
	}

	// 显式 -1：无限（尊重）
	pNeg := &OpenAIProvider{model: "m", maxTokens: -1}
	if got := pNeg.resolveMaxTokens(0); got != -1 {
		t.Fatalf("显式 -1 应原样携带（无限），got %d", got)
	}

	// 服务端学习到的上限优先于任何来源
	pLimit := &OpenAIProvider{model: "m", maxTokens: 999999}
	pLimit.maxTokensLimit.Store(int64(8192))
	if got := pLimit.resolveMaxTokens(0); got != 8192 {
		t.Fatalf("应被服务端上限夹住，got %d", got)
	}

	// 服务端报过「不接受该字段」后一律不携带
	pNo := &OpenAIProvider{model: "m", maxTokens: 4096}
	pNo.noMaxTokens.Store(true)
	if got := pNo.resolveMaxTokens(4096); got != 0 {
		t.Fatalf("noMaxTokens 学习位应压过一切，got %d", got)
	}
}

// TestMaxTokensWireEncoding 三态在请求体上的字节形态：-1 必须原样上送（无限），
// 0 必须不出现该字段（禁用）。
//
// 依赖 go-openai 的字段声明 `MaxTokens int \`json:"max_tokens,omitempty"\“：0 是零值被
// 省略，-1 不是零值因此会序列化。这条测试就是钉住这个假设——换成指针类型或去掉
// omitempty 都会让「0 禁用」或「-1 无限」静默失效。
func TestMaxTokensWireEncoding(t *testing.T) {
	for _, c := range []struct {
		name string
		mt   int
		want string // "" = 不应出现
	}{
		{"禁用（0）", 0, ""},
		{"无限（-1）", -1, "-1"},
		{"指定（8192）", 8192, "8192"},
	} {
		t.Run(c.name, func(t *testing.T) {
			b, err := json.Marshal(openai.ChatCompletionRequest{Model: "m", MaxTokens: c.mt})
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			raw, ok := m["max_tokens"]
			if c.want == "" {
				if ok {
					t.Fatalf("0 不该出现在请求体，实际 %s", raw)
				}
				return
			}
			if !ok {
				t.Fatalf("缺 max_tokens 字段（期望 %s）", c.want)
			}
			if string(raw) != c.want {
				t.Fatalf("max_tokens = %s, want %s", raw, c.want)
			}
		})
	}
}
