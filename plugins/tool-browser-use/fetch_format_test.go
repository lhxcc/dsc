package main

import (
	"strings"
	"testing"
)

// TestParseFetchFormat format 参数的三种取值、别名、默认与旧布尔兼容。
//
// 为什么不静默回退：模型以为拿到 layout、实际是 text（或反过来）时会做出错误的
// 版面判断（比如断言「表格缺列」），报错比猜更安全。
func TestParseFetchFormat(t *testing.T) {
	cases := []struct {
		in     string
		legacy bool
		want   fetchFormat
		bad    bool
	}{
		{"", false, formatLayout, false}, // 默认
		{"   ", false, formatLayout, false},
		{"layout", false, formatLayout, false},
		{"LAYOUT", false, formatLayout, false},
		{" visual ", false, formatLayout, false},
		{"text", false, formatText, false},
		{"innerText", false, formatText, false},
		{"html", false, formatHTML, false},
		{"raw", false, formatHTML, false},
		{"", true, formatHTML, false}, // 旧参数 return_html=true
		{"markdown", false, "", true},
		{"layout,html", false, "", true},
	}
	for _, c := range cases {
		got, err := parseFetchFormat(c.in, c.legacy)
		if c.bad {
			if err == nil {
				t.Fatalf("parseFetchFormat(%q) 应报错，实际 %q", c.in, got)
			}
			if !strings.Contains(err.Error(), "layout") || !strings.Contains(err.Error(), "html") {
				t.Fatalf("报错应列出可选值：%v", err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("parseFetchFormat(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("parseFetchFormat(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestComposeNotes 多条提示拼接；空提示略去，避免出现「；；」或孤零零的分号。
func TestComposeNotes(t *testing.T) {
	if got := composeNotes("", "b", ""); got != "b" {
		t.Fatalf("空提示应略去，实际 %q", got)
	}
	if got := composeNotes("a", "b"); got != "a；b" {
		t.Fatalf("应以中文分号连接，实际 %q", got)
	}
	if got := composeNotes(); got != "" {
		t.Fatalf("全空应返回空串，实际 %q", got)
	}
}

// TestFallbackNote 只有「请求 layout、实际降级成 text」时才提示；显式要 text/html 不该提示。
func TestFallbackNote(t *testing.T) {
	if got := fallbackNote("text(fallback)"); got == "" {
		t.Fatal("降级必须有提示，否则模型会把并排结构当成缺失")
	}
	for _, f := range []fetchFormat{formatLayout, formatText, formatHTML} {
		if got := fallbackNote(f); got != "" {
			t.Fatalf("%s 不该有降级提示：%q", f, got)
		}
	}
}
