package core

import (
	"context"
	"errors"
	"sync"
	"testing"

	"dsc/proto"
)

// TestOverflowParse 从各家 provider 的错误文本里取（实际用量, 上限）。
// 解析不出来不是错误——退化为「按超出比例收紧」，只是要多一轮。
func TestOverflowParse(t *testing.T) {
	cases := []struct {
		msg         string
		actual, max int
	}{
		{"prompt is too long: 1075596 tokens > 1048576 maximum", 1075596, 1048576},
		{"This model's maximum context length is 8192 tokens. However, your messages resulted in 10000 tokens.", 10000, 8192},
		{"context window: 128000 tokens", 0, 128000},
		{"rate limit exceeded", 0, 0},
	}
	for _, c := range cases {
		actual, max := overflowParse(c.msg)
		if actual != c.actual || max != c.max {
			t.Fatalf("%q → (%d,%d)，期望 (%d,%d)", c.msg, actual, max, c.actual, c.max)
		}
	}
	// 千分位
	if actual, max := overflowParse("prompt is too long: 1,075,596 tokens > 1,048,576 maximum"); actual != 1075596 || max != 1048576 {
		t.Fatalf("千分位解析失败: %d / %d", actual, max)
	}
}

// TestProviderOverflowSlidesWindowInsteadOfEmergencyCompress provider 报超窗时，宿主
// 自己收紧预算并在重试时滑窗——**不**再把超窗派给压缩插件做有损的紧急压缩。
//
// 这是用户提出的架构修正：紧急压缩产出的是没有内容的占位摘要，一次就把几百条真实消息
// 永久埋掉；滑窗只影响这一次请求，会话日志一字不动。
//
// 场景取真实形态：我们的估算认为装得下（15K < 20K 窗口），provider 实际算了 30K。
// 错误文本里带着这两个数，宿主据此把偏差记成 2.0，重试时裁到 0.9*窗口/2 = 9K。
func TestProviderOverflowSlidesWindowInsteadOfEmergencyCompress(t *testing.T) {
	m := newRouterManager()
	m.contextWindow = 20_000
	p := &overflowProvider{errMsg: "prompt is too long: 30000 tokens > 20000 maximum"}
	m.llms["p"] = p
	m.llmOrder = []string{"p"}
	m.agentLLMName = "p"

	// 一个声明 compaction 的插件：它只该收到 pre-step，绝不该收到 request-error
	backend := &fakeHook{events: make(chan *proto.OnEventRequest, 16)}
	m.mu.Lock()
	m.toolHookClients["compactor"] = backend
	m.toolHookOrder = []string{"compactor"}
	m.coreMetadata["compactor"] = declaresCompaction()
	m.compactionBackend = "compactor"
	m.mu.Unlock()

	msgs := []*proto.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 60; i++ { // ≈13.5K，我们的估算认为装得下 20K 窗口
		msgs = append(msgs, mkTurn(i, "abcdefghij")...)
	}
	// 夹具必须「我们算起来装得下」，否则第一趟就被收口裁掉，走不到 4xx 这条路
	if est := EstimateProtoMessagesTokens(msgs); est > m.contextWindow {
		t.Fatalf("夹具过大：估算 %d 已超窗口 %d，走不到 provider 超窗路径", est, m.contextWindow)
	}

	srv := &llmAggregateServer{m: m}
	if _, err := srv.Chat(context.Background(), &proto.ChatRequest{SessionId: "s", Messages: msgs}); err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if p.calls != 2 {
		t.Fatalf("provider 调用次数 = %d，期望 2（一次被拒 + 一次重试）", p.calls)
	}

	// 重试发出去的那份必须显著变小：偏差 2.0 → 目标 9K
	sent := p.lastMessages
	ptrs := make([]*Message, len(sent))
	for i := range sent {
		ptrs[i] = &sent[i]
	}
	if est := EstimateMessagesTokens(ptrs); est > 9_000 {
		t.Fatalf("重试仍发出 %d tokens（预算应≈9000），滑窗没生效", est)
	}
	// 裁剪不得留下孤儿工具结果
	calls := map[string]bool{}
	for _, msg := range sent {
		if msg.ToolCallID != "" && !calls[msg.ToolCallID] {
			t.Fatalf("出现孤立 tool 结果: %s", msg.ToolCallID)
		}
		for _, tc := range msg.ToolCalls {
			calls[tc.ID] = true
		}
	}

	// 插件侧：只收到 pre-step，零 request-error
	for {
		select {
		case req := <-backend.events:
			if req.GetName() == string(EventAgentRequestError) {
				t.Fatal("超窗不应再派给压缩插件做紧急压缩（有损），应由宿主滑窗")
			}
			continue
		default:
		}
		break
	}

	// provider 报的上限小于配置窗口时，以它为准（不必每次撞墙）
	if got := m.effectiveWindow("s"); got != 20_000 {
		t.Fatalf("effectiveWindow = %d", got)
	}
	adj, ok := m.overflowFor("s")
	if !ok || adj.inflation < 1.9 {
		t.Fatalf("应记录实测偏差 ≈2.0，得到 %+v", adj)
	}
}

// TestOverflowAdjustmentKeepsStricterValue 连续超窗时取更严的：偏差取大、上限取小。
// 后一次较宽松的报数不该把前一次学到的教训抹掉。
func TestOverflowAdjustmentKeepsStricterValue(t *testing.T) {
	m := newRouterManager()
	m.contextWindow = 100_000
	m.noteOverflow("s", 300_000, 0, 100_000) // 偏差 3.0
	m.noteOverflow("s", 120_000, 0, 100_000) // 偏差 1.2，不该覆盖
	adj, _ := m.overflowFor("s")
	if adj.inflation < 2.9 {
		t.Fatalf("应保留更严的偏差 3.0，得到 %v", adj.inflation)
	}

	// 上限取小
	m.noteOverflow("t", 100_000, 90_000, 80_000)
	m.noteOverflow("t", 100_000, 95_000, 80_000)
	adj, _ = m.overflowFor("t")
	if adj.maxTokens != 90_000 {
		t.Fatalf("应保留更小的上限 90000，得到 %d", adj.maxTokens)
	}

	// 成功后清掉：一次异常不该让会话永远按放大系数裁
	m.clearOverflow("s")
	if _, ok := m.overflowFor("s"); ok {
		t.Fatal("成功后应清掉收紧记录")
	}
}

// TestOverflowAdjustmentClamped 偏差有上下限：只放大、且不能大到把预算压没。
func TestOverflowAdjustmentClamped(t *testing.T) {
	m := newRouterManager()
	m.contextWindow = 1000
	m.noteOverflow("s", 1, 0, 1000) // provider 报得比我们少 → 不缩小
	adj, _ := m.overflowFor("s")
	if adj.inflation != 1 {
		t.Fatalf("偏差不应小于 1，得到 %v", adj.inflation)
	}
	m.noteOverflow("t", 10_000_000, 0, 1) // 极端值
	adj, _ = m.overflowFor("t")
	if adj.inflation > overflowInflationMax {
		t.Fatalf("偏差应夹在 %v，得到 %v", overflowInflationMax, adj.inflation)
	}
}

// overflowProvider 首次用「超窗」错误拒绝，之后成功；记录实际发出的消息。
type overflowProvider struct {
	mu           sync.Mutex
	calls        int
	lastMessages []Message
	errMsg       string
}

func (p *overflowProvider) Chat(_ context.Context, msgs []Message, _ []Tool, _ int) (*ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.lastMessages = append([]Message(nil), msgs...)
	if p.calls == 1 {
		return nil, errors.New(p.errMsg)
	}
	return &ChatResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *overflowProvider) ChatStream(context.Context, []Message, []Tool, int) (<-chan *ChatStreamResponse, error) {
	return nil, errors.New("streaming not used in this test")
}

func (p *overflowProvider) Name(context.Context) string       { return "overflow-provider" }
func (p *overflowProvider) Version(context.Context) string    { return "test" }
func (p *overflowProvider) HealthCheck(context.Context) error { return nil }
func (p *overflowProvider) VisionEnabled() bool               { return false }

// TestNoteOverflowLowersContextWindow provider 亲口报了更小的上限时，把进程级窗口
// 一起降下来：配置可以写得比服务端容量大，那条假设不成立后，标题栏、压力估算、
// 裁窗比例都会跟着错。只降不升——报得比配置大时保留配置（可能是有意调低省成本）。
func TestNoteOverflowLowersContextWindow(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	m.SetContextWindow(1310720) // 用户「128K 后面补个 0」

	m.noteOverflow("sess-over", 1075596, 1048576, 900000)
	if got := m.ContextWindow(); got != 1048576 {
		t.Fatalf("应降到 provider 报的上限，got %d", got)
	}
	// 后续报一个更大的上限，不得把窗口抬回去
	m.noteOverflow("sess-over", 0, 2000000, 900000)
	if got := m.ContextWindow(); got != 1048576 {
		t.Fatalf("只降不升，got %d", got)
	}
}
