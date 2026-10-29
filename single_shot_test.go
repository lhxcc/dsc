package main

import "testing"

// TestShouldReadStdin 守「-headless 不读 stdin」这条单发契约。
//
// 背景：-input 兼顾两种用途——终端一次性提问，以及 `printf "a\nb\n" | dsc -input a`
// 的多轮管道。多轮靠 stdin 是否被重定向判定，但 CI 里 stdin 常是一个开着不关的句柄
// 而不是 EOF：于是任务跑完了、进程还在等下一行输入，谁也不打印任何东西。实测
// `-headless -input ...` 就这么挂住过（看起来像模型卡死）。
func TestShouldReadStdin(t *testing.T) {
	cases := []struct {
		name            string
		singleShot      bool
		stdinRedirected bool
		want            bool
	}{
		{"headless 不读 stdin（即使被重定向）", true, true, false},
		{"headless 且 stdin 是终端", true, false, false},
		{"普通 -input + 管道 stdin：多轮", false, true, true},
		{"普通 -input + 终端 stdin：单轮", false, false, false},
	}
	for _, c := range cases {
		if got := shouldReadStdin(c.singleShot, c.stdinRedirected); got != c.want {
			t.Errorf("%s: shouldReadStdin(%v, %v) = %v, 期望 %v",
				c.name, c.singleShot, c.stdinRedirected, got, c.want)
		}
	}
}
