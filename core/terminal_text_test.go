package core

import (
	"strings"
	"testing"
)

// CollapseCarriageReturns 是「终端最终可见状态」的唯一实现（TUI 渲染与 shell 工具
// 出口共用）。这里按平台相关的输入形态逐类钉住，跨平台兼容性靠它守：
// LF（Unix）、CRLF（Windows）、纯 CR 重绘（进度条）、行尾孤立 CR。
func TestCollapseCarriageReturns(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "进度条只留最终帧",
			in:   "Updating files:  10% (478/4780)\rUpdating files:  46% (2199/4780)\rUpdating files: 100% (4780/4780), done.\n",
			want: "Updating files: 100% (4780/4780), done.\n",
		},
		{"多段无换行结尾", "a\rb\rc", "c"},
		{"CRLF 归一（Windows 文本）", "第一行\r\n第二行\r\n", "第一行\n第二行\n"},
		{"行尾孤立 CR（无换行时不留空行）", "a\rb\r", "b"},
		{"空段视为空行", "\r\n", "\n"},
		{"纯 LF 原样（Unix）", "plain\nlines\n", "plain\nlines\n"},
		{"无 CR 早退", "no carriage returns here", "no carriage returns here"},
		{"混合：LF 行与重绘行共存", "head\n1%\r2%\rtail\n", "head\ntail\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := CollapseCarriageReturns(c.in)
			if got != c.want {
				t.Fatalf("CollapseCarriageReturns(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.ContainsRune(got, '\r') {
				t.Fatalf("结果仍含 CR: %q", got)
			}
		})
	}
}

// 幂等性：归一结果再归一不变（工具出口与渲染层都会调用，不能互相打脸）。
func TestCollapseCarriageReturnsIdempotent(t *testing.T) {
	in := "a\rb\r\nc\r\n"
	once := CollapseCarriageReturns(in)
	if twice := CollapseCarriageReturns(once); twice != once {
		t.Fatalf("非幂等: once=%q twice=%q", once, twice)
	}
}
