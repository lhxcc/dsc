package core

import (
	"context"
	"testing"

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

// TestAskEmitsUserQuestionEvent 提问前广播 userquestion/asked，供通知插件提醒用户；
// 没有 provider 时不得广播（请求根本没到用户面前）。
func TestAskEmitsUserQuestionEvent(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	var got []UserQuestionAskedEvent
	m.events.On(EventUserQuestionAsked, func(ec EventContext) (any, error) {
		if ev, ok := ec.Data.(UserQuestionAskedEvent); ok {
			got = append(got, ev)
		}
		return nil, nil
	})

	// 未注册 provider：不广播（此时没有 UI 呈现）
	req := &userquestions.Request{Questions: []userquestions.Question{{ID: "q", Header: "确认", Question: "继续？"}}}
	if _, err := m.Ask(context.Background(), req); err == nil {
		t.Fatal("未注册 provider 应返回错误")
	}
	if len(got) != 0 {
		t.Fatalf("未呈现给用户时不应广播，实际 %d 次", len(got))
	}

	// 注册 provider：提问前广播一次，载荷带问题数与标题
	if err := m.RegisterUserQuestionProvider(func(ctx context.Context, r *userquestions.Request) (*userquestions.Answer, error) {
		if len(got) != 1 {
			t.Fatalf("调 provider 时事件应已广播，实际 %d 次", len(got))
		}
		return &userquestions.Answer{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ask(context.Background(), req); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if len(got) != 1 || got[0].Questions != 1 || got[0].Header != "确认" {
		t.Fatalf("事件载荷不对：%+v", got)
	}
}
