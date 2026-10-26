package main

import (
	"strings"
	"testing"
)

// 本文件覆盖 head / tail / wc 的旗标处理与计数语义。
//
// 动机：此前三者对未识别旗标**静默忽略**，比报错更糟——实测 `head -2 f.txt` 输出 10 行、
// `tail -2 f.txt` 输出最后 10 行（模型据此拿到错结果却毫不知情）、`head -c 5 f.txt` 把 "5"
// 当文件名去打开、`wc -m f.txt` 只打印一个空格行。现统一为「支持常用形式 + 未支持旗标响亮
// 报错」，用例锁住新行为与 GNU 口径（负数/加号/字节/表头）。
// 期望值均以 GNU coreutils 实测核对。

// headTailTree 五行样例文件，便于按行号断言。
func headTailTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"h.txt": "l1\nl2\nl3\nl4\nl5\n",
		"w.txt": "a b\nc\n",
	})
	return dir
}

// TestInternalHeadFlags 行数形式（-n N、-nN、传统 -N、负数）与字节形式（-c N、-c -N）。
func TestInternalHeadFlags(t *testing.T) {
	dir := headTailTree(t)

	cases := []struct{ script, want string }{
		{`head -n 2 h.txt`, "l1\nl2\n"},
		{`head -n2 h.txt`, "l1\nl2\n"},
		{`head -2 h.txt`, "l1\nl2\n"}, // 传统 -N：此前静默输出 10 行
		{`head -n 0 h.txt`, ""},
		{`head -n -2 h.txt`, "l1\nl2\nl3\n"}, // 除末尾 2 行外的全部
		{`head -c 4 h.txt`, "l1\nl"},         // 4 字节，不补换行
		{`head -c4 h.txt`, "l1\nl"},
		{`head -c -4 h.txt`, "l1\nl2\nl3\nl4"}, // 除末尾 4 字节外
	}
	for _, c := range cases {
		out, err := runShell(t, dir, c.script)
		if got := exitCode(t, err); got != 0 {
			t.Fatalf("%s 应成功: exit=%d out=%q", c.script, got, out)
		}
		if out != c.want {
			t.Fatalf("%s 输出不符:\n got %q\nwant %q", c.script, out, c.want)
		}
	}
}

// TestInternalTailFlags 行数（-n N、-N、+N 自第 N 行起）与字节（-c N、-c +N）形式，
// 以及 n=0 的空输出。
func TestInternalTailFlags(t *testing.T) {
	dir := headTailTree(t)

	cases := []struct{ script, want string }{
		{`tail -n 2 h.txt`, "l4\nl5\n"},
		{`tail -n2 h.txt`, "l4\nl5\n"},
		{`tail -2 h.txt`, "l4\nl5\n"}, // 传统 -N：此前静默输出最后 10 行
		{`tail -n 0 h.txt`, ""},
		{`tail -n +2 h.txt`, "l2\nl3\nl4\nl5\n"}, // 自第 2 行起
		{`tail +2 h.txt`, "l2\nl3\nl4\nl5\n"},    // 传统 +N
		{`tail -n -2 h.txt`, "l4\nl5\n"},         // -n -K 与 -n K 同义
		{`tail -c 4 h.txt`, "\nl5\n"},            // 末尾 4 字节
		{`tail -c +4 h.txt`, "l2\nl3\nl4\nl5\n"}, // 自第 4 字节起
	}
	for _, c := range cases {
		out, err := runShell(t, dir, c.script)
		if got := exitCode(t, err); got != 0 {
			t.Fatalf("%s 应成功: exit=%d out=%q", c.script, got, out)
		}
		if out != c.want {
			t.Fatalf("%s 输出不符:\n got %q\nwant %q", c.script, out, c.want)
		}
	}
}

// TestInternalHeadTailPipesAndHeaders 管道入参（不可 seek 的滚动缓冲路径）、多文件表头
// 与 -q/-v、超长单行不被静默截断。
func TestInternalHeadTailPipesAndHeaders(t *testing.T) {
	dir := headTailTree(t)

	// 管道输入（stdin 非 *os.File，走滚动缓冲分支）
	if out, err := runShell(t, dir, `printf 'a\nb\nc\n' | tail -n 2`); exitCode(t, err) != 0 || out != "b\nc\n" {
		t.Fatalf("管道 tail -n 2: err=%v out=%q", err, out)
	}
	if out, err := runShell(t, dir, `printf 'a\nb\nc\n' | tail -c 3`); exitCode(t, err) != 0 || out != "\nc\n" {
		t.Fatalf("管道 tail -c 3: err=%v out=%q", err, out)
	}
	if out, err := runShell(t, dir, `printf 'abcde' | head -c 3`); exitCode(t, err) != 0 || out != "abc" {
		t.Fatalf("管道 head -c 3: err=%v out=%q", err, out)
	}

	// 多文件默认打表头；-q 抑制；-v 单文件也打
	out, err := runShell(t, dir, `head -n 1 h.txt w.txt`)
	if exitCode(t, err) != 0 || out != "==> h.txt <==\nl1\n==> w.txt <==\na b\n" {
		t.Fatalf("多文件表头输出不符: err=%v out=%q", err, out)
	}
	if out, err = runShell(t, dir, `head -q -n 1 h.txt w.txt`); exitCode(t, err) != 0 || out != "l1\na b\n" {
		t.Fatalf("-q 应抑制表头: err=%v out=%q", err, out)
	}
	if out, err = runShell(t, dir, `head -v -n 1 h.txt`); exitCode(t, err) != 0 || out != "==> h.txt <==\nl1\n" {
		t.Fatalf("-v 应强制表头: err=%v out=%q", err, out)
	}

	// 超长单行（> 64 KiB）：Scanner 时代会被静默截断，现按完整行输出
	long := strings.Repeat("x", 100000)
	writeTree(t, dir, map[string]string{"long.txt": long + "\n" + "short\n"})
	if out, err = runShell(t, dir, `tail -n 2 long.txt`); exitCode(t, err) != 0 || out != long+"\nshort\n" {
		t.Fatalf("超长单行应完整输出: err=%v out_len=%d", err, len(out))
	}
}

// TestInternalHeadTailErrors 未支持旗标响亮报错（此前静默忽略），缺参数与非法参数同样报错。
func TestInternalHeadTailErrors(t *testing.T) {
	dir := headTailTree(t)

	for _, c := range []struct{ script, wantErr string }{
		{`head -5f h.txt`, "unsupported option: -5f"},
		{`head -c 5 -f h.txt`, "unsupported option: -f"},
		{`tail -f h.txt`, "unsupported option: -f"},
		{`head -n x h.txt`, `invalid argument "x" for -n`},
		{`head -n`, "requires an argument"},
		{`tail -n x h.txt`, `invalid argument "x" for -n`},
	} {
		out, errText, err := runShellIO(t, dir, c.script)
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("%s 应退出码 2: exit=%d out=%q stderr=%q", c.script, got, out, errText)
		}
		if !strings.Contains(errText, c.wantErr) {
			t.Fatalf("%s 的 stderr 应含 %q: got %q", c.script, c.wantErr, errText)
		}
	}
	// 报错文案须给出可执行的支持清单（模型据此自我纠正）
	if _, errText, _ := runShellIO(t, dir, `tail -f h.txt`); !strings.Contains(errText, "-n N/-nN") {
		t.Fatalf("tail 报错应列出支持清单: %q", errText)
	}
}

// TestInternalWcFlags wc 的旗标白名单与列序：-l/-w/-c/-m（字符按 UTF-8 码点），列序固定为
// 行、词、字符、字节（与旗标书写顺序无关，对齐 GNU）。
func TestInternalWcFlags(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"t.txt":   "a b\n",
		"cjk.txt": "你好\n",
	})

	cases := []struct{ script, want string }{
		{`wc -l t.txt`, "1 t.txt\n"},
		{`wc -w t.txt`, "2 t.txt\n"},
		{`wc -c t.txt`, "4 t.txt\n"},
		{`wc -cl t.txt`, " 1 4 t.txt\n"},     // 列序固定：行在前、字节在后
		{`wc -lc t.txt`, " 1 4 t.txt\n"},     // 与旗标顺序无关
		{`wc -m cjk.txt`, "3 cjk.txt\n"},     // 码点计数且含换行（实测 GNU：printf '你好\n' | wc -m → 3）
		{`wc -mc cjk.txt`, " 3 7 cjk.txt\n"}, // 字符 3、字节 7
	}
	for _, c := range cases {
		out, err := runShell(t, dir, c.script)
		if got := exitCode(t, err); got != 0 {
			t.Fatalf("%s 应成功: exit=%d out=%q", c.script, got, out)
		}
		if out != c.want {
			t.Fatalf("%s 输出不符:\n got %q\nwant %q", c.script, out, c.want)
		}
	}
}

// TestInternalWcErrors 未识别旗标响亮报错（此前静默忽略，`wc -m` 甚至只打印一个空格行）。
func TestInternalWcErrors(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"t.txt": "a\n"})

	for _, arg := range []string{"-L", "-x", "-mz"} {
		out, errText, err := runShellIO(t, dir, "wc "+arg+" t.txt")
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("wc %s 应退出码 2: exit=%d out=%q stderr=%q", arg, got, out, errText)
		}
		if !strings.Contains(errText, "unsupported option") || !strings.Contains(errText, "-l -w -c -m") {
			t.Fatalf("wc %s 的 stderr 应列出支持清单: %q", arg, errText)
		}
	}
}
