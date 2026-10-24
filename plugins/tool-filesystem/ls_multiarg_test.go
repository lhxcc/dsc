package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 本文件锁定内建 ls 的多参数输出规则（GNU 语义）。
//
// 回归背景：此前内建 ls 按「参数个数 > 1」给**每个**参数都印一行「路径:」标题，
// 于 `ls -t build-*/x.exe | head -1` 得到的是 "路径:"（带冒号，不是有效路径），
// 模型据此判定「脚本里选错了树」；而同一命令在外部 sh 里跑（系统 ls）输出正常。
// 同一条命令两种行为，比单纯的 bug 更难查。
//
// GNU 的真实规则（本机实测）：标题只属于**目录清单**，且只在存在多段清单时出现
// ——多个目录，或文件段与目录段并存。多个普通文件（含 -d 列目录自身）自成一整段，
// 不印任何标题；单个目录参数也不印标题。

// lsFixture 建立 f1/f2 + d1/x + d2/y，并按 mtime 明确排序：d1x 最新，其次 f2，最后 f1。
func lsFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"d1", "d2"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel string, mtime time.Time) {
		full := filepath.Join(dir, rel)
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	write("f1", base)
	write("f2", base.Add(10*time.Minute))
	write(filepath.Join("d1", "x"), base.Add(20*time.Minute))
	write(filepath.Join("d2", "y"), base)
	return dir
}

// TestLsMultipleFilesNoHeaders 多个普通文件参数不印标题（这就是把 head -1 坑掉的那条）。
func TestLsMultipleFilesNoHeaders(t *testing.T) {
	dir := lsFixture(t)

	out, err := runShell(t, dir, `ls -1 f1 f2`)
	if err != nil {
		t.Fatalf("ls -1 f1 f2: %v (out=%q)", err, out)
	}
	if got, want := strings.TrimSpace(out), "f1\nf2"; got != want {
		t.Fatalf("ls -1 f1 f2 = %q, want %q（不得有 \"名字:\" 标题行）", got, want)
	}

	// -t 排序同样不印标题，且按 mtime 新在前
	out, err = runShell(t, dir, `ls -t f1 f2 d1/x`)
	if err != nil {
		t.Fatalf("ls -t: %v", err)
	}
	if got, want := strings.TrimSpace(out), "d1/x\nf2\nf1"; got != want {
		t.Fatalf("ls -t f1 f2 d1/x = %q, want %q", got, want)
	}
	if strings.Contains(out, ":") {
		t.Fatalf("多文件输出不得含标题冒号: %q", out)
	}
}

// TestLsPipeHeadTakesRealPath 回归现场：`ls -t ... | head -1` 必须拿到可用的相对路径。
func TestLsPipeHeadTakesRealPath(t *testing.T) {
	dir := lsFixture(t)
	if err := os.MkdirAll(filepath.Join(dir, "build-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "build-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFiles := map[string]time.Time{
		"build-a/x.exe": time.Now().Add(-time.Hour),
		"build-b/x.exe": time.Now().Add(-time.Minute), // 最新
	}
	for rel, mt := range writeFiles {
		full := filepath.Join(dir, rel)
		if err := os.WriteFile(full, []byte("MZ"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	out, err := runShell(t, dir, `ls -t build-*/x.exe | head -n 1`)
	if err != nil {
		t.Fatalf("ls | head: %v", err)
	}
	got := strings.TrimSpace(out)
	if got != "build-b/x.exe" {
		t.Fatalf("head -1 = %q, want build-b/x.exe（最新那个，且不得带冒号）", got)
	}
	if strings.HasSuffix(got, ":") {
		t.Fatalf("head -1 拿到了标题行（带冒号），下游会把无效路径当文件名: %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(dir, got)); statErr != nil {
		t.Fatalf("head -1 的结果不是有效路径: %q (%v)", got, statErr)
	}
}

// TestLsDirSectionHeaders 目录清单才印标题：单目录不印，多目录/文件+目录并存才印。
func TestLsDirSectionHeaders(t *testing.T) {
	dir := lsFixture(t)

	out, err := runShell(t, dir, `ls d1`)
	if err != nil {
		t.Fatalf("ls d1: %v", err)
	}
	if got, want := strings.TrimSpace(out), "x"; got != want {
		t.Fatalf("单个目录参数不应印标题: %q, want %q", got, want)
	}

	out, err = runShell(t, dir, `ls d1 d2`)
	if err != nil {
		t.Fatalf("ls d1 d2: %v", err)
	}
	if got, want := out, "d1:\nx\n\nd2:\ny\n"; got != want {
		t.Fatalf("两个目录应有标题与空行分隔:\n got %q\nwant %q", got, want)
	}

	out, err = runShell(t, dir, `ls f1 d1`)
	if err != nil {
		t.Fatalf("ls f1 d1: %v", err)
	}
	if got, want := out, "f1\n\nd1:\nx\n"; got != want {
		t.Fatalf("文件段在前（无标题）、目录段在后（有标题）:\n got %q\nwant %q", got, want)
	}
}

// TestLsDashDNoHeaders -d 让目录当条目列出，等同纯文件段，不印标题。
func TestLsDashDNoHeaders(t *testing.T) {
	dir := lsFixture(t)
	out, err := runShell(t, dir, `ls -d d1 d2`)
	if err != nil {
		t.Fatalf("ls -d: %v", err)
	}
	if got, want := strings.TrimSpace(out), "d1\nd2"; got != want {
		t.Fatalf("ls -d d1 d2 = %q, want %q", got, want)
	}
}

// TestLsMissingPathStillReports 分类阶段的错误照旧报错并按 GNU 以 2 退出，
// 其余参数仍需正常输出（不能因为一个错误参数而整段不列）。
func TestLsMissingPathStillReports(t *testing.T) {
	dir := lsFixture(t)
	out, err := runShell(t, dir, `ls f1 nope f2`)
	if err == nil {
		t.Fatal("存在无法访问的参数时应以非零码退出")
	}
	if !strings.Contains(out, "f1") || !strings.Contains(out, "f2") {
		t.Fatalf("有效参数仍应列出，实际 %q", out)
	}
	if strings.Contains(out, "nope:") {
		t.Fatalf("无法访问的参数不应被印成目录标题: %q", out)
	}
}

// TestLsEmptyDirPrintsNothing 空目录（或只有隐藏项且未加 -a）时 ls 必须零输出。
//
// 此前非 -l/-1 分支无条件 Fprintln，空目录会吐一个空行——模型会把空行读成"有输出"，
// 脚本里 `ls | wc -l` 也会多数一行。GNU ls 对空目录不输出任何字节。
func TestLsEmptyDirPrintsNothing(t *testing.T) {
	dir := t.TempDir()
	for _, script := range []string{`ls`, `ls -1`, `mkdir .hidden && ls`} {
		if out, err := runShell(t, dir, script); err != nil || out != "" {
			t.Fatalf("%s 应零输出，实际 %q (err=%v)", script, out, err)
		}
	}
	// 有内容时仍按 GNU 单行空格分隔输出
	if out, _ := runShell(t, dir, `mkdir a b && ls`); out != "a  b\n" {
		t.Fatalf("非空目录应输出 %q，实际 %q", "a  b\n", out)
	}
}
