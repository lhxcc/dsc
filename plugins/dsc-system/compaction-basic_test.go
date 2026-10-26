package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dsc-sdk"
	"dsc/proto"
)

// fakeCompactionBasicLLM 聚合 LLM 的 fake（llmChat 接口）：计数调用、返回固定摘要。
type fakeCompactionBasicLLM struct {
	calls   int
	content string
	err     error
}

func (f *fakeCompactionBasicLLM) Chat(ctx context.Context, messages []*proto.Message, maxTokens int32) (*proto.ChatResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &proto.ChatResponse{Content: f.content}, nil
}

// newTestCompactionBasicServer 构建受控驻留（绕开 env：直接设字段）。
func newTestCompactionBasicServer(t *testing.T, window int) *compactionBasicServer {
	t.Helper()
	s := newCompactionBasicServer()
	s.window = window
	return s
}

// bigMsgs 生成 n 条约 tokensPer 条的大消息（user/assistant 交替）。
//
// 实现按 dsc.EstimateTextTokens 的口径构造：ASCII 约 4 字节/token，故每个 token 写 4 个
// 字节。此前实现直接写 charsPer 个字节——那时估算把每个字符当 1 token，两者恰好相等；
// 估算改成「按字符分类累加」后（修掉纯 ASCII 高估 4~5 倍的 bug），这里必须同步换算，
// 否则夹具只有预期压力的 1/4，压缩阈值再也够不到。
func bigMsgs(n, tokensPer int) []*proto.Message {
	msgs := make([]*proto.Message, 0, n)
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		msgs = append(msgs, &proto.Message{
			Role:    role,
			Content: strings.Repeat("x", tokensPer*4),
		})
	}
	return msgs
}

func mustPreStep(t *testing.T, s *compactionBasicServer, msgs []*proto.Message) string {
	t.Helper()
	return mustPreStepSession(t, s, "sess-c1", msgs)
}

func mustPreStepSession(t *testing.T, s *compactionBasicServer, sess string, msgs []*proto.Message) string {
	t.Helper()
	msgsJSON, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal msgs: %v", err)
	}
	evJSON, err := json.Marshal(map[string]any{
		"agent": "agent-react-loop", "session": sess, "messages_json": string(msgsJSON),
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	res, err := s.handleHostEvent(context.Background(), "agent/pre-step", string(evJSON))
	if err != nil {
		t.Fatalf("pre-step: %v", err)
	}
	return res
}

// mustPreStepUsage 携带 last_usage_tokens（宿主透传的上次服务端上报用量）的 pre-step。
// mustPreStepProjected 构造 pre-step 事件并带上宿主给的投影压力。
// 本插件只认 projected_tokens（压力合成在宿主，见 core.projectPressure）；
// last_usage_tokens 仍照旧带上，用于确认插件**没有**再自作主张用它。
func mustPreStepProjected(t *testing.T, s *compactionBasicServer, sess string, msgs []*proto.Message, projected, lastUsage int) string {
	t.Helper()
	msgsJSON, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal msgs: %v", err)
	}
	evJSON, err := json.Marshal(map[string]any{
		"agent": "agent-react-loop", "session": sess, "messages_json": string(msgsJSON),
		"projected_tokens": projected, "last_usage_tokens": lastUsage,
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	res, err := s.handleHostEvent(context.Background(), "agent/pre-step", string(evJSON))
	if err != nil {
		t.Fatalf("pre-step: %v", err)
	}
	return res
}

func mustPreStepUsage(t *testing.T, s *compactionBasicServer, sess string, msgs []*proto.Message, lastUsage int) string {
	t.Helper()
	return mustPreStepProjected(t, s, sess, msgs, lastUsage, lastUsage)
}

// parseRewrite 解析 {"messages": [...]} 改写结果（非空且含至少一条消息）。
func parseRewrite(t *testing.T, res string) []*proto.Message {
	t.Helper()
	var m struct {
		Messages []*proto.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(res), &m); err != nil {
		t.Fatalf("parse rewrite %q: %v", res, err)
	}
	if len(m.Messages) == 0 {
		t.Fatalf("rewrite has no messages: %q", res)
	}
	return m.Messages
}

// TestCompactionBasicDisabledByZeroWindow 驻留禁用（窗口 0）：一切事件静止。
func TestCompactionBasicDisabledByZeroWindow(t *testing.T) {
	s := newTestCompactionBasicServer(t, 0)
	msgs := bigMsgs(6, 1200)
	if res := mustPreStep(t, s, msgs); res != "" {
		t.Fatalf("disabled resident must not rewrite, got %q", res)
	}
	res, err := s.handleHostEvent(context.Background(), "agent/request-error",
		`{"agent":"a","code":"context_window_exceeded"}`)
	if err != nil || res != "" {
		t.Fatalf("disabled resident must ignore request-error, got %q err %v", res, err)
	}
}

// TestCompactionBasicPressureLLMSummary 压力触发：超阈值时保留尾部、前段经 LLM 摘要、
// 返回 {"messages":[...]}；同载荷重放复用状态（LLM 不再被调用）。
func TestCompactionBasicPressureLLMSummary(t *testing.T) {
	s := newTestCompactionBasicServer(t, 10000) // 阈值 8000；保留预算 max(1600,1024)=1600
	fake := &fakeCompactionBasicLLM{content: "LLM-SUMMARY"}
	s.llm = fake

	msgs := bigMsgs(16, 1200) // 每条 1204 token（CJK 感知估算），共 19264
	res := mustPreStep(t, s, msgs)
	if res == "" {
		t.Fatalf("over-threshold pre-step must rewrite")
	}
	got := parseRewrite(t, res)
	// 保留尾部：预算 1600，单条 1204 → 只留最后 1 条（1204≤1600、2408>1600 →
	// retainIdx=15）；改写 = 1 摘要 + 1 条尾段
	if len(got) != 2 {
		t.Fatalf("rewrite = %d messages, want 2 (summary + tail)", len(got))
	}
	if !strings.Contains(got[0].GetContent(), "LLM-SUMMARY") {
		t.Fatalf("head must be LLM summary, got %q", got[0].GetContent())
	}
	if got[1].GetContent() != msgs[15].GetContent() {
		t.Fatalf("tail message must be preserved verbatim")
	}
	if fake.calls != 1 {
		t.Fatalf("LLM must be called once, got %d", fake.calls)
	}

	// 同载荷重放：指纹命中，状态复用，不再调 LLM
	res2 := mustPreStep(t, s, msgs)
	got2 := parseRewrite(t, res2)
	if len(got2) != 2 || got2[0].GetContent() != got[0].GetContent() {
		t.Fatalf("replay must reuse state deterministically")
	}
	if fake.calls != 1 {
		t.Fatalf("replay must not re-summarize, LLM calls = %d", fake.calls)
	}
}

// TestCompactionBasicFingerprintMismatchResets 历史被改写（指纹失配）：记录作废、
// 从头重新评估并重新摘要。
func TestCompactionBasicFingerprintMismatchResets(t *testing.T) {
	s := newTestCompactionBasicServer(t, 10000)
	fake := &fakeCompactionBasicLLM{content: "S1"}
	s.llm = fake
	msgs := bigMsgs(16, 1200)
	if res := mustPreStep(t, s, msgs); res == "" {
		t.Fatalf("must rewrite")
	}
	// 改写历史头部 → 指纹失配
	msgs[0].Content = "rewritten-history"
	fake.content = "S2"
	res := mustPreStep(t, s, msgs)
	got := parseRewrite(t, res)
	if !strings.Contains(got[0].GetContent(), "S2") {
		t.Fatalf("stale records must be discarded and re-summarized, got %q", got[0].GetContent())
	}
	if fake.calls != 2 {
		t.Fatalf("LLM calls after reset = %d, want 2", fake.calls)
	}
}

// TestCompactionBasicTruncateFallback LLM 未互联：截断式退化摘要（首条+末条拼接）。
func TestCompactionBasicTruncateFallback(t *testing.T) {
	s := newTestCompactionBasicServer(t, 10000) // llm 为 nil
	msgs := bigMsgs(16, 1200)
	res := mustPreStep(t, s, msgs)
	got := parseRewrite(t, res)
	if len(got) != 2 {
		t.Fatalf("rewrite = %d, want 2", len(got))
	}
	if !strings.Contains(got[0].GetContent(), "[压缩摘要]") {
		t.Fatalf("nil-LLM path must produce truncate summary, got %q", got[0].GetContent())
	}
}

// TestCompactionBasicLLMFailureDegrades LLM 调用失败：退化截断式，不把失败上抛。
func TestCompactionBasicLLMFailureDegrades(t *testing.T) {
	s := newTestCompactionBasicServer(t, 10000)
	s.llm = &fakeCompactionBasicLLM{err: errors.New("provider down")}
	msgs := bigMsgs(16, 1200)
	res := mustPreStep(t, s, msgs)
	if res == "" {
		t.Fatalf("failure must degrade to truncate rewrite")
	}
	if got := parseRewrite(t, res); !strings.Contains(got[0].GetContent(), "[压缩摘要]") {
		t.Fatalf("must fall back to truncate summary")
	}
}

// TestCompactionBasicEmergencyRetainLast 溢出紧急压缩：保留最后 1 条、截断式摘要、
// 返回 {"retry": true}；非溢出错误码不触发。
func TestCompactionBasicEmergencyRetainLast(t *testing.T) {
	s := newTestCompactionBasicServer(t, 1000) // 阈值 800：估算 7224 ≥ 阈值
	s.retainMin = 10000                        // 保留预算盖过全部消息 → pre-step 不动作（全部落保留区）
	msgs := bigMsgs(6, 1200)                   // 每条 1204 token（CJK 感知估算），共 7224
	// 第一次 pre-step：估算超阈值但保留区覆盖全部 → 不改写
	if res := mustPreStep(t, s, msgs); res != "" {
		t.Fatalf("all-in-retain pre-step must not rewrite, got %q", res)
	}
	// 紧急：压缩 [0,5)（保留最后 1 条）
	res, err := s.handleHostEvent(context.Background(), "agent/request-error",
		`{"agent":"a","code":"context_window_exceeded"}`)
	if err != nil || res != `{"retry": true}` {
		t.Fatalf("emergency must request retry, res %q err %v", res, err)
	}
	// 重试的 pre-step：指纹命中 → 应用改写
	res2 := mustPreStep(t, s, msgs)
	got := parseRewrite(t, res2)
	if len(got) != 2 {
		t.Fatalf("after emergency rewrite = %d messages, want 2", len(got))
	}
	if !strings.Contains(got[0].GetContent(), "emergency") {
		t.Fatalf("emergency record must be marked, got %q", got[0].GetContent())
	}
	if got[1].GetContent() != msgs[5].GetContent() {
		t.Fatalf("last message must be preserved")
	}
	// 非溢出错误码：不触发
	res3, err := s.handleHostEvent(context.Background(), "agent/request-error",
		`{"agent":"a","code":"rate_limited"}`)
	if err != nil || res3 != "" {
		t.Fatalf("non-overflow error must be ignored, got %q", res3)
	}
}

// TestCompactionBasicSmallHistoryNoOp 消息太少/用量低：零开销不改写。
func TestCompactionBasicSmallHistoryNoOp(t *testing.T) {
	s := newTestCompactionBasicServer(t, 10000)
	msgs := bigMsgs(3, 400) // 300 token << 8000
	if res := mustPreStep(t, s, msgs); res != "" {
		t.Fatalf("small history must not rewrite, got %q", res)
	}
}

// TestCompactionBasicTriggersOnReportedUsage 回归「纯启发式低估导致压缩不触发」：
// 字节启发式估算（不含工具定义、CJK 每字 3 字节被 /4 低估）把真实用量显著估低
// （实测真实 83.56% 而插件估算仅 60.08%），但宿主已透传上次服务端上报用量
// （last_usage_tokens）超阈值时，必须触发压缩改写。
func TestCompactionBasicTriggersOnReportedUsage(t *testing.T) {
	s := newTestCompactionBasicServer(t, 10000) // 阈值 8000
	s.llm = &fakeCompactionBasicLLM{content: "LLM-SUMMARY"}

	msgs := bigMsgs(8, 1200) // 每条 1204 token（CJK 感知估算），共 9632
	res := mustPreStepUsage(t, s, "sess-u1", msgs, 9000)
	if res == "" {
		t.Fatalf("上报用量 9000 >= 阈值 8000 时必须触发压缩")
	}
	got := parseRewrite(t, res)
	if len(got) >= len(msgs) || !strings.Contains(got[0].GetContent(), "LLM-SUMMARY") {
		t.Fatalf("rewrite 未落地：%d msgs, head=%q", len(got), got[0].GetContent())
	}
}

// TestCompactionBasicMeasuresItsOwnSurface 压力必须按「本插件要发出的表面」算：
//   - 尚无压缩记录时，其表面 == 请求表面 → 采信宿主投影（含工具目录与信封判据）；
//   - 已有压缩记录时，表面已被改写（摘要 + 保留尾部），宿主投影仍按原始列表算，
//     若照用会把已压到 ~2 条的视图当成 900k → 每步再摘要一次（回归：把 80% 阈值
//     退化成「保留区一满就压」，实测会多打一次 LLM）。
func TestCompactionBasicMeasuresItsOwnSurface(t *testing.T) {
	s := newTestCompactionBasicServer(t, 10000) // 阈值 8000
	fake := &fakeCompactionBasicLLM{content: "LLM-SUMMARY"}
	s.llm = fake

	// ① 无记录：宿主投影 9000 ≥ 阈值 → 压缩（哪怕本地估算只有 ~4.8k）
	small := bigMsgs(4, 1200)
	res := mustPreStepProjected(t, s, "sess-own-1", small, 9000, 9000)
	if res == "" {
		t.Fatal("宿主投影 9000 ≥ 阈值时必须压缩")
	}
	if fake.calls != 1 {
		t.Fatalf("应只摘要一次，got %d", fake.calls)
	}

	// ② 无记录：宿主投影 7000 < 阈值 → 不压缩（插件不得用本地估算越权决策）
	big := bigMsgs(8, 1200)
	if res := mustPreStepProjected(t, s, "sess-own-2", big, 7000, 7000); res != "" {
		t.Fatalf("宿主投影低于阈值时不得压缩，got %q", res)
	}

	// ③ 有记录：尾部只略增（渲染后远低于阈值），宿主投影仍按原始 19k 给 →
	// 允许返回既有改写（隐藏已压段），但**不得新增摘要**。
	msgs := bigMsgs(16, 1200)
	if mustPreStepProjected(t, s, "sess-own-3", msgs, 19000, 19000) == "" {
		t.Fatal("首次应压缩")
	}
	before := fake.calls
	tailPlus2 := append(append([]*proto.Message{}, msgs...), bigMsgs(2, 1200)...)
	if res := mustPreStepProjected(t, s, "sess-own-3", tailPlus2, 21000, 21000); res == "" {
		t.Fatal("有记录时应返回既有改写（否则模型看到未压缩的全量历史）")
	}
	if fake.calls != before {
		t.Fatalf("渲染后表面 ~2.4k << 阈值 8000，不得再摘要；摘要次数 %d → %d", before, fake.calls)
	}

	// ④ 尾部长到渲染后表面超阈值 → 允许再摘要
	tailPlus8 := append(append([]*proto.Message{}, msgs...), bigMsgs(8, 1200)...)
	mustPreStepProjected(t, s, "sess-own-3", tailPlus8, 27000, 27000)
	if fake.calls <= before {
		t.Fatalf("渲染后表面超过阈值时应再次摘要，摘要次数仍为 %d", fake.calls)
	}
}

// TestCompactionWindowFromContextWindowEnv 窗口来源优先级回归：宿主把有效窗口
// （配置 context_window / 端点探测值）以 DSC_CONTEXT_WINDOW 注入所有插件，压缩器
// 必须采信——此前只认自己的专属键，云端 1M 窗口仍按 128K 判压力，几步就压缩。
func TestCompactionWindowFromContextWindowEnv(t *testing.T) {
	t.Setenv("DSC_COMPACTION_BASIC_CONTEXT_WINDOW", "")
	t.Setenv("DSC_CONTEXT_WINDOW", "1048576")
	if s := newCompactionBasicServer(); s.window != 1048576 {
		t.Fatalf("应采信 DSC_CONTEXT_WINDOW=1048576，got %d", s.window)
	}

	// 插件专属键优先于通用键
	t.Setenv("DSC_COMPACTION_BASIC_CONTEXT_WINDOW", "262144")
	if s := newCompactionBasicServer(); s.window != 262144 {
		t.Fatalf("专属键应优先，got %d", s.window)
	}

	// 两者都缺席 → 128K 默认（本地兜底不变）
	t.Setenv("DSC_COMPACTION_BASIC_CONTEXT_WINDOW", "")
	t.Setenv("DSC_CONTEXT_WINDOW", "")
	if s := newCompactionBasicServer(); s.window != 131072 {
		t.Fatalf("无配置时默认 131072，got %d", s.window)
	}

	// 专属键显式 0 = 驻留禁用（不得被通用键顶回）
	t.Setenv("DSC_CONTEXT_WINDOW", "1048576")
	t.Setenv("DSC_COMPACTION_BASIC_CONTEXT_WINDOW", "0")
	if s := newCompactionBasicServer(); s.window != 0 {
		t.Fatalf("显式 0 应禁用，got %d", s.window)
	}
}

// toolPairMsgs 构造「assistant + 三个工具结果」的消息列表：user 前缀足够大以触发
// 压力压缩，三个工具结果各约 charsPerTool/4 token，使按 token 累积的保留边界恰好
// 落在工具块中间（这是必须被吸附的场景）。
func toolPairMsgs(charsPerTool int) []*proto.Message {
	return []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: strings.Repeat("x", 40000)},
		{Role: "assistant", ToolCalls: []*proto.ToolCall{
			{Id: "a", Name: "t", ArgumentsJson: "{}"},
			{Id: "b", Name: "t", ArgumentsJson: "{}"},
			{Id: "c", Name: "t", ArgumentsJson: "{}"},
		}},
		{Role: "tool", Content: strings.Repeat("x", charsPerTool), ToolCallId: "a"},
		{Role: "tool", Content: strings.Repeat("x", charsPerTool), ToolCallId: "b"},
		{Role: "tool", Content: strings.Repeat("x", charsPerTool), ToolCallId: "c"},
		{Role: "user", Content: "now"},
	}
}

// TestCompactionBasicSnapsToToolPairBoundary 回归：保留边界落在工具块中间时，
// 必须吸附到拥有这些调用的 assistant，否则改写结果以孤立 tool 消息开头，
// OpenAI 兼容端点会拒绝整轮请求。
func TestCompactionBasicSnapsToToolPairBoundary(t *testing.T) {
	s := newTestCompactionBasicServer(t, 10000) // 阈值 8000；保留预算 max(1600,1024)=1600
	s.llm = &fakeCompactionBasicLLM{content: "SUMMARY"}

	res := mustPreStep(t, s, toolPairMsgs(600))
	if res == "" {
		t.Fatal("expected rewrite above pressure threshold")
	}
	msgs := parseRewrite(t, res)
	if msgs[2].GetRole() != "assistant" || len(msgs[2].GetToolCalls()) != 3 {
		t.Fatalf("retain boundary not snapped to owning assistant (index 2 = %q, calls=%d)",
			msgs[2].GetRole(), len(msgs[2].GetToolCalls()))
	}
	if err := dsc.ValidateToolPairing(msgs); err != nil {
		t.Fatalf("rewritten history violates tool pairing: %v", err)
	}
}

// TestCompactionBasicEmergencySnapsToToolPairBoundary 回归：溢出紧急压缩的保留区
// 以最后一条为界，若最后一条是工具结果，必须吸附到其所属 assistant——否则尾部
// 就是一条孤立 tool 消息。
func TestCompactionBasicEmergencySnapsToToolPairBoundary(t *testing.T) {
	s := newTestCompactionBasicServer(t, 1_000_000) // 远高于压力阈值：pre-step 只缓存 lastMsgs
	msgs := []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: strings.Repeat("x", 40000)},
		{Role: "assistant", ToolCalls: []*proto.ToolCall{
			{Id: "a", Name: "t", ArgumentsJson: "{}"},
			{Id: "b", Name: "t", ArgumentsJson: "{}"},
		}},
		{Role: "tool", Content: strings.Repeat("x", 2400), ToolCallId: "a"},
		{Role: "tool", Content: strings.Repeat("x", 2400), ToolCallId: "b"},
	}
	if res := mustPreStep(t, s, msgs); res != "" {
		t.Fatalf("low pressure must not rewrite, got %q", res)
	}
	res, err := s.handleHostEvent(context.Background(), "agent/request-error",
		`{"agent":"agent-react-loop","session":"sess-c1","code":"context_window_exceeded"}`)
	if err != nil {
		t.Fatalf("request-error: %v", err)
	}
	if !strings.Contains(res, `"retry":true`) && !strings.Contains(res, `"retry": true`) {
		t.Fatalf("emergency compaction should request retry, got %q", res)
	}

	// 下一次 pre-step 应用已记录：改写结果必须保持工具对完整。
	rewritten := parseRewrite(t, mustPreStep(t, s, msgs))
	if err := dsc.ValidateToolPairing(rewritten); err != nil {
		t.Fatalf("emergency rewrite violates tool pairing: %v", err)
	}
	start := -1
	for i, m := range rewritten {
		if m.GetRole() == "assistant" && len(m.GetToolCalls()) == 2 {
			start = i
			break
		}
	}
	if start < 0 || start+1 >= len(rewritten) || rewritten[start+1].GetRole() != "tool" {
		t.Fatalf("emergency retain region does not start at owning assistant: %+v", rewritten)
	}
}
