package tui

import (
	"strings"
	"testing"
)

// 现场：`git worktree add` 的进度条用 \r 就地刷新，文本原样进终端后光标跳回第 0 列，
// 卡片左边距被覆盖、还露出上一段残字（截图上那串「Upd / ing files: NN%」）。
// 渲染前必须归一：每条物理行只保留最后一个非空 \r 段。
func TestCollapseCarriageReturns(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{
			name: "进度条只留最终状态",
			in:   "Updating files:  10% (478/4780)\rUpdating files:  46% (2199/4780)\rUpdating files: 100% (4780/4780), done.\n",
			want: "Updating files: 100% (4780/4780), done.\n",
		},
		{
			name: "同行多段 + 无换行结尾",
			in:   "a\rb\rc",
			want: "c",
		},
		{
			name: "CRLF 归一且保留多行",
			in:   "第一行\r\n第二行\r\n",
			want: "第一行\n第二行\n",
		},
		{
			name: "空段视为空行",
			in:   "\r\n",
			want: "\n",
		},
		{
			name: "无 CR 时原样返回",
			in:   "plain\nlines\n",
			want: "plain\nlines\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := collapseCarriageReturns(c.in)
			if got != c.want {
				t.Fatalf("collapseCarriageReturns(%q) = %q, want %q", c.in, got, c.want)
			}
			if strings.ContainsRune(got, '\r') {
				t.Fatalf("结果仍含 CR: %q", got)
			}
		})
	}
}

// 渲染层兜底：无论走哪个入口，卡片里都不应再有裸 CR，且展示的是最终进度。
func TestRenderToolResultStripsCarriageReturns(t *testing.T) {
	in := "Updating files:  46% (2199/4780)\rUpdating files: 100% (4780/4780), done.\n[exit_code: 0]\n"
	out := renderToolResult(in, false)
	if strings.ContainsRune(out, '\r') {
		t.Fatalf("渲染结果含裸 CR（会覆盖左边距）: %q", out)
	}
	if !strings.Contains(out, "100% (4780/4780), done.") {
		t.Fatalf("应展示最终进度: %q", out)
	}
	if strings.Contains(out, "46%") {
		t.Fatalf("中间态进度不应残留: %q", out)
	}
}
