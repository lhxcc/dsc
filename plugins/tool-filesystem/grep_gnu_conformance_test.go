package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/interp"
)

// TestGrepMatchesGNU 把「与 GNU grep 输出一致」钉成回归测试。
//
// 期望值不是推导出来的，是在同一份样例目录上用 GNU grep 实跑后抄下来的（含退出码）。
// 覆盖的正是手写实现最容易走样的几处：
//   - 文件名前缀的三种决定方式：默认按目标数、-h 强制不带、-H 强制带，且后写者生效；
//   - -r 的路径写法保真：`./sub` 不能退化成 `sub`，显式 `.` 要出 `./x`；
//   - -e 可重复，语义是「任一命中」（此前每个 -e 覆盖上一个，静默少匹配）；
//   - -A 的上下文分组之间要有 `--`，且上下文行用 `-` 而非 `:` 作前缀；
//   - -o 与 -n 组合、-c 多文件逐文件计数；
//   - GNU 的三档退出码：0 命中 / 1 无命中 / 2 出错。
func TestGrepMatchesGNU(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write("a.txt", "foo bar\nFOO\nfoobar\nbar foo baz\n中文 foo 測試\n")
	write("multi.txt", "foo one\nx\ny\nfoo two\nz\n")
	write("nonewline.txt", "tail foo")
	write("sub/b.txt", "foo in sub\n")

	cases := []struct {
		cmd  string
		want string
		code int
	}{
		{"grep -n foo a.txt",
			"1:foo bar\n3:foobar\n4:bar foo baz\n5:中文 foo 測試", 0},
		// 多文件默认带前缀
		{"grep foo a.txt sub/b.txt",
			"a.txt:foo bar\na.txt:foobar\na.txt:bar foo baz\na.txt:中文 foo 測試\nsub/b.txt:foo in sub", 0},
		// -h：强制不带前缀（多文件时也去掉）
		{"grep -h foo a.txt sub/b.txt",
			"foo bar\nfoobar\nbar foo baz\n中文 foo 測試\nfoo in sub", 0},
		// -H：单文件也强制带前缀
		{"grep -H foo a.txt",
			"a.txt:foo bar\na.txt:foobar\na.txt:bar foo baz\na.txt:中文 foo 測試", 0},
		// 组合短旗标里后写者生效
		{"grep -hH foo a.txt",
			"a.txt:foo bar\na.txt:foobar\na.txt:bar foo baz\na.txt:中文 foo 測試", 0},
		{"grep -Hh foo a.txt sub/b.txt",
			"foo bar\nfoobar\nbar foo baz\n中文 foo 測試\nfoo in sub", 0},
		// -r 保留参数的书写形态：./sub 不能退化成 sub
		{"grep -r foo ./sub", "./sub/b.txt:foo in sub", 0},
		// -e 取并集
		{"grep -e foo -e FOO a.txt",
			"foo bar\nFOO\nfoobar\nbar foo baz\n中文 foo 測試", 0},
		// -A 上下文：命中行 `:`、上下文行 `-`、分组间 `--`
		{"grep -A1 foo multi.txt", "foo one\nx\n--\nfoo two\nz", 0},
		{"grep -o -n foo a.txt", "1:foo\n3:foo\n4:foo\n5:foo", 0},
		// -c 多文件逐文件计数
		{"grep -c foo a.txt sub/b.txt", "a.txt:4\nsub/b.txt:1", 0},
		// 无命中：打印 0 但仍退 1
		{"grep -c zzz a.txt", "0", 1},
		// 缺文件：退 2
		{"grep foo nope.txt", "", 2},
		// 最后一行无换行也要被找到
		{"grep -n tail nonewline.txt", "1:tail foo", 0},
	}

	for _, c := range cases {
		out, _, runErr := runShellIO(t, dir, c.cmd)
		code := 0
		if runErr != nil {
			if es, ok := runErr.(interp.ExitStatus); ok {
				code = int(es)
			} else {
				code = 1
			}
		}
		if got := strings.TrimRight(out, "\n"); got != c.want {
			t.Errorf("%s\n got: %q\nwant: %q", c.cmd, got, c.want)
		}
		if code != c.code {
			t.Errorf("%s\n exit: %d, want %d", c.cmd, code, c.code)
		}
	}

	// -s 的意义在 stderr：错误信息被抑制，退出码不变。
	out, errOut, runErr := runShellIO(t, dir, "grep -s foo nope.txt")
	if strings.TrimSpace(out) != "" || errOut != "" {
		t.Fatalf("-s 应静默且无输出，实际 stdout=%q stderr=%q", out, errOut)
	}
	if es, ok := runErr.(interp.ExitStatus); !ok || int(es) != 2 {
		t.Fatalf("-s 不改变退出码（应为 2），实际 %v", runErr)
	}
}
