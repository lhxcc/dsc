package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"dsc/core"
	"dsc/jobs"
)

// wakeSnap 便捷构造完成快照。
func wakeSnap(id string) jobs.JobSnapshot {
	return jobs.JobSnapshot{ID: id, Kind: "workflow", Label: "x", Status: jobs.StatusCompleted}
}

func TestWakeup(t *testing.T) {
	m := New(&stubAgent{frames: []*core.RunStreamResponse{{Status: "success"}}}, nil, context.Background(), "m", "minimal", 131072)

	// 空闲时收到完成通知 → 自动开启一轮，预算扣减，通知清空
	_, cmd := m.Update(jobDoneMsg{snapshot: wakeSnap("workflow-1")})
	if !m.thinking {
		t.Fatal("idle wakeup should start a turn")
	}
	if cmd == nil {
		t.Fatal("wakeup should return a start cmd")
	}
	if m.wakeBudget != maxConsecutiveWakes-1 {
		t.Fatalf("budget = %d, want %d", m.wakeBudget, maxConsecutiveWakes-1)
	}
	if len(m.pendingWakeups) != 0 {
		t.Fatalf("notifications should be consumed by wakeup, got %+v", m.pendingWakeups)
	}
	// 通知是给模型的唤醒信号，**不打印到对话区**（此前渲染成 dim 系统提示，
	// 用户看到一屏 background job 噪音）。
	if full := strings.Join(m.lines, "\n"); strings.Contains(full, "background job workflow-1") {
		t.Fatalf("notice 不应打印到前端, got: %q", full)
	}

	// 忙碌时收到通知 → 排队，不自动发
	m.thinking = false // 上一轮已结束
	m.streaming = true // 模拟新一轮忙碌
	_, cmd = m.Update(jobDoneMsg{snapshot: wakeSnap("workflow-2")})
	if cmd != nil {
		t.Fatal("busy should not auto-wake")
	}
	if len(m.pendingWakeups) != 1 {
		t.Fatalf("busy notification should queue, got %+v", m.pendingWakeups)
	}
	m.streaming = false

	// 预算耗尽 → 通知排队，不自动发
	m.wakeBudget = 0
	_, cmd = m.Update(jobDoneMsg{snapshot: wakeSnap("workflow-3")})
	if cmd != nil || m.thinking {
		t.Fatal("exhausted budget should not wake")
	}
	if len(m.pendingWakeups) != 2 {
		t.Fatalf("queued notifications = %d, want 2", len(m.pendingWakeups))
	}

	// 用户消息 → 前置注入排队通知 + 恢复预算
	m.input.SetValue("继续")
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.wakeBudget != maxConsecutiveWakes {
		t.Fatalf("user message should restore budget, got %d", m.wakeBudget)
	}
	if len(m.pendingWakeups) != 0 {
		t.Fatalf("notifications should be injected, got %+v", m.pendingWakeups)
	}
}

// TestSplitJobDoneNoticesReplay 重放时把用户消息里前置的后台通知拆出来：通知按
// 系统提示、用户正文按用户气泡，二者不粘连。label 含换行（heredoc）也能匹配。
func TestSplitJobDoneNoticesReplay(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.width = 80

	n1 := renderJobDoneNotice(jobs.JobSnapshot{
		ID: "shell-2", Kind: "shell",
		Label: "shell: cd /workspace && python - <<'PY'\np = 'x'", Status: jobs.StatusCompleted,
	})
	n2 := renderJobDoneNotice(jobs.JobSnapshot{
		ID: "shell-3", Kind: "shell",
		Label: "shell: cd /workspace && ./build", Status: jobs.StatusCompleted,
	})
	content := n1 + "\n\n" + n2 + "\n\n量这条边际成本曲线"

	// 完成通知是给模型的唤醒信号，不当用户内容渲染：回放只剩用户气泡本身。
	// （旧行为是把它们渲染成 dim 系统提示，重启后会看到一屏 background job 噪音。）
	blocks := m.renderHistoryBlocks([]*core.AgentDebugMessage{{Role: "user", Content: content}})
	if len(blocks) != 1 {
		t.Fatalf("应只剩 1 个用户气泡（通知不打印）, got %d: %#v", len(blocks), blocks)
	}
	if !strings.Contains(blocks[0], "量这条边际成本曲线") {
		t.Fatalf("用户气泡应含用户正文, got %q", blocks[0])
	}
	if strings.Contains(blocks[0], "background job") {
		t.Fatalf("用户气泡不应含通知文本, got %q", blocks[0])
	}
}

// 纯通知（无用户正文）不得产生任何块，否则回放会出现一屏无来龙去脉的噪声。
func TestReplayPureNoticeRendersNothing(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.width = 80
	n := renderJobDoneNotice(jobs.JobSnapshot{ID: "shell-9", Kind: "shell", Label: "shell: make", Status: jobs.StatusCompleted})
	if blocks := m.renderHistoryBlocks([]*core.AgentDebugMessage{{Role: "user", Content: n}}); len(blocks) != 0 {
		t.Fatalf("纯通知不应渲染任何块, got %#v", blocks)
	}
}

// TestRenderJobDoneNoticeNoDupKind Label 已含 kind 前缀时不再重复（避免 shell: shell:）。
func TestRenderJobDoneNoticeNoDupKind(t *testing.T) {
	s := renderJobDoneNotice(jobs.JobSnapshot{ID: "shell-1", Kind: "shell", Label: "shell: ls", Status: jobs.StatusCompleted})
	if strings.Contains(s, "shell: shell:") {
		t.Fatalf("不应出现重复 kind 前缀: %q", s)
	}
	if !strings.Contains(s, "(shell: ls)") {
		t.Fatalf("应为 (shell: ls), got %q", s)
	}
}
