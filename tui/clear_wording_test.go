package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"dsc/core"
)

// /clear 的描述曾写「清空聊天记录」，容易被读成「删除会话记录」——它其实只清屏。
// 这里既钉住文案（必须说明是清显示、且不得声称删除记录），也钉住行为边界。
func TestClearCommandWordingAndScope(t *testing.T) {
	// ① 菜单 hint 与 /help 文本口径一致，都不许再写成「清空聊天记录」
	var hint string
	for _, it := range slashCommands {
		if it.label == "/clear" {
			hint = it.hint
		}
	}
	if hint == "" {
		t.Fatal("菜单里找不到 /clear")
	}
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	help := m.buildHelpText()

	for name, text := range map[string]string{"菜单 hint": hint, "帮助文本": help} {
		if !strings.Contains(text, "清屏") || !strings.Contains(text, "保留") {
			t.Fatalf("%s 未说明「只清显示、记录保留」: %q", name, text)
		}
		if strings.Contains(text, "清空聊天记录") {
			t.Fatalf("%s 仍写着有歧义的「清空聊天记录」: %q", name, text)
		}
	}

	// ② 行为：只清显示缓冲，不碰模型历史注入设置
	ag := &stubAgent{}
	m = New(ag, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.appendMessage("历史正文")
	m.historyAll = []*core.AgentDebugMessage{{Role: "user", Content: "早前的问题"}}
	m.historyPage = 1
	m.historyLoaded = 1

	handled, _ := m.runSlashCommand("/clear")
	if !handled {
		t.Fatal("/clear 应被识别为内建命令")
	}
	if len(m.lines) != 0 {
		t.Fatalf("清屏后仍有正文: %v", m.lines)
	}
	if m.historyAll != nil || m.historyLoaded != 0 || m.historyPage != 0 {
		t.Fatalf("回放缓冲未复位: all=%v loaded=%d page=%d", m.historyAll != nil, m.historyLoaded, m.historyPage)
	}
	if len(ag.historyCalls) != 0 {
		t.Fatalf("/clear 不应改 history_injection（那是 /settings history 的职责）: %v", ag.historyCalls)
	}
	if m.input.Value() != "" {
		t.Fatalf("清屏后输入框应清空, got %q", m.input.Value())
	}
}
