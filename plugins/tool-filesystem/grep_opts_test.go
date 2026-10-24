package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestGrepBRELiteralParens POSIX BRE 里未转义的 "(" 是普通字符：`grep "add_library(x"`
// 必须能匹配含括号的行。此前把模式原样交给 RE2，报 "missing closing )"（现场 27 次）。
func TestGrepBRELiteralParens(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"CMakeLists.txt": "add_library(strata_mmq STATIC a.cpp)\nproject(strata VERSION 1)\n",
	})
	out, errb, err := runShellNilStdin(t, dir, `grep -n "add_library(strata_mmq" CMakeLists.txt`)
	if err != nil {
		t.Fatalf("BRE 模式应可用，stderr=%q err=%v", errb, err)
	}
	if !strings.Contains(out, "add_library(strata_mmq") {
		t.Fatalf("应命中含括号的行，out=%q", out)
	}
	// BRE 里 \(…\) 才是分组：应能匹配 str(ata_mmq)
	out2, _, err2 := runShellNilStdin(t, dir, `grep -c "str\(ata\)_mmq" CMakeLists.txt`)
	if err2 != nil {
		t.Fatalf("BRE 分组应可用: %v", err2)
	}
	if strings.TrimSpace(out2) != "1" {
		t.Fatalf("BRE 分组应命中 1 行，got %q", out2)
	}
}

// TestGrepCommonFlags -A/-B/-m/-o/-F/--include 这些日常旗标必须支持：
// 现场 24 次 "unsupported option" 全部出自这里。
func TestGrepCommonFlags(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.log": "l1\nERROR x\nl3\nl4\nERROR y\nl6\n",
		"b.txt": "ERROR plain\n",
		"c.bin": "nothing\n",
	})

	// -A1：命中行后带 1 行上下文
	out, _, err := runShellNilStdin(t, dir, `grep -A1 ERROR a.log`)
	if err != nil {
		t.Fatalf("-A 失败: %v", err)
	}
	if !strings.Contains(out, "l3") {
		t.Fatalf("-A1 应带后置上下文，out=%q", out)
	}
	// -m1：每个文件最多 1 条命中
	out, _, err = runShellNilStdin(t, dir, `grep -m1 -c ERROR a.log`)
	if err != nil {
		t.Fatalf("-m 失败: %v", err)
	}
	if strings.TrimSpace(out) != "1" {
		t.Fatalf("-m1 应只计 1 条，got %q", out)
	}
	// -o：只输出命中片段
	out, _, err = runShellNilStdin(t, dir, `grep -o "ERROR [a-z]*" a.log`)
	if err != nil {
		t.Fatalf("-o 失败: %v", err)
	}
	if strings.Count(strings.TrimSpace(out), "\n") != 1 || strings.Contains(out, "l1") {
		t.Fatalf("-o 只应输出命中片段，got %q", out)
	}
	// -F：模式按字面（含 . 不当作通配）
	out, _, err = runShellNilStdin(t, dir, `grep -c -F "ERROR x" a.log`)
	if err != nil || strings.TrimSpace(out) != "1" {
		t.Fatalf("-F 字面匹配应命中 1 行，got %q err=%v", out, err)
	}
	// --include：递归时按文件名过滤
	out, _, err = runShellNilStdin(t, dir, `grep -r --include=*.txt -l ERROR .`)
	if err != nil {
		t.Fatalf("--include 失败: %v", err)
	}
	if !strings.Contains(out, "b.txt") || strings.Contains(out, "a.log") {
		t.Fatalf("--include 应只搜 .txt，got %q", out)
	}
	// 组合短旗标 -rn / -oE
	if _, _, err := runShellNilStdin(t, dir, `grep -rn "ERROR" . >/dev/null`); err != nil {
		t.Fatalf("-rn 失败: %v", err)
	}
	if _, _, err := runShellNilStdin(t, dir, `grep -oE "ERROR [a-z]+" a.log >/dev/null`); err != nil {
		t.Fatalf("-oE 失败: %v", err)
	}
}

// TestRmCombinedFlags `rm -rf dir` 必须等价于 -r -f：此前整串被当作文件名，
// 报 cannot remove '-rf'（现场 22 次）。
func TestRmCombinedFlags(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"d/x.txt": "x"})
	if _, errb, err := runShellNilStdin(t, dir, `rm -rf d`); err != nil {
		t.Fatalf("rm -rf 应成功，stderr=%q err=%v", errb, err)
	}
	if out, _, _ := runShellNilStdin(t, dir, `ls`); strings.Contains(out, "d") {
		t.Fatalf("目录应已删除，ls=%q", out)
	}
	// -fr 同义，且不存在的目标配 -f 不报错
	if _, errb, err := runShellNilStdin(t, dir, `rm -fr nonexistent`); err != nil {
		t.Fatalf("rm -fr 不存在的目标应静默，stderr=%q err=%v", errb, err)
	}
	// 未知旗标要响亮报错，而不是当路径删除
	if _, errb, err := runShellNilStdin(t, dir, `rm -Z x`); err == nil || !strings.Contains(errb, "unsupported option") {
		t.Fatalf("未知旗标应报错，err=%v stderr=%q", err, errb)
	}
}

// TestLsAcceptsTimeStyleAndA `ls -A` 与 `--time-style=…` 应被接受（现场报不支持）。
func TestLsAcceptsTimeStyleAndA(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{".hidden": "h", "shown.txt": "s"})
	out, errb, err := runShellNilStdin(t, dir, `ls -A`)
	if err != nil {
		t.Fatalf("ls -A 应可用，stderr=%q err=%v", errb, err)
	}
	if !strings.Contains(out, ".hidden") {
		t.Fatalf("ls -A 应含隐藏文件，out=%q", out)
	}
	if _, errb, err := runShellNilStdin(t, dir, `ls -l --time-style=+%m-%d_%H:%M`); err != nil {
		t.Fatalf("--time-style 应被接受，stderr=%q err=%v", errb, err)
	}
}

// TestTmpMapsToDSC temp Windows 上 /tmp 不是真实路径（会被解析到当前盘根）：
// 写到 /tmp 的临时脚本应落到 DSC 自己的 temp 目录，Unix 上保持真实 /tmp。
func TestTmpMapsToDSC(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("/tmp 在 Unix 上是真实挂载点，不做映射")
	}
	dir := t.TempDir()
	temp := t.TempDir()
	t.Setenv("DSC_TEMP_DIR", temp)
	if _, errb, err := runShellNilStdin(t, dir, `echo hi > /tmp/dsc_probe.txt`); err != nil {
		t.Fatalf("写 /tmp 失败: stderr=%q err=%v", errb, err)
	}
	if _, err := os.Stat(filepath.Join(temp, "dsc_probe.txt")); err != nil {
		t.Fatalf("/tmp 应映射到 DSC temp 目录: %v", err)
	}
	if out, _, err := runShellNilStdin(t, dir, `cat /tmp/dsc_probe.txt`); err != nil || strings.TrimSpace(out) != "hi" {
		t.Fatalf("读回内容不符: out=%q err=%v", out, err)
	}
}

// TestGrepQuietFlag -q 是谓词用法：只以退出码表态，一律不输出；命中即停（不再读
// 后续目标/剩余行）。与 -l/-c 同给时输出仍被抑制（GNU 亦然），且 -qc 无命中退 1
// （-c 平时退 0 的例外不适用于 -q）。
func TestGrepQuietFlag(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.txt": "foo\nfoobar\n",
		"b.txt": "nothing\n",
	})

	// 命中：无输出、退出 0
	out, errb, err := runShellIO(t, dir, `grep -q foobar a.txt`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("-q 命中应退 0: exit=%d out=%q stderr=%q", got, out, errb)
	}
	if out != "" {
		t.Fatalf("-q 不应输出: %q", out)
	}

	// 无命中：退出 1
	out, _, err = runShellIO(t, dir, `grep -q zzz a.txt`)
	if got := exitCode(t, err); got != 1 {
		t.Fatalf("-q 无命中应退 1: exit=%d out=%q", got, out)
	}

	// 与 -c/-l 同给：输出仍被抑制（对齐 GNU）
	for _, flag := range []string{"-qc", "-ql"} {
		out, _, err = runShellIO(t, dir, `grep `+flag+` foo a.txt`)
		if got := exitCode(t, err); got != 0 {
			t.Fatalf("grep %s 命中应退 0: exit=%d out=%q", flag, got, out)
		}
		if out != "" {
			t.Fatalf("grep %s 不应输出: %q", flag, out)
		}
	}

	// -qc 无命中仍退 1（-c 的「输出 0 且退 0」例外不适用于 -q）
	out, _, err = runShellIO(t, dir, `grep -qc zzz a.txt`)
	if got := exitCode(t, err); got != 1 {
		t.Fatalf("grep -qc 无命中应退 1: exit=%d out=%q", got, out)
	}

	// 递归谓词用法：多文件时一旦命中即停，不因未命中文件而整体失败
	out, _, err = runShellIO(t, dir, `grep -rq foo .`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("grep -rq 命中应退 0: exit=%d out=%q", got, out)
	}
	if out != "" {
		t.Fatalf("grep -rq 不应输出: %q", out)
	}
}

// TestGrepLineRegexpFlag -x 只匹配整行（`grep -x foo` 不命中 foobar）；与 -w 同给时
// 以 -x 为准（GNU：-w 在 -x 下不起作用）。实测 GNU 对照：
//
//	$ grep -x foo      → foo
//	$ grep -xw 'foo bar' → foo bar
func TestGrepLineRegexpFlag(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.txt": "foo\nfoobar\nfoo bar\n",
	})

	out, errb, err := runShellIO(t, dir, `grep -x foo a.txt`)
	if err != nil {
		t.Fatalf("-x 失败: stderr=%q err=%v", errb, err)
	}
	if out != "foo\n" {
		t.Fatalf("-x 应只命中整行 foo: %q", out)
	}

	// -x 下 -w 不干扰：整行匹配必然蕴含整词匹配，不能再套 \b（行首尾非词字符会误判）
	out, _, err = runShellIO(t, dir, `grep -xw 'foo bar' a.txt`)
	if err != nil {
		t.Fatalf("-xw 失败: %v", err)
	}
	if out != "foo bar\n" {
		t.Fatalf("-xw 应命中整行: %q", out)
	}

	// 无整行命中：退 1
	out, _, err = runShellIO(t, dir, `grep -x oo a.txt`)
	if got := exitCode(t, err); got != 1 {
		t.Fatalf("-x 无整行命中应退 1: exit=%d out=%q", got, out)
	}

	// -x 与 -E/-F 组合：-F 时整行按字面匹配
	writeTree(t, dir, map[string]string{"b.txt": "a.b\naxb\n"})
	out, _, err = runShellIO(t, dir, `grep -xF 'a.b' b.txt`)
	if err != nil {
		t.Fatalf("-xF 失败: %v", err)
	}
	if out != "a.b\n" {
		t.Fatalf("-xF 应只按字面匹配整行: %q", out)
	}
}
