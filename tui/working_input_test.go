package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestPasteAllowedWhileWorking 模型工作期间（thinking/streaming）粘贴仍写入输入框。
// 此前 PasteMsg 被 thinking/streaming 门控直接丢弃，与「工作态仍允许打字」不一致。
func TestPasteAllowedWhileWorking(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.thinking = true

	m.Update(tea.PasteMsg{Content: "pasted text"})

	if got := m.input.Value(); !strings.Contains(got, "pasted text") {
		t.Fatalf("工作态粘贴应写入输入框, got %q", got)
	}
}

// TestBodySelectionAllowedWhileWorking 模型工作期间仍可开始正文选区，并脱离底部
// 跟随（避免流式新内容把视口顶走）。
func TestBodySelectionAllowedWhileWorking(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.thinking = true
	m.pinnedToBottom = true

	model, _ := m.Update(tea.MouseClickMsg{X: 0, Y: 2, Button: tea.MouseLeft})
	m = model.(*Model)

	if !m.sel.active {
		t.Fatal("工作态点击正文应开始选区")
	}
	if m.pinnedToBottom {
		t.Fatal("工作态开始选区应脱离底部跟随")
	}
}
