package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// 输入框占位符讲「输入框独有、且不写出来没人会发现」的事：@ 引用，以及选区与
// 复制的键位。别写「输入消息」这种自明信息（输入框就是用来输入的），把宽度让给键位。
//
// 回归背景：占位符曾是「输入消息；@ 引用文件或图片」。删掉前四个字后腾出的空间给了
// Shift+←/→（选字）与复制键——这两个键没有其它入口能发现它们。复制只提示
// Ctrl+Shift+C：Ctrl+C 同样能复制（实现见键处理），但运行期它是中断键，明示它等于
// 教用户误按一下终止任务，故只在 /help 里说明其语义。
func TestComposerPlaceholderTeachesComposerOnlySkills(t *testing.T) {
	m := New(nil, nil, context.Background(), "m", "standard", 0)

	ph := m.input.Placeholder
	for _, want := range []string{"@", "文件", "图片", "Shift+←/→", "选字", "Ctrl+Shift+C", "复制"} {
		if !strings.Contains(ph, want) {
			t.Fatalf("占位符应含 %q，实际 %q", want, ph)
		}
	}
	for _, no := range []string{"输入消息", "回车发送", "Enter 发送"} {
		if strings.Contains(ph, no) {
			t.Fatalf("占位符不应写自明/重复信息 %q，实际 %q", no, ph)
		}
	}
	// 不明示 Ctrl+C：运行期它是中断键，摆在占位符里会诱导误按终止任务
	if strings.Contains(ph, "Ctrl+C") {
		t.Fatalf("占位符不应明示 Ctrl+C（运行期是中断键），实际 %q", ph)
	}
}

// 占位符必须一行放得下：折行会把输入区顶高一行（此前就是这样），窄终端宁可截断
// 也不要换行。输入框内容宽 = 终端宽度 - 8 - 提示符 2 列，占位符 53 列，
// 故 64 列起可完整显示；更窄只截断尾部，不换行。
func TestComposerPlaceholderStaysOnOneLine(t *testing.T) {
	for _, w := range []int{120, 80, 64} {
		m := New(nil, nil, context.Background(), "m", "standard", 0)
		m.Update(tea.WindowSizeMsg{Width: w, Height: 30})

		lines := strings.Split(ansi.Strip(m.composerView()), "\n")
		if rows := len(lines) - 2; rows != 1 { // 去掉上/下边框
			t.Fatalf("width=%d 时输入区有 %d 行内容（占位符折行了）", w, rows)
		}
		if body := ansi.Strip(lines[1]); !strings.Contains(body, "Ctrl+Shift+C 复制") {
			t.Fatalf("width=%d 时占位符被截断，看不到复制键位: %q", w, body)
		}
	}

	// 更窄的终端：允许截断尾部，但必须仍是一行
	for _, w := range []int{60, 50} {
		m := New(nil, nil, context.Background(), "m", "standard", 0)
		m.Update(tea.WindowSizeMsg{Width: w, Height: 30})
		if lines := strings.Split(ansi.Strip(m.composerView()), "\n"); len(lines)-2 != 1 {
			t.Fatalf("width=%d 时占位符折行了（应为截断）", w)
		}
	}
}

// 状态栏仍承担其余按键提示，避免有人以后把「发送/换行/退出」又搬回输入框。
func TestStatusBarCarriesKeyHints(t *testing.T) {
	m := New(nil, nil, context.Background(), "m", "standard", 0)
	m.width = 120
	line := m.statusBar()
	for _, want := range []string{"Enter 发送", "Ctrl+J 换行", "Ctrl+Q 退出"} {
		if !strings.Contains(line, want) {
			t.Errorf("状态栏应保留按键提示 %q，实际 %q", want, line)
		}
	}
}
