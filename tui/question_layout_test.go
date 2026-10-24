package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"dsc/core"
	"dsc/userquestions"
	"github.com/charmbracelet/x/ansi"
)

// newSizedQuestionModel 建一个带明确终端尺寸、已显示问题的模型。
func newSizedQuestionModel(t *testing.T, width, height int, q userquestions.Question) *Model {
	t.Helper()
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.width, m.high = width, height
	m.Update(questionMsg{request: &userquestions.Request{Questions: []userquestions.Question{q}},
		answer: make(chan *userquestions.Answer, 1), err: make(chan error, 1)})
	if m.question == nil {
		t.Fatal("question 未进入待回答状态")
	}
	return m
}

// TestQuestionBoxGrowsToFit 中等长度的问题：框体自动长高容纳全部文字，
// 不出现滚动条、也不被裁掉。
func TestQuestionBoxGrowsToFit(t *testing.T) {
	body := strings.Join([]string{
		"第一行问题描述。",
		"第二行补充说明，稍微长一点，用来确认多行也能完整显示。",
		"第三行结论。",
	}, "\n")
	m := newSizedQuestionModel(t, 100, 40, userquestions.Question{
		ID: "q", Header: "确认", Question: body,
		Detail:  "这是模型给的细节说明，此前 TUI 完全不显示。",
		Options: []userquestions.Option{{Label: "是"}, {Label: "否"}},
	})
	view := ansi.Strip(m.questionView())
	for _, want := range []string{"第一行问题描述", "第二行补充说明", "第三行结论", "这是模型给的细节说明", "是", "否"} {
		if !strings.Contains(view, want) {
			t.Fatalf("框内应完整显示 %q，实际：\n%s", want, view)
		}
	}
	if strings.Contains(view, "PgUp/PgDn") {
		t.Fatalf("内容放得下时不应出现滚动提示：\n%s", view)
	}
	// 高度必须与内容一致（自动长高），且不超过预算
	rows := m.questionRows()
	if rows < 8 {
		t.Fatalf("框体应随内容长高，实际仅 %d 行", rows)
	}
	if rows > m.questionBudgetRows() {
		t.Fatalf("框体 %d 行超出终端预算 %d 行", rows, m.questionBudgetRows())
	}
}

// TestQuestionBoxWrapsLongLine 超长单行必须折行，不得横向溢出终端。
func TestQuestionBoxWrapsLongLine(t *testing.T) {
	long := strings.Repeat("这是一段很长的模型输出，用于验证自动折行。", 6)
	m := newSizedQuestionModel(t, 80, 40, userquestions.Question{ID: "q", Question: long})
	view := ansi.Strip(m.questionView())
	for _, line := range strings.Split(view, "\n") {
		if w := ansi.StringWidth(line); w > 80 {
			t.Fatalf("行宽 %d 超过终端宽度 80：%q", w, line)
		}
	}
	// 折行后内容应整段可见（含结尾）
	if !strings.Contains(view, "验证自动折行。") {
		t.Fatalf("折行后仍应看到全部文字：\n%s", view)
	}
}

// TestQuestionBoxScrollsWhenTallerThanScreen 内容高于终端时：框体吃满预算并给出
// 滚动提示，PgDn 能看到后面的内容（此前是直接溢出屏幕、开头被裁掉）。
func TestQuestionBoxScrollsWhenTallerThanScreen(t *testing.T) {
	var opts []userquestions.Option
	for i := 0; i < 30; i++ {
		opts = append(opts, userquestions.Option{Label: "选项 " + strings.Repeat("x", i+1)})
	}
	q := userquestions.Question{ID: "q", Header: "长列表", Question: strings.Repeat("很长的说明文字。", 20), Options: opts}
	m := newSizedQuestionModel(t, 80, 24, q)

	view := ansi.Strip(m.questionView())
	if !strings.Contains(view, "PgUp/PgDn") {
		t.Fatalf("内容溢出时应出现滚动提示：\n%s", view)
	}
	// 框体不得超出终端可用高度
	if rows := m.questionRows(); rows > m.questionBudgetRows() {
		t.Fatalf("框体 %d 行超出预算 %d 行", rows, m.questionBudgetRows())
	}
	if m.questionRows() >= m.high {
		t.Fatalf("框体 %d 行不应占满整个终端 %d 行", m.questionRows(), m.high)
	}

	// 初始能看到开头的说明文字
	if !strings.Contains(view, "很长的说明文字") {
		t.Fatalf("初始窗口应显示开头内容：\n%s", view)
	}
	// PgDn 之后应滚到别的位置（顶部内容变化）
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if after := ansi.Strip(m.questionView()); after == view {
		t.Fatalf("PgDn 后视图应变化（发生了滚动）")
	}
}

// TestQuestionCursorStaysVisible 选项多到超出窗口时，移动光标必须把它带进可见区，
// 否则用户会在看不见选项的情况下盲选。
func TestQuestionCursorStaysVisible(t *testing.T) {
	var opts []userquestions.Option
	for i := 0; i < 40; i++ {
		opts = append(opts, userquestions.Option{Label: "opt-" + strings.Repeat("y", i+1)})
	}
	m := newSizedQuestionModel(t, 80, 20, userquestions.Question{ID: "q", Question: "选一个", Options: opts})

	for i := 0; i < 20; i++ {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		lay := m.questionLayoutOf(m.question)
		cur := lay.optStart[m.question.cursor]
		if cur < lay.scroll || cur >= lay.scroll+lay.window {
			t.Fatalf("第 %d 次下移后选中项在窗口外：cur=%d scroll=%d window=%d", i+1, cur, lay.scroll, lay.window)
		}
	}
}

// TestQuestionOverlayFitsTerminal 整屏渲染的行数不得超过终端高度——这是用户看到的
// 最终结果：此前内容一长，覆盖层从顶部被 AltScreen 裁掉，问题开头完全读不到。
func TestQuestionOverlayFitsTerminal(t *testing.T) {
	var opts []userquestions.Option
	for i := 0; i < 25; i++ {
		opts = append(opts, userquestions.Option{Label: "选项 " + strings.Repeat("z", i+1)})
	}
	q := userquestions.Question{
		ID: "q", Header: "超长提问", Question: strings.Repeat("模型写了一长串说明。", 25), Options: opts,
	}
	for _, size := range [][2]int{{80, 24}, {100, 30}, {60, 20}, {120, 40}} {
		m := newSizedQuestionModel(t, size[0], size[1], q)
		m.ready = true
		view := m.View().Content
		lines := strings.Split(view, "\n")
		// 末行为空是常见结尾，按非空行计
		rows := len(lines)
		for rows > 0 && strings.TrimSpace(ansi.Strip(lines[rows-1])) == "" {
			rows--
		}
		if rows > size[1] {
			t.Fatalf("%dx%d：渲染 %d 行超出终端高度", size[0], size[1], rows)
		}
	}
}

// TestQuestionBellFallback 没有通知能力时退回终端响铃；有插件提供 notify 能力时
// 不重复响（插件已经响了）。
func TestQuestionBellFallback(t *testing.T) {
	// 无 manager（测试/早期）：保守地响铃，总比无声好
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	if !m.questionBellNeeded() {
		t.Fatal("无 manager 时应退回响铃")
	}

	// 有 manager 但没有插件提供 notify：响铃（插件不存在的情况）
	manager := core.NewManager(&core.ManagerConfig{})
	m2 := New(&stubAgent{}, manager, context.Background(), "m", "minimal", 131072)
	if manager.HasPluginProvidingCapability("notify") {
		t.Fatal("空 manager 不应报告 notify 能力")
	}
	if !m2.questionBellNeeded() {
		t.Fatal("无 notify 插件时应退回响铃")
	}
}
