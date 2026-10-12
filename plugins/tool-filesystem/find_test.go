package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// 本文件覆盖内建 find（internalcmds_find.go）。
//
// 动机：`find` 不在 mvdan/sh 内建、也不在本插件内部命令表中，Windows 上只能回落 PATH
// 查找而失败——而 shell 工具描述把 find 列为可用示例。此处按 GNU find 常用子集进程内
// 实现，用例锁住：谓词子集、隐式 AND 与 -a/-o/!/括号、-maxdepth/-mindepth、-path 的
// fnmatch 跨分隔符语义、-prune 惯用式、输出顺序确定、以及未支持谓词/缺路径的行为。

// setupFindTree 构造 find 用例的目录树（含 .git 与 node_modules——与 grep -r 相反，
// find 不跳过二者，用例据此断言）：
//
//	.git/config          git config
//	docs/guide.md        # Guide
//	node_modules/dep.js  dep
//	src/a.js             a
//	src/nested/b.txt     b
//	src/other.js         other
//	top.txt              top
func setupFindTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		".git/config":         "git config\n",
		"docs/guide.md":       "# Guide\n",
		"node_modules/dep.js": "dep\n",
		"src/a.js":            "a\n",
		"src/nested/b.txt":    "b\n",
		"src/other.js":        "other\n",
		"top.txt":             "top\n",
	})
	return dir
}

// TestInternalFindNameType 名称/类型谓词与输出顺序：条目按名升序深度优先，故同一
// 目录树的输出逐字节确定（工具结果进入模型上下文，顺序不得随文件系统返回序漂移）；
// find 不跳过 .git/node_modules（它们的条目照常出现在结果里）。
func TestInternalFindNameType(t *testing.T) {
	dir := setupFindTree(t)

	cases := []struct{ script, want string }{
		{`find . -name '*.txt'`, "./src/nested/b.txt\n./top.txt\n"},
		{`find . -type f -name '*.js'`, "./node_modules/dep.js\n./src/a.js\n./src/other.js\n"},
		{`find . -type d -name 'src'`, "./src\n"},
		// 路径参数按书写形态输出：参数为 src 时不带 "./" 前缀
		{`find src -name '*.js'`, "src/a.js\nsrc/other.js\n"},
		// 显式 -a 与隐式 AND 等价
		{`find . -type f -a -name '*.md'`, "./docs/guide.md\n"},
		// 无路径参数时按 . 搜索
		{`cd src && find -name '*.js'`, "./a.js\n./other.js\n"},
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

// TestInternalFindInamePathDepth -iname 大小写不敏感；-path 的 * 跨路径分隔符
// （`-path '*/nested/*.txt'` 是模型惯用式）；-maxdepth/-mindepth 边界；-maxdepth 0
// 只输出路径参数自身。
func TestInternalFindInamePathDepth(t *testing.T) {
	dir := setupFindTree(t)

	cases := []struct{ script, want string }{
		{`find . -iname 'A.JS'`, "./src/a.js\n"},
		{`find . -path '*/nested/*.txt'`, "./src/nested/b.txt\n"},
		{`find . -maxdepth 1 -name '*.txt'`, "./top.txt\n"},
		{`find . -mindepth 2 -name '*.txt'`, "./src/nested/b.txt\n"},
		{`find . -maxdepth 0`, ".\n"},
		{`find . -mindepth 1 -maxdepth 1`, "./.git\n./docs\n./node_modules\n./src\n./top.txt\n"},
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

// TestInternalFindLogicAndPrune 逻辑组合（-o、!、括号）与 -prune 惯用式：
// `-path ./node_modules -prune -o -name '*.js' -print` 必须打印 node_modules 自身
// 但不进入其内部（故 dep.js 不出现）——这正是 -path 需按书写形态（"./..."）匹配、
// 且 -prune 需在求值命中后阻止下探的原因。
func TestInternalFindLogicAndPrune(t *testing.T) {
	dir := setupFindTree(t)

	cases := []struct{ script, want string }{
		{`find . -name '*.txt' -o -name '*.md'`, "./docs/guide.md\n./src/nested/b.txt\n./top.txt\n"},
		{`find . -type f ! -name '*.js'`, "./.git/config\n./docs/guide.md\n./src/nested/b.txt\n./top.txt\n"},
		{`find . \( -name '*.md' -o -name '*.txt' \) -type f`, "./docs/guide.md\n./src/nested/b.txt\n./top.txt\n"},
		{`find . -path ./node_modules -prune -o -name '*.js' -print`, "./node_modules\n./src/a.js\n./src/other.js\n"},
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

// TestInternalFindNoMatchAndMissingPath 无命中退出码 0（与 grep 的 1 不同，对齐 GNU
// find）；路径不存在时报一行并退出码 1，其余路径照常处理。
func TestInternalFindNoMatchAndMissingPath(t *testing.T) {
	dir := setupFindTree(t)

	out, err := runShell(t, dir, `find . -name 'zzz-nope'`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("无命中应退出码 0: exit=%d out=%q", got, out)
	}
	if out != "" {
		t.Fatalf("无命中不应有输出: %q", out)
	}

	out, errText, err := runShellIO(t, dir, `find nope-dir -name x`)
	if got := exitCode(t, err); got != 1 {
		t.Fatalf("路径不存在应退出码 1: exit=%d out=%q stderr=%q", got, out, errText)
	}
	if !strings.Contains(errText, "nope-dir") {
		t.Fatalf("stderr 应点名缺失路径: %q", errText)
	}
}

// TestInternalFindUnsupportedPredicate 未支持谓词（-exec/-delete/-print0 等）必须响亮
// 报错并列出支持清单——find 被 ExecHandler 无条件拦截，不存在回落到真实 find 的路径，
// 静默失败会让模型空转。
func TestInternalFindUnsupportedPredicate(t *testing.T) {
	dir := setupFindTree(t)

	for _, script := range []string{
		`find . -name '*.js' -exec rm {} \;`,
		`find . -name '*.pyc' -delete`,
		`find . -size +1M`,
	} {
		out, errText, err := runShellIO(t, dir, script)
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("%s 应退出码 2: exit=%d out=%q stderr=%q", script, got, out, errText)
		}
		if !strings.Contains(errText, "unsupported predicate") || !strings.Contains(errText, "supported here") {
			t.Fatalf("%s 的 stderr 应列出支持谓词: %q", script, errText)
		}
	}

	// 谓词参数非法同样报错而非静默按假值处理
	if _, errText, err := runShellIO(t, dir, `find . -maxdepth x`); exitCode(t, err) != 2 ||
		!strings.Contains(errText, "invalid -maxdepth") {
		t.Fatalf("非法 -maxdepth 参数应报错: stderr=%q", errText)
	}
	if _, errText, err := runShellIO(t, dir, `find . -type z`); exitCode(t, err) != 2 ||
		!strings.Contains(errText, "invalid -type") {
		t.Fatalf("非法 -type 参数应报错: stderr=%q", errText)
	}
}

// TestInternalFindDoesNotFollowLinks 遍历中遇到的目录链接不下探——既防环，也避免
// node_modules 之类的「链接农场」被重复遍历（pnpm 在 Windows 上正是以 junction 组织
// node_modules，真实工作区里很常见）；把链接本身作为路径参数时则按 Stat 解、正常下探
// （模型据此可显式进入链接目录）。
//
// 链接形态按环境能力覆盖（符号链接 + Windows junction），不按 GOOS 硬编码跳过。
func TestInternalFindDoesNotFollowLinks(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"real/inside.txt": "x\n"})
	names, isSymlink := makeDirLinks(t, dir, filepath.Join(dir, "real"))
	if len(names) == 0 {
		t.Skip("当前环境无法创建目录链接（符号链接需特权、junction 亦不可用），跳过链接语义断言")
	}
	t.Logf("链接形态: %v（符号链接: %v）", names, isSymlink)

	// 遍历中遇到链接：一律不下探，故不出现 <link>/inside.txt；真实目录照常列出
	out, err := runShell(t, dir, `find . -type f`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("find -type f 应成功: exit=%d out=%q", got, out)
	}
	if !strings.Contains(out, "./real/inside.txt") {
		t.Fatalf("真实目录内的文件应列出: %q", out)
	}
	for _, n := range names {
		if strings.Contains(out, n+"/inside.txt") {
			t.Fatalf("不应下探遍历中遇到的链接 %s: %q", n, out)
		}
	}

	// 链接作为路径参数：按 Stat 解（指向目录即下探）
	for _, n := range names {
		out, err = runShell(t, dir, `find `+n+` -type f`)
		if got := exitCode(t, err); got != 0 {
			t.Fatalf("find %s -type f 应成功: exit=%d out=%q", n, got, out)
		}
		if want := n + "/inside.txt\n"; out != want {
			t.Fatalf("链接作为路径参数应正常下探: got %q want %q", out, want)
		}
	}

	// 符号链接按 lstat 视为链接本身（-type l 可见）。Windows junction 由 Go 报为
	// irregular（既非目录也非符号链接），-type l 不匹配——Go 对 junction 的既有口径，
	// 非本实现的取舍，故仅在符号链接形态下断言。
	if isSymlink["link"] {
		out, err = runShell(t, dir, `find . -type l`)
		if got := exitCode(t, err); got != 0 {
			t.Fatalf("find -type l 应成功: exit=%d out=%q", got, out)
		}
		if out != "./link\n" {
			t.Fatalf("符号链接应作为链接本身列出（junction 不匹配 -type l）: %q", out)
		}
	}
}
