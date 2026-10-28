package core

import (
	"context"
	"strings"
	"testing"

	"dsc/proto"
)

// mkTurn 造一条「user + assistant(tool_call) + tool」的完整往返，便于验证裁剪不会
// 留下孤儿工具结果。
func mkTurn(i int, body string) []*proto.Message {
	callID := "call-" + body
	return []*proto.Message{
		{Role: "user", Content: strings.Repeat(body, 40)},
		{Role: "assistant", ToolCalls: []*proto.ToolCall{{Id: callID, Name: "shell", ArgumentsJson: `{}`}}},
		{Role: "tool", ToolCallId: callID, Content: strings.Repeat(body, 40)},
	}
}

func sizeOf(msgs []*proto.Message) int { return EstimateProtoMessagesTokens(msgs) }

// TestCapMessagesToWindowKeepsSystemAndPairing 硬上限裁剪必须：保留首条 system、
// 保留最后一条、且保留段不以 tool 结果开头（那会变成 provider 直接拒的孤儿结果）。
func TestCapMessagesToWindowKeepsSystemAndPairing(t *testing.T) {
	msgs := []*proto.Message{{Role: "system", Content: "deployment instructions"}}
	for i := 0; i < 30; i++ {
		msgs = append(msgs, mkTurn(i, "abcdefghij")...)
	}
	limit := EstimateProtoMessagesTokens(msgs) / 4

	kept, dropped := CapMessagesToWindow(msgs, 0, limit)
	if dropped == 0 {
		t.Fatal("应发生裁剪")
	}
	if kept[0].GetRole() != "system" {
		t.Fatalf("首条 system 必须保留，实际 %q", kept[0].GetRole())
	}
	if kept[len(kept)-1].GetContent() != msgs[len(msgs)-1].GetContent() {
		t.Fatal("最后一条不能被裁掉")
	}
	if kept[1].GetRole() == "tool" {
		t.Fatal("保留段不能以 tool 结果开头（孤儿结果会让 provider 直接 400）")
	}
	if got := EstimateProtoMessagesTokens(kept); got > limit {
		t.Fatalf("裁剪后仍超预算：%d > %d", got, limit)
	}
	if len(kept) != len(msgs)-dropped-0 && len(kept) != len(msgs)-dropped {
		t.Fatalf("条数不自洽：原 %d，丢 %d，留 %d", len(msgs), dropped, len(kept))
	}
}

// TestCapMessagesToWindowNoOpWhenFits 装得下时一字不改（避免无谓的前缀缓存失效）。
func TestCapMessagesToWindowNoOpWhenFits(t *testing.T) {
	msgs := []*proto.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "hi"}}
	kept, dropped := CapMessagesToWindow(msgs, 0, 1_000_000)
	if dropped != 0 || len(kept) != len(msgs) {
		t.Fatalf("不该裁：dropped=%d kept=%d", dropped, len(kept))
	}
}

// TestEnforceRequestCeilingAppliesInflation 上限判定要按放大后的估算做：
// 我们自己算「刚好装下」，但上次 provider 报的用量比我们的估算高，那就等于实际超窗——
// 必须裁。这一条正是事故的一半成因：算得不准，于是以为装得下，到不了压缩环节。
func TestEnforceRequestCeilingAppliesInflation(t *testing.T) {
	// 直接构造足够大的消息（每条约 260K tokens），避免用「边追加边估算」的 O(n²) 夹具。
	block := strings.Repeat("abcdefghij", 100_000) // 1MB ASCII ≈ 260K tokens
	msgs := []*proto.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, &proto.Message{Role: "user", Content: block})
	}
	window := 1_000_000
	if raw := sizeOf(msgs); raw < window*2 {
		t.Fatalf("夹具不足：%d", raw)
	}

	trimmed, dropped, _, _ := enforceRequestCeiling(msgs, 0, window, 1)
	if dropped == 0 {
		t.Fatal("超窗必须裁")
	}
	if got := sizeOf(trimmed); got > int(float64(window)*contextCeilingRatio) {
		t.Fatalf("裁剪后仍超 90%% 上限：%d", got)
	}

	// 放大系数只会更严
	trimmed2, _, _, _ := enforceRequestCeiling(msgs, 0, window, 1.2)
	if sizeOf(trimmed2) > sizeOf(trimmed) {
		t.Fatal("放大系数应让结果更小")
	}
}

// TestEstimateInflation 偏差系数来自锚点里同一请求的两个数（provider 用量 / 我们的估算）。
func TestEstimateInflation(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	tools := []*proto.Tool{{Name: "shell", Description: "run", ParametersJson: `{"type":"object"}`}}
	hash := ToolsFingerprint(tools)

	m.recordSessionAnchor("s1", sessionUsageAnchor{
		promptTokens: 1_100_000, sentEstimate: 1_000_000,
		toolsHash: hash, model: m.modelName, contextWindow: 1_048_576,
	})
	if got := m.estimateInflation("s1", hash, m.modelName, 1_048_576); got < 1.09 || got > 1.11 {
		t.Fatalf("偏差系数 = %v，期望约 1.1", got)
	}

	// 估算偏大时不用（缩小会白丢历史）
	m.recordSessionAnchor("s2", sessionUsageAnchor{
		promptTokens: 800_000, sentEstimate: 1_000_000,
		toolsHash: hash, model: m.modelName, contextWindow: 1_048_576,
	})
	if got := m.estimateInflation("s2", hash, m.modelName, 1_048_576); got != 1 {
		t.Fatalf("偏大不应缩小，得到 %v", got)
	}

	// 信封不可比（工具目录不同 / 窗口不同）→ 不用
	if got := m.estimateInflation("s1", "other-hash", m.modelName, 1_048_576); got != 1 {
		t.Fatalf("工具目录不同应退回 1，得到 %v", got)
	}
	if got := m.estimateInflation("s1", hash, m.modelName, 131072); got != 1 {
		t.Fatalf("窗口不同应退回 1，得到 %v", got)
	}

	// 无锚点（首次请求/重启）→ 1
	if got := m.estimateInflation("nope", hash, m.modelName, 1_048_576); got != 1 {
		t.Fatalf("无锚点应为 1，得到 %v", got)
	}

	// 极端偏差要夹住，避免一次异常用量把窗口吃得只剩一条
	m.recordSessionAnchor("s3", sessionUsageAnchor{
		promptTokens: 3_000_000, sentEstimate: 1_000_000,
		toolsHash: hash, model: m.modelName, contextWindow: 1_048_576,
	})
	if got := m.estimateInflation("s3", hash, m.modelName, 1_048_576); got != estimateInflationMax {
		t.Fatalf("应夹到 %v，得到 %v", estimateInflationMax, got)
	}
}

// TestFinishPreStepTrimsOversizedRequest 收口阶段必须真的把超窗请求压进窗口——
// 这是「不发必然被拒的请求」的最后一层，且它不依赖任何插件。
func TestFinishPreStepTrimsOversizedRequest(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	m.contextWindow = 20_000
	srv := &llmAggregateServer{m: m}

	msgs := []*proto.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 200; i++ {
		msgs = append(msgs, mkTurn(i, "abcdefghij")...)
	}
	if sizeOf(msgs) <= m.contextWindow {
		t.Fatalf("夹具不足：%d 未超窗口 %d", sizeOf(msgs), m.contextWindow)
	}
	req := &proto.ChatRequest{Messages: msgs, SessionId: "s"}
	out, dropped := srv.finishPreStep(req, msgs, false)
	if dropped == 0 {
		t.Fatal("应发生裁剪")
	}
	if len(out.Messages) >= len(msgs) {
		t.Fatalf("应丢弃最旧的一段：原 %d，现 %d", len(msgs), len(out.Messages))
	}

	got := EstimateProtoMessagesTokens(out.Messages)
	if got > m.contextWindow {
		t.Fatalf("收口后仍超窗：%d > %d", got, m.contextWindow)
	}
	if got > int(float64(m.contextWindow)*contextCeilingRatio)+1 {
		t.Fatalf("应收紧到 %.0f%% 以内：%d", contextCeilingRatio*100, got)
	}
	if out.Messages[len(out.Messages)-1].GetContent() != msgs[len(msgs)-1].GetContent() {
		t.Fatal("最新一条不能被裁掉")
	}
	// 展示口径要跟着实际发出的表面走（不是原始全量）。计量实例由宿主装配，
	// 单测里可能缺席——缺席时跳过该断言，不假装验证过。
	if m.ctxMeters != nil {
		if surface := m.ContextTokens("s"); surface <= 0 || surface > m.contextWindow {
			t.Fatalf("展示的上下文占用应等于实际表面且不超窗，得到 %d", surface)
		}
	}
}

// TestFinishPreStepKeepsFittingRequestUntouched 装得下时原样返回同一个请求对象
// （零改写 → 不产生新的请求、不动展示口径）。
func TestFinishPreStepKeepsFittingRequestUntouched(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	m.contextWindow = 1_000_000
	srv := &llmAggregateServer{m: m}
	msgs := []*proto.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "hi"}}
	req := &proto.ChatRequest{Messages: msgs, SessionId: "s"}
	if out, dropped := srv.finishPreStep(req, msgs, false); out != req || dropped != 0 {
		t.Fatal("装得下应原样返回同一个请求且不裁")
	}
}

// TestOverWindowSlidesInsteadOfAskingBackendToCompress 超窗时的处置顺序：**直接滑窗**，
// 不先请压缩后端做一次紧急压缩。
//
// 两条理由，各自独立成立：
//  1. 紧急路径只能产出没有内容的占位摘要（一类后端写「N 条消息已压缩」的固定模板，
//     另一类 emergency=true 强制截断摘要、跳过 LLM）。一次就把几百条真实消息永久埋进
//     一句空话；滑窗只影响这一次请求，会话日志一字不动，下一轮仍能正常压缩。
//  2. 滑窗一次必然装得下；「压一个范围再重试」不保证收敛——1.07M 那次连续三次重试都没
//     压进窗口，最后整轮失败。
//
// 所以这里断言：请求被压进窗口，且后端**没有**收到 request-error。真正该触发紧急压缩的
// 是 provider 的 4xx（那说明估算失真），而不是宿主的预测。
func TestOverWindowSlidesInsteadOfAskingBackendToCompress(t *testing.T) {
	m, _ := hookManager()
	m.contextWindow = 20_000
	backend := &fakeHook{events: make(chan *proto.OnEventRequest, 8)}
	m.mu.Lock()
	m.toolHookClients["compactor"] = backend
	m.toolHookOrder = []string{"compactor"}
	m.coreMetadata["compactor"] = declaresCompaction()
	m.compactionBackend = "compactor"
	m.mu.Unlock()

	msgs := []*proto.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 200; i++ {
		msgs = append(msgs, mkTurn(i, "abcdefghij")...)
	}
	if sizeOf(msgs) <= m.contextWindow {
		t.Fatalf("夹具不足：%d 未超窗口 %d", sizeOf(msgs), m.contextWindow)
	}

	srv := &llmAggregateServer{m: m}
	out, err := srv.applyPreStepHook(context.Background(), &proto.ChatRequest{SessionId: "s", Messages: msgs})
	if err != nil {
		t.Fatalf("pre-step: %v", err)
	}
	if got := sizeOf(out.Messages) + EstimateProtoToolTokens(out.Tools); got > m.contextWindow {
		t.Fatalf("发出的请求仍超窗：%d > %d", got, m.contextWindow)
	}

	// 后端只应收到 pre-step（一次），不得收到 request-error
	preSteps, errEvents := 0, 0
	for {
		select {
		case req := <-backend.events:
			switch req.GetName() {
			case string(EventAgentPreStep):
				preSteps++
			case string(EventAgentRequestError):
				errEvents++
			}
			continue
		default:
		}
		break
	}
	if errEvents != 0 {
		t.Fatalf("超窗不应触发 request-error（紧急压缩只该由 provider 4xx 触发），实际 %d 次", errEvents)
	}
	if preSteps != 1 {
		t.Fatalf("pre-step 应只跑一次（不重来），实际 %d 次", preSteps)
	}
}
