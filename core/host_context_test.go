package core

import (
	"context"
	"strings"
	"testing"

	"dsc/proto"
	"dsc/proto/metadata"
	"dsc/userquestions"
)

// TestHostContextMetersPrefersReported agent 上报的投影值是权威值（决策发生在 agent 进程），
// 宿主只保管与发布；没有上报值时退回实测 prompt 侧用量。
func TestHostContextMetersPrefersReported(t *testing.T) {
	h := NewHostContextMeters(1_000_000)

	// 没有任何帧：未知，返回 0（前端应显示「未知」而不是 0%）
	if got := h.ContextTokens("s1"); got != 0 {
		t.Fatalf("无数据时应返回 0，got %d", got)
	}

	// 只有实测（agent 未上报投影，例如旧版 agent）：退回 prompt 侧
	h.NoteUsage("s1", &Usage{PromptTokens: 498_000}, 0)
	if got := h.ContextTokens("s1"); got != 498_000 {
		t.Fatalf("无上报值时应退回实测 498000，got %d", got)
	}

	// 有上报值：以它为准（199000 与实测不同，模拟"下一发预计"）
	h.NoteUsage("s1", &Usage{PromptTokens: 498_000, ContextTokens: 512_000}, 512_000)
	if got := h.ContextTokens("s1"); got != 512_000 {
		t.Fatalf("应优先用上报的投影值 512000，got %d", got)
	}

	// 会话隔离：另一会话不受影响
	if got := h.ContextTokens("s2"); got != 0 {
		t.Fatalf("不同会话应相互独立，got %d", got)
	}
	h.NoteUsage("s2", &Usage{PromptTokens: 1000}, 0)
	if got := h.ContextTokens("s1"); got != 512_000 {
		t.Fatalf("另一会话的帧不应改写 s1，got %d", got)
	}
}

// TestHostContextMetersPrefersActualSurface 本步被插件改写过请求时，展示口径必须
// 跟着「真正发出去的东西」走：压缩后端把 900k 压成 40k 之后，界面若还显示 900k，
// 用户看到的就是「都 90% 了还不压缩」，而压缩插件按 40k 判断根本没到阈值。
func TestHostContextMetersPrefersActualSurface(t *testing.T) {
	h := NewHostContextMeters(1_000_000)
	h.NoteUsage("s1", &Usage{PromptTokens: 900_000, ContextTokens: 900_000}, 900_000)
	if got := h.ContextTokens("s1"); got != 900_000 {
		t.Fatalf("改写前应显示上报投影 900000，got %d", got)
	}

	// 本步压缩成 40k：实际发出的表面优先
	h.NoteActualSurface("s1", 40_000)
	if got := h.ContextTokens("s1"); got != 40_000 {
		t.Fatalf("改写后应显示实际表面 40000，got %d", got)
	}

	// 下一步没有改写 → 清掉实际表面，回到上报投影
	h.NoteActualSurface("s1", 0)
	if got := h.ContextTokens("s1"); got != 900_000 {
		t.Fatalf("未改写的一步应回到上报投影 900000，got %d", got)
	}
}

// TestPreStepRewriteUpdatesDisplayedSurface 压缩后端经 pre-step 改写请求后，宿主记录
// 的「实际发出的表面」必须立刻成为展示值；下一步没有改写则退回 agent 上报的投影。
// 这是「到了 80% 却不压缩」的直接解药：那个 80% 描述的是一个没发出去的请求。
func TestPreStepRewriteUpdatesDisplayedSurface(t *testing.T) {
	m := newRouterManager()
	m.ctxMeters = NewHostContextMeters(200_000)
	m.ctxMeters.NoteUsage("sess-x", &Usage{PromptTokens: 180_000, ContextTokens: 180_000}, 180_000)

	tools := []*proto.Tool{{Name: "shell", Description: "run", ParametersJson: `{"type":"object"}`}}
	big := []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: strings.Repeat("長", 4000)},
	}
	f := &fakeHook{
		events:      make(chan *proto.OnEventRequest, 4),
		eventResult: `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"tail"}]}`,
	}
	m.toolHookClients["p1"] = f
	m.toolHookOrder = []string{"p1"}
	// 只有配置选定的压缩后端才有「改写请求」的采纳权（避免多个声明 compaction 的
	// 插件层层改写），故这里把它指定为后端。
	m.compactionBackend = "p1"

	srv := &llmAggregateServer{m: m}
	req := &proto.ChatRequest{SessionId: "sess-x", Messages: big, Tools: tools}
	out, _ := srv.applyPreStepHook(context.Background(), req)
	if len(out.Messages) != 2 || out.Messages[1].Content != "tail" {
		t.Fatalf("改写未生效: %+v", out.Messages)
	}
	want := EstimateProtoMessagesTokens(out.Messages) + EstimateProtoToolTokens(tools)
	if want >= 180_000 {
		t.Fatalf("夹具无效：改写后的表面 %d 没有明显变小", want)
	}
	if got := m.ContextTokens("sess-x"); got != want {
		t.Fatalf("展示值 = %d，期望按实际发出的表面 %d", got, want)
	}

	// 下一步没有插件改写（result 为空）：展示退回 agent 上报的投影
	f.eventResult = ""
	_, _ = srv.applyPreStepHook(context.Background(), req)
	if got := m.ContextTokens("sess-x"); got != 180_000 {
		t.Fatalf("未改写的一步应回到上报投影 180000，got %d", got)
	}
}

// TestHostContextMetersReset 切会话/压缩改写历史后清空，避免旧数字残留。
func TestHostContextMetersReset(t *testing.T) {
	h := NewHostContextMeters(1000)
	h.NoteUsage("s1", &Usage{PromptTokens: 500}, 800)
	if got := h.ContextTokens("s1"); got != 800 {
		t.Fatalf("reset 前应为 800，got %d", got)
	}
	h.Reset("s1")
	if got := h.ContextTokens("s1"); got != 0 {
		t.Fatalf("reset 后应清空，got %d", got)
	}
	// reset 后再来一帧只带实测：仍能工作（meter 重建）
	h.NoteUsage("s1", &Usage{PromptTokens: 300}, 0)
	if got := h.ContextTokens("s1"); got != 300 {
		t.Fatalf("reset 后重新锚定应为 300，got %d", got)
	}
}

// TestHostContextMetersNilSafe 空输入不得 panic（TUI 每帧都会喂）。
func TestHostContextMetersNilSafe(t *testing.T) {
	h := NewHostContextMeters(0)
	h.NoteUsage("", &Usage{PromptTokens: 1}, 1)
	h.NoteUsage("s", nil, 0)
	if got := h.ContextTokens("s"); got != 0 {
		t.Fatalf("应返回 0，got %d", got)
	}
	var m *Manager
	if got := m.ContextTokens("s"); got != 0 {
		t.Fatalf("nil Manager 应返回 0，got %d", got)
	}
	m.NoteContextUsage("s", &Usage{PromptTokens: 1}, 1) // 不得 panic
	m.ResetContextMeters("s")
	m.SetContextWindow(1)
}

// TestAskEmitsUserQuestionEvent 只有存在 notify 能力时才广播 userquestion/asked：
// 「不装通知插件 = 静音」是用户可控的开关，宿主不能照发不误。
func TestAskEmitsUserQuestionEvent(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	var got []UserQuestionAskedEvent
	m.events.On(EventUserQuestionAsked, func(ec EventContext) (any, error) {
		if ev, ok := ec.Data.(UserQuestionAskedEvent); ok {
			got = append(got, ev)
		}
		return nil, nil
	})
	if err := m.RegisterUserQuestionProvider(func(ctx context.Context, r *userquestions.Request) (*userquestions.Answer, error) {
		return &userquestions.Answer{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	req := &userquestions.Request{Questions: []userquestions.Question{{ID: "q", Header: "确认", Question: "继续？"}}}

	// 没有插件提供 notify：不广播（用户要的就是这个静音开关）
	if _, err := m.Ask(context.Background(), req); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("无 notify 能力时不应广播，实际 %d 次", len(got))
	}

	// 有插件声明 notify 能力：广播一次，载荷带问题数与标题
	m.coreMetadata["dsc-notify"] = &metadata.PluginInfo{
		Capabilities: map[string]string{"notify": "true"},
	}
	if !m.HasPluginProvidingCapability("notify") {
		t.Fatal("夹具未生效：应报告 notify 能力")
	}
	if _, err := m.Ask(context.Background(), req); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got) != 1 || got[0].Questions != 1 || got[0].Header != "确认" {
		t.Fatalf("事件载荷不对：%+v", got)
	}
}
