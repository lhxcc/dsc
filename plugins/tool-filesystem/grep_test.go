package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖内建 grep 的递归搜索（-r/-R 与组合短选项 -rn）。
//
// 真实报告：模型执行 `grep -rn <pat> .` 被拒「grep: unsupported option: -rn」——内建
// grep 只认 -i/-v/-n/-c/-E/-e，缺 -r，而 grep 由 interp.ExecHandler 无条件拦截（不回落
// 外部程序），故模型在 Linux 上也无法改用系统 grep，任务直接卡住。这里按 GNU grep -r
// 口径补齐：目录参数递归、每行带 path:line:text、命中顺序确定、二进制文件只报
// 「Binary file ... matches」、噪声目录（.git/node_modules/__pycache__）不进入结果。

// setupGrepTree 构造递归搜索用的目录树（均为文本文件；.git 与 node_modules 下亦有命中，
// 用于断言它们被跳过）：
//
//	.git/config           needme git
//	node_modules/dep.js   needme dep
//	src/a.js              needme a
//	src/nested/b.txt      needme b
//	src/other.js          （无命中，供 -c 断言计数 0）
//	top.txt               needme top
func setupGrepTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".git/config":         "needme git\n",
		"node_modules/dep.js": "needme dep\n",
		"src/a.js":            "needme a\n",
		"src/nested/b.txt":    "needme b\n",
		"src/other.js":        "no hit here\n",
		"top.txt":             "needme top\n",
	})
	return dir
}

// TestInternalGrepRecursive 报告场景回归：`grep -rn` 组合短选项可用、递归命中每行带
// path:line:text，且顺序按路径名升序确定（工具结果进入模型上下文，顺序不得随文件系统
// 返回序漂移）；.git 与 node_modules 内的命中不进入结果。
func TestInternalGrepRecursive(t *testing.T) {
	dir := setupGrepTree(t)

	out, err := runShell(t, dir, `grep -rn needme .`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("grep -rn 应成功（报告中被拒为 unsupported option: -rn）: exit=%d out=%q", got, out)
	}
	want := "src/a.js:1:needme a\n" +
		"src/nested/b.txt:1:needme b\n" +
		"top.txt:1:needme top\n"
	if out != want {
		t.Fatalf("grep -rn 输出不符:\n got %q\nwant %q", out, want)
	}
	for _, noise := range []string{".git/", "node_modules/"} {
		if strings.Contains(out, noise) {
			t.Fatalf("噪声目录 %s 的命中不应进入结果: %q", noise, out)
		}
	}

	// -r 与 -R 同义、目录参数亦可（等价于 .）：同一棵树的命中集合一致
	outR, err := runShell(t, dir, `grep -R -n needme src`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("grep -R -n src 应成功: exit=%d out=%q", got, outR)
	}
	if wantR := "src/a.js:1:needme a\nsrc/nested/b.txt:1:needme b\n"; outR != wantR {
		t.Fatalf("grep -R -n src 输出不符:\n got %q\nwant %q", outR, wantR)
	}

	// 无路径时 -r 按 . 搜索（对齐 GNU grep -r）
	outDot, err := runShell(t, dir, `cd src && grep -rn needme | wc -l`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("cd src && grep -rn 应成功: exit=%d out=%q", got, outDot)
	}
	if strings.TrimSpace(outDot) != "2" {
		t.Fatalf("cd src 后递归应命中 2 行，got %q", outDot)
	}
}

// TestInternalGrepRecursiveInvertCountAndExit 递归与既有旗标组合：-c 逐文件计数
// （含 0 命中文件，对齐 GNU grep -c）、-v 反选、无命中时退出码 1。
func TestInternalGrepRecursiveInvertCountAndExit(t *testing.T) {
	dir := setupGrepTree(t)

	out, err := runShell(t, dir, `grep -rc needme src`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("grep -rc 应成功: exit=%d out=%q", got, out)
	}
	if want := "src/a.js:1\nsrc/nested/b.txt:1\nsrc/other.js:0\n"; out != want {
		t.Fatalf("grep -rc 逐文件计数不符:\n got %q\nwant %q", out, want)
	}

	out, err = runShell(t, dir, `grep -rv needme src/a.js src/other.js`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("grep -rv 应成功: exit=%d out=%q", got, out)
	}
	if !strings.Contains(out, "src/other.js:no hit here") {
		t.Fatalf("反选应输出未命中行: %q", out)
	}

	out, err = runShell(t, dir, `grep -rn zzznotfound .`)
	if got := exitCode(t, err); got != 1 {
		t.Fatalf("无命中应退出码 1: exit=%d out=%q", got, out)
	}
}

// TestInternalGrepDirectoryWithoutRecursive 目录参数未给 -r 时按 POSIX 报
// 「Is a directory」并给可执行提示（模型据此改用 -r），退出码 2；不得静默按空结果返回。
func TestInternalGrepDirectoryWithoutRecursive(t *testing.T) {
	dir := setupGrepTree(t)

	out, errText, err := runShellIO(t, dir, `grep needme src`)
	if got := exitCode(t, err); got != 2 {
		t.Fatalf("目录参数未给 -r 应退出码 2: exit=%d out=%q stderr=%q", got, out, errText)
	}
	if !strings.Contains(errText, "Is a directory") || !strings.Contains(errText, "-r") {
		t.Fatalf("stderr 应说明是目录并提出 -r 提示: %q", errText)
	}
	if out != "" {
		t.Fatalf("目录参数不应产生命中输出: %q", out)
	}
}

// TestInternalGrepRecursiveBinaryFile 二进制文件命中按 GNU 口径只报一行
// 「Binary file <path> matches」，不把二进制内容灌进模型上下文。
func TestInternalGrepRecursiveBinaryFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blob.bin"), []byte("head\x00needme\x00tail\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "text.txt"), []byte("needme text\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runShell(t, dir, `grep -rn needme .`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("含二进制文件的递归搜索应成功: exit=%d out=%q", got, out)
	}
	if !strings.Contains(out, "Binary file blob.bin matches") {
		t.Fatalf("二进制命中应报 Binary file ... matches: %q", out)
	}
	if strings.Contains(out, "\x00") || strings.Contains(out, "head") {
		t.Fatalf("二进制内容不得进入输出: %q", out)
	}
	if !strings.Contains(out, "text.txt:1:needme text") {
		t.Fatalf("文本文件仍应逐行回显: %q", out)
	}
}

// TestInternalGrepRecursiveDoesNotFollowLinks 递归搜索遇到目录链接不下探：否则
// node_modules 之类的「链接农场」（pnpm 在 Windows 上以 junction 组织）会被重复搜索
// ——同一文件以两条不同路径各命中一次，既浪费上下文也误导模型文件个数。
// 链接形态按环境能力覆盖（符号链接 + Windows junction）。
func TestInternalGrepRecursiveDoesNotFollowLinks(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"real/inside.txt": "needme\n"})
	names, _ := makeDirLinks(t, dir, filepath.Join(dir, "real"))
	if len(names) == 0 {
		t.Skip("当前环境无法创建目录链接（符号链接需特权、junction 亦不可用），跳过链接语义断言")
	}
	t.Logf("链接形态: %v", names)

	out, err := runShell(t, dir, `grep -rn needme .`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("grep -rn 应成功: exit=%d out=%q", got, out)
	}
	if n := strings.Count(out, "needme"); n != 1 {
		t.Fatalf("链接目标只应命中一次（下探链接会重复命中）: n=%d out=%q", n, out)
	}
	if !strings.Contains(out, "real/inside.txt:1:needme") {
		t.Fatalf("真实目录内的命中应带路径前缀: %q", out)
	}
	for _, n := range names {
		if strings.Contains(out, n+"/") {
			t.Fatalf("不应下探遍历中遇到的链接 %s: %q", n, out)
		}
	}
}
