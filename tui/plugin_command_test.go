package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestSplitSlashCommand 命令拆分：名字与参数分开，参数原文保留（不清洗空白），
// 空命令不算命令。
func TestSplitSlashCommand(t *testing.T) {
	cases := []struct {
		in, name, args string
		ok             bool
	}{
		{"/context", "context", "", true},
		{"/context --json", "context", "--json", true},
		{"/a  b", "a", "b", true},
		{"  /x  y  ", "x", "y", true},
		{"/", "", "", false},
		{"", "", "", false},
		{"no-slash", "no-slash", "", true},
	}
	for _, c := range cases {
		name, args, ok := splitSlashCommand(c.in)
		if name != c.name || args != c.args || ok != c.ok {
			t.Errorf("splitSlashCommand(%q) = (%q,%q,%v), want (%q,%q,%v)",
				c.in, name, args, ok, c.name, c.args, c.ok)
		}
	}
}

// TestNoBuiltinPluginCommands 核心/TUI 里不得内置任何插件命令名：
// 没有插件声明时，帮助与补全菜单里都不该出现某个具体命令。
//
// 这是分层约束的回归测试——曾经 /context、/acp 被写死在 TUI 的 switch 里。
func TestNoBuiltinPluginCommands(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)

	help := ansi.Strip(m.buildHelpText())
	for _, banned := range []string{"/context", "/acp"} {
		if strings.Contains(help, banned) {
			t.Fatalf("帮助文本不应内置插件命令 %q：\n%s", banned, help)
		}
	}
	for _, it := range m.slashCommandItems() {
		if it.label == "/context" || it.label == "/acp" {
			t.Fatalf("补全菜单不应内置插件命令 %q", it.label)
		}
	}
	// 未知命令原样返回 false：交给上层按普通输入处理，而不是假装处理过
	if handled, _ := m.runSlashCommand("/context"); handled {
		t.Fatal("没有插件声明时 /context 不应被视为已处理")
	}
}

// TestPluginCommandMsgRenders 插件命令的输出原样展示；出错时以错误样式追加，
// 有 text 时两者都显示（插件可能给出半成品输出 + 失败原因）。
func TestPluginCommandMsgRenders(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)

	m.Update(pluginCommandMsg{name: "context", text: "占用: 42%"})
	view := ansi.Strip(strings.Join(m.lines, ""))
	if !strings.Contains(view, "DSC · /context") || !strings.Contains(view, "占用: 42%") {
		t.Fatalf("应显示命令标题与插件输出：%s", view)
	}

	m.lines = nil // 清空后测错误分支
	m.Update(pluginCommandMsg{name: "context", err: errors.New("插件未实现")})
	view = ansi.Strip(strings.Join(m.lines, ""))
	if !strings.Contains(view, "插件未实现") {
		t.Fatalf("出错时应显示原因：%s", view)
	}

	m.lines = nil
	m.Update(pluginCommandMsg{name: "context", text: "部分结果", err: errors.New("超时")})
	view = ansi.Strip(strings.Join(m.lines, ""))
	if !strings.Contains(view, "部分结果") || !strings.Contains(view, "超时") {
		t.Fatalf("有输出又有错误时两者都应显示：%s", view)
	}
}

// TestPluginCommandsEmptyWithoutManager 无管理器时命令表为空（帮助/补全不得崩）。
func TestPluginCommandsEmptyWithoutManager(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	if got := m.pluginCommands(); len(got) != 0 {
		t.Fatalf("无 manager 时应为空，got %+v", got)
	}
}
