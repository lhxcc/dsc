package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestComposerPromptIsAscii 输入框提示符必须只含 ASCII。
//
// 真实光标的位置是算出来的：左侧 padding + 提示符列宽 + 已输入文字的列宽
// （见 inputCursorAbs）。提示符里只要有一个字形的**实际列宽**与 lipgloss 的测量
// 不一致，整行文字就会相对我们的模型整体偏移，光标随之落到中文的第二个单元格上
// ——看到的就是「中文只被选中一半」。
//
// 原来的「❯ 」（U+276F）正是这种字形：它的东亚宽度是 Neutral，多数字体没有这个
// 字形，终端回退到中文字体后按全角 2 列绘制，而我们算 1 列。ASCII 的 ">" 是
// Narrow，任何字体都是 1 列。
func TestComposerPromptIsAscii(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.input.Focus()
	m.input.SetValue("中文abc")
	m.input.CursorEnd()

	line := strings.Split(ansi.Strip(m.input.View()), "\n")[0]
	if !strings.HasPrefix(line, "> ") {
		t.Fatalf("输入框提示符应为 \"> \"，实际行首: %q", line)
	}
	for _, r := range line[:2] {
		if r > 0x7f {
			t.Fatalf("提示符含非 ASCII 字符 %q：其实际列宽不可控，会让光标整列偏移", r)
		}
	}
}

// TestCursorXMatchesRenderedPrefix 光标 X 必须恰好等于「padding + 提示符 +
// 已输入文字到插入点的列宽」。中英文混排、逐列移动时都要成立。
func TestCursorXMatchesRenderedPrefix(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.input.Focus()

	const value = `"O:/Agents/Strada/start-kvarn-kvmem.sh"]  较好性能的参数，`
	m.input.SetValue(value)
	runes := []rune(value)

	const promptWidth, padding = 2, 1
	for k := 0; k <= len(runes); k++ {
		m.input.SetCursorColumn(k)
		cur := m.inputCursorAbs()
		if cur == nil {
			t.Fatal("inputCursorAbs 不应返回 nil")
		}
		want := padding + promptWidth + ansi.StringWidth(string(runes[:k]))
		if cur.X != want {
			t.Fatalf("光标在第 %d 个字符后: X=%d, want %d（前缀 %q）", k, cur.X, want, string(runes[:k]))
		}
	}
}
