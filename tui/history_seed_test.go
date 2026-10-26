package tui

import (
	"context"
	"strings"
	"testing"

	"dsc/core"
)

// TestSeedHistoryRendersAndConsumes 启动历史在首个窗口尺寸后被渲染到正文顶部：
// 分隔提示 + user/assistant 内容都出现，pendingHistory 被清空，重复调用不再追加。
func TestSeedHistoryRendersAndConsumes(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.width = 80
	m.pendingHistory = []*core.AgentDebugMessage{
		{Role: "user", Content: "earlier question"},
		{Role: "assistant", Content: "earlier answer"},
	}

	m.seedHistory()

	if m.pendingHistory != nil {
		t.Fatalf("pendingHistory 应被清空, got %+v", m.pendingHistory)
	}
	full := strings.Join(m.lines, "\n")
	for _, want := range []string{"恢复的最近历史", "earlier question", "earlier answer"} {
		if !strings.Contains(full, want) {
			t.Fatalf("渲染结果缺少 %q, got: %q", want, full)
		}
	}

	// 幂等：再次调用不应重复追加（pendingHistory 已空）
	n := len(m.lines)
	m.seedHistory()
	if len(m.lines) != n {
		t.Fatalf("第二次 seedHistory 不应追加内容, lines %d → %d", n, len(m.lines))
	}
}

// TestSeedHistoryEmptyIsNoop 无历史时是空操作（新会话/注入关闭）。
func TestSeedHistoryEmptyIsNoop(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.width = 80
	m.seedHistory()
	if len(m.lines) != 0 {
		t.Fatalf("无历史不应渲染任何内容, got %+v", m.lines)
	}
}
