package tui

import (
	"context"
	"strings"
	"testing"
)

// TestSettingsHistoryCommand 校验 /settings history N|off|unlimited 触发 agent 设置，
// 并给出可读回显。
func TestSettingsHistoryCommand(t *testing.T) {
	cases := []struct {
		cmd   string
		want  int
		label string
	}{
		{"/settings history 10", 10, "最近 10 条"},
		{"/settings history off", -1, "不注入恢复历史"},
		{"/settings history -1", -1, "不注入恢复历史"},
		{"/settings history 0", 0, "不限制（默认）"},
		{"/settings history unlimited", 0, "不限制（默认）"},
		{"/settings history on", 0, "不限制（默认）"},
	}
	for _, c := range cases {
		ag := &stubAgent{}
		m := New(ag, nil, context.Background(), "m", "minimal", 131072)
		handled, cmd := m.runSlashCommand(c.cmd)
		if !handled {
			t.Fatalf("%s 应被处理", c.cmd)
		}
		if cmd != nil {
			t.Fatalf("%s cmd = %v, want nil", c.cmd, cmd)
		}
		if len(ag.historyCalls) != 1 || ag.historyCalls[0] != c.want {
			t.Fatalf("%s historyCalls = %v, want [%d]", c.cmd, ag.historyCalls, c.want)
		}
		full := strings.Join(m.lines, "\n")
		if !strings.Contains(full, c.label) {
			t.Fatalf("%s 应回显 %q, got: %q", c.cmd, c.label, full)
		}
	}
}

// TestSettingsHistoryInvalid 非法参数给出用法提示且不触发 agent。
func TestSettingsHistoryInvalid(t *testing.T) {
	ag := &stubAgent{}
	m := New(ag, nil, context.Background(), "m", "minimal", 131072)
	handled, _ := m.runSlashCommand("/settings history abc")
	if !handled {
		t.Fatal("/settings 应被处理")
	}
	if len(ag.historyCalls) != 0 {
		t.Fatalf("非法参数不应触发 agent, historyCalls = %v", ag.historyCalls)
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "用法") {
		t.Fatalf("应显示用法提示, got: %q", full)
	}
}

// TestParseHistoryInjection 参数解析边界（DSC 三值：负值=不注入、0=未定义即默认、N 条）。
func TestParseHistoryInjection(t *testing.T) {
	ok := map[string]int{
		"": 0, "on": 0, "unlimited": 0, "default": 0, "0": 0,
		"off": -1, "-1": -1, "-5": -1, // 任意负值都表示不注入
		"1": 1, "50": 50,
	}
	for in, want := range ok {
		got, err := parseHistoryInjection(in)
		if err != nil || got != want {
			t.Errorf("parseHistoryInjection(%q) = (%d, %v), want (%d, nil)", in, got, err, want)
		}
	}
	for _, in := range []string{"abc", "1.5"} {
		if _, err := parseHistoryInjection(in); err == nil {
			t.Errorf("parseHistoryInjection(%q) 应报错", in)
		}
	}
}
