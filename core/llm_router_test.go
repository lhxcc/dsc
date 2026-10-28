package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dsc/proto"
)

// newRouterManager 构造路由测试用的 Manager：替换事件总线去掉默认 retry，
// 使 provider 失败即触发 fallback（不被重试掩盖）。
func newRouterManager() *Manager {
	m := NewManager(&ManagerConfig{})
	m.events = NewEventBus()
	return m
}

func TestAggregateChatFallsBack(t *testing.T) {
	m := newRouterManager()
	primary := &mockLLMProvider{failChat: 1}
	backup := &mockLLMProvider{}
	m.llms["primary"] = primary
	m.llms["backup"] = backup
	m.llmOrder = []string{"primary", "backup"}
	m.agentLLMName = "primary"

	srv := &llmAggregateServer{m: m}
	resp, err := srv.Chat(context.Background(), &proto.ChatRequest{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if primary.chatCalls != 1 || backup.chatCalls != 1 {
		t.Fatalf("calls = primary:%d backup:%d, want 1/1 (fallback after primary failure)", primary.chatCalls, backup.chatCalls)
	}
}

func TestAggregateChatPrimaryFirst(t *testing.T) {
	m := newRouterManager()
	primary := &mockLLMProvider{}
	backup := &mockLLMProvider{}
	m.llms["primary"] = primary
	m.llms["backup"] = backup
	m.llmOrder = []string{"primary", "backup"}
	m.agentLLMName = "primary"

	srv := &llmAggregateServer{m: m}
	if _, err := srv.Chat(context.Background(), &proto.ChatRequest{}); err != nil {
		t.Fatalf("chat: %v", err)
	}
	if primary.chatCalls != 1 || backup.chatCalls != 0 {
		t.Fatalf("calls = primary:%d backup:%d, want 1/0 (backup unused)", primary.chatCalls, backup.chatCalls)
	}
}

func TestAggregateChatAllFail(t *testing.T) {
	m := newRouterManager()
	m.llms["a"] = &mockLLMProvider{failChat: 5}
	m.llms["b"] = &mockLLMProvider{failChat: 5}
	m.llmOrder = []string{"a", "b"}
	m.agentLLMName = "a"

	srv := &llmAggregateServer{m: m}
	if _, err := srv.Chat(context.Background(), &proto.ChatRequest{}); err == nil {
		t.Fatal("expected error when all providers fail")
	}
}

func TestAggregateChatStreamFallbackOnStartFailure(t *testing.T) {
	m := newRouterManager()
	primary := &mockLLMProvider{failStart: 1}
	backup := &mockLLMProvider{}
	m.llms["primary"] = primary
	m.llms["backup"] = backup
	m.llmOrder = []string{"primary", "backup"}
	m.agentLLMName = "primary"

	srv := &llmAggregateServer{m: m}
	stream := &mockChatStream{}
	if err := srv.ChatStream(&proto.ChatRequest{}, stream); err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if len(stream.sent) != 1 || stream.sent[0].Content != "hi" {
		t.Fatalf("sent = %+v, want one 'hi' frame", stream.sent)
	}
}

func TestAggregateChatStreamNoFallbackAfterFrames(t *testing.T) {
	m := newRouterManager()
	primary := &mockLLMProvider{midErr: true}
	backup := &mockLLMProvider{}
	m.llms["primary"] = primary
	m.llms["backup"] = backup
	m.llmOrder = []string{"primary", "backup"}
	m.agentLLMName = "primary"

	srv := &llmAggregateServer{m: m}
	stream := &mockChatStream{}
	err := srv.ChatStream(&proto.ChatRequest{}, stream)
	if err == nil || err.Error() != "LLM stream error: mid stream error" {
		t.Fatalf("err = %v, want mid stream error", err)
	}
	// 已发帧后失败：不切换 provider，backup 不调用
	if backup.chatCalls != 0 {
		t.Fatalf("backup should not be used after frames, calls = %d", backup.chatCalls)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d frames, want 1", len(stream.sent))
	}
}

func TestLLMRouteOrderPrimaryFirst(t *testing.T) {
	m := newRouterManager()
	m.llms["primary"] = &mockLLMProvider{}
	m.llms["backup"] = &mockLLMProvider{}
	m.llms["other"] = &mockLLMProvider{}
	m.llmOrder = []string{"primary", "backup", "other"}
	m.agentLLMName = "backup" // primary 声明为 backup

	nap := m.llmRouteSnapshot()
	order := make([]string, 0, len(nap))
	for _, np := range nap {
		order = append(order, np.name)
	}
	want := []string{"backup", "primary", "other"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i, n := range want {
		if order[i] != n {
			t.Fatalf("order[%d] = %s, want %s (order=%v)", i, order[i], n, order)
		}
	}
}

func TestLLMRouteOrderSkipsUnloaded(t *testing.T) {
	m := newRouterManager()
	m.llms["a"] = &mockLLMProvider{}
	m.llmOrder = []string{"a", "ghost"} // ghost 未加载
	m.agentLLMName = ""

	nap := m.llmRouteSnapshot()
	order := make([]string, 0, len(nap))
	for _, np := range nap {
		order = append(order, np.name)
	}
	if len(order) != 1 || order[0] != "a" {
		t.Fatalf("order = %v, want [a] (unloaded skipped)", order)
	}
}

// TestApplyPreStepHookCarriesLastUsageTokens 回归「纯启发式低估导致压缩不触发」：
// 宿主按会话记录最近一次成功请求的上报 prompt 用量，并在 agent/pre-step 事件透传
// （last_usage_tokens）；压缩插件以该精确底数判定压力，而非仅靠字节启发式估算。
// 覆盖跨层字段保真：Manager 记录 → llmAggregateServer.applyPreStepHook → 插件事件 JSON。
func TestApplyPreStepHookCarriesLastUsageTokens(t *testing.T) {
	m := newRouterManager()
	m.recordSessionAnchor("sess-x", sessionUsageAnchor{promptTokens: 90000})

	f := &fakeHook{events: make(chan *proto.OnEventRequest, 1)}
	m.toolHookClients["p1"] = f
	m.toolHookOrder = []string{"p1"}

	srv := &llmAggregateServer{m: m}
	req := &proto.ChatRequest{
		SessionId: "sess-x",
		Messages:  []*proto.Message{{Role: "user", Content: "hi"}},
	}
	out, hookErr := srv.applyPreStepHook(context.Background(), req)
	if hookErr != nil {
		t.Fatalf("applyPreStepHook: %v", hookErr)
	}
	if out == nil || len(out.Messages) != 1 || out.Messages[0].Content != "hi" {
		t.Fatalf("无改写插件时请求必须原样通过, got %+v", out)
	}
	select {
	case ev := <-f.events:
		if ev.GetName() != string(EventAgentPreStep) {
			t.Fatalf("event name = %s", ev.GetName())
		}
		var payload AgentPreStepEvent
		if err := json.Unmarshal([]byte(ev.GetDataJson()), &payload); err != nil {
			t.Fatalf("parse event: %v", err)
		}
		if payload.LastUsageTokens != 90000 {
			t.Fatalf("last_usage_tokens = %d, want 90000", payload.LastUsageTokens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pre-step event not dispatched to hook")
	}
}

// TestRecordSessionUsageRequiresPositive 空会话 id / 非正用量不记录（内部调用与非流式请求
// 不污染会话状态），lastSessionAnchor 返回空锚点由 pre-step 退回启发式估算。
func TestRecordSessionUsageRequiresPositive(t *testing.T) {
	m := newRouterManager()
	m.recordSessionAnchor("", sessionUsageAnchor{promptTokens: 90000})
	m.recordSessionAnchor("sess-y", sessionUsageAnchor{promptTokens: 0})
	if a, ok := m.lastSessionAnchor("sess-y"); ok || a.promptTokens != 0 {
		t.Fatalf("lastSessionAnchor = %+v ok=%v, want 空锚点", a, ok)
	}
}

// TestApplyPreStepHookProjectsPressure 跨层回归：宿主把「锚点 + 当前表面」合成投影压力
// 并随 pre-step 事件下发（projected_tokens / envelope_matched），压缩插件直接读它。
// 两种情形都要对：
//   - 信封一致且上次用量未被改写（usage >= 当次估算）→ 实测底数 + 差量；
//   - 工具目录变了 → 判定不可比，退回纯估算（不能拿过期锚点掩盖更大的表面）。
func TestApplyPreStepHookProjectsPressure(t *testing.T) {
	m := newRouterManager()
	f := &fakeHook{events: make(chan *proto.OnEventRequest, 4)}
	m.toolHookClients["p1"] = f
	m.toolHookOrder = []string{"p1"}
	srv := &llmAggregateServer{m: m}

	// 真实会话量级：消息 913354 + 工具目录 10037
	msgs := []*proto.Message{{Role: "user", Content: "hi"}}
	tools := []*proto.Tool{{Name: "shell", Description: "run", ParametersJson: `{"type":"object"}`}}
	curEstimate := EstimateProtoMessagesTokens(msgs) + EstimateProtoToolTokens(tools)

	// 情形 1：同信封，上次实测 900（>= 当次估算）→ 投影 = 900 + (cur - 800)
	fp := ToolsFingerprint(tools)
	m.recordSessionAnchor("sess-p", sessionUsageAnchor{
		promptTokens: 900, sentEstimate: 800, toolsHash: fp,
	})
	req := &proto.ChatRequest{SessionId: "sess-p", Messages: msgs, Tools: tools}
	if _, err := srv.applyPreStepHook(context.Background(), req); err != nil {
		t.Fatalf("applyPreStepHook: %v", err)
	}
	ev := <-f.events
	var payload AgentPreStepEvent
	if err := json.Unmarshal([]byte(ev.GetDataJson()), &payload); err != nil {
		t.Fatalf("parse event: %v", err)
	}
	if !payload.EnvelopeMatched {
		t.Fatal("工具目录相同应判定信封一致")
	}
	if want := 900 + (curEstimate - 800); payload.ProjectedTokens != want {
		t.Fatalf("projected_tokens = %d, want %d", payload.ProjectedTokens, want)
	}
	if payload.LastUsageTokens != 900 {
		t.Fatalf("last_usage_tokens = %d, want 900", payload.LastUsageTokens)
	}

	// 情形 2：换工具目录 → 不可比，投影退回当前估算
	otherTools := []*proto.Tool{{Name: "shell2", Description: "run2", ParametersJson: `{"type":"object"}`}}
	req2 := &proto.ChatRequest{SessionId: "sess-p", Messages: msgs, Tools: otherTools}
	if _, err := srv.applyPreStepHook(context.Background(), req2); err != nil {
		t.Fatalf("applyPreStepHook: %v", err)
	}
	ev2 := <-f.events
	var payload2 AgentPreStepEvent
	if err := json.Unmarshal([]byte(ev2.GetDataJson()), &payload2); err != nil {
		t.Fatalf("parse event: %v", err)
	}
	want2 := EstimateProtoMessagesTokens(msgs) + EstimateProtoToolTokens(otherTools)
	if payload2.EnvelopeMatched || payload2.ProjectedTokens != want2 {
		t.Fatalf("信封变化后应退回估算 %d：matched=%v projected=%d",
			want2, payload2.EnvelopeMatched, payload2.ProjectedTokens)
	}
}
