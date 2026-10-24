package tui

import (
	"context"
	"strings"
	"testing"
)

// 输入框占位符只讲「在这里能做什么」，不重复按键提示——按键由底部状态栏统一承担。
//
// 回归背景：占位符曾写「输入消息，回车发送，Shift+Enter/Ctrl+J 换行，Ctrl+Q 退出」，
// 与状态栏右侧「Enter 发送 · 选中复制 · Ctrl+J 换行 · Ctrl+Q 退出」重复，且两处措辞
// 还不一致（回车 vs Enter）。腾出的位置改为提示 @ 引用（输入框独有的能力）。
func TestComposerPlaceholderHintsAtReference(t *testing.T) {
	m := New(nil, nil, context.Background(), "m", "standard", 0)

	ph := m.input.Placeholder
	if !strings.Contains(ph, "@") {
		t.Fatalf("占位符应提示 @ 引用，实际 %q", ph)
	}
	if !strings.Contains(ph, "文件") || !strings.Contains(ph, "图片") {
		t.Fatalf("占位符应说明 @ 可引用文件或图片，实际 %q", ph)
	}
	// 按键提示不得重复：这些键已由状态栏给出（重复既冗长又会两边漂移）
	for _, dup := range []string{"Ctrl+Q", "Ctrl+J", "回车发送", "Enter 发送", "Shift+Enter"} {
		if strings.Contains(ph, dup) {
			t.Fatalf("占位符不应重复按键提示 %q（状态栏已有），实际 %q", dup, ph)
		}
	}
	// 占位符要短：输入框宽度有限，过长会折成两行（此前就是两行）
	if n := len([]rune(ph)); n > 20 {
		t.Fatalf("占位符过长（%d 字），会在窄终端折行：%q", n, ph)
	}
}

// 状态栏仍是按键提示的唯一出处，避免有人以后把提示又搬回输入框。
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
