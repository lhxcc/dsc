package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
)

// TestCaretDiagnosticFlagsInvisibleRunes /caret 要能把「看不见的可疑字符」指出来：
// 光标压在汉字半格上时，元凶通常是零宽字符、变体选择符、emoji 连字或字体缺字的
// 星光平面字符——它们在屏幕上不可见，只能靠逐字符反查定位。
func TestCaretDiagnosticFlagsInvisibleRunes(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.input.Focus()

	// 纯中文 + ASCII：不应有任何可疑字符
	m.input.SetValue("中文abc 123")
	if got := m.caretDiagnostic(); !strings.Contains(got, "可疑字符：无") {
		t.Fatalf("纯中文/ASCII 不应报可疑字符:\n%s", got)
	}

	// 变体选择符（把 ☺ 变成彩色 emoji）、零宽空格、星光平面字符：都要被点名
	m.input.SetValue("a\uFE0Fb\u200Bc\U0001F468d")
	got := m.caretDiagnostic()
	for _, want := range []string{"U+FE0F", "U+200B", "U+1F468"} {
		if !strings.Contains(got, want) {
			t.Fatalf("诊断应列出 %s:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "可疑字符：\n") {
		t.Fatalf("应进入可疑字符分支:\n%s", got)
	}
}

// TestCaretDiagnosticReportsGeometry /caret 同时给出光标坐标与模型宽度，便于与
// 屏幕上的实际位置对照（差几列一眼可见）。
func TestCaretDiagnosticReportsGeometry(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.input.Focus()
	m.input.SetValue("你好")
	m.input.CursorEnd()

	got := m.caretDiagnostic()
	// 2 个全角字 = 4 列；光标 X = padding 1 + 提示符 2 + 4 = 7
	if !strings.Contains(got, "模型宽度 4 列") {
		t.Fatalf("应报告模型宽度 4 列:\n%s", got)
	}
	if !strings.Contains(got, "光标：屏幕 X=7") {
		t.Fatalf("应报告光标 X=7:\n%s", got)
	}
}
