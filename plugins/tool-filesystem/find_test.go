package main

import (
	"os"
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
// `-path ./node_modules -prune -o -name '*.js' -print` 命中 node_modules 时只剪枝、
// 不进入其内部（故 dep.js 不出现），且因表达式含显式 -print（动作）而不补隐式输出，
// node_modules 自身也不打印——与 GNU find 实测一致：
//
//	$ find . -path ./node_modules -prune -o -name '*.js' -print
//	./src/a.js
//
// 这也正是 -path 需按书写形态（"./..."）匹配、-prune 需在求值命中后阻止下探的原因。
func TestInternalFindLogicAndPrune(t *testing.T) {
	dir := setupFindTree(t)

	cases := []struct{ script, want string }{
		{`find . -name '*.txt' -o -name '*.md'`, "./docs/guide.md\n./src/nested/b.txt\n./top.txt\n"},
		{`find . -type f ! -name '*.js'`, "./.git/config\n./docs/guide.md\n./src/nested/b.txt\n./top.txt\n"},
		{`find . \( -name '*.md' -o -name '*.txt' \) -type f`, "./docs/guide.md\n./src/nested/b.txt\n./top.txt\n"},
		{`find . -path ./node_modules -prune -o -name '*.js' -print`, "./src/a.js\n./src/other.js\n"},
		// -prune 单独出现（无显式动作）时仍补隐式 -print，剪枝对象自身照常输出
		{`find . -path ./node_modules -prune -o -name '*.js'`, "./node_modules\n./src/a.js\n./src/other.js\n"},
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

// TestInternalFindUnsupportedPredicate 未支持谓词（-execdir/-delete/-print0 等）必须响亮
// 报错并列出支持清单——find 被 ExecHandler 无条件拦截，不存在回落到真实 find 的路径，
// 静默失败会让模型空转。
func TestInternalFindUnsupportedPredicate(t *testing.T) {
	dir := setupFindTree(t)

	for _, script := range []string{
		`find . -name '*.js' -execdir rm {} \;`,
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

// TestInternalFindExecSemicolon `-exec ... \;`：每个条目执行一次，退出码即谓词真假。
// 子命令必须走与 shell 相同的分派（echo 是 shell 内建、grep 是内部命令），否则测试
// 环境 PATH 为空时必然 127；同时验证出现显式动作后不再补隐式 -print（否则
// `-exec rm {} \;` 会额外吐一串路径污染输出）。
func TestInternalFindExecSemicolon(t *testing.T) {
	dir := setupFindTree(t)

	cases := []struct{ script, want string }{
		// 静默执行：只输出 echo 的内容，路径不再被隐式打印
		{`find . -name '*.js' -exec echo {} \;`, "./node_modules/dep.js\n./src/a.js\n./src/other.js\n"},
		// 退出码为谓词真假：仅 dep.js 命中，故只对它执行后续 -print
		{`find . -type f -exec grep -l '^dep$' {} \; -print`,
			"./node_modules/dep.js\n./node_modules/dep.js\n"},
		// -exec + grep -q：静默判据，命中才 -print（模型筛文件最常用的组合）
		{`find . -name '*.js' -exec grep -q other {} \; -print`, "./src/other.js\n"},
		// {} 按子串替换（对齐 GNU），不限于独占一个参数
		{`find . -name 'top.txt' -exec echo 'x{}y' \;`, "x./top.txty\n"},
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

// TestInternalFindExecBatch `-exec ... {} +`：收集路径后批量执行一次，恒为真；
// 无命中时一次也不执行（对齐 GNU）。
func TestInternalFindExecBatch(t *testing.T) {
	dir := setupFindTree(t)

	// 命中两个文件，批量命令只起一次、按遍历顺序接收路径
	out, err := runShell(t, dir, `find . -type f -exec grep -lE 'other|top' {} +`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("批量 -exec 应成功: exit=%d out=%q", got, out)
	}
	if want := "./src/other.js\n./top.txt\n"; out != want {
		t.Fatalf("批量 -exec 输出不符:\n got %q\nwant %q", out, want)
	}

	// 无命中：命令一次都不执行，故无输出
	out, err = runShell(t, dir, `find . -name '*.nope' -exec echo X {} +`)
	if got := exitCode(t, err); got != 0 || out != "" {
		t.Fatalf("无命中不应执行批量命令: exit=%d out=%q", got, out)
	}
}

// TestInternalFindExecSideEffect `-exec` 的真实副作用（内部 rm）确实落盘。
func TestInternalFindExecSideEffect(t *testing.T) {
	dir := setupFindTree(t)

	out, err := runShell(t, dir, `find . -name 'top.txt' -exec rm {} \; && echo done`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("find -exec rm 应成功: exit=%d out=%q", got, out)
	}
	if out != "done\n" {
		t.Fatalf("副作用执行后应继续下一命令: %q", out)
	}
	if _, statErr := os.Lstat(filepath.Join(dir, "top.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("top.txt 应已被 -exec rm 删除: err=%v", statErr)
	}
}

// TestInternalFindExecSyntaxErrors `-exec` 语法缺失必须报错退出 2，而不是静默跳过
// （find 被 ExecHandler 无条件拦截，静默等于让模型空转）。
func TestInternalFindExecSyntaxErrors(t *testing.T) {
	dir := setupFindTree(t)

	cases := []struct{ script, wantErr string }{
		{`find . -exec echo {}`, "missing ';' or '+'"},
		{`find . -exec`, "missing ';' or '+'"},
		{`find . -exec echo x +`, "'{}' must be the last argument"},
	}
	for _, c := range cases {
		out, errText, err := runShellIO(t, dir, c.script)
		if got := exitCode(t, err); got != 2 {
			t.Fatalf("%s 应退出码 2: exit=%d out=%q stderr=%q", c.script, got, out, errText)
		}
		if !strings.Contains(errText, c.wantErr) {
			t.Fatalf("%s 的 stderr 应含 %q: %q", c.script, c.wantErr, errText)
		}
	}
}

// TestInternalFindExecBuiltinScope `-exec` 只放行无副作用的内建（echo/printf/test/
// true/false/pwd）：exit/cd 之类若直接调内建会掐掉会话或搬走遍历上下文（GNU 的 -exec
// 起外部进程，天然没有这个问题），故按外部程序处理而失败——但 find 自身必须继续跑完。
func TestInternalFindExecBuiltinScope(t *testing.T) {
	dir := setupFindTree(t)

	// pwd 是放行内建：输出当前目录（路径分隔符已归一为正斜杆，故按末段名比对）
	out, err := runShell(t, dir, `find . -maxdepth 0 -exec pwd \;`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("find -exec pwd 应成功: exit=%d out=%q", got, out)
	}
	if got := filepath.Base(strings.TrimSpace(out)); got != filepath.Base(filepath.FromSlash(dir)) && got != filepath.Base(dir) {
		t.Fatalf("应输出工作目录: %q", out)
	}

	// exit 不放行：即使模型写了 -exec exit，也不能把整个 shell 掐掉
	out, _, err = runShellIO(t, dir, `find . -maxdepth 0 -exec exit \; ; echo after`)
	if got := exitCode(t, err); got != 0 {
		t.Fatalf("find -exec exit 不应使 shell 异常退出: exit=%d out=%q", got, out)
	}
	if !strings.Contains(out, "after") {
		t.Fatalf("find -exec exit 后 shell 应继续执行后续命令: %q", out)
	}
}
