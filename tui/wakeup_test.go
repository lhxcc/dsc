package tui

import (
	"context"
	"strings"
	"testing"

	"dsc/core"
	"dsc/jobs"
)

// wakeSnap 便捷构造完成快照。
func wakeSnap(id string) jobs.JobSnapshot {
	return jobs.JobSnapshot{ID: id, Kind: "workflow", Label: "x", Status: jobs.StatusCompleted}
}

func TestWakeup(t *testing.T) {
	m := New(&stubAgent{frames: []*core.RunStreamResponse{{Status: "success"}}}, nil, context.Background(), "m", "minimal", 131072)

	// ① 空闲时收到完成通知 → 立即开启一轮（不排队、不打印）
	_, cmd := m.Update(jobDoneMsg{snapshot: wakeSnap("workflow-1")})
	if !m.thinking {
		t.Fatal("idle wakeup should start a turn")
	}
	if cmd == nil {
		t.Fatal("wakeup should return a start cmd")
	}
	if len(m.pendingWakeups) != 0 {
		t.Fatalf("通知应被唤醒消费, got %+v", m.pendingWakeups)
	}
	// 通知只喂模型，不进对话区
	if full := strings.Join(m.lines, "\n"); strings.Contains(full, "background job workflow-1") {
		t.Fatalf("notice 不应打印到前端, got: %q", full)
	}

	// ② 忙碌时收到完成通知 → **立即注入当前回合**，不排队等回合结束
	m.thinking = false
	m.streaming = true
	_, cmd = m.Update(jobDoneMsg{snapshot: wakeSnap("workflow-2")})
	if cmd == nil {
		t.Fatal("忙碌时应返回注入命令（立即送达模型），而不是静默排队")
	}
	if len(m.pendingWakeups) != 0 {
		t.Fatalf("通知已交给注入通道，不应再留在队列, got %+v", m.pendingWakeups)
	}
	// 注入成功（stub 返回 nil）→ 队列保持为空
	_, _ = m.Update(jobNoticeInjectedMsg{text: "background job workflow-2 (workflow: x) finished [status: completed]. Read its output with job_output."})
	if len(m.pendingWakeups) != 0 {
		t.Fatalf("注入成功后不应回落队列, got %+v", m.pendingWakeups)
	}
	m.streaming = false

	// ③ 注入失败 → 回落队列（不静默丢弃），随后空闲可被唤醒补送
	m.thinking = false
	m.streaming = true
	_, _ = m.Update(jobDoneMsg{snapshot: wakeSnap("workflow-3")})
	_, cmd = m.Update(jobNoticeInjectedMsg{text: "notice-3", err: context.Canceled})
	if len(m.pendingWakeups) != 1 {
		t.Fatalf("注入失败应回落队列, got %+v", m.pendingWakeups)
	}
	// 此刻仍忙碌 → 补送命令为空；回合结束后 tryWakeup 才会发
	if cmd != nil && !m.thinking {
		t.Logf("补送命令: %v", cmd)
	}
	m.streaming = false
	_, _ = m.Update(jobDoneMsg{snapshot: wakeSnap("workflow-4")})
	if !m.thinking {
		t.Fatal("空闲时应重新唤醒并带上积压通知")
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

// TestDropNoticeAfterResultConsumed 模型已经把某个任务的终态结果拿到手，队列里
// 还没送出的那条完成通知必须作废——通知是「结果已就绪」的通讯，结果已上屏后
// 再补送只会让它重复读一遍。
func TestDropNoticeAfterResultConsumed(t *testing.T) {
	notices := []string{
		"background job shell-51cb632 (shell: go build ./...) finished [status: completed]. Read its output with job_output.",
		"background job shell-a1b2c3d (shell: go test ./...) finished [status: completed]. Read its output with job_output.",
	}
	got := dropNoticeFor(notices, "shell-51cb632")
	if len(got) != 1 || !strings.Contains(got[0], "shell-a1b2c3d") {
		t.Fatalf("只应删掉已消费的那条，got %v", got)
	}
	// 只有终态才等于「结果已经拿到手」；running 的读取不作废通知。
	if jobOutputTerminal("(no output yet)\n[status: running]") {
		t.Fatal("running 不应视为已拿到结果")
	}
	if !jobOutputTerminal("BUILD OK\n[status: completed]") {
		t.Fatal("completed 应视为已拿到结果")
	}
	// 参数解析：拿不到 job_id 时不动队列
	if id := jobIDFromToolArgs(`{"job_id":"shell-51cb632"}`); id != "shell-51cb632" {
		t.Fatalf("job_id 解析失败: %q", id)
	}
	if kept := dropNoticeFor(notices, jobIDFromToolArgs(`{}`)); len(kept) != len(notices) {
		t.Fatalf("无 job_id 时不应删除任何通知: %v", kept)
	}
}
