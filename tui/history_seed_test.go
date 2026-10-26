package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"dsc/core"
)

// TestSeedHistoryRendersTailWithoutDivider 首个窗口尺寸后只渲染完整历史的尾部
// historyPage 条；不再打印「恢复的最近历史」分隔提示。
func TestSeedHistoryRendersTailWithoutDivider(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.historyAll = []*core.AgentDebugMessage{
		{Role: "user", Content: "oldest question"},
		{Role: "assistant", Content: "older answer"},
		{Role: "user", Content: "recent question"},
		{Role: "assistant", Content: "recent answer"},
	}
	m.historyPage = 2
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	if m.historyLoaded != 2 {
		t.Fatalf("初始应加载尾部 2 条, got %d", m.historyLoaded)
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "recent question") || !strings.Contains(full, "recent answer") {
		t.Fatalf("应渲染最近两条, got: %q", full)
	}
	if strings.Contains(full, "oldest question") || strings.Contains(full, "older answer") {
		t.Fatalf("初始不应渲染更旧消息, got: %q", full)
	}
	if strings.Contains(full, "恢复的最近历史") {
		t.Fatalf("分隔提示应已移除, got: %q", full)
	}
}

// TestLoadOlderHistoryPrependsInOrder 滚到顶部时按页向前回填，且更旧的消息排在
// 更上方；读完后返回 false（不再有旧消息可读）。
func TestLoadOlderHistoryPrependsInOrder(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.historyAll = []*core.AgentDebugMessage{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: "a1"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
		{Role: "user", Content: "q3"},
		{Role: "assistant", Content: "a3"},
	}
	m.historyPage = 2
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	// 初始为 q3/a3；回填一页后应为 q2/a2/q3/a3
	if !m.loadOlderHistory() {
		t.Fatal("应能回填一页")
	}
	m.render()
	if m.historyLoaded != 4 {
		t.Fatalf("回填后 historyLoaded = %d, want 4", m.historyLoaded)
	}
	full := strings.Join(m.lines, "\n")
	if i, j := strings.Index(full, "q2"), strings.Index(full, "q3"); i < 0 || j < 0 || i > j {
		t.Fatalf("更旧的 q2 应排在 q3 上方, got: %q", full)
	}

	// 再回填一页 → 读到头 q1/a1/q2/a2/q3/a3
	if !m.loadOlderHistory() {
		t.Fatal("应能回填最后一页")
	}
	m.render()
	if m.historyLoaded != 6 {
		t.Fatalf("全部加载后 historyLoaded = %d, want 6", m.historyLoaded)
	}
	full = strings.Join(m.lines, "\n")
	if i, j := strings.Index(full, "q1"), strings.Index(full, "q2"); i < 0 || j < 0 || i > j {
		t.Fatalf("最旧的 q1 应排在最上方, got: %q", full)
	}

	// 已无更旧消息
	if m.loadOlderHistory() {
		t.Fatal("读到头后不应再回填")
	}
}

// TestLoadOlderHistoryNoopWithoutHistory 没有初始历史时（注入/显示关闭）不回填。
func TestLoadOlderHistoryNoopWithoutHistory(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.ready = true
	m.width = 80
	if m.loadOlderHistory() {
		t.Fatal("无历史时不应回填")
	}
}
