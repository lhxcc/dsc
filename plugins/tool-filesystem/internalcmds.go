package main

// 进程内实现的常用 POSIX 工具（内建式）。选用 mvdan/sh 的初衷是希望它自带标准
// POSIX 命令、不依赖外部 PATH；但其 interp 只实现了 shell 语言内建（cd/pwd/echo/
// printf/test 等），mkdir/ls/cat 等常用工具系外部程序，须按 PATH 查找。在 Windows
// 且插件子进程 PATH 被宿主过滤时这些工具全部失效（exit 127）。
//
// 这里是修复：经 interp.ExecHandler 拦截「既非内建、也非 shell 函数」的简单命令，
// 命中下列命令即进程内用 Go stdlib 实现（纯 filepath/os/io，无外部依赖、跨全平台
// 编译），未命中则回退默认 PATH 外部执行。语义对齐各自标准的常见用法（含 -p/-r/
// -l/-a/-d/-i/-v/-n 等常用旗标），并额外保留 tool-filesystem 已生效的
// /workspace 虚拟根映射（见 mapWorkspacePaths，AST 层已把字面量重写为真实路径，
// 故此处拿到的均是已映射后的绝对/相对路径）。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"dsc-sdk"
	"dsc/core"
	"mvdan.cc/sh/v3/interp"
)

// defaultExecHandler 保持「未命中内部工具」时原有的 PATH 外部程序执行语义
// （mvdan/sh 默认 kill timeout 为 2s，对齐未设置 ExecHandler 前的库默认行为）。
var defaultExecHandler = interp.DefaultExecHandler(2 * time.Second)

// windowsToPosix 记录【确属 Windows 专属、POSIX 无效】的命令 → 标准 POSIX 替代，供模型
// 误用 Windows 命令时给出可执行的修正提示。DSC 的 shell 是 POSIX（mvdan/sh）解释器，
// 这些命令不是可执行文件也不在内部命令集，直接执行必然失败——与其让模型空转，不如在
// 报错里明确把它引导回标准 POSIX 命令。mkdir/rmdir/date/ping 等本就是 POSIX 命令，不列入。
var windowsToPosix = map[string]string{
	"dir":      "ls 或 ls -la",
	"findstr":  "grep",
	"copy":     "cp",
	"xcopy":    "cp -r",
	"del":      "rm",
	"erase":    "rm",
	"move":     "mv",
	"ren":      "mv",
	"rename":   "mv",
	"more":     "cat（可分页用 cat | less）",
	"where":    "which",
	"cls":      "无需（DSC 终端由 TUI 管理）",
	"tasklist": "ps",
	"taskkill": "kill",
	"ipconfig": "ip addr 或 ifconfig",
	"md":       "mkdir -p",
	"rd":       "rm -r",
	"attrib":   "chmod",
}

// posixHint 返回对已知 Windows 专属命令的 POSIX 替代指引；非 Windows 专属命令返回空。
// 只在命令执行失败时追加，避免干扰真实外部命令的正常报错。
func posixHint(cmd string) string {
	if alt, ok := windowsToPosix[cmd]; ok {
		return fmt.Sprintf("\n提示：%q 是 Windows 专属命令，DSC 的 shell 是 POSIX（mvdan/sh）解释器，不提供该命令。请改用标准 POSIX 命令：%s", cmd, alt)
	}
	return ""
}

// slashErr 把错误字符串里的路径归一为正斜杆（Windows 上 os.* 错误内嵌反斜杆
// 路径，直接回显给模型/用户时与其余正斜杆路径风格不一致）。委托 core.SlashErrText
// 公共实现（与 str_replace_editor 的 error 版归一收敛同一份替换逻辑，见
// core/errslash.go；对齐 AGENTS.md 重复逻辑必须抽取）。
func slashErr(err error) string {
	if err == nil {
		return ""
	}
	return core.SlashErrText(err.Error())
}

// shellExecHandler 是 interp.ExecHandler 的入口：命中内部工具表走进程内实现，
// 否则回退默认外部执行；对 Windows 专属命令失败时附加 POSIX 替代指引。
func shellExecHandler(ctx context.Context, args []string) error {
	if fn, ok := internalCommands[args[0]]; ok {
		hc := interp.HandlerCtx(ctx)
		return fn(ctx, hc, args[1:])
	}
	err := defaultExecHandler(ctx, args)
	if err != nil {
		if hint := posixHint(args[0]); hint != "" {
			return fmt.Errorf("%v%s", err, hint)
		}
	}
	return err
}

// internalCommand 进程内实现一个命令：Write 到 hc.Stdout/hc.Stderr，
// 非零退出用 interp.NewExitStatus 返回。
type internalCommand func(ctx context.Context, hc interp.HandlerContext, args []string) error

// internalCommands 进程内命令表：命中即不再回落 PATH 外部程序（含 Windows 上
// C:\Windows\tree.com、C:\Windows\System32\sort.exe 之类同名程序）。find 与 sort/uniq
// 的实现独立成文件（前者含表达式解析与 fnmatch，后者含排序键/fnmatch 无关的比较口径，
// 见 internalcmds_find.go、internalcmds_sort.go）。
var internalCommands = map[string]internalCommand{
	"mkdir": cmdMkdir,
	"ls":    cmdLs,
	"cat":   cmdCat,
	"touch": cmdTouch,
	"rm":    cmdRm,
	"cp":    cmdCp,
	"mv":    cmdMv,
	"find":  cmdFind,
	"grep":  cmdGrep,
	"sort":  cmdSort,
	"uniq":  cmdUniq,
	"head":  cmdHead,
	"tail":  cmdTail,
	"wc":    cmdWc,
	"tree":  cmdTree,
}

// ---------- 共享辅助 ----------

// res 把参数里的路径解析为绝对路径（相对路径基于 hc.Dir，即 shell 当前工作目录）。
// Windows 上 /dev/null 特判归一为 NUL 设备：mvdan 的 DefaultOpenHandler 对重定向
// 目标做了同样特判（handler.go），但作为命令参数（如 cat /dev/null）不经该路径，
// 而它又不是合法的文件路径（会被 Join 到工作区下报错），对齐特判使两种用法一致。
func res(hc interp.HandlerContext, p string) string {
	if p == "" {
		return ""
	}
	if runtime.GOOS == "windows" && p == "/dev/null" {
		return "NUL"
	}
	// Windows 上没有真正的 /tmp（会被解析成当前盘根）：把 /tmp 与 /tmp/... 映射到
	// DSC 自己的临时目录，模型的 Unix 习惯写法（临时脚本、diff 中间产物）都能落地。
	// Unix 上 /tmp 是真实挂载点，不动。
	if runtime.GOOS == "windows" && (p == "/tmp" || strings.HasPrefix(p, "/tmp/")) {
		rel := strings.TrimPrefix(strings.TrimPrefix(p, "/tmp"), "/")
		if rel == "" {
			return dsc.TempDir()
		}
		return dsc.PJoin(dsc.TempDir(), rel)
	}
	if filepath.IsAbs(p) {
		return p
	}
	return dsc.PJoin(hc.Dir, p)
}

// openReader 打开文件（"-" 表示 stdin）读取；返回待 close 句柄。
// stdin 为 nil 时（生产 runner 用 interp.StdIO(nil, ...)）按空输入处理：
// 否则任何在 nil io.Reader 上读的内建命令（grep/cat/sort…）都会 nil 解引用 panic。
func openReader(hc interp.HandlerContext, file string) (io.Reader, func(), error) {
	if file == "-" {
		if hc.Stdin == nil {
			return strings.NewReader(""), func() {}, nil
		}
		return hc.Stdin, func() {}, nil
	}
	f, err := os.Open(res(hc, file))
	if err != nil {
		return nil, nil, err
	}
	return f, func() { _ = f.Close() }, nil
}

// lineReader 逐行读取（对齐 bufio.ScanLines：以 \n 分行、剥行尾 \r、末行无换行也算一行），
// 且不受 bufio.Scanner 默认 64 KiB 行长上限约束——Scanner 超限会直接停止扫描，那等于静默
// 丢弃余下内容（head/tail/sort 都吃过这个亏），故统一走此实现。
type lineReader struct {
	br      *bufio.Reader
	readErr error
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{br: bufio.NewReader(r)} }

// next 取下一行；ok=false 表示已到末尾或读取失败（用 err() 区分）。
func (lr *lineReader) next() (string, bool) {
	for lr.readErr == nil {
		s, err := lr.br.ReadString('\n')
		if len(s) > 0 {
			return strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r"), true
		}
		if err != nil {
			if err != io.EOF {
				lr.readErr = err
			}
			return "", false
		}
	}
	return "", false
}

// err 返回读取过程中的真实错误（正常到末尾返回 nil）。
func (lr *lineReader) err() error { return lr.readErr }

// capHint 限制环形缓冲的初始容量：计数可能被模型写成极大值（如 tail -n 999999999），按 N
// 直接预分配会瞬时吃掉巨量内存；append 会按需增长，初始容量给个合理上限即可。
func capHint(n int64) int {
	if n <= 0 {
		return 0
	}
	if n > 4096 {
		return 4096
	}
	return int(n)
}

// ---------- mkdir ----------

func cmdMkdir(ctx context.Context, hc interp.HandlerContext, args []string) error {
	parents := false
	var dirs []string
	for _, a := range args {
		switch a {
		case "-p", "--parents":
			parents = true
		case "--":
		default:
			dirs = append(dirs, a)
		}
	}
	if len(dirs) == 0 {
		fmt.Fprintln(hc.Stderr, "mkdir: missing operand")
		return interp.NewExitStatus(2)
	}
	failed := false
	for _, d := range dirs {
		if d == "" {
			continue
		}
		p := res(hc, d)
		var err error
		if err = ctx.Err(); err != nil {
			return err
		}
		if parents {
			err = os.MkdirAll(p, 0o755)
		} else {
			err = os.Mkdir(p, 0o755)
		}
		if err != nil {
			fmt.Fprintf(hc.Stderr, "mkdir: cannot create directory '%s': %v\n", d, err)
			failed = true
		}
	}
	if failed {
		return interp.NewExitStatus(1)
	}
	return nil
}

// ---------- ls ----------

func cmdLs(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var showAll, showAlmostAll, long, listSelf, singleCol, classify, byTime, bySize, reverse bool
	var paths []string
	endOpt := false
	for _, a := range args {
		switch {
		case endOpt:
			paths = append(paths, a)
		case a == "--":
			endOpt = true
		case strings.HasPrefix(a, "--time-style"):
			// 仅影响时间列格式：本实现的 -l 已输出稳定可读时间戳，接受该选项即可
		case strings.HasPrefix(a, "--color"), a == "--group-directories-first":
			// 输出修饰：接受即忽略
		case a == "--all":
			showAll = true
		case a == "--almost-all":
			showAlmostAll = true
		case strings.HasPrefix(a, "-") && len(a) > 1 && a != "-":
			ok := true
			for _, f := range a[1:] {
				switch f {
				case 'a':
					showAll = true
				case 'A':
					// -A：除 . 与 .. 外的全部。本实现本就不列 . 与 ..，故与 -a 同义
					showAlmostAll = true
				case 'l':
					long = true
				case 'd':
					listSelf = true
				case 'h':
				case '1':
					singleCol = true
				case 'F':
					// -F 分类符：目录 /、可执行 *、符号链接 @、FIFO |、套接字 =
					classify = true
				case 't':
					// -t 按修改时间排序（新在前）
					byTime = true
				case 'S':
					// -S 按大小排序（大在前）
					bySize = true
				case 'r':
					// -r 逆序
					reverse = true
				default:
					ok = false
				}
			}
			if !ok {
				fmt.Fprintf(hc.Stderr, "ls: unsupported option: %s\n", a)
				return interp.NewExitStatus(2)
			}
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	multi := len(paths) > 1
	exit := 0
	for idx, p0 := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := res(hc, p0)
		fi, err := os.Lstat(p)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "ls: cannot access '%s': %s\n", p0, slashErr(err))
			exit = 2
			continue
		}
		if multi {
			if idx > 0 {
				fmt.Fprintln(hc.Stdout)
			}
			fmt.Fprintln(hc.Stdout, p0+":")
		}
		if fi.IsDir() && !listSelf {
			if err := lsDir(ctx, hc, p, showAll || showAlmostAll, long, singleCol, classify, byTime, bySize, reverse); err != nil {
				return err
			}
		} else {
			lsEntry(hc, fi, p0, long, classify)
		}
	}
	if exit != 0 {
		return interp.NewExitStatus(2)
	}
	return nil
}

// lsItem 单个目录项：名字 + stat（-t/-S/-F 均需要 FileInfo，故一次性取齐）。
type lsItem struct {
	name string
	fi   os.FileInfo
}

// sortLsItems 对目录项排序：默认按名升序；-t 按修改时间新在前；-S 按大小大在前；
// -r 整体逆序。Stable 保证同键（如同 mtime）时保持目录序，输出确定。
func sortLsItems(items []lsItem, byTime, bySize, reverse bool) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if reverse {
			a, b = b, a
		}
		switch {
		case byTime:
			return a.fi.ModTime().After(b.fi.ModTime())
		case bySize:
			return a.fi.Size() > b.fi.Size()
		default:
			return a.name < b.name
		}
	})
}

// lsClassifySuffix 返回 -F 分类符（POSIX ls -F 语义）。
func lsClassifySuffix(fi os.FileInfo) string {
	m := fi.Mode()
	switch {
	case m&os.ModeSymlink != 0:
		return "@"
	case m&os.ModeNamedPipe != 0:
		return "|"
	case m&os.ModeSocket != 0:
		return "="
	case m.IsDir():
		return "/"
	case m.Perm()&0o111 != 0:
		return "*"
	}
	return ""
}

// lsName 返回展示名：classify 时追加 -F 分类符。
func lsName(name string, fi os.FileInfo, classify bool) string {
	if classify {
		return name + lsClassifySuffix(fi)
	}
	return name
}

func lsDir(ctx context.Context, hc interp.HandlerContext, p string, showAll, long, singleCol, classify, byTime, bySize, reverse bool) error {
	entries, err := os.ReadDir(p)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "ls: %v\n", err)
		return interp.NewExitStatus(2)
	}
	items := make([]lsItem, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !showAll && strings.HasPrefix(name, ".") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, lsItem{name: name, fi: fi})
	}
	sortLsItems(items, byTime, bySize, reverse)
	if long {
		for _, it := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			lsEntry(hc, it.fi, it.name, true, classify)
		}
	} else if singleCol {
		for _, it := range items {
			if err := ctx.Err(); err != nil {
				return err
			}
			fmt.Fprintln(hc.Stdout, lsName(it.name, it.fi, classify))
		}
	} else {
		names := make([]string, 0, len(items))
		for _, it := range items {
			names = append(names, lsName(it.name, it.fi, classify))
		}
		fmt.Fprintln(hc.Stdout, strings.Join(names, "  "))
	}
	return nil
}

func lsEntry(hc interp.HandlerContext, fi os.FileInfo, name string, long, classify bool) {
	if long {
		sz := fi.Size()
		t := fi.ModTime().Format("Jan _2 15:04")
		fmt.Fprintf(hc.Stdout, "%s %8d %s %s\n", fi.Mode().String(), sz, t, lsName(name, fi, classify))
	} else {
		fmt.Fprintln(hc.Stdout, lsName(name, fi, classify))
	}
}

// ---------- cat ----------

func cmdCat(ctx context.Context, hc interp.HandlerContext, args []string) error {
	files := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--" {
			continue
		}
		files = append(files, a)
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	exit := 0
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, close, err := openReader(hc, f)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "cat: %s: %v\n", f, err)
			exit = 1
			continue
		}
		if _, err := io.Copy(hc.Stdout, r); err != nil {
			fmt.Fprintf(hc.Stderr, "cat: %s: %v\n", f, err)
			exit = 1
		}
		close()
	}
	if exit != 0 {
		return interp.NewExitStatus(1)
	}
	return nil
}

// ---------- touch ----------

func cmdTouch(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var files []string
	for _, a := range args {
		if a == "--" {
			continue
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			// 简化的 touch 不加 -t/-d/-m 等时间参数支持，但容忍未知旗标继续创建
			continue
		}
		files = append(files, a)
	}
	if len(files) == 0 {
		fmt.Fprintln(hc.Stderr, "touch: missing file operand")
		return interp.NewExitStatus(2)
	}
	failed := false
	now := time.Now()
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := res(hc, f)
		h, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "touch: cannot touch '%s': %v\n", f, err)
			failed = true
			continue
		}
		_ = h.Close()
		if err := os.Chtimes(p, now, now); err != nil && !failed {
			failed = true
		}
	}
	if failed {
		return interp.NewExitStatus(1)
	}
	return nil
}

// ---------- rm ----------

func cmdRm(ctx context.Context, hc interp.HandlerContext, args []string) error {
	recursive, force, endOpt := false, false, false
	var files []string
	for _, a := range args {
		switch {
		case endOpt:
			files = append(files, a)
		case a == "--":
			endOpt = true
		case a == "--recursive":
			recursive = true
		case a == "--force":
			force = true
		case len(a) > 1 && a[0] == '-':
			// 组合短旗标：`rm -rf dir` 是日常写法，此前只认独立的 "-r"/"-f"，
			// 整串 "-rf" 被当成路径，报 cannot remove '-rf'（现场报告复现 22 次）。
			for _, f := range a[1:] {
				switch f {
				case 'r', 'R':
					recursive = true
				case 'f':
					force = true
				case 'v', 'i', 'd':
					// -v 冗长、-i 交互确认（无交互终端等同忽略）、-d 删空目录：
					// 接受即可，不改变本实现语义
				default:
					fmt.Fprintf(hc.Stderr, "rm: unsupported option: -%c（支持：-r/-R -f -d -v）\n", f)
					return interp.NewExitStatus(2)
				}
			}
		default:
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		if !force {
			fmt.Fprintln(hc.Stderr, "rm: missing operand")
			return interp.NewExitStatus(2)
		}
		return nil
	}
	failed := false
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := res(hc, f)
		fi, statErr := os.Lstat(p)
		if statErr != nil {
			if force {
				continue
			}
			fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': %s\n", f, slashErr(statErr))
			failed = true
			continue
		}
		var err error
		if fi.IsDir() && !recursive {
			fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': Is a directory\n", f)
			failed = true
			continue
		}
		err = os.RemoveAll(p)
		if err != nil && !force {
			fmt.Fprintf(hc.Stderr, "rm: cannot remove '%s': %v\n", f, err)
			failed = true
		}
	}
	if failed {
		return interp.NewExitStatus(1)
	}
	return nil
}

// ---------- cp / mv ----------

func cmdCp(ctx context.Context, hc interp.HandlerContext, args []string) error {
	recursive := false
	var ops []string
	for _, a := range args {
		switch a {
		case "-r", "-R", "--recursive":
			recursive = true
		case "--":
		default:
			ops = append(ops, a)
		}
	}
	return copyOrMove(ctx, hc, ops, recursive, false)
}

func cmdMv(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var ops []string
	for _, a := range args {
		if a == "--" {
			continue
		}
		ops = append(ops, a)
	}
	return copyOrMove(ctx, hc, ops, true, true)
}

func copyOrMove(ctx context.Context, hc interp.HandlerContext, ops []string, recursive, move bool) error {
	verb := "cp"
	if move {
		verb = "mv"
	}
	if len(ops) < 2 {
		fmt.Fprintf(hc.Stderr, "%s: missing file operand\n", verb)
		return interp.NewExitStatus(2)
	}
	dst := ops[len(ops)-1]
	srcs := ops[:len(ops)-1]
	dstIsDir := false
	if fi, err := os.Stat(res(hc, dst)); err == nil && fi.IsDir() {
		dstIsDir = true
	}
	if len(srcs) > 1 && !dstIsDir {
		fmt.Fprintf(hc.Stderr, "%s: target '%s' is not a directory\n", verb, dst)
		return interp.NewExitStatus(1)
	}
	failed := false
	for _, s := range srcs {
		if err := ctx.Err(); err != nil {
			return err
		}
		src := res(hc, s)
		fi, err := os.Lstat(src)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "%s: %s: %v\n", verb, s, err)
			failed = true
			continue
		}
		// 目标程序：多源/目标为目录→并入该目录。
		dstPath := res(hc, dst)
		if dstIsDir {
			dstPath = dsc.PJoin(dstPath, filepath.Base(src))
		}
		if fi.IsDir() && !recursive {
			fmt.Fprintf(hc.Stderr, "%s: %s: is a directory (not copied); use -r\n", verb, s)
			failed = true
			continue
		}
		var errOp error
		if move {
			errOp = moveEntry(src, dstPath)
		} else {
			errOp = copyEntry(src, dstPath, fi)
		}
		if errOp != nil {
			fmt.Fprintf(hc.Stderr, "%s: %s -> %s: %s\n", verb, s, dst, slashErr(errOp))
			failed = true
		}
	}
	if failed {
		return interp.NewExitStatus(1)
	}
	return nil
}

// moveEntry 移动：优先 os.Rename；跨设备失败时降级为 拷贝+删除。
func moveEntry(src, dst string) error {
	if err := os.MkdirAll(dsc.PDir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyEntry(src, dst, nil); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

// copyEntry 复制文件或目录树（fi 为 src 的 stat，可为 nil 时自取）。
func copyEntry(src, dst string, fi os.FileInfo) error {
	if fi == nil {
		var err error
		fi, err = os.Lstat(src)
		if err != nil {
			return err
		}
	}
	if fi.IsDir() {
		return dsc.PWalkDir(src, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := dsc.PRel(src, p)
			if rel == "." {
				return os.MkdirAll(dst, 0o755)
			}
			dp := dsc.PJoin(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(dp, 0o755)
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if target, e := os.Readlink(p); e == nil {
					if e = os.Symlink(target, dp); e != nil {
						return e
					}
				}
				return nil
			}
			return copyFile(p, dp)
		})
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(dsc.PDir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// ---------- grep ----------
//
// 实现见 grep.go（参数解析、BRE→RE2 转换、上下文行、--include 过滤）。
// 此处仅保留递归遍历共用的噪声目录表，供 grep/find 共用口径。

// grepSkipDirs 递归搜索（-r/-R）时跳过的噪声目录：VCS 元数据与依赖/缓存目录。
// .git 内多为二进制对象、node_modules/__pycache__ 内是依赖与缓存，模型在真实项目里
// grep -rn 时它们只会带来海量无意义命中，与编辑器目录列举、技能扫描的噪声目录口径一致。
// 其余目录（含 .github、.env 等点条目）一律照常搜索——不静默丢弃可能的真实命中。
var grepSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"__pycache__":  true,
	".svn":         true,
	".hg":          true,
}

// ---------- head / tail ----------

// headTailMode 计数模式：行（默认）或字节（-c）。
type headTailMode int

const (
	headTailLines headTailMode = iota
	headTailBytes
)

// headTailArgs head/tail 的解析结果（两者参数面相同，故解析与执行各一份、共用）。
type headTailArgs struct {
	n     int64        // 计数：行数或字节数
	fromN bool         // +N 语义（tail）：自第 N 行/字节起至末尾
	mode  headTailMode // -c 时为按字节
	quiet bool         // -q：多文件也不打表头
	force bool         // -v：单文件也打表头
	files []string
}

// headTailSupported 报错文案里列出的受支持旗标。
func headTailSupported(tail bool) string {
	if tail {
		return "-n N/-nN、-c N/-cN、传统 -N 与 +N、-q、-v；不支持 -f/--follow"
	}
	return "-n N/-nN、-c N/-cN、传统 -N、-q、-v"
}

// parseHeadTail 解析 head/tail 参数。
//
// 此前实现对未识别旗标**静默忽略**，比报错更糟——`head -2 f.txt` 会输出 10 行、`tail -2`
// 同理、`head -c 5 f.txt` 会把 "5" 当成文件名去打开，模型据此拿到错结果却毫不知情。现改为：
// 支持传统 -N 与 -n/-c 的常用形式（含 tail 的 +N），其余一律响亮报错并列出支持清单。
func parseHeadTail(cmd string, args []string, tail bool) (headTailArgs, error) {
	a := headTailArgs{n: 10, mode: headTailLines}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			a.files = append(a.files, args[i+1:]...)
			i = len(args)
		case arg == "-" || (!strings.HasPrefix(arg, "-") && !(tail && strings.HasPrefix(arg, "+"))):
			a.files = append(a.files, arg)
		case strings.HasPrefix(arg, "-n") || strings.HasPrefix(arg, "-c"):
			mode := headTailLines
			if arg[1] == 'c' {
				mode = headTailBytes
			}
			val := arg[2:]
			if val == "" {
				if i+1 >= len(args) {
					return a, fmt.Errorf("option %s requires an argument", arg)
				}
				i++
				val = args[i]
			}
			nv, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				return a, fmt.Errorf("invalid argument %q for %s", val, arg)
			}
			a.mode, a.n = mode, nv
			// GNU tail -n +N：自第 N 行/字节起至末尾（head 的 +N 只是等价于 N）
			a.fromN = tail && strings.HasPrefix(val, "+")
		case arg == "-q" || arg == "--quiet":
			a.quiet = true
		case arg == "-v" || arg == "--verbose":
			a.force = true
		case tail && strings.HasPrefix(arg, "+"):
			// 传统 tail +N：自第 N 行起至末尾
			nv, err := strconv.ParseInt(arg[1:], 10, 64)
			if err != nil {
				return a, fmt.Errorf("invalid argument %q", arg)
			}
			a.fromN, a.n, a.mode = true, nv, headTailLines
		case isLegacyCount(arg):
			// 传统 -N：行数
			nv, err := strconv.ParseInt(arg[1:], 10, 64)
			if err != nil {
				return a, fmt.Errorf("invalid argument %q", arg)
			}
			a.n, a.mode = nv, headTailLines
		default:
			return a, fmt.Errorf("unsupported option: %s (supported here: %s)", arg, headTailSupported(tail))
		}
	}
	return a, nil
}

// isLegacyCount 判断是否为传统 -N 形式（连字符后全为数字，如 -5）。
func isLegacyCount(arg string) bool {
	if len(arg) < 2 || arg[0] != '-' {
		return false
	}
	for i := 1; i < len(arg); i++ {
		if arg[i] < '0' || arg[i] > '9' {
			return false
		}
	}
	return true
}

func cmdHead(ctx context.Context, hc interp.HandlerContext, args []string) error {
	a, err := parseHeadTail("head", args, false)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "head: %s\n", err)
		return interp.NewExitStatus(2)
	}
	return runHeadTail(ctx, hc, "head", a, false)
}

func cmdTail(ctx context.Context, hc interp.HandlerContext, args []string) error {
	a, err := parseHeadTail("tail", args, true)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "tail: %s\n", err)
		return interp.NewExitStatus(2)
	}
	return runHeadTail(ctx, hc, "tail", a, true)
}

// runHeadTail 执行 head/tail：多文件默认打 `==> name <==` 表头（-q 抑制、-v 单文件也打）。
func runHeadTail(ctx context.Context, hc interp.HandlerContext, cmd string, a headTailArgs, tail bool) error {
	if len(a.files) == 0 {
		a.files = []string{"-"}
	}
	header := a.force || (len(a.files) > 1 && !a.quiet)
	for _, f := range a.files {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, closeFn, err := openReader(hc, f)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "%s: %s: %s\n", cmd, f, slashErr(err))
			return interp.NewExitStatus(1)
		}
		if header {
			fmt.Fprintf(hc.Stdout, "==> %s <==\n", f)
		}
		var runErr error
		switch {
		case a.mode == headTailBytes && tail:
			runErr = tailBytes(hc.Stdout, r, a)
		case a.mode == headTailBytes:
			runErr = headBytes(hc.Stdout, r, a.n)
		case tail:
			runErr = tailLines(hc.Stdout, r, a)
		default:
			runErr = headLines(hc.Stdout, r, a.n)
		}
		closeFn()
		if runErr != nil {
			fmt.Fprintf(hc.Stderr, "%s: %s: %s\n", cmd, f, slashErr(runErr))
			return interp.NewExitStatus(1)
		}
	}
	return nil
}

// headLines 输出前 n 行；n<0 时输出「除末尾 |n| 行之外的全部」（GNU head -n -K），以
// |n|+1 行环形缓冲流式实现，内存 O(|n|)、无需整体读入。
func headLines(w io.Writer, r io.Reader, n int64) error {
	lr := newLineReader(r)
	if n >= 0 {
		for i := int64(0); i < n; i++ {
			line, ok := lr.next()
			if !ok {
				return lr.err()
			}
			fmt.Fprintln(w, line)
		}
		return nil
	}
	ring := make([]string, 0, capHint(-n))
	for {
		line, ok := lr.next()
		if !ok {
			return lr.err()
		}
		ring = append(ring, line)
		if int64(len(ring)) > -n {
			fmt.Fprintln(w, ring[0])
			ring = ring[1:]
		}
	}
}

// tailLines 输出末尾 n 行（n<0 与 n 同义，对齐 GNU tail -n -K）；fromN 时自第 n 行起输出
// 至末尾（GNU tail -n +N / 传统 +N）。
func tailLines(w io.Writer, r io.Reader, a headTailArgs) error {
	lr := newLineReader(r)
	if a.fromN {
		for i := int64(1); i < a.n; i++ {
			if _, ok := lr.next(); !ok {
				return lr.err()
			}
		}
		for {
			line, ok := lr.next()
			if !ok {
				return lr.err()
			}
			fmt.Fprintln(w, line)
		}
	}
	n := a.n
	if n < 0 {
		n = -n
	}
	ring := make([]string, 0, capHint(n))
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		ring = append(ring, line)
		if int64(len(ring)) > n {
			ring = ring[1:]
		}
	}
	if err := lr.err(); err != nil {
		return err
	}
	for _, l := range ring {
		fmt.Fprintln(w, l)
	}
	return nil
}

// headBytes 输出前 n 字节（n<0 时输出除末尾 |n| 字节外的全部）。可 seek 的普通文件按大小
// 直接截断（不必整体读入）；管道等不可 seek 的输入在 n<0 时需整体读入（罕见组合，接受）。
func headBytes(w io.Writer, r io.Reader, n int64) error {
	if n >= 0 {
		_, err := io.CopyN(w, r, n)
		if err == io.EOF {
			return nil
		}
		return err
	}
	if f, ok := r.(*os.File); ok {
		if size, err := f.Seek(0, io.SeekEnd); err == nil {
			if _, err := f.Seek(0, io.SeekStart); err == nil {
				take := size + n
				if take < 0 {
					take = 0
				}
				_, cerr := io.CopyN(w, f, take)
				if cerr == io.EOF {
					return nil
				}
				return cerr
			}
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if int64(len(data)) > -n {
		data = data[:int64(len(data))+n]
	}
	_, werr := w.Write(data)
	return werr
}

// tailBytes 输出末尾 n 字节；fromN 时自第 n 字节起输出至末尾。可 seek 的普通文件直接定位
// （tail -c 100 大文件不必整体读入）；管道等不可 seek 的输入用滚动缓冲只保留末尾 n 字节。
func tailBytes(w io.Writer, r io.Reader, a headTailArgs) error {
	if a.fromN {
		if skip := a.n - 1; skip > 0 {
			if _, err := io.CopyN(io.Discard, r, skip); err != nil && err != io.EOF {
				return err
			}
		}
		_, err := io.Copy(w, r)
		return err
	}
	n := a.n
	if n < 0 {
		n = -n
	}
	if n == 0 {
		return nil
	}
	if f, ok := r.(*os.File); ok {
		if size, err := f.Seek(0, io.SeekEnd); err == nil {
			start := size - n
			if start < 0 {
				start = 0
			}
			if _, err := f.Seek(start, io.SeekStart); err == nil {
				_, cerr := io.Copy(w, f)
				return cerr
			}
		}
	}
	buf := make([]byte, 0, capHint(n))
	chunk := make([]byte, 32*1024)
	for {
		nr, err := r.Read(chunk)
		if nr > 0 {
			buf = append(buf, chunk[:nr]...)
			if int64(len(buf)) > n {
				buf = buf[int64(len(buf))-n:]
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	_, werr := w.Write(buf)
	return werr
}

// ---------- wc ----------

// cmdWc 统计行/词/字节/字符：-l 行、-w 词、-c 字节、-m 字符（UTF-8 码点）。
//
// 此前实现把**任意字母**都收进旗标集合，未识别的（如 -m/-L/--bytes）会被静默忽略：`wc -m f`
// 因 printWc 找不到可打印项而只输出一个空格行——静默错结果。现改为白名单 + 响亮报错。
func cmdWc(ctx context.Context, hc interp.HandlerContext, args []string) error {
	flags := map[byte]bool{}
	var files []string
	for _, a := range args {
		switch {
		case a == "--":
		case strings.HasPrefix(a, "-") && len(a) > 1 && a != "-":
			for _, f := range a[1:] {
				switch byte(f) {
				case 'l', 'w', 'c', 'm':
					flags[byte(f)] = true
				default:
					fmt.Fprintf(hc.Stderr, "wc: unsupported option: -%c (supported here: -l -w -c -m)\n", f)
					return interp.NewExitStatus(2)
				}
			}
		default:
			files = append(files, a)
		}
	}
	if len(files) == 0 {
		files = []string{"-"}
	}
	var tl, tw, tc, tm int64
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		r, close, err := openReader(hc, f)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "wc: %s: %s\n", f, slashErr(err))
			return interp.NewExitStatus(1)
		}
		l, w, c, m := countWc(r)
		tl += l
		tw += w
		tc += c
		tm += m
		// 对齐 GNU wc：stdin（"-"）单文件时不回显名字（如 find ... | wc -l 只出计数）
		displayName := f
		if f == "-" && len(files) == 1 {
			displayName = ""
		}
		printWc(hc, flags, l, w, m, c, displayName)
		close()
	}
	if len(files) > 1 {
		printWc(hc, flags, tl, tw, tm, tc, "total")
	}
	return nil
}

// countWc 统计行/词/字节/字符：字符按 UTF-8 码点计（对齐 GNU wc -m），跨读取块的不完整
// 多字节序列留待下一块续接；末尾残缺序列与非法字节各计一个字符。
func countWc(r io.Reader) (lines, words, bytes, chars int64) {
	buf := make([]byte, 4096)
	var pending []byte
	inWord := false
	for {
		n, err := r.Read(buf)
		data := append(pending, buf[:n]...) // pending 已复制，可安全拼接
		pending = nil
		for i := 0; i < len(data); i++ {
			b := data[i]
			bytes++
			if b == '\n' {
				lines++
			}
			if b == ' ' || b == '\t' || b == '\n' || b == '\r' {
				inWord = false
			} else if !inWord {
				inWord = true
				words++
			}
		}
		rest := data
		for len(rest) > 0 {
			r0, size := utf8.DecodeRune(rest)
			if r0 == utf8.RuneError && size == 1 && !utf8.FullRune(rest) {
				break // 不完整序列：留待下一块续接
			}
			chars++
			rest = rest[size:]
		}
		if len(rest) > 0 {
			pending = append([]byte(nil), rest...)
		}
		if err != nil {
			break
		}
	}
	chars += int64(utf8.RuneCount(pending))
	return
}

// printWc 按 GNU 的固定列序输出：行、词、字符、字节（与旗标书写顺序无关）。
func printWc(hc interp.HandlerContext, flags map[byte]bool, l, w, m, c int64, name string) {
	if len(flags) == 0 {
		if name == "" {
			fmt.Fprintf(hc.Stdout, " %7d %7d %7d\n", l, w, c)
		} else {
			fmt.Fprintf(hc.Stdout, " %7d %7d %7d %s\n", l, w, c, name)
		}
		return
	}
	var parts []string
	if flags['l'] {
		parts = append(parts, strconv.FormatInt(l, 10))
	}
	if flags['w'] {
		parts = append(parts, strconv.FormatInt(w, 10))
	}
	if flags['m'] {
		parts = append(parts, strconv.FormatInt(m, 10))
	}
	if flags['c'] {
		parts = append(parts, strconv.FormatInt(c, 10))
	}
	if name == "" {
		fmt.Fprintf(hc.Stdout, " %s\n", strings.Join(parts, " "))
	} else {
		fmt.Fprintf(hc.Stdout, " %s %s\n", strings.Join(parts, " "), name)
	}
}

// ---------- tree ----------

// cmdTree 目录树输出（GNU tree 常用子集）：-L n 限制展开层数、-I pattern 排除
// （| 分隔多模式，按 basename 匹配）、-a 含隐藏项、-d 仅目录。进程内实现的原因
// 与其余内建一致：Windows 上 PATH 命中的往往是 C:\Windows\tree.com——不支持
// -L/-I，输出还是系统 OEM 码页编码，且搜索不到文件内容；内建化后跨平台一致。
func cmdTree(ctx context.Context, hc interp.HandlerContext, args []string) error {
	depth := -1 // 无限制
	var excludes []string
	all, dirsOnly := false, false
	rootArg := "."
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-a":
			all = true
		case a == "-d":
			dirsOnly = true
		case a == "--":
		case a == "-L" && i+1 < len(args):
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 1 {
				fmt.Fprintf(hc.Stderr, "tree: invalid level: %s\n", args[i+1])
				return interp.NewExitStatus(2)
			}
			depth = n
			i++
		case strings.HasPrefix(a, "-L") && len(a) > 2:
			n, err := strconv.Atoi(a[2:])
			if err != nil || n < 1 {
				fmt.Fprintf(hc.Stderr, "tree: invalid level: %s\n", a[2:])
				return interp.NewExitStatus(2)
			}
			depth = n
		case a == "-I" && i+1 < len(args):
			for _, pat := range strings.Split(args[i+1], "|") {
				if pat != "" {
					excludes = append(excludes, pat)
				}
			}
			i++
		case strings.HasPrefix(a, "-") && a != "-":
			fmt.Fprintf(hc.Stderr, "tree: unsupported option: %s\n", a)
			return interp.NewExitStatus(2)
		default:
			rootArg = a
		}
	}
	rootPath := res(hc, rootArg)
	fi, err := os.Stat(rootPath)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "tree: %s: %s\n", rootArg, slashErr(err))
		return interp.NewExitStatus(2)
	}
	if !fi.IsDir() {
		fmt.Fprintf(hc.Stderr, "tree: %s: Not a directory\n", rootArg)
		return interp.NewExitStatus(2)
	}
	fmt.Fprintln(hc.Stdout, rootArg)
	var dirs, files int64
	if err := treeWalk(ctx, hc, rootPath, "", depth, 1, excludes, all, dirsOnly, &dirs, &files); err != nil {
		return err
	}
	fmt.Fprintf(hc.Stdout, "\n%d director%s, %d file%s\n",
		dirs, plural("y", "ies", dirs), files, plural("", "s", files))
	return nil
}

// plural 单复数选词：n==1 取单数词尾，否则复数。
func plural(singular, pluralSuffix string, n int64) string {
	if n == 1 {
		return singular
	}
	return pluralSuffix
}

// treeWalk 递归打印目录树。level 从 1 计（根的直接子项为 1），depth>0 时仅展开
// level<=depth 的目录（更深的目录列出但不展开，对齐 GNU tree -L）。目录在前、
// 按名升序，输出确定。返回 ctx 错误（取消）。
func treeWalk(ctx context.Context, hc interp.HandlerContext, dir, prefix string, depth, level int, excludes []string, all, dirsOnly bool, dirs, files *int64) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(hc.Stdout, "%s[opendir %s]: %s\n", prefix, filepath.ToSlash(dir), slashErr(err))
		return nil
	}
	type treeEntry struct {
		name  string
		isDir bool
	}
	var list []treeEntry
	for _, e := range entries {
		name := e.Name()
		if !all && strings.HasPrefix(name, ".") {
			continue
		}
		if matchAnyPattern(name, excludes) {
			continue
		}
		isDir := e.IsDir()
		if dirsOnly && !isDir {
			continue
		}
		list = append(list, treeEntry{name: name, isDir: isDir})
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].isDir != list[j].isDir {
			return list[i].isDir
		}
		return list[i].name < list[j].name
	})
	for i, it := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		last := i == len(list)-1
		branch := "├── "
		if last {
			branch = "└── "
		}
		fmt.Fprintln(hc.Stdout, prefix+branch+it.name)
		if !it.isDir {
			*files++
			continue
		}
		*dirs++
		if depth != -1 && level >= depth {
			continue
		}
		child := prefix + "│   "
		if last {
			child = prefix + "    "
		}
		if err := treeWalk(ctx, hc, dsc.PJoin(dir, it.name), child, depth, level+1, excludes, all, dirsOnly, dirs, files); err != nil {
			return err
		}
	}
	return nil
}

// matchAnyPattern basename 是否命中任一排除模式（GNU tree -I 语义，| 已拆分）。
func matchAnyPattern(name string, patterns []string) bool {
	for _, pat := range patterns {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
	}
	return false
}
