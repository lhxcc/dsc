package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"dsc/core"
)

// TestWorkingStateArmsAnimation 不变式：工作行可见时，驱动它的 tick 链必须在跑。
//
// spinner.Tick 是一次性命令——处理 TickMsg 时若不在工作态就返回 nil，链断掉。
// 因此任何进入工作态的路径都必须重新点火；漏掉一条，工作行就停在**中途帧**：
// 行还在，帧与秒数都不动，直到本轮结束才消失。这条用例先让链在空闲时自然断掉，
// 再由 first 帧把状态置为工作中，断言链被重新点燃。
func TestWorkingStateArmsAnimation(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// 空闲时收到一个 tick：链就此终止
	m.thinking, m.streaming = false, false
	if _, cmd := m.Update(m.spinner.Tick()); cmd != nil {
		t.Fatal("空闲时 tick 链应终止，不应返回下一个 tick")
	}
	if m.animTicking {
		t.Fatal("空闲后 animTicking 应归位为 false")
	}

	// first 帧把状态置为工作中（不经过 startTurn 的路径，如迟到/唤醒帧）
	ch := make(chan *core.RunStreamResponse, 1)
	m.Update(streamFrame{input: "x", ch: ch, first: true})
	if !m.streaming {
		t.Fatal("first 帧应进入 streaming 状态")
	}
	if !m.animTicking {
		t.Fatal("进入工作态后必须重新点火 tick 链（否则动画停在中途帧）")
	}

	// 点火后帧号应能继续推进
	before := m.thinkAnim.frame
	m.Update(m.spinner.Tick())
	if got := m.thinkAnim.frame; got != before+1 {
		t.Fatalf("工作态下 tick 应推进动画帧: %d → %d", before, got)
	}
}

// TestTurnEndDisarmsAnimation 本轮结束（success 帧）必须把链标志归位，
// 否则下一轮 startTurn 会因为「标志仍是 true」而不再点火，动画再次卡住。
func TestTurnEndDisarmsAnimation(t *testing.T) {
	m := New(&stubAgent{frames: []*core.RunStreamResponse{{Status: "success"}}}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	m.thinking = true
	m.animTicking = true
	m.Update(streamFrame{frame: &core.RunStreamResponse{Status: "success"}})
	if m.thinking || m.streaming {
		t.Fatal("success 后应回到空闲")
	}
	if m.animTicking {
		t.Fatal("本轮结束后 animTicking 必须归位，否则下一轮不会重新点火")
	}
	// 工作行随之消失
	if view := m.View().Content; containsSpinnerLine(view) {
		t.Fatalf("本轮结束后不应再有工作行: %q", view)
	}
}

// TestWakeupAfterSuccessFrame 后台任务在本轮进行中完成时，通知排队；本轮以
// success 收尾后必须尝试唤醒——否则通知会一直躺在队列里等用户敲字。
func TestWakeupAfterSuccessFrame(t *testing.T) {
	m := New(&stubAgent{frames: []*core.RunStreamResponse{{Status: "success"}}}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// 本轮进行中，任务完成 → 通知排队
	m.thinking = true
	m.Update(jobDoneMsg{snapshot: wakeSnap("workflow-9")})
	if len(m.pendingWakeups) != 1 {
		t.Fatalf("忙碌时通知应排队，got %+v", m.pendingWakeups)
	}

	// success 收尾 → 自动唤醒（把通知作为新一轮输入）
	_, cmd := m.Update(streamFrame{frame: &core.RunStreamResponse{Status: "success"}})
	if !m.thinking {
		t.Fatal("本轮收尾后应被排队通知唤醒")
	}
	if cmd == nil {
		t.Fatal("唤醒应返回启动命令")
	}
	if len(m.pendingWakeups) != 0 {
		t.Fatalf("通知应被唤醒消费，got %+v", m.pendingWakeups)
	}
	if !m.animTicking {
		t.Fatal("唤醒后必须点火动画链")
	}
}

// containsSpinnerLine 判断视图里是否还有工作行（spinner + 秒数提示）。
func containsSpinnerLine(view string) bool {
	return strings.Contains(view, "Ctrl+C 取消")
}
