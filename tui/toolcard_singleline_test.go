package tui

import (
	"strings"
	"testing"
)

// TestRenderToolCallFoldsNewlines 多行命令进卡片前必须折叠成单行。
//
// 现实触发：模型把 `powershell -NoProfile -Command "<多行脚本>"` 作为 shell 参数，
// 参数里带真实换行。卡片是单行结构（"  ● Verb(arg)"），换行会让第二行从终端第 0 列
// 输出——左边距被撕开，整块看上去"向左歪"。
func TestRenderToolCallFoldsNewlines(t *testing.T) {
	// 用短命令，避免被 60 列截断掩盖折叠效果
	args := `{"command":"cd /workspace && pwsh -Command \"\nGet-Child\n\""}`
	got := renderToolCall("shell", args)
	if strings.Contains(got, "\n") {
		t.Fatalf("卡片标题不得含换行（会把缩进撕开）: %q", got)
	}
	if !strings.HasPrefix(got, "  ") {
		t.Fatalf("卡片应以两空格缩进起头: %q", got)
	}
	if !strings.Contains(got, "Get-Child") {
		t.Fatalf("命令正文应保留: %q", got)
	}
	if strings.Contains(got, `\n`) || strings.Contains(got, `\t`) {
		t.Fatalf("不得留下转义序列残渣: %q", got)
	}
}

// TestRenderToolCallFoldsDescription 描述行同样是单行，含换行时要折叠。
func TestRenderToolCallFoldsDescription(t *testing.T) {
	args := `{"command":"ls","description":"line one\nline two"}`
	got := renderToolCall("shell", args)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("应为「命令卡 + 描述行」两行，got %d: %q", len(lines), got)
	}
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			t.Fatalf("不得出现空行: %q", got)
		}
	}
	if !strings.Contains(lines[1], "line one line two") {
		t.Fatalf("描述应折叠为一行: %q", lines[1])
	}
}

// TestRenderToolCallShowsContextSearchArgs 会话检索的卡片要像 shell 那样带上「搜索组合」：
// 关键词是主参，模型显式给了 limit 时补在同一个括号里（不同 limit 是两次不同的检索）。
// 之前这张卡只有 ● ContextSearch，看不出模型到底搜了什么。
func TestRenderToolCallShowsContextSearchArgs(t *testing.T) {
	got := renderToolCall("context_search", `{"query":"SQLITE_BUSY 记忆插件"}`)
	if !strings.Contains(got, "ContextSearch") || !strings.Contains(got, "SQLITE_BUSY 记忆插件") {
		t.Fatalf("应显示工具名与关键词: %q", got)
	}
	if strings.Contains(got, "·") {
		t.Fatalf("未显式给 limit 时不应补条数: %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatalf("卡片必须是单行: %q", got)
	}

	got = renderToolCall("context_search", `{"query":"multi\nline keyword","limit":20}`)
	if strings.Contains(got, "\n") {
		t.Fatalf("关键词里的换行也要折叠: %q", got)
	}
	if !strings.Contains(got, "multi line keyword · 20") {
		t.Fatalf("应带出显式的条数上限: %q", got)
	}

	// 关键词很长时，整体截断不得把条数挤掉（先给它留宽度再截关键词）
	long := strings.Repeat("长", 80)
	got = renderToolCall("context_search", `{"query":"`+long+`","limit":20}`)
	if !strings.Contains(got, "· 20") {
		t.Fatalf("截断后仍应保留条数上限: %q", got)
	}
}

// TestSingleLine 折叠规则：CR/LF/TAB 与连续空白 → 单个空格，首尾不留空白。
func TestSingleLine(t *testing.T) {
	cases := map[string]string{
		"a\nb":     "a b",
		"a\r\nb":   "a b",
		"a\t\tb":   "a b",
		"  a  b  ": "a b",
		"\n\n":     "",
		"單\n行\t測試": "單 行 測試",
	}
	for in, want := range cases {
		if got := singleLine(in); got != want {
			t.Errorf("singleLine(%q) = %q, want %q", in, got, want)
		}
	}
}
