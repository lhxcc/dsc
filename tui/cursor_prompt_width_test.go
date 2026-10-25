package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestComposerPromptMatchesCursorAssumption 提示符的列宽必须与光标算式里的假设一致。
//
// 真实光标的位置按「padding + 提示符 + 已输入文字到插入点的列宽」算出来
// （见 inputCursorAbs）。前缀里唯一由我们写死、而不是从文字量出来的就是提示符：
// `SetPromptFunc(2, ...)` 的 2 就是它。改了提示符文案却没改这个数，光标就会整体
// 偏移一到两列——中文输入时表现为「只选中半个字」。
//
// 顺带说明为什么监视这个：U+276F 的东亚宽度是 Neutral，若终端把它交给中文字体按
// 全角绘制，屏幕上它会占 2 列而不是 1 列；此时这条算式本身没错，错的是「终端实际
// 绘制的列宽」与「我们量到的列宽」不一致——那是字体层面的事，DSC 侧只能保证自己
// 的假设自洽（所以这里钉住 2）。
func TestComposerPromptMatchesCursorAssumption(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.input.Focus()

	const prompt = "❯ "
	if got := ansi.StringWidth(prompt); got != 2 {
		t.Fatalf("提示符 %q 的列宽 %d 与 SetPromptFunc(2, ...) 的假设不符", prompt, got)
	}

	line := strings.Split(ansi.Strip(m.input.View()), "\n")[0]
	if !strings.HasPrefix(line, prompt) {
		t.Fatalf("输入框首行应以 %q 开头，实际: %q", prompt, line)
	}

	// 光标在行首：X = 左侧 padding(1) + 提示符(2)
	m.input.CursorStart()
	cur := m.inputCursorAbs()
	if cur == nil {
		t.Fatal("inputCursorAbs 不应返回 nil")
	}
	if cur.X != 1+2 {
		t.Fatalf("行首光标 X = %d, want %d", cur.X, 1+2)
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
