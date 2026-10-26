package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// 结果卡的续行缩进必须与首行 gutter 同宽。
// 回归：connector 是 "  └ "（含 └，U+2514 占 3 字节），用 len() 得 6 当缩进，
// 续行就比首行多 2 格——实测首行从第 4 列、后续行从第 6 列，整块向右歪。
func TestResultGutterAlignment(t *testing.T) {
	cases := [][]string{
		{"x1", "x2", "x3"},             // 普通多行结果
		{"x1", "x2\n", ""},             // 末尾空行
		{"x1", "x2", "x3", "x4", "x5"}, // 多行进度日志
	}
	for _, lines := range cases {
		out := renderToolResult(strings.Join(lines, "\n"), false)
		for i, ln := range strings.Split(out, "\n") {
			idx := strings.Index(ln, "x")
			if idx < 0 {
				continue // 空行跳过
			}
			if w := lipgloss.Width(ln[:idx]); w != 4 {
				t.Fatalf("第 %d 行内容从第 %d 列开始，want 4（与 gutter 同宽）: %q", i+1, w, ln)
			}
		}
	}
}

// 卡片视图（renderViewSpec 路径）同样走 resultGutter，一并守住。
func TestResultGutterAlignInCardView(t *testing.T) {
	body := "x1\nx2"
	out := resultGutter(body)
	for i, ln := range strings.Split(out, "\n") {
		idx := strings.Index(ln, "x")
		if idx < 0 {
			continue
		}
		if w := lipgloss.Width(ln[:idx]); w != 4 {
			t.Fatalf("卡片第 %d 行从第 %d 列开始，want 4: %q", i+1, w, ln)
		}
	}
}
