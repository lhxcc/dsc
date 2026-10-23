package tui

import (
	"context"
	"io"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"dsc/userquestions"
)

// TestQuestionCustomInputThroughEventLoop 用真实 bubbletea 事件循环回归用户路径：
// 方向键移到「✎ 其他（手动输入）」→ Enter → 输入文本 → Enter 提交。
// 关键点是「模型思考中」这一真实状态（ask_user_question 阻塞在工具调用里），
// 按键必须仍然进入自定义输入而不是被运行态门控吞掉。
func TestQuestionCustomInputThroughEventLoop(t *testing.T) {
	pr, pw := io.Pipe()
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 4096)
	m.thinking = true
	p := tea.NewProgram(m, tea.WithInput(pr), tea.WithOutput(io.Discard), tea.WithoutRenderer())
	go func() { _, _ = p.Run() }()
	defer p.Quit()
	time.Sleep(150 * time.Millisecond)

	ansCh := make(chan *userquestions.Answer, 1)
	errCh := make(chan error, 1)
	p.Send(questionMsg{request: &userquestions.Request{Questions: []userquestions.Question{{
		ID:       "q",
		Question: "pick?",
		Options:  []userquestions.Option{{Label: "A"}, {Label: "B"}, {Label: "C"}},
	}}}, answer: ansCh, err: errCh})
	time.Sleep(150 * time.Millisecond)

	// ↓↓↓ Enter → 兜底项进入自定义输入
	if _, err := pw.Write([]byte("\x1b[B\x1b[B\x1b[B\r")); err != nil {
		t.Fatalf("write nav: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if m.question == nil || !m.question.customMode {
		t.Fatalf("customMode not active after selecting custom option")
	}

	if _, err := pw.Write([]byte("hello\r")); err != nil {
		t.Fatalf("write text: %v", err)
	}
	select {
	case ans := <-ansCh:
		if len(ans.Answers) != 1 || ans.Answers[0].Custom != "hello" {
			t.Fatalf("custom answer = %+v", ans)
		}
	case e := <-errCh:
		t.Fatalf("ask error: %v", e)
	case <-time.After(3 * time.Second):
		t.Fatalf("no answer delivered; customMode=%v input=%q",
			m.question != nil && m.question.customMode, m.input.Value())
	}
}

// TestQuestionPasteThroughEventLoop 真实事件循环下的括号粘贴：模型等待回答
// （thinking）期间粘贴，必须直接成为自定义回答，而不是被运行态门控丢弃。
func TestQuestionPasteThroughEventLoop(t *testing.T) {
	pr, pw := io.Pipe()
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 4096)
	m.thinking = true
	p := tea.NewProgram(m, tea.WithInput(pr), tea.WithOutput(io.Discard), tea.WithoutRenderer())
	go func() { _, _ = p.Run() }()
	defer p.Quit()
	time.Sleep(150 * time.Millisecond)

	ansCh := make(chan *userquestions.Answer, 1)
	errCh := make(chan error, 1)
	p.Send(questionMsg{request: &userquestions.Request{Questions: []userquestions.Question{{
		ID:       "q",
		Question: "pick?",
		Options:  []userquestions.Option{{Label: "A"}, {Label: "B"}},
	}}}, answer: ansCh, err: errCh})
	time.Sleep(150 * time.Millisecond)

	// 括号粘贴（终端 Cmd+V 的典型编码）
	if _, err := pw.Write([]byte("\x1b[200~Hello 世界\x1b[201~")); err != nil {
		t.Fatalf("write paste: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if m.question == nil || !m.question.customMode {
		t.Fatal("paste should enter custom input mode")
	}
	if got := m.input.Value(); got != "Hello 世界" {
		t.Fatalf("input = %q, want pasted text", got)
	}

	if _, err := pw.Write([]byte("\r")); err != nil {
		t.Fatalf("write enter: %v", err)
	}
	select {
	case ans := <-ansCh:
		if len(ans.Answers) != 1 || ans.Answers[0].Custom != "Hello 世界" {
			t.Fatalf("custom answer = %+v", ans)
		}
	case e := <-errCh:
		t.Fatalf("ask error: %v", e)
	case <-time.After(3 * time.Second):
		t.Fatalf("no answer delivered; input=%q", m.input.Value())
	}
}
