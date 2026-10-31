package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"dsc/core"
)

// TestProbeContextWindowFromModels 标准 OpenAI 兼容端点：`/v1/models` 的 `meta.n_ctx`
// 是 llama.cpp 家族给出的**运行时**容量，最权威。
//
// 现场背景：本地服务实际开在 256K，DSC 却按 128K 跑（用户从界面上看出「容量只有一半」）。
func TestProbeContextWindowFromModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"local","meta":{"n_ctx":262144},"object":"model"}]}`)
	}))
	defer srv.Close()

	if got := probeContextWindow(srv.URL + "/v1"); got != 262144 {
		t.Fatalf("probe = %d, want 262144", got)
	}
}

// TestProbeContextWindowFallsBackToProps `/v1/models` 不吐 meta 时，llama.cpp / Strata 的
// `/props` 里 `default_generation_settings.n_ctx` 仍然写着运行时容量——只探 models 的
// 实现会在这里一无所获，然后退回 128K。
func TestProbeContextWindowFallsBackToProps(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models", "/models":
			fmt.Fprint(w, `{"object":"list","data":[{"id":"local"}]}`)
		case "/props":
			fmt.Fprint(w, `{"build_info":"x","default_generation_settings":{"n_ctx":262144},"total_slots":1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if got := probeContextWindow(srv.URL); got != 262144 {
		t.Fatalf("probe = %d, want 262144（应回退到 /props）", got)
	}
}

// TestProbeContextWindowTakesMaxAcrossModels 网关常把别名/小模型一起列出来，
// 取 data[0] 会拿到错的容量，必须取最大。
func TestProbeContextWindowTakesMaxAcrossModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"alias","meta":{"n_ctx":32768}},{"id":"big","meta":{"n_ctx":262144}}]}`)
	}))
	defer srv.Close()

	if got := probeContextWindow(srv.URL + "/v1"); got != 262144 {
		t.Fatalf("probe = %d, want 262144（多个条目取最大）", got)
	}
}

// TestProbeContextWindowRetriesLocalEndpoint 本地服务在加载模型 / slot 占满时会先回 503，
// 一次失败就退回 128K 会让整个进程按一半容量工作。本地端点必须重试到成功。
func TestProbeContextWindowRetriesLocalEndpoint(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if atomic.AddInt32(&hits, 1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable) // 模型还在加载
			return
		}
		fmt.Fprint(w, `{"data":[{"id":"local","meta":{"n_ctx":262144}}]}`)
	}))
	defer srv.Close()

	got, attempts := probeContextWindowWithRetry(srv.URL)
	if got != 262144 {
		t.Fatalf("probe = %d, want 262144（重试后应拿到容量）", got)
	}
	if attempts < 2 {
		t.Fatalf("attempts = %d，本地端点失败后必须重试", attempts)
	}
}

// TestProbeContextWindowNoRetryForCloud 云端探测失败就是「没有这项信息」，不要白等。
func TestProbeContextWindowNoRetryForCloud(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	// httptest 监听在 127.0.0.1，但这里要验证「域名端点不重试」，所以直接调单次探测。
	if got := probeContextWindow(srv.URL); got != 0 {
		t.Fatalf("probe = %d, want 0", got)
	}
	if n := atomic.LoadInt32(&hits); n > 5 {
		t.Fatalf("单次探测应最多试 4 条路径，实际 %d 次请求", n)
	}
}

// TestLooksLikeLocalEndpoint 决定「探测失败要不要重试」：本机/内网重试，域名不重试。
func TestLooksLikeLocalEndpoint(t *testing.T) {
	cases := []struct {
		base string
		want bool
	}{
		{"http://127.0.0.1:8008/v1", true},
		{"http://localhost:8008", true},
		{"http://192.168.124.6:8008", true},
		{"http://10.1.2.3:8080/v1", true},
		{"http://[::1]:8008", true},
		{"https://api.deepseek.com/v1", false},
		{"https://example.local", true},
		{"not a url", true}, // 解析不了：按本地处理（宁可多等两秒也别少算容量）
	}
	for _, c := range cases {
		if got := looksLikeLocalEndpoint(c.base); got != c.want {
			t.Errorf("looksLikeLocalEndpoint(%q) = %v, want %v", c.base, got, c.want)
		}
	}
}

// TestContextProbePathsCoverEndpointShapes 三种端点形态都要覆盖到：
// OpenAI 兼容（/v1/models）、不带 /v1 的网关（/models）、llama.cpp（/props）。
func TestContextProbePathsCoverEndpointShapes(t *testing.T) {
	got := contextProbePaths("http://h:8008/v1/")
	want := []string{
		"http://h:8008/v1/props",     // 运行时属性优先
		"http://h:8008/v1/v1/props",  // base 已含 /v1 时是 404，无害
		"http://h:8008/v1/v1/models", // 同上
		"http://h:8008/v1/models",
	}
	if len(got) != len(want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("paths[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// 不带 /v1 的 base：props 之后紧跟两条模型表入口。
	got = contextProbePaths("http://h:8008")
	if got[0] != "http://h:8008/props" || got[1] != "http://h:8008/v1/props" {
		t.Fatalf("paths = %v", got)
	}
	if got[2] != "http://h:8008/v1/models" || got[3] != "http://h:8008/models" {
		t.Fatalf("模型表入口顺序不对: %v", got)
	}
}

// TestEndpointForLogDropsCredentials 日志里不能出现 base_url 的凭据/查询串。
func TestEndpointForLogDropsCredentials(t *testing.T) {
	got := endpointForLog("https://user:sk-secret@example.com:8443/v1?key=sk-2")
	if got != "https://example.com:8443" {
		t.Fatalf("endpointForLog = %q", got)
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
	if net.ParseIP("127.0.0.1") == nil {
		t.Fatal("sanity: net.ParseIP")
	}
}

// TestProbeMaxOutputBudget 三档结论：服务端接受窗口值 / 报文里给了上限 / 都不给就退 1/4。
func TestProbeMaxOutputBudget(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		want    int
		wantSrc string
	}{
		{"接受窗口值", 200, "", 262144, "accepted"},
		{"报文给区间（DeepSeek 写法）", 400,
			`{"error":{"message":"Invalid max_tokens value, the valid range of max_tokens is [1, 393216]"}}`,
			393216, "server-range"},
		{"报文给上限（网关写法）", 400,
			`{"error":"max_tokens must be less than or equal to 8192"}`, 8192, "server-range"},
		{"报文没给数字：退 1/4", 400, `{"error":"max_tokens is invalid"}`, 65536, "quarter-fallback"},
		{"没权限", 401, `{"error":"unauthorized"}`, 0, "unauthorized"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" {
					http.NotFound(w, r)
					return
				}
				// 探测必须带 max_tokens=n_ctx 且是流式（拿到状态码就断开）
				var req struct {
					MaxTokens int  `json:"max_tokens"`
					Stream    bool `json:"stream"`
				}
				body, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(body, &req); err != nil {
					t.Errorf("探测请求体不是 JSON: %v", err)
				}
				if req.MaxTokens != 262144 {
					t.Errorf("探测应先用窗口值当 max_tokens，实际 %d", req.MaxTokens)
				}
				if !req.Stream {
					t.Error("探测必须用流式：只要状态码，不要生成内容")
				}
				if c.status != 200 {
					w.WriteHeader(c.status)
					fmt.Fprint(w, c.body)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()
			got, src := probeMaxOutputBudget(srv.URL+"/v1", "openai", "m", "k", 262144)
			if got != c.want || src != c.wantSrc {
				t.Fatalf("probeMaxOutputBudget = (%d,%q), want (%d,%q)", got, src, c.want, c.wantSrc)
			}
		})
	}
}

// TestProbeMaxOutputBudgetAnthropicUsesMessagesEndpoint Anthropic 兼容口走 /v1/messages
// （协议不同），探测不能拿 OpenAI 的 /chat/completions 去打。
func TestProbeMaxOutputBudgetAnthropicUsesMessagesEndpoint(t *testing.T) {
	var hit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		if r.Header.Get("anthropic-version") == "" {
			t.Error("Anthropic 探测必须带 anthropic-version")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	if got, _ := probeMaxOutputBudget(srv.URL+"/v1", "anthropic", "m", "k", 8192); got != 8192 {
		t.Fatalf("probe = %d", got)
	}
	if hit != "/v1/messages" {
		t.Fatalf("Anthropic 应打 /v1/messages，实际 %q", hit)
	}
}

// TestMaxTokensLimitFromError 报文解析的边界：别把 prompt 长度当成 max_tokens 上限。
func TestMaxTokensLimitFromError(t *testing.T) {
	cases := map[string]int{
		"Invalid max_tokens value, the valid range of max_tokens is [1, 393216]": 393216,
		"max_tokens must be less than or equal to 8192":                          8192,
		"max_tokens: -1 is invalid, maximum value is 65535":                      65535,
		"this model's maximum context length is 262144 tokens":                   0, // 说的是上下文，不是输出
		"something else entirely":                                                0,
	}
	for body, want := range cases {
		if got := maxTokensLimitFromError(body); got != want {
			t.Errorf("maxTokensLimitFromError(%q) = %d, want %d", body, got, want)
		}
	}
}

// TestActiveLLMFlavorAndExplicit 探测格式与「用户是否显式给了上限」的判据。
func TestActiveLLMFlavorAndExplicit(t *testing.T) {
	if got := activeLLMFlavor("anthropic"); got != "anthropic" {
		t.Fatalf("flavor = %q", got)
	}
	if got := activeLLMFlavor("openai"); got != "openai" {
		t.Fatalf("flavor = %q", got)
	}

	cfg := &core.Config{Plugins: []core.PluginEntry{
		{Name: "llm-openai", Type: "llm"},
		{Name: "tool-x", Type: "tool", Env: map[string]string{"DSC_MAX_OUTPUT_TOKENS": "1"}},
	}}
	if explicitMaxOutputTokens(cfg, "openai") {
		t.Fatal("非 LLM 插件上的同名键不算用户显式设置")
	}
	cfg.Plugins[0].Env = map[string]string{"DSC_OPENAI_MAX_OUTPUT_TOKENS": "8192"}
	if !explicitMaxOutputTokens(cfg, "openai") {
		t.Fatal("接口专属名应算显式设置")
	}
}
