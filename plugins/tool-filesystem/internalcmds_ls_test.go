package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// runShellIn 在 dir 里用生产 handler 跑一段脚本，返回 stdout 与 err。
func runShellIn(t *testing.T, dir, script string) (string, error) {
	t.Helper()
	var out, errb bytes.Buffer
	opts := append(shellHandlers(), interp.StdIO(nil, &out, &errb), interp.Dir(dir))
	r, err := interp.New(opts...)
	if err != nil {
		t.Fatalf("interp.New: %v", err)
	}
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "")
	if err != nil {
		t.Fatalf("parse %q: %v", script, err)
	}
	err = r.Run(context.Background(), prog)
	return out.String(), err
}

// TestLsPipedIsOnePerLine 回归「ls 列布局把整目录拼成一行」：
// GNU ls 只在 stdout 是终端时排多列；非终端（管道/重定向/工具抓输出）必须一行
// 一个，否则 `ls | wc -l` 恒为 1（现场：2824 个条目的目录报 1，模型据此误判规模）。
func TestLsPipedIsOnePerLine(t *testing.T) {
	dir := t.TempDir()
	const n = 40
	for i := 0; i < n; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 核心回归：管道里的行数 == 条目数
	got, err := runShellIn(t, dir, "ls | wc -l")
	if err != nil {
		t.Fatalf("ls | wc -l: %v", err)
	}
	if strings.TrimSpace(got) != fmt.Sprint(n) {
		t.Fatalf("ls | wc -l = %q, want %d（列布局会把它压成 1 行）", strings.TrimSpace(got), n)
	}

	// 非终端下默认输出就是一行一个（与 GNU 一致）
	plain, err := runShellIn(t, dir, "ls")
	if err != nil {
		t.Fatalf("ls: %v", err)
	}
	if lines := strings.Count(plain, "\n"); lines != n {
		t.Fatalf("默认 ls 行数 = %d, want %d（非终端不应排多列）", lines, n)
	}
	if strings.Contains(plain, "  ") {
		t.Fatalf("非终端输出不应含列分隔的双空格: %q", plain)
	}

	// -1 与 -l 不受影响
	if one, err := runShellIn(t, dir, "ls -1 | wc -l"); err != nil || strings.TrimSpace(one) != fmt.Sprint(n) {
		t.Fatalf("ls -1 | wc -l = %q err=%v", strings.TrimSpace(one), err)
	}
	if long, err := runShellIn(t, dir, "ls -l | wc -l"); err != nil || strings.TrimSpace(long) != fmt.Sprint(n) {
		t.Fatalf("ls -l | wc -l = %q err=%v", strings.TrimSpace(long), err)
	}

	// 空目录：0 行（此前无条件 Fprintln 会多出一行）
	empty := t.TempDir()
	if got, err := runShellIn(t, empty, "ls | wc -l"); err != nil || strings.TrimSpace(got) != "0" {
		t.Fatalf("空目录 ls | wc -l = %q err=%v", strings.TrimSpace(got), err)
	}

	// 显式文件参数：同样一行一个
	if got, err := runShellIn(t, dir, "ls f0*.txt | wc -l"); err != nil || strings.TrimSpace(got) != "10" {
		t.Fatalf("ls f0*.txt | wc -l = %q err=%v", strings.TrimSpace(got), err)
	}
}

// TestWcSingleCountNoPadding 单个计数（wc -l / -w / -c / -m）不补前导空格，与 GNU 一致：
// 结果要能直接参与比较，如 `[ "$(ls | wc -l)" = 40 ]`。多计数仍按列对齐（另见 headtail_test）。
func TestWcSingleCountNoPadding(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ script, want string }{
		{`printf 'x\ny\n' | wc -l`, "2\n"},
		{`printf 'ab cd\n' | wc -w`, "2\n"},
		{`printf 'abcd' | wc -c`, "4\n"},
	}
	for _, c := range cases {
		got, err := runShell(t, dir, c.script)
		if err != nil {
			t.Fatalf("%s: %v", c.script, err)
		}
		if got != c.want {
			t.Fatalf("%s = %q, want %q（单计数不得有前导空格）", c.script, got, c.want)
		}
	}
}
