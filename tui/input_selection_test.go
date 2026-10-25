package tui

import (
	"context"
	"testing"

	"charm.land/bubbletea/v2"
)

// newInputModel 构造一个已就绪、输入框聚焦、光标在末尾的模型（选区相关用例共用）。
func newInputModel(t *testing.T, value string) *Model {
	t.Helper()
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.input.Focus()
	m.input.SetValue(value)
	m.input.CursorEnd()
	return m
}

// TestInputSelectionShiftArrows 输入框支持 Shift+←/→ 选字（bubbles v2.2.1 起
// textarea 自带选区），取走选区后输入内容不受影响、选区收起。
func TestInputSelectionShiftArrows(t *testing.T) {
	m := newInputModel(t, "你好abc")

	for i := 0; i < 3; i++ {
		m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	}
	if !m.input.HasSelection() {
		t.Fatal("Shift+← 应建立选区")
	}
	if got := m.input.SelectedText(); got != "abc" {
		t.Fatalf("选中内容 = %q, want %q", got, "abc")
	}

	// takeInputSelection 是复制动作的前半段（不碰系统剪贴板），便于测试
	if got := m.takeInputSelection(); got != "abc" {
		t.Fatalf("takeInputSelection = %q, want %q", got, "abc")
	}
	if m.input.HasSelection() {
		t.Fatal("取走选区后应收起（否则 Ctrl+C 会一直停在复制语义上）")
	}
	if m.input.Value() != "你好abc" {
		t.Fatalf("选区操作不得改动输入内容: %q", m.input.Value())
	}
}

// TestInputSelectionShiftRight 反方向也要能选：Shift+→ 从行首向右选中。
func TestInputSelectionShiftRight(t *testing.T) {
	m := newInputModel(t, "你好abc")
	m.input.CursorStart()

	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight, Mod: tea.ModShift})
	if got := m.input.SelectedText(); got != "你好" {
		t.Fatalf("Shift+→ 两次应选中 %q，实际 %q", "你好", got)
	}
}

// TestInputSelectAll Ctrl+A 全选输入框内容。
func TestInputSelectAll(t *testing.T) {
	m := newInputModel(t, "你好abc")

	m.Update(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
	if !m.input.HasSelection() {
		t.Fatal("Ctrl+A 应全选")
	}
	if got := m.input.SelectedText(); got != "你好abc" {
		t.Fatalf("全选内容 = %q, want %q", got, "你好abc")
	}
}

// TestInputSelectionTypingReplaces 选区上直接键入替换选中内容（编辑器常规行为），
// 不能出现「选中了却插在光标处」这种半套行为。
func TestInputSelectionTypingReplaces(t *testing.T) {
	m := newInputModel(t, "你好abc")

	for i := 0; i < 3; i++ {
		m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	}
	m.Update(tea.KeyPressMsg{Code: 'X', Text: "X"})

	if got := m.input.Value(); got != "你好X" {
		t.Fatalf("键入应替换选区，got %q", got)
	}
	if m.input.HasSelection() {
		t.Fatal("替换后选区应消失")
	}
}

// TestInputSelectionEscClears 选区上按 Esc 取消选区，输入内容不受影响。
func TestInputSelectionEscClears(t *testing.T) {
	m := newInputModel(t, "你好abc")
	for i := 0; i < 3; i++ {
		m.Update(tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModShift})
	}
	if !m.input.HasSelection() {
		t.Fatal("前置条件：应有选区")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.input.HasSelection() {
		t.Fatal("Esc 应取消选区")
	}
	if got := m.input.Value(); got != "你好abc" {
		t.Fatalf("取消选区不得改动内容，got %q", got)
	}
}

// TestCtrlShiftCDoesNotInterrupt Ctrl+Shift+C 是「安全」复制键：即使本轮正在运行，
// 也不得被当成中断（用户误在任务跑到一半时按它，不该把任务终止掉）。无选区时它
// 什么都不做——这正是占位符只提示这个键的原因。
func TestCtrlShiftCDoesNotInterrupt(t *testing.T) {
	m := newInputModel(t, "随手打的一点内容")
	m.thinking = true // 模拟本轮运行中

	m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift})
	if !m.thinking {
		t.Fatal("Ctrl+Shift+C 不应中断正在运行的一轮")
	}
	if got := m.input.Value(); got != "随手打的一点内容" {
		t.Fatalf("无选区时 Ctrl+Shift+C 不应改动输入，got %q", got)
	}
}
