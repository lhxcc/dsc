package tui

import (
	"context"
	"strings"
	"testing"

	"dsc/core"
	"dsc/cron"
)

// TestCronDoneNoticeRespectsJobNotify 定时任务落定是否叫醒模型，由**任务自己声明**
// （Job.Notify）：cron 是长期循环的程序性工作，一律通知会让分钟级任务每轮消耗一次
// 唤醒（一轮 LLM 调用），一律不通知又会让真正要跟进的产出没人知道。
func TestCronDoneNoticeRespectsJobNotify(t *testing.T) {
	m := New(&stubAgent{frames: []*core.RunStreamResponse{{Status: "success"}}}, nil, context.Background(), "m", "minimal", 131072)

	// ① notify=false（默认）：静默——不唤醒、不排队、不打印
	if _, cmd := m.Update(cronDoneMsg{job: cron.Job{ID: "c1", Name: "housekeeping", LastStatus: "success"}}); cmd != nil {
		t.Fatal("未声明 notify 的任务不应唤醒模型")
	}
	if m.thinking || len(m.pendingWakeups) != 0 {
		t.Fatalf("未声明 notify 的任务不应排队：thinking=%v pending=%v", m.thinking, m.pendingWakeups)
	}

	// ② notify=true 且空闲：立即开一轮（通知只喂模型，不进对话区）
	if _, cmd := m.Update(cronDoneMsg{job: cron.Job{ID: "c2", Name: "daily-report", Notify: true, LastStatus: "success"}}); cmd == nil || !m.thinking {
		t.Fatalf("notify 任务在空闲时应立即唤醒：thinking=%v cmd=%v", m.thinking, cmd)
	}
	if len(m.pendingWakeups) != 0 {
		t.Fatalf("唤醒应消费掉通知，仍排队: %v", m.pendingWakeups)
	}
	if full := strings.Join(m.lines, "\n"); strings.Contains(full, "cron job c2") {
		t.Fatalf("通知不应打印到前端: %q", full)
	}
}

// TestCronDoneNoticeInjectsWhileBusy 忙碌时走注入（下一次 LLM 迭代就能看到），
// 不排队等回合结束——否则又变成「早就完成了，过后才集中爆发」。
func TestCronDoneNoticeInjectsWhileBusy(t *testing.T) {
	m := New(&stubAgent{frames: []*core.RunStreamResponse{{Status: "success"}}}, nil, context.Background(), "m", "minimal", 131072)
	m.thinking = true
	_, cmd := m.Update(cronDoneMsg{job: cron.Job{ID: "c3", Name: "patrol", Notify: true, LastStatus: "error"}})
	if cmd == nil {
		t.Fatal("忙碌时应返回注入命令")
	}
	if len(m.pendingWakeups) != 0 {
		t.Fatalf("通知已交给注入通道，不应留在队列: %v", m.pendingWakeups)
	}
	// 注入失败 → 回落队列（不静默丢弃）
	m.Update(jobNoticeInjectedMsg{text: "cron notice", err: context.Canceled})
	if len(m.pendingWakeups) != 1 {
		t.Fatalf("注入失败应回落队列，got %v", m.pendingWakeups)
	}
}

// TestCronNoticeNotRenderedInReplay 定时任务通知同样是「只给模型」的内容：
// 实时与回放都不渲染，否则重启后会看到一屏没有来龙去脉的 cron 文本。
func TestCronNoticeNotRenderedInReplay(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.width = 80
	notice := renderCronDoneNotice(cron.Job{ID: "c9", Name: "daily", LastStatus: "success"})

	// 纯通知 → 不产生任何块
	if blocks := m.renderHistoryBlocks([]*core.AgentDebugMessage{{Role: "user", Content: notice}}); len(blocks) != 0 {
		t.Fatalf("纯 cron 通知不应渲染，got %#v", blocks)
	}
	// 通知 + 用户正文 → 只剩用户气泡
	blocks := m.renderHistoryBlocks([]*core.AgentDebugMessage{{Role: "user", Content: notice + "\n\n用户正文"}})
	if len(blocks) != 1 {
		t.Fatalf("应只剩 1 个用户气泡, got %d", len(blocks))
	}
	if strings.Contains(blocks[0], "cron job c9") {
		t.Fatalf("用户气泡不应含通知文本: %q", blocks[0])
	}
	if !strings.Contains(blocks[0], "用户正文") {
		t.Fatalf("用户气泡应含用户正文: %q", blocks[0])
	}
}
