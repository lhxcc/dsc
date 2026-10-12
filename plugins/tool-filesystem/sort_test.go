package main

import (
	"strings"
	"testing"
)

// 本文件覆盖内建 sort / uniq（internalcmds_sort.go）。
//
// 动机：`sort | uniq -c | sort -rn` 是模型统计文本出现次数的标准管道，但 Windows 上
// `sort` 会命中非 POSIX 的 System32\sort.exe（`-n`/`-r`/`-u`/`-k`/`-t` 全被当成文件名
// 而报错）、`uniq` 则根本不存在（exit 127）。用例锁住进程内实现的 POSIX 口径：数值/
// 逆序/去重/忽略大小写/字段键、C locale 字节序、键相等时的整行兜底顺序（输出确定）、
// 长行不受 64 KiB 限制、以及未支持选项的响亮报错。

// TestInternalSortBasic 基本排序：多文件拼接、stdin 管道、C locale 字节序（大写在前，
// 不随系统 locale 漂移）、空输入。
func TestInternalSortBasic(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.txt": "banana\napple\n",
		"b.txt": "cherry\n",
	})

	cases := []struct{ script, want string }{
		{`sort a.txt`, "apple\nbanana\n"},
		{`sort a.txt b.txt`, "apple\nbanana\ncherry\n"},
		{`cat b.txt a.txt | sort`, "apple\nbanana\ncherry\n"},
		// C locale 字节序：'B'(0x42) < 'a'(0x61)
		{`printf 'a\nB\n' | sort`, "B\na\n"},
		{`printf '' | sort`, ""},
		// 末行无换行也算一行
		{`printf 'b\na' | sort`, "a\nb\n"},
		// CRLF 输入按行切分剥 \r，输出统一 \n
		{`printf 'b\r\na\r\n' | sort`, "a\nb\n"},
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

// TestInternalSortNumericFlags 数值/逆序/去重/忽略大小写及其组合（含模型最常用的 -rn）。
func TestInternalSortNumericFlags(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"n.txt": "10\n9\n2\n100\n",
		"w.txt": "Apple\nbanana\napple\nBanana\n",
	})

	cases := []struct{ script, want string }{
		{`sort -n n.txt`, "2\n9\n10\n100\n"},
		{`sort -rn n.txt`, "100\n10\n9\n2\n"},
		{`sort -u w.txt`, "Apple\nBanana\napple\nbanana\n"},
		{`sort -f w.txt`, "Apple\napple\nbanana\nBanana\n"},
		{`sort -fu w.txt`, "Apple\nbanana\n"},
		// -n 取数值前缀：无数字前缀按 0（实测 GNU：-3|x|2.5）、负号与小数可识别
		{`printf 'x\n-3\n2.5\n' | sort -n`, "-3\nx\n2.5\n"},
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

// TestInternalSortKeySpecs 字段键：-t 指定分隔符、-k N 至行尾、-k N,M 限定字段、
// 键级数值修饰、以及「键相等时以整行兜底」的确定顺序（GNU last-resort 比较的实测口径）。
func TestInternalSortKeySpecs(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"k.txt":  "x 3\nx 1\nx 2\n",
		"c.txt":  "pear:30\napple:20\nfig:10\n",
		"e.txt":  "b 1\na 1\n",
		"m.txt":  "a 2 z\na 10 b\n",
		"1f.txt": "x y\nx\n",
	})

	cases := []struct{ script, want string }{
		{`sort -k2 k.txt`, "x 1\nx 2\nx 3\n"},
		{`sort -t: -k2 c.txt`, "fig:10\napple:20\npear:30\n"},
		{`sort -t: -k2,2n c.txt`, "fig:10\napple:20\npear:30\n"}, // 数值键
		// -k2 数值键：键相等时整行兜底 → a 1 在 b 1 前
		{`sort -n -k2 -k1 e.txt`, "a 1\nb 1\n"},
		{`sort -k2,2 -k1,1 e.txt`, "a 1\nb 1\n"},
		// -k2,2 限定第 2 字段（不做至行尾扩展）：10 < 2（字节序）
		{`sort -k2,2 m.txt`, "a 10 b\na 2 z\n"},
		// 字段少的行键为 "" → 排在有效键之前
		{`sort -k1 1f.txt`, "x\nx y\n"},
		// 键级逆序只作用于键：键相等时兜底比较仍按全局档（此处未给 -r），实测 GNU 同样
		// 输出 x|x y——两行第 1 字段都是 "x"，故顺序由整行兜底比较决定
		{`sort -k1,1r 1f.txt`, "x\nx y\n"},
		// 全局 -r 才会连兜底比较一并取反
		{`sort -r -k1,1 1f.txt`, "x y\nx\n"},
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

// TestInternalSortUniqueByKey -u 的去重判据是键相等（GNU 实测口径：`sort -n -u` 下
// 1 与 01 视为相等），而非整行字节相等。
func TestInternalSortUniqueByKey(t *testing.T) {
	dir := t.TempDir()

	cases := []struct{ script, want string }{
		{`printf '01\n1\n2\n' | sort -n -u`, "01\n2\n"},
		{`printf 'a 1\nb 1\nc 2\n' | sort -k2,2 -u`, "a 1\nc 2\n"},
		{`printf 'b\na\nb\n' | sort -u`, "a\nb\n"},
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

// TestInternalSortLongLine 超长单行不受 bufio.Scanner 默认 64 KiB 上限约束（排序对象可能
// 是 minified JS 之类超长单行）：两条 12 万字符的行必须完整输出（含行序与字节数）。
func TestInternalSortLongLine(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"long.txt": strings.Repeat("b", 120000) + "\n" + strings.Repeat("a", 120000) + "\n"})

	out, err := runShell(t, dir, `sort long.txt`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("超长行排序应成功: exit=%d out_len=%d", got, len(out))
	}
	if want := strings.Repeat("a", 120000) + "\n" + strings.Repeat("b", 120000) + "\n"; out != want {
		t.Fatalf("超长行排序结果不符（前缀 a=%v 长度=%d want=%d）", strings.HasPrefix(out, "a"), len(out), len(want))
	}
}

// TestInternalSortErrors 未支持选项、非法键规格、缺失文件的行为：报错退出码 2 且不静默
// 产出错误结果（sort 被 ExecHandler 无条件拦截，不存在回落到外部程序的可能）。
func TestInternalSortErrors(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"ok.txt": "b\na\n"})

	for _, c := range []struct{ script, wantErr string }{
		{`sort -c ok.txt`, "unsupported option: -c"},
		{`sort -k2.3 ok.txt`, "unsupported -k key (field offset .C)"},
		{`sort -k0 ok.txt`, "invalid -k key"},
		{`sort -k2x ok.txt`, "unsupported -k modifier"},
		{`sort -t '' ok.txt`, "requires a non-empty separator"},
		{`sort -k`, "requires an argument"},
	} {
		out, errText, err := runShellIO(t, dir, c.script)
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("%s 应退出码 2: exit=%d out=%q stderr=%q", c.script, got, out, errText)
		}
		if !strings.Contains(errText, c.wantErr) {
			t.Fatalf("%s 的 stderr 应含 %q: got %q", c.script, c.wantErr, errText)
		}
	}

	// 缺失文件：报一行、退出码 2，其余文件仍照常排序输出（与 grep/head/tail 多文件一致）
	out, errText, err := runShellIO(t, dir, `sort nope.txt ok.txt`)
	if got := exitCode(t, err); got != 2 {
		t.Fatalf("缺失文件应退出码 2: exit=%d out=%q", got, out)
	}
	if !strings.Contains(errText, "nope.txt") {
		t.Fatalf("stderr 应点名缺失文件: %q", errText)
	}
	if out != "a\nb\n" {
		t.Fatalf("其余文件仍应输出排序结果: %q", out)
	}
}

// TestInternalUniq uniq 的相邻去重语义与旗标（-c 计数格式对齐 GNU 的右对齐 7 列 + 空格；
// 非相邻重复不合并）。
func TestInternalUniq(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"u.txt": "a\na\nb\na\n"})

	cases := []struct{ script, want string }{
		{`uniq u.txt`, "a\nb\na\n"},
		{`uniq -c u.txt`, "      2 a\n      1 b\n      1 a\n"},
		{`uniq -d u.txt`, "a\n"},
		// -u 是「相邻运行段长度为 1」：末尾的单行 a 属独立运行段，故保留（实测 GNU 同为 b|a）
		{`uniq -u u.txt`, "b\na\n"},
		{`printf 'A\na\n' | uniq -i`, "A\n"},
		{`printf 'A\na\n' | uniq`, "A\na\n"},
		// 统计管道：sort | uniq -c | sort -rn
		{`printf 'b\na\nb\nc\nb\n' | sort | uniq -c | sort -rn`, "      3 b\n      1 a\n      1 c\n"},
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

// TestInternalUniqErrors 未支持选项、多输入操作数、缺失文件：报错退出码 2。
func TestInternalUniqErrors(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.txt": "x\n", "b.txt": "y\n"})

	for _, c := range []struct{ script, wantErr string }{
		{`uniq -z a.txt`, "unsupported option: -z"},
		{`uniq a.txt b.txt`, "extra operand: b.txt"},
		{`uniq nope.txt`, "nope.txt"},
	} {
		out, errText, err := runShellIO(t, dir, c.script)
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("%s 应退出码 2: exit=%d out=%q stderr=%q", c.script, got, out, errText)
		}
		if !strings.Contains(errText, c.wantErr) {
			t.Fatalf("%s 的 stderr 应含 %q: got %q", c.script, c.wantErr, errText)
		}
	}
}
