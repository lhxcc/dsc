package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"dsc/core"
)

// TestWorkingStateArmsAnimation 不变式：工作行可见时，驱动它的时钟必须在跑。
//
// 工作行是否出现只看 workingLineVisible()（时钟在跑 && 工作中）。因此任何进入
// 工作态的路径都必须点火；漏掉一条，工作行就不出现（而不是冻在某一帧上）。
// 这条用例先让时钟在空闲时自然停掉，再由 first 帧把状态置为工作中，断言重新点火。
func TestWorkingStateArmsAnimation(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// 空闲时收到一拍：链就此终止
	m.thinking, m.streaming = false, false
	if _, cmd := m.Update(animTickMsg{gen: m.animGen}); cmd != nil {
		t.Fatal("空闲时时钟应终止，不应返回下一拍")
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
	m.Update(animTickMsg{gen: m.animGen})
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

// TestWorkingLineVanishesWhenClockStops 模型离开＝时钟停：工作行必须**整行消失**，
// 不能把最后一帧冻在屏幕上。
//
// 中文动画未扫到的字是「同色背景」的实心遮罩块，扫到一半就停住的话，屏幕上是
// 「深度██」——前几个字亮着、后面还是两块实心方块，看起来像渲染到一半被打断。
// 遮罩只在下一秒会被替换时才有意义，所以停表就等于行消失。
func TestWorkingLineVanishesWhenClockStops(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.thinking = true
	m.armAnim()
	// 光带扫到第 3 个字：后两个字仍是遮罩块（正是用户看到的停在「123 **」）
	m.thinkAnim = thinkAnim{kind: thinkAnimZh, frame: 2 * zhFramesPerStep}
	if !containsSpinnerLine(m.View().Content) {
		t.Fatal("时钟在跑时应显示工作行")
	}
	withLine := m.vpHeight()

	// 模型离开：工作态与时钟一并停（本轮结束的各条路径都走 disarmAnim）
	m.thinking, m.streaming = false, false
	m.disarmAnim()

	view := m.View().Content
	if containsSpinnerLine(view) {
		t.Fatalf("时钟停后工作行必须整行消失，不能留下冻住的帧: %q", view)
	}
	if got := m.vpHeight(); got != withLine+thinkingRow {
		t.Fatalf("工作行消失后 viewport 应收回那一行（%d → %d），否则界面留一条空行", withLine, got)
	}
}

// TestDisarmedClockStaysDown 停表换代后，在途的旧 tick 到达时必须什么都不做：
// 旧链不能把时钟悄悄复活（复活会与新一轮的链并行，动画走快一倍）。
func TestDisarmedClockStaysDown(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.thinking = true
	m.armAnim()
	stale := m.animGen
	m.disarmAnim()

	// 即使又回到工作态，旧代 tick 也不得续链、不得置回时钟标志
	m.thinking = true
	if _, cmd := m.Update(animTickMsg{gen: stale}); cmd != nil {
		t.Fatal("旧代 tick 不应续链")
	}
	if m.animTicking {
		t.Fatal("旧代 tick 不应把时钟标志置回 true")
	}
	// 新一轮自己点火才恢复工作行
	m.armAnim()
	if !containsSpinnerLine(m.View().Content) {
		t.Fatal("重新点火后工作行应回来")
	}
}

// TestArmAnimKeepsSingleChain 重复点火（startTurn 之后紧跟 first 帧）只保留一条链：
// 旧链作废，新链一拍只推进一帧。
func TestArmAnimKeepsSingleChain(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.thinking = true
	m.armAnim()
	first := m.animGen
	m.armAnim() // 重复点火
	if _, cmd := m.Update(animTickMsg{gen: first}); cmd != nil {
		t.Fatal("旧链 tick 不应续链（同一时刻两条链会让动画走快）")
	}
	before := m.thinkAnim.frame
	m.Update(animTickMsg{gen: m.animGen})
	if got := m.thinkAnim.frame; got != before+1 {
		t.Fatalf("新链一拍应恰好推进一帧: %d → %d", before, got)
	}
}

// TestInterruptHidesWorkingLine Ctrl+C 中断＝本轮结束：工作行立即消失，
// 不等下一拍 tick 自己发现（否则会先冻在半途帧上最多 100ms）。
func TestInterruptHidesWorkingLine(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.thinking = true
	m.armAnim()
	m.thinkAnim = thinkAnim{kind: thinkAnimZh, frame: 2 * zhFramesPerStep}

	m.interruptTurn()
	if m.animTicking {
		t.Fatal("中断后时钟标志必须归位")
	}
	if view := m.View().Content; containsSpinnerLine(view) {
		t.Fatalf("中断后工作行应立即消失: %q", view)
	}
}
