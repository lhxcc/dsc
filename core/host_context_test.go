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
	// 只有**岗位持有者**才有「改写请求」的采纳权（避免多个声明 compaction 的插件
	// 层层改写）：岗位 = 配置点名 + 已加载 + 声明 compaction 能力，三者缺一不算当值。
	m.compactionBackend = "p1"
	m.coreMetadata["p1"] = &metadata.PluginInfo{Capabilities: map[string]string{"compaction": "true"}}

	srv := &llmAggregateServer{m: m}
	req := &proto.ChatRequest{SessionId: "sess-x", Messages: big, Tools: tools}
	out, hookErr := srv.applyPreStepHook(context.Background(), req)
	if hookErr != nil {
		t.Fatalf("applyPreStepHook: %v", hookErr)
	}
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
	if _, err := srv.applyPreStepHook(context.Background(), req); err != nil {
		t.Fatalf("applyPreStepHook: %v", err)
	}
	if got := m.ContextTokens("sess-x"); got != 180_000 {
		t.Fatalf("未改写的一步应回到上报投影 180000，got %d", got)
	}
}

// TestHostContextMetersFallsBackToMeasurement 兜底必须读「实测用量」本身，而不是
// meter.Project(0) 算出的启发式误差。
//
// Project(0) = 实测 − 该次请求的估算，是「估算偏差」，量级几万且几乎不随会话增长。
// 曾经它被当成「已用」的兜底：会话涨到 730K（provider 实测），界面却一直显示
// 33K / 3.29%，整轮不动——而速率、缓存命中照常变（它们读的是另一个数）。
func TestHostContextMetersFallsBackToMeasurement(t *testing.T) {
	h := NewHostContextMeters(1_048_576)
	// 只有实测、没有上报投影：兜底应给出 731391 本身
	h.NoteUsage("s1", &Usage{PromptTokens: 731_391}, 0)
	if got := h.ContextTokens("s1"); got != 731_391 {
		t.Fatalf("无上报值时应退回实测 731391，got %d（这是误差值而非规模）", got)
	}
	// 会话继续增长：兜底跟着涨，不再钉在原处
	h.NoteUsage("s1", &Usage{PromptTokens: 780_000}, 0)
	if got := h.ContextTokens("s1"); got != 780_000 {
		t.Fatalf("实测增长时必须跟着变，got %d", got)
	}
}

// TestPreStepWithoutRewriteClearsStaleActualSurface 没有插件改写的那一步必须作废上一发
// 记录的「实际表面」，否则界面会一直显示一份不存在的请求。
//
// 这是「已用」冻结的另一条路径：某一步被改写成很小的表面（例如 33K），之后每一步
// 都没有改写（或分发提前返回），残留的 33K 就永远压着上报值不动。
func TestPreStepWithoutRewriteClearsStaleActualSurface(t *testing.T) {
	m := newRouterManager()
	m.ctxMeters = NewHostContextMeters(1_048_576)
	m.ctxMeters.NoteUsage("sess-x", &Usage{PromptTokens: 731_391, ContextTokens: 731_391}, 731_391)

	// 模拟「上一步被压缩后端改写成了很小的表面」
	m.NoteActualSurface("sess-x", 33_000)
	if got := m.ContextTokens("sess-x"); got != 33_000 {
		t.Fatalf("改写时应显示实际表面 33000，got %d", got)
	}

	// 本步没有任何 hook 客户端（提前返回路径）→ 必须清掉残留
	srv := &llmAggregateServer{m: m}
	req := &proto.ChatRequest{SessionId: "sess-x", Messages: []*proto.Message{{Role: "user", Content: "hi"}}}
	if _, err := srv.applyPreStepHook(context.Background(), req); err != nil {
		t.Fatalf("applyPreStepHook: %v", err)
	}
	if got := m.ContextTokens("sess-x"); got != 731_391 {
		t.Fatalf("未改写的一步应回到上报投影 731391，got %d（残留的实际表面没被清掉）", got)
	}
}

// TestHostContextMetersSeed 启动预置：恢复有历史的会话时，用户发出第一个请求之前
// 就该有读数（上下文里已经躺着这段历史），第一帧真实用量到达后被替换。
//
// 预置只填「兜底」那一层：不写上报值、不写实际表面，避免把历史读数当成当前请求的
// 权威值。
func TestHostContextMetersSeed(t *testing.T) {
	h := NewHostContextMeters(1_048_576)
	h.Seed("s1", 40347)
	if got := h.ContextTokens("s1"); got != 40347 {
		t.Fatalf("预置后应显示 40347，got %d", got)
	}
	// 新会话不预置：仍是 0（前端显示「未知」而不是 0%）
	if got := h.ContextTokens("brand-new"); got != 0 {
		t.Fatalf("未预置的会话应为 0，got %d", got)
	}
	// 第一帧真实用量到达：预置值被替换，不再残留
	h.NoteUsage("s1", &Usage{PromptTokens: 512_000, ContextTokens: 512_000}, 512_000)
	if got := h.ContextTokens("s1"); got != 512_000 {
		t.Fatalf("首帧用量应替换预置值，got %d", got)
	}
	// 非法入参不写脏数据
	h.Seed("s2", 0)
	h.Seed("", 1234)
	if got := h.ContextTokens("s2"); got != 0 {
		t.Fatalf("非正预置值应被忽略，got %d", got)
	}
}

// TestHostContextMetersSeedClampedToCurrentWindow 预置值来自上一次请求，而那次可能是
// 在 1M 云模型上跑的；用户这次换成 128K 本地模型，预置值必须被**当前**窗口夹住。
//
// 真实时序：LoadHistoryReplay 先 Seed（此刻计量器窗口还是 0，因为 SetContextWindow
// 还没跑），tui.New 才把 128K 设进来。所以夹取只能在读取时做——写入时还不知道窗口。
// 不夹的话标题会显示「714K/128K」（557%），那是一个永远不会被发出的请求：
// 发之前 agent 会先把它裁进窗口。
func TestHostContextMetersSeedClampedToCurrentWindow(t *testing.T) {
	h := NewHostContextMeters(0) // 宿主装配时的初始值：窗口未知
	h.Seed("s1", 714_000)        // 上一次在 1M 窗口下的请求
	h.SetWindow(131_072)         // 这次换成本地 128K

	if got := h.ContextTokens("s1"); got != 131_072 {
		t.Fatalf("预置值应被当前窗口夹住 131072，got %d（否则显示 714K/128K）", got)
	}

	// 接上第一帧真实用量：按实测走（已经裁进窗口）
	h.NoteUsage("s1", &Usage{PromptTokens: 120_000, ContextTokens: 120_000}, 120_000)
	if got := h.ContextTokens("s1"); got != 120_000 {
		t.Fatalf("首帧用量应替换预置值，got %d", got)
	}

	// 上报值本身超窗（理论上不该出现）：同样夹住，界面不显示 >100%
	h.NoteUsage("s1", &Usage{PromptTokens: 300_000, ContextTokens: 300_000}, 300_000)
	if got := h.ContextTokens("s1"); got != 131_072 {
		t.Fatalf("超窗的上报值应被夹住，got %d", got)
	}

	// 窗口未知（0）时无从比较：原样返回，不臆造上限
	unknown := NewHostContextMeters(0)
	unknown.Seed("s2", 714_000)
	if got := unknown.ContextTokens("s2"); got != 714_000 {
		t.Fatalf("窗口未知时应原样返回 714000，got %d", got)
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

// TestContextTokensCalibratesRewrittenSurface 压缩后端改写请求后，宿主只能用自己的
// 估算量那份新表面，而估算对中文/工具负载系统性偏低（实测偏低约 20%）。现场：
// 标题栏 397K（原始估算）、/context 面板 487K（实测口径）、provider 实报 498777
// 用同一请求的实测校准后，标题栏必须回到真实量级。
func TestContextTokensCalibratesRewrittenSurface(t *testing.T) {
	h := NewHostContextMeters(1_048_576)

	// 压缩后宿主量到的表面（估算 397K），紧接着是同一次请求的实测 498777
	h.NoteActualSurface("s", 397_000)
	h.NoteUsage("s", &Usage{PromptTokens: 498_777, ContextTokens: 498_777}, 498_777)
	if got := h.ContextTokens("s"); got < 490_000 || got > 505_000 {
		t.Fatalf("校准后应接近实测 498777，got %d", got)
	}

	// 下一次改写后表面变小：校准继续作用在新表面上，而不是退回原始估算
	h.NoteActualSurface("s", 240_000)
	if got := h.ContextTokens("s"); got < 290_000 || got > 310_000 {
		t.Fatalf("校准应继续作用于新表面：got %d", got)
	}

	// 离谱的实测比例（0.03%）不能污染校准
	h.NoteActualSurface("s", 300_000)
	h.NoteUsage("s", &Usage{PromptTokens: 100, ContextTokens: 100}, 100)
	if got := h.ContextTokens("s"); got < 350_000 || got > 400_000 {
		t.Fatalf("异常比例应被夹住，got %d", got)
	}
}
