package acp

import "testing"

// TestFormatTokenCountUses1024 单位底数必须与 DSC 全局一致（1024）。
//
// 用 1000 会在 1M 窗口上打出 1.05M（1048576/1000/1000），而 TUI 与配置里都是
// 1048576 = 1M；同一个会话在两处显示不同的量，比数字本身不准更糟——之前就是这样。
func TestFormatTokenCountUses1024(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1023, "1023"},
		{1024, "1K"},
		{1536, "1K"},
		{39_936, "39K"},
		{430_080, "420K"},
		{1_000_000, "976K"}, // 1M 十进制不到 999K，仍按 K 显示（与 tui.shortTokens 同门限）
		{999 * 1024, "1.0M"},
		{1_048_576, "1.0M"},
		{1_500_000, "1.4M"},
	}
	for _, c := range cases {
		if got := FormatTokenCount(c.in); got != c.want {
			t.Errorf("FormatTokenCount(%d) = %q, want %q", c.in, got, c.want)
		}
	}

	// 反向守卫：1000 进制会给出的两个典型错值，不应再出现
	if got := FormatTokenCount(1_048_576); got == "1.05M" {
		t.Fatalf("1M 被按 1000 折算成 %q", got)
	}
	if got := FormatTokenCount(420_000); got == "420K" {
		t.Fatalf("420000 不是 420K（1024 底应为 410K），实际 %q", got)
	}
}
