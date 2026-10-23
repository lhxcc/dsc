package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"

	dsc "dsc-sdk"
	"mvdan.cc/sh/v3/interp"
)

// grep 实现：POSIX 语义对齐 + 常用旗标。
//
// 为什么要有 BRE 转换：Go 的 regexp 是 ERE/RE2 语法，而 `grep`（不带 -E）的模式是
// POSIX BRE——在 BRE 里 `(`、`)`、`{`、`}`、`+`、`?`、`|` 都是**普通字符**。
// 直接把模式丢给 regexp 会让 `grep "add_library(foo" file` 报
// `error parsing regexp: missing closing )`，而真实 grep 能正常匹配。
// 现场报告里 27 次 grep 失败全部源于此，另有 24 次源于缺旗标（-A/-B/-m/-o/-F/
// --include）。两者都属实现缺失，不是模型用错。

// grepTarget 单个搜索目标：display 为模型可见路径（保留参数的书写形态，递归时为
// 「参数/相对路径」，按工作区路径纪律统一正斜杆），path 为磁盘路径（"-" 表示 stdin）。
type grepTarget struct {
	display string
	path    string
}

// grepOpts 解析后的 grep 选项。
type grepOpts struct {
	ignoreCase    bool
	invert        bool
	lineNum       bool
	countOnly     bool
	recursive     bool
	filesOnly     bool // -l：只输出含命中的文件名
	onlyMatching  bool // -o：只输出命中部分
	fixed         bool // -F：模式按字面文本
	extended      bool // -E/-G：ERE（Go regexp 原生）
	word          bool // -w：整词匹配
	before, after int  // -B/-A/-C 上下文行数
	maxCount      int  // -m N：每个文件最多命中 N 行（0 = 不限）

	includes    []string // --include=GLOB
	excludes    []string // --exclude=GLOB
	excludeDirs []string // --exclude-dir=GLOB
}

// grepSupported 报错时列出的受支持旗标（缺旗标时报错要能指引模型改用可用写法）。
const grepSupported = "-i -v -n -c -l -o -F -E -w -r/-R -m N -A N -B N -C N " +
	"--include=GLOB --exclude=GLOB --exclude-dir=GLOB"

func cmdGrep(ctx context.Context, hc interp.HandlerContext, args []string) error {
	opts, pat, patSet, files, err := parseGrepArgs(hc, args)
	if err != nil {
		return err
	}
	if !patSet {
		fmt.Fprintln(hc.Stderr, "grep: missing pattern")
		return interp.NewExitStatus(2)
	}
	re, cerr := compileGrepPattern(pat, opts)
	if cerr != nil {
		fmt.Fprintf(hc.Stderr, "grep: %v\n", cerr)
		return interp.NewExitStatus(2)
	}
	if len(files) == 0 {
		if opts.recursive {
			files = []string{"."}
		} else {
			files = []string{"-"}
		}
	}

	exit := 0
	targets := make([]grepTarget, 0, len(files))
	for _, f := range files {
		if f == "-" {
			targets = append(targets, grepTarget{display: f, path: "-"})
			continue
		}
		p := res(hc, f)
		fi, statErr := os.Stat(p)
		if statErr != nil {
			fmt.Fprintf(hc.Stderr, "grep: %s: %s\n", f, slashErr(statErr))
			exit = 2
			continue
		}
		if !fi.IsDir() {
			targets = append(targets, grepTarget{display: f, path: p})
			continue
		}
		if !opts.recursive {
			fmt.Fprintf(hc.Stderr, "grep: %s: Is a directory (use -r to search recursively)\n", f)
			exit = 2
			continue
		}
		grepCollect(ctx, hc, p, dsc.PClean(f), &opts, &targets)
	}

	// 多目标或 -r 时每行带文件名前缀（对齐 grep -r 的 path:line:text）
	showName := opts.recursive || len(targets) > 1
	matchedTotal := 0
	for _, tg := range targets {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, err := grepOne(hc, tg, re, opts, showName)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "grep: %s: %s\n", tg.display, slashErr(err))
			exit = 2
			continue
		}
		matchedTotal += count
		if opts.countOnly {
			if showName {
				fmt.Fprintf(hc.Stdout, "%s:%d\n", tg.display, count)
			} else {
				fmt.Fprintf(hc.Stdout, "%d\n", count)
			}
		}
	}
	if exit != 0 {
		return interp.NewExitStatus(uint8(exit))
	}
	// 无命中退出 1（对齐 GNU grep）。-l 同样如此：它只改变输出形态，不改变「是否有
	// 命中」这一事实。这一点对 `find ... -exec grep -l ... {} \;` 至关重要——-exec
	// 正是拿退出码当谓词真假，若 -l 无命中仍退 0，则「列出含 X 的文件」会退化成
	// 「列出全部文件」。-c 例外：DSC 约定输出计数（含 0）且退 0，供 `grep -c | ...`
	// 之类的管道取数。
	if !opts.countOnly && matchedTotal == 0 {
		return interp.NewExitStatus(1)
	}
	return nil
}

// parseGrepArgs 解析 grep 参数：支持组合短旗标（-rn、-oE）、紧贴取值（-m3、-A2）
// 与长选项（--include=…）。未知旗标明确报错并列出支持清单，不静默忽略。
func parseGrepArgs(hc interp.HandlerContext, args []string) (grepOpts, string, bool, []string, error) {
	var opts grepOpts
	var pat string
	patSet := false
	var files []string
	endOpt := false

	setPat := func(v string) {
		if !patSet {
			pat, patSet = v, true
		} else {
			files = append(files, v)
		}
	}
	needVal := func(i *int, rest string, name string) (string, error) {
		if rest != "" {
			return rest, nil
		}
		if *i+1 >= len(args) {
			fmt.Fprintf(hc.Stderr, "grep: option requires an argument -- %s\n", name)
			return "", fmt.Errorf("missing-arg")
		}
		*i++
		return args[*i], nil
	}

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case endOpt:
			setPat(a)
		case a == "--":
			endOpt = true
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a[2:], "=")
			switch name {
			case "include", "exclude", "exclude-dir":
				v := val
				if !hasVal {
					var err error
					if v, err = needVal(&i, "", name); err != nil {
						return opts, "", false, nil, interp.NewExitStatus(2)
					}
				}
				switch name {
				case "include":
					opts.includes = append(opts.includes, v)
				case "exclude":
					opts.excludes = append(opts.excludes, v)
				default:
					opts.excludeDirs = append(opts.excludeDirs, v)
				}
			case "color", "colour", "line-buffered", "binary-files", "no-messages":
				// 输出修饰类选项：不影响匹配结果，接受即忽略
			case "fixed-strings":
				opts.fixed = true
			case "extended-regexp":
				opts.extended = true
			case "basic-regexp":
				opts.extended = false
			case "ignore-case":
				opts.ignoreCase = true
			case "invert-match":
				opts.invert = true
			case "line-number":
				opts.lineNum = true
			case "count":
				opts.countOnly = true
			case "files-with-matches":
				opts.filesOnly = true
			case "only-matching":
				opts.onlyMatching = true
			case "word-regexp":
				opts.word = true
			case "recursive", "dereference-recursive":
				opts.recursive = true
			case "max-count", "after-context", "before-context", "context":
				v := val
				if !hasVal {
					var err error
					if v, err = needVal(&i, "", name); err != nil {
						return opts, "", false, nil, interp.NewExitStatus(2)
					}
				}
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n < 0 {
					fmt.Fprintf(hc.Stderr, "grep: invalid argument %q for --%s\n", v, name)
					return opts, "", false, nil, interp.NewExitStatus(2)
				}
				switch name {
				case "max-count":
					opts.maxCount = n
				case "after-context":
					opts.after = n
				case "before-context":
					opts.before = n
				default:
					opts.before, opts.after = n, n
				}
			default:
				fmt.Fprintf(hc.Stderr, "grep: unsupported option: --%s（支持：%s）\n", name, grepSupported)
				return opts, "", false, nil, interp.NewExitStatus(2)
			}
		case len(a) > 1 && a[0] == '-':
			for j := 1; j < len(a); j++ {
				f := a[j]
				switch f {
				case 'i':
					opts.ignoreCase = true
				case 'v':
					opts.invert = true
				case 'n':
					opts.lineNum = true
				case 'c':
					opts.countOnly = true
				case 'l':
					opts.filesOnly = true
				case 'o':
					opts.onlyMatching = true
				case 'F':
					opts.fixed = true
				case 'E':
					opts.extended = true
				case 'G':
					opts.extended = false
				case 'w':
					opts.word = true
				case 'r', 'R':
					opts.recursive = true
				case 'h', 'H', 's':
					// 文件名前缀/错误输出修饰：接受即忽略（前缀由目标数决定）
				case 'e':
					v, err := needVal(&i, a[j+1:], "e")
					if err != nil {
						return opts, "", false, nil, interp.NewExitStatus(2)
					}
					pat, patSet = v, true
					j = len(a)
				case 'm', 'A', 'B', 'C':
					v, err := needVal(&i, a[j+1:], string(f))
					if err != nil {
						return opts, "", false, nil, interp.NewExitStatus(2)
					}
					n, err := strconv.Atoi(strings.TrimSpace(v))
					if err != nil || n < 0 {
						fmt.Fprintf(hc.Stderr, "grep: invalid context length argument %q\n", v)
						return opts, "", false, nil, interp.NewExitStatus(2)
					}
					switch f {
					case 'm':
						opts.maxCount = n
					case 'A':
						opts.after = n
					case 'B':
						opts.before = n
					default:
						opts.before, opts.after = n, n
					}
					j = len(a)
				default:
					fmt.Fprintf(hc.Stderr, "grep: unsupported option: -%c（支持：%s）\n", f, grepSupported)
					return opts, "", false, nil, interp.NewExitStatus(2)
				}
			}
		default:
			setPat(a)
		}
	}
	return opts, pat, patSet, files, nil
}

// compileGrepPattern 按选项把模式编译为 RE2：-F 走字面文本，默认按 POSIX BRE
// 语义转换（见 breToERE），-E 直接用 ERE。
func compileGrepPattern(pat string, opts grepOpts) (*regexp.Regexp, error) {
	switch {
	case opts.fixed:
		pat = regexp.QuoteMeta(pat)
	case !opts.extended:
		pat = breToERE(pat)
	}
	if opts.word {
		pat = `\b(?:` + pat + `)\b`
	}
	if opts.ignoreCase {
		pat = "(?i)" + pat
	}
	return regexp.Compile(pat)
}

// breToERE 把 POSIX BRE 转成 RE2 可用的 ERE 形式。
//
// BRE 的分组/区间/交替/量词必须转义才生效（\(…\)、\{m,n\}、\|），未转义的
// `(`、`)`、`{`、`}`、`+`、`?`、`|` 是普通字符。Go 的 regexp 恰好相反，
// 因此：未转义的上述字符 → 转义为字面量；已转义的 → 去掉反斜杆交给 RE2 当元字符。
// 其余转义序列（\b、\.、\*、\[、\d …）原样保留。注意 RE2 不支持反向引用 \1，
// 这类模式会得到"invalid escape sequence"错误——与 Go 生态一致，属已知限制。
func breToERE(pat string) string {
	var b strings.Builder
	b.Grow(len(pat) + 8)
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		if c == '\\' {
			if i+1 >= len(pat) {
				b.WriteByte('\\') // 尾部孤立反斜杆：交给 RE2 报错
				break
			}
			n := pat[i+1]
			if strings.ContainsRune("(){}|+?", rune(n)) {
				b.WriteByte(n) // BRE 转义元字符 → ERE 元字符
			} else {
				b.WriteByte('\\')
				b.WriteByte(n)
			}
			i++
			continue
		}
		if strings.ContainsRune("(){}|+?", rune(c)) {
			b.WriteByte('\\')
			b.WriteByte(c) // BRE 下是字面量：转义后交给 RE2
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// grepOne 搜索单个目标并回显命中，返回命中行数。磁盘文件先探测是否二进制：
// 二进制文件不逐行回显（二进制「行」对模型毫无意义，只会把乱码灌进上下文），
// 命中时按 GNU 口径只报一行 `Binary file <path> matches`。stdin 不做探测
// （与既有管道行为一致：管道内容按文本处理）。
func grepOne(hc interp.HandlerContext, tg grepTarget, re *regexp.Regexp, opts grepOpts, showName bool) (int, error) {
	if tg.path == "-" {
		r := io.Reader(hc.Stdin)
		if r == nil {
			// 生产 runner 不提供 stdin（interp.StdIO(nil, ...)）→ 空输入，
			// 避免 bufio.Scanner(nil) 在 Scan 内 nil 解引用。
			r = strings.NewReader("")
		}
		return grepStream(hc, r, re, opts, showName, tg.display), nil
	}
	f, err := os.Open(tg.path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	if head, _ := br.Peek(8192); bytes.IndexByte(head, 0) >= 0 {
		binOpts := opts
		binOpts.countOnly = true
		binOpts.filesOnly = false
		n := grepStream(hc, br, re, binOpts, false, tg.display)
		if n > 0 && !opts.countOnly && !opts.filesOnly {
			fmt.Fprintf(hc.Stdout, "Binary file %s matches\n", tg.display)
		}
		return n, nil
	}
	return grepStream(hc, br, re, opts, showName, tg.display), nil
}

// grepCollect 递归收集 dir 下可搜索的普通文件（对齐 grep -r）：目录项按名升序深度
// 优先——os.ReadDir 已按文件名排序，故同一目录树的命中顺序确定；工具结果会进入模型
// 上下文，有序产物不得随文件系统返回序漂移（确定性要求）。
//
// 遍历中不跟随目录与文件符号链接（防环、防挂起）；跳过 grepSkipDirs 噪声目录与
// --exclude-dir；--include/--exclude 按文件名过滤；单个目录不可读只报一行并继续。
func grepCollect(ctx context.Context, hc interp.HandlerContext, dir, display string, opts *grepOpts, out *[]grepTarget) {
	if err := ctx.Err(); err != nil {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "grep: %s: %s\n", display, slashErr(err))
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			if grepSkipDirs[name] || matchAnyGlob(opts.excludeDirs, name) {
				continue
			}
			grepCollect(ctx, hc, dsc.PJoin(dir, name), dsc.PJoin(display, name), opts, out)
			continue
		}
		if !e.Type().IsRegular() {
			continue
		}
		if !grepFileWanted(name, display, opts) {
			continue
		}
		*out = append(*out, grepTarget{display: dsc.PJoin(display, name), path: dsc.PJoin(dir, name)})
	}
}

// grepFileWanted 依 --include/--exclude 判定文件是否参与搜索。
func grepFileWanted(name, dirPath string, opts *grepOpts) bool {
	if len(opts.includes) > 0 && !matchAnyGlob(opts.includes, name) && !matchAnyGlob(opts.includes, dirPath) {
		return false
	}
	if matchAnyGlob(opts.excludes, name) || matchAnyGlob(opts.excludes, dirPath) {
		return false
	}
	return true
}

// matchAnyGlob 按 shell 通配符匹配（路径用 path.Match；含 '/' 的模式按整路径匹配）。
func matchAnyGlob(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// grepStream 把 r 按行匹配并按选项输出；返回命中行数（-o/-l 亦按命中行计）。
// 支持 -A/-B/-C 上下文行：上下文行用 '-' 分隔前缀（对齐 GNU），命中行用 ':'，
// 相邻命中区间重叠时用 '--' 分隔，避免重复输出同一行。
func grepStream(hc interp.HandlerContext, r io.Reader, re *regexp.Regexp, opts grepOpts, prefix bool, name string) int {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var ring []string // 最近 before 行（尚未输出）
	ln, count := 0, 0
	afterLeft := 0
	lastPrinted := 0
	emit := func(lineNo int, text string, isMatch bool) {
		if opts.countOnly || opts.filesOnly {
			return
		}
		// 组间分隔符仅在有上下文行时输出（GNU 口径）：无 -A/-B/-C 时命中行
		// 直接相邻输出，不插 "--"（否则下游 wc -l / 计数会被凭空多出一行）。
		if (opts.before > 0 || opts.after > 0) && lastPrinted > 0 && lineNo > lastPrinted+1 {
			fmt.Fprintln(hc.Stdout, "--")
		}
		if prefix {
			sep := ":"
			if !isMatch {
				sep = "-"
			}
			fmt.Fprintf(hc.Stdout, "%s%s", name, sep)
		}
		if opts.lineNum {
			sep := ":"
			if !isMatch {
				sep = "-"
			}
			fmt.Fprintf(hc.Stdout, "%d%s", lineNo, sep)
		}
		fmt.Fprintln(hc.Stdout, text)
		lastPrinted = lineNo
	}

	for sc.Scan() {
		ln++
		line := sc.Text()
		hit := re.MatchString(line)
		if opts.invert {
			hit = !hit
		}
		if hit {
			// -m 是「最多输出 N 条命中」，先判后计，否则 -c 会多算一条
			if opts.maxCount > 0 && count >= opts.maxCount {
				break
			}
			count++
			// 先补上之前缓存的前置上下文
			start := ln - len(ring)
			for i, prev := range ring {
				emit(start+i, prev, false)
			}
			ring = ring[:0]
			if opts.onlyMatching && !opts.countOnly && !opts.filesOnly {
				for _, m := range re.FindAllString(line, -1) {
					if prefix {
						fmt.Fprintf(hc.Stdout, "%s:", name)
					}
					if opts.lineNum {
						fmt.Fprintf(hc.Stdout, "%d:", ln)
					}
					fmt.Fprintln(hc.Stdout, m)
				}
			} else {
				emit(ln, line, true)
			}
			afterLeft = opts.after
			continue
		}
		if afterLeft > 0 {
			emit(ln, line, false)
			afterLeft--
			continue
		}
		if opts.before > 0 {
			ring = append(ring, line)
			if len(ring) > opts.before {
				ring = ring[1:]
			}
		}
	}
	if opts.filesOnly && count > 0 {
		fmt.Fprintln(hc.Stdout, name)
	}
	return count
}
