package main

import (
	"context"
	dsc "dsc-sdk"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dsc/proto"
	"dsc/session"
	"google.golang.org/grpc"
)

// 本文件覆盖「历史总量超出上下文窗口」的兜底：从 1M 云端切回 128K 本地这类场景。
//
// 修复前的真实行为（探针实测）：内联压缩把整段历史交给同一个模型做摘要，压缩请求自身
// 就超窗 → `compact history failed: context length exceeded: 801207 > 131072`，整轮失败，
// 既没压缩也没发主请求。
//
// 修复后：先把派生历史裁进窗口（只动本次请求，不动会话日志），再让压缩处理窗口内的部分。
// 不变量：**发出去的请求绝不超过窗口**；单轮自身超窗则明确报错而非盲发。

// windowEnforcingLLM 像真实 provider 那样在输入超窗时拒绝（Chat 与 ChatStream 都拒），
// 从而证明「最终发出的请求」确实在窗口内，而不只是「我们认为它在窗口内」。
type windowEnforcingLLM struct {
	compactMockLLM

	window int

	mu         sync.Mutex
	chatInputs []int
	streamIn   []int
	streamMsgs []*proto.Message
}

func (m *windowEnforcingLLM) Chat(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (*proto.ChatResponse, error) {
	n := dsc.EstimateProtoMessagesTokens(in.Messages)
	m.mu.Lock()
	m.chatInputs = append(m.chatInputs, n)
	m.mu.Unlock()
	if n > m.window {
		return nil, fmt.Errorf("context length exceeded: %d > %d", n, m.window)
	}
	return &proto.ChatResponse{Content: "【压缩摘要】此前对话的要点。"}, nil
}

func (m *windowEnforcingLLM) ChatStream(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[proto.ChatStreamResponse], error) {
	n := dsc.EstimateProtoMessagesTokens(in.Messages)
	m.mu.Lock()
	m.streamIn = append(m.streamIn, n)
	m.streamMsgs = in.Messages
	m.mu.Unlock()
	if n > m.window {
		return nil, fmt.Errorf("context length exceeded: %d > %d", n, m.window)
	}
	return &compactMockStream{}, nil
}

// buildOversizeSession 构造 rounds 轮、每条约 perMsgTokens 估算 token 的历史（含轮次标记，
// 便于断言「会话全量历史仍在」）。总量取 rounds*2*perMsg，远大于测试用的窗口。
func buildOversizeSession(t *testing.T, a *ReactLoopAgent, rounds, perMsgTokens int) {
	t.Helper()
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	// ASCII 约 4 字节/token（dsc.EstimateTextTokens 的口径），故按 4 倍字节数构造 perMsgTokens 规模
	body := strings.Repeat("x", perMsgTokens*4)
	for i := 0; i < rounds; i++ {
		mark := fmt.Sprintf("#round-%d", i)
		a.sess.Append(session.UserMessage, &session.UserMessageData{Content: mark + body, Source: "user"},
			&session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.AssistantMessage, &session.AssistantMessageData{Content: mark + body},
			&session.SurfaceOp{Op: session.SurfaceAppend})
	}
	markSessionRestored(t, a)
}

// TestOversizeHistoryTrimmedToWindow 历史远超窗口时：不报错、照常请求、且发出去的请求
// 一定在窗口内；会话日志不删除任何记录（切回大窗口模型时全量历史可原样恢复）。
func TestOversizeHistoryTrimmedToWindow(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	buildOversizeSession(t, a, 10, 10000) // 20 条 × 10k ≈ 200K 估算 = 4 倍窗口

	before := dsc.EstimateProtoMessagesTokens(a.sess.DeriveMessages())
	if before <= a.contextWindow*2 {
		t.Fatalf("夹具不够大：估算 %d，应至少为窗口两倍", before)
	}

	llm := &windowEnforcingLLM{window: a.contextWindow}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	var mu sync.Mutex
	var frames []*dsc.RunStreamResponse
	res, err := a.runLoop(context.Background(), "继续", nil, func(f *dsc.RunStreamResponse) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("超窗历史不应让整轮失败（修复前报 compact history failed）: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("结果状态 = %s, 期望 success", res.Status)
	}

	llm.mu.Lock()
	streamIn := append([]int(nil), llm.streamIn...)
	streamMsgs := llm.streamMsgs
	chatInputs := append([]int(nil), llm.chatInputs...)
	llm.mu.Unlock()

	// 1) 主请求发出且不超窗（provider 侧同样校验，超窗会直接报错）
	if len(streamIn) == 0 {
		t.Fatal("主请求未发出")
	}
	if streamIn[0] > a.contextWindow {
		t.Fatalf("主请求 %d tokens 超过窗口 %d（绝不允许超过）", streamIn[0], a.contextWindow)
	}
	for _, n := range chatInputs {
		if n > a.contextWindow {
			t.Fatalf("压缩请求 %d tokens 超过窗口 %d", n, a.contextWindow)
		}
	}

	// 2) 确实裁掉了旧记录（而不是把 200K 硬发出去）
	if got := dsc.EstimateProtoMessagesTokens(streamMsgs); got >= before/2 {
		t.Fatalf("主请求估算 = %d, 期望远小于全量 %d（应裁掉最旧的部分）", got, before)
	}

	// 3) 会话全量历史仍在（裁剪只作用于本次请求，不写回、不删除）
	after := dsc.EstimateProtoMessagesTokens(a.sess.DeriveMessages())
	if after < before {
		t.Fatalf("会话历史被裁剪/删除：%d -> %d（裁剪不得写回会话）", before, after)
	}
	full := a.sess.DeriveMessages()
	if len(full) < 3 || !strings.Contains(full[1].Content, "#round-0") {
		t.Fatal("最早一轮的历史应从会话中仍然可派生（切回大窗口时应原样恢复）")
	}

	// 4) 模型被明确告知历史被省略（否则它以为自己看到了全部）
	if len(streamMsgs) == 0 || streamMsgs[0].Role != "system" ||
		!strings.Contains(streamMsgs[0].Content, "已省略") {
		t.Fatalf("system 应说明历史被省略，实际首条 = %+v", streamMsgs[0])
	}

	// 5) 用户可见提示
	mu.Lock()
	notified := false
	for _, f := range frames {
		if strings.Contains(f.Output, "上下文裁剪") {
			notified = true
			break
		}
	}
	mu.Unlock()
	if !notified {
		t.Fatal("未输出上下文裁剪提示帧")
	}
}

// TestOversizeTrimKeepsUserBoundary 裁剪起点必须落在 user 消息上：否则 tool_use 与
// tool_result 会被切断（provider 直接拒），且 Anthropic 要求首条非 system 是 user。
func TestOversizeTrimKeepsUserBoundary(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	body := strings.Repeat("x", 10000*4) // ≈ 10000 tokens
	for i := 0; i < 10; i++ {
		callID := fmt.Sprintf("call-%d", i)
		a.sess.Append(session.UserMessage, &session.UserMessageData{Content: body, Source: "user"},
			&session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.AssistantMessage, &session.AssistantMessageData{
			ToolCalls: []*proto.ToolCall{{Id: callID, Name: "read", ArgumentsJson: "{}"}},
		}, &session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.ToolResult, &session.ToolResultData{CallID: callID, Content: body},
			&session.SurfaceOp{Op: session.SurfaceAppend})
	}
	markSessionRestored(t, a)

	full := a.sess.DeriveMessages()
	msgs, dropped := fitMessagesToWindow(full, a.contextWindow, dsc.EstimateProtoMessagesTokens(full))
	if dropped == 0 {
		t.Fatalf("应发生裁剪（总估算 %d, 预算 %d）",
			dsc.EstimateProtoMessagesTokens(a.sess.DeriveMessages()), int(float64(a.contextWindow)*oversizeTrimRatio))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("首条应为 system，实际 %q", msgs[0].Role)
	}
	if msgs[1].Role != "user" {
		t.Fatalf("裁剪后首条非 system 消息应为 user，实际 %q", msgs[1].Role)
	}
	// 每个 tool 消息都必须有前置的 assistant tool_call（配对完整）
	known := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			known[tc.Id] = true
		}
		if m.Role == "tool" && !known[m.ToolCallId] {
			t.Fatalf("裁剪后出现孤立 tool_result（call_id=%s），配对被切断", m.ToolCallId)
		}
	}
}

// TestSingleTurnOverflowFoldsAndContinues 单轮自身就超窗（当前输入一次 60k token 到
// 50k 窗口）：历史怎么丢都救不回来，但把这条过长的消息折叠（保留首尾、省略中间）之后
// 请求照发——会话继续走下去，而不是整轮失败。
func TestSingleTurnOverflowFoldsAndContinues(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	a.sess.Append(session.UserMessage, &session.UserMessageData{Content: "先前的短消息", Source: "user"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	markSessionRestored(t, a)

	llm := &windowEnforcingLLM{window: a.contextWindow}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	_, err := a.runLoop(context.Background(), strings.Repeat("x", 60*1000*4), nil, func(f *dsc.RunStreamResponse) {})
	if err != nil {
		t.Fatalf("折叠后应能继续（原先是整轮报错），实际: %v", err)
	}
	llm.mu.Lock()
	streamCalls := len(llm.streamIn)
	folded := false
	for _, m := range llm.streamMsgs {
		if strings.Contains(m.GetContent(), "已折叠") {
			folded = true
		}
	}
	llm.mu.Unlock()
	if streamCalls == 0 {
		t.Fatal("应发出请求（折叠后装进窗口）")
	}
	if !folded {
		t.Fatal("过长的那条消息应被折叠（保留首尾）后再发出")
	}
}

// TestFoldOversizeMessagesHasFloor 折叠有下限：过短的消息不值得折（超窗不来自消息正文
// 时反复折叠只会破坏上下文），且折叠绝不能丢掉 tool_call_id——那是结果配对的凭据。
func TestFoldOversizeMessagesHasFloor(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 200
	msgs := []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "短"},
		{Role: "tool", Content: "结果", ToolCallId: "c1"},
	}
	out, n := a.foldOversizeMessages(msgs, nil)
	if n != 0 {
		t.Fatalf("过短消息不应折叠，got %d", n)
	}
	if len(out) != len(msgs) {
		t.Fatalf("条数不得变化: %d", len(out))
	}
	if out[2].GetToolCallId() != "c1" {
		t.Fatalf("折叠不得丢失 tool_call_id: %q", out[2].GetToolCallId())
	}
}

// TestNoTrimForAsciiHistoryWithinWindow 纯 ASCII/代码会话不得被误判超窗。
//
// 现场：1M 窗口、服务端实际 332K（32%）的代码会话，被当时的估算算成 1.1M（高估 5.3 倍），
// 于是每一轮都触发裁剪并打印提示。根治是 dsc.EstimateTextTokens 改为按字符分类累加
// （ASCII 4 字节/token），此处用同一口径构造「真实占窗口 40%」的历史，断言不裁剪、不提示。
func TestNoTrimForAsciiHistoryWithinWindow(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	// 10 条 × 2000 tokens ≈ 20000 tokens ≈ 窗口 40%（ASCII 4 字节/token）
	body := strings.Repeat("x", 2000*4)
	for i := 0; i < 5; i++ {
		a.sess.Append(session.UserMessage, &session.UserMessageData{Content: body, Source: "user"},
			&session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.AssistantMessage, &session.AssistantMessageData{Content: body},
			&session.SurfaceOp{Op: session.SurfaceAppend})
	}
	markSessionRestored(t, a)

	est := dsc.EstimateProtoMessagesTokens(a.sess.DeriveMessages())
	if est < a.contextWindow*35/100 || est > a.contextWindow*45/100 {
		t.Fatalf("夹具应约占窗口 40%%，实际估算 %d（窗口 %d）——估算口径又漂了？", est, a.contextWindow)
	}

	llm := &windowEnforcingLLM{window: a.contextWindow}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}
	var mu sync.Mutex
	var trimmed bool
	res, err := a.runLoop(context.Background(), "继续", nil, func(f *dsc.RunStreamResponse) {
		if strings.Contains(f.Output, "上下文裁剪") {
			mu.Lock()
			trimmed = true
			mu.Unlock()
		}
	})
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("状态 = %s", res.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	if trimmed {
		t.Fatal("历史只占窗口 40%，不应触发裁剪（估算被高估才会这样）")
	}
	// 全量历史都应发出去（11 条历史/当前消息 + system）
	if got := len(llm.streamMsgs); got < 11 {
		t.Fatalf("主请求消息数 = %d, 期望全量（不应丢历史）", got)
	}
}

// TestTrimNoticePrintedOncePerDropCount 真超窗时，同一省略条数只提示一次。
//
// 现场表现：每轮都打印「已省略最早 93 条消息」，把消息区刷满。真超窗时每轮裁掉的是
// 同一段，重复提示没有新信息；条数变化时才应再提示。
func TestTrimNoticePrintedOncePerDropCount(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	buildOversizeSession(t, a, 10, 10000) // 估算 200K ≈ 4 倍窗口

	llm := &windowEnforcingLLM{window: a.contextWindow}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	countTrimFrames := func() int {
		var mu sync.Mutex
		n := 0
		_, err := a.runLoop(context.Background(), "继续", nil, func(f *dsc.RunStreamResponse) {
			if strings.Contains(f.Output, "上下文裁剪") {
				mu.Lock()
				n++
				mu.Unlock()
			}
		})
		if err != nil {
			t.Fatalf("runLoop: %v", err)
		}
		return n
	}

	if got := countTrimFrames(); got != 1 {
		t.Fatalf("首次裁剪应提示 1 次，实际 %d 次", got)
	}
	// 第二轮：历史结构不变（只是多了当前用户消息与助手回复），省略条数应相同 → 不再提示
	if got := countTrimFrames(); got != 0 {
		t.Fatalf("同一省略条数不应重复提示，第二轮实际提示 %d 次", got)
	}
}

// TestSwitchSessionResetsUsage 切换会话必须清掉上一会话的用量与裁剪状态。
//
// 此前 SwitchSession 只换 sess 与 turnCounter，lastPromptTokens 留着上一会话的值。
// 一旦上个会话占满 1M 窗口，切到新会话后「已用容量」仍显示旧值，新会话的空历史会被
// 判定超窗，触发裁剪/压缩（并把提示打进新会话）。
func TestSwitchSessionResetsUsage(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 1000000
	a.llmServiceID, a.toolServiceID = 1, 1

	// 模拟上一会话已用到 900K，且上一轮做过裁剪
	a.usageMu.Lock()
	a.lastPromptTokens = 900000
	a.lastTrimDropped = 93
	a.usageMu.Unlock()

	if err := a.SwitchSession(context.Background(), "another-session"); err != nil {
		t.Fatalf("SwitchSession: %v", err)
	}

	a.usageMu.Lock()
	gotTokens, gotDropped := a.lastPromptTokens, a.lastTrimDropped
	a.usageMu.Unlock()
	if gotTokens != 0 {
		t.Fatalf("切会话后 lastPromptTokens 应清零，实际 %d", gotTokens)
	}
	if gotDropped != 0 {
		t.Fatalf("切会话后裁剪提示状态应清零，实际 %d", gotDropped)
	}
}

// usageReportingLLM 流里回报固定 prompt 用量，用于验证 usage 帧携带上下文投影。
type usageReportingLLM struct {
	compactMockLLM
	promptTokens int
}

func (m *usageReportingLLM) ChatStream(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[proto.ChatStreamResponse], error) {
	// 回报的用量取本次请求的真实估算（真实 provider 也是这样：prompt_tokens 反映实际发送量），
	// 否则「实测 30000 vs 投影 1300」只是 mock 在撒谎，测不出任何东西。
	est := int32(dsc.EstimateProtoMessagesTokens(in.Messages))
	m.mu.Lock()
	m.streamMsgs = in.Messages
	m.mu.Unlock()
	return &compactMockStream{usage: &proto.Usage{
		PromptTokens: est,
		TotalTokens:  est + 10,
	}}, nil
}

// TestUsageFramesCarryContextTokens 展示口径与决策口径必须同源：
// agent 随 usage 帧上报 core.TokenMeter 的投影值（Usage.ContextTokens），
// 宿主据此显示「已用」，不再自己按 prompt_tokens 另算一套。
func TestUsageFramesCarryContextTokens(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 200000
	a.llmServiceID, a.toolServiceID = 1, 1
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	a.sess.Append(session.UserMessage, &session.UserMessageData{Content: strings.Repeat("x", 4000), Source: "user"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	markSessionRestored(t, a)

	llm := &usageReportingLLM{promptTokens: 30000}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	var frames []*dsc.RunStreamResponse
	if _, err := a.runLoop(context.Background(), "继续", nil, func(f *dsc.RunStreamResponse) {
		frames = append(frames, f)
	}); err != nil {
		t.Fatalf("runLoop: %v", err)
	}

	sawUsage := false
	for _, f := range frames {
		if f.Usage == nil {
			continue
		}
		sawUsage = true
		if f.Usage.ContextTokens <= 0 {
			t.Fatalf("usage 帧应携带 ContextTokens（上下文投影），实际 %+v", f.Usage)
		}
		// 正常回合里投影应贴近实测 prompt（同一份上下文的两种量法，差一行回复的量级）
		if diff := int(f.Usage.ContextTokens) - int(f.Usage.PromptTokens); diff > 2000 || diff < -2000 {
			t.Fatalf("ContextTokens=%d 与实测 PromptTokens=%d 偏差过大（%+d）",
				f.Usage.ContextTokens, f.Usage.PromptTokens, diff)
		}
	}
	if !sawUsage {
		t.Fatal("未发出任何带 usage 的帧")
	}
}

// TestTrimToWindowClosesLoopWhenProjectionFarAboveEstimate 投影值远高于消息估算时，
// 裁剪必须收敛到判定口径，而不是「裁一次就判超窗」。
//
// 修复前的真实行为：裁剪按消息估算算预算，超窗判定却读投影值（实测锚点 + 差量）。
// 两者差两三倍时会出现「已省略最早 6384 条消息」之后仍然报 441195 > 131072——消息区
// 看着像裁剪没执行，其实是裁完仍不满足判定口径。闭环收紧让两边收敛到同一个数。
func TestTrimToWindowClosesLoopWhenProjectionFarAboveEstimate(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 10000
	a.meter = dsc.NewTokenMeter(10000)
	// 锚点仍在窗口内（不触发「锚点超窗」降级），但 provider 实测是启发式估算的 3 倍
	a.meter.SampleUsage(9000, 3000)

	msgs := []*proto.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 30; i++ {
		msgs = append(msgs, &proto.Message{Role: "user", Content: strings.Repeat("内容", 100)})
	}
	projected := a.projectedFit(msgs, nil)
	if projected <= a.contextWindow {
		t.Fatalf("夹具无效：裁前应超窗，got %d", projected)
	}

	out, dropped := a.trimToWindow(msgs, nil, projected)
	if dropped == 0 {
		t.Fatal("应发生裁剪")
	}
	if fit := a.projectedFit(out, nil); fit > a.contextWindow {
		t.Fatalf("闭环裁剪后仍超窗 %d > %d（说明只裁了一次就判超窗）", fit, a.contextWindow)
	}
	if len(out) == 0 || out[0].Role != "system" {
		t.Fatalf("system 消息不得被裁掉，got %d 条，首条 %v", len(out), out)
	}
}
