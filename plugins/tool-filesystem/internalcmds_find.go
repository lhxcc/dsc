package main

// 内建 find：进程内实现（纯 Go，跨全平台、不依赖 PATH）。
//
// 动机：`find` 是模型在 shell 里最常用的一条命令（按名/类型列举文件），但它既不在
// mvdan/sh 的 shell 内建里，也不在本插件的内部命令表中——Windows 上只会回落 PATH
// 查找而失败，而 shell 工具描述又把 find 列为「可用 POSIX 命令」示例，模型在
// Windows 上必然撞墙。此处按 GNU find 的常用子集进程内实现；未支持的谓词一律响亮
// 报错——find 被 ExecHandler 无条件拦截，不存在回落到真实 find 的路径，静默失败
// 只会让模型空转。
//
// 支持：
//   - 路径参数（可多个，缺省 "."）；条目按名升序深度优先（os.ReadDir 已排序），同一
//     目录树的输出确定——工具结果进入模型上下文，顺序不得随文件系统返回序漂移
//   - 谓词：-name/-iname/-path（fnmatch：* 可跨分隔符）、-type f|d|l|c|b|p|s（可逗号并列）
//   - 全局选项：-maxdepth N、-mindepth N
//   - 逻辑：隐式 AND、-a/-and、-o/-or、!/-not、括号（shell 中写作 \( \)）
//   - 动作：-print（默认即 print：谓词为真即输出路径）、-prune（求值命中后不再下探该目录）、
//     -exec CMD ... {} \;（每个条目执行一次）与 -exec CMD ... {} +（收集后批量执行，
//     恒为真）
//
// -exec 的子命令走与本 shell 相同的分派：内部命令表（grep/rm/wc/...）→ shell 内建
// （echo/printf/test/...）→ PATH 外部程序。否则 Windows 上 PATH 被宿主过滤时
// `-exec rm {} \;` 必然退出码 127，等于没支持。{} 在参数中按子串替换（对齐 GNU：
// `sh -c 'echo {}'` 也替换）。
//
// 不支持（报 `find: unsupported predicate: X` 并列出支持清单，退出码 2）：-execdir、
// -delete、-print0、-size、-newer、-mtime、-perm、-user、-empty、-quit、-regex 等。
//
// 与 grep -r 的差别：find 不跳过 .git/node_modules 等目录——其输出只是路径，不构成
// 上下文洪水，且模型惯用 `-path ... -prune` 自行排除，静默跳过会丢真实结果。
//
// 符号链接：路径参数按 stat 解（指向目录即下探，符合直觉）；遍历中遇到的符号链接按
// lstat 视为链接本身（`-type l` 可见）且不下探，与 GNU 默认 -P 一致，天然防环。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	dsc "dsc-sdk"
	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
)

// findSupported 错误文案里列出的受支持谓词清单，供模型自我纠正。
const findSupported = "-name -iname -path -type -maxdepth -mindepth -print -prune " +
	"-exec CMD ... {} \\; -exec CMD ... {} + -a -o ! ( )"

// findLimits -maxdepth/-mindepth 解析结果（maxdepth 为 -1 表示未限制）。
type findLimits struct {
	maxdepth int
	mindepth int
}

// findEntry 遍历中被求值的一项。
type findEntry struct {
	display string      // 模型可见路径（路径参数保留书写形态，条目为「参数/相对路径」）
	path    string      // 磁盘路径
	name    string      // 条目名（-name/-iname 的匹配对象）
	fi      os.FileInfo // 路径参数取 Stat，遍历条目取 Lstat
	pruned  bool        // 本次求值中 -prune 命中：该项若为目录则不再下探
}

// findExpr 表达式节点：'a' 与、'o' 或、'n' 非、't' 谓词。
type findExpr struct {
	kind byte
	kids []*findExpr
	test func(*findEntry) bool
}

func (x *findExpr) eval(e *findEntry) bool {
	switch x.kind {
	case 't':
		return x.test(e)
	case 'n':
		return !x.kids[0].eval(e)
	case 'a':
		return x.kids[0].eval(e) && x.kids[1].eval(e)
	case 'o':
		return x.kids[0].eval(e) || x.kids[1].eval(e)
	}
	return false
}

// findDisplayPath 把参数的路径分隔符归一为正斜杆（先连续双反斜杆、后单反斜杆），
// 但**不做 Clean**：保留模型书写形态（"." 保持 "."、"src/" 保持 "src/"），使
// `-path ./node_modules` 之类惯用模式与其书写一致（GNU find 亦按书写形态匹配/输出）。
func findDisplayPath(p string) string {
	p = strings.ReplaceAll(p, `\\`, "/")
	return strings.ReplaceAll(p, "\\", "/")
}

// findJoin 拼接展示路径（同样不做 Clean，保留 "./" 前缀等书写形态）。
func findJoin(dir, name string) string {
	switch {
	case dir == "":
		return name
	case strings.HasSuffix(dir, "/"):
		return dir + name
	default:
		return dir + "/" + name
	}
}

func cmdFind(ctx context.Context, hc interp.HandlerContext, args []string) error {
	// 路径参数：表达式之前的非选项参数（缺省 "."）
	var paths []string
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") || a == "!" || a == "(" {
			break
		}
		paths = append(paths, a)
	}
	if len(paths) == 0 {
		paths = []string{"."}
	}
	x, pe, err := parseFindExpr(ctx, hc, args[i:])
	if err != nil {
		fmt.Fprintf(hc.Stderr, "find: %s\n", err)
		return interp.NewExitStatus(2)
	}
	// 隐式 -print：表达式里出现过 -print/-exec 等动作时不再补（对齐 GNU，否则
	// `find . -name '*.go' -exec rm {} \;` 会在执行之外再吐一串路径，`{} +` 的批量
	// 用法尤其会被污染）。-prune 是谓词不是动作，单独出现仍补 -print。
	print := !pe.hasAction

	status := 0
	for _, arg := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		p := res(hc, arg)
		fi, statErr := os.Stat(p)
		if statErr != nil {
			fmt.Fprintf(hc.Stderr, "find: %s: %s\n", arg, slashErr(statErr))
			status = 1
			continue
		}
		display := findDisplayPath(arg)
		e := findEntry{display: display, path: p, name: filepath.Base(display), fi: fi}
		if err := findWalk(ctx, hc, e, 0, x, *pe.lim, print); err != nil {
			return err
		}
	}
	// `{} +` 形式：遍历结束后统一执行（GNU 亦为「攒够一批就跑」）
	for _, b := range pe.batches {
		b.flush(ctx, hc)
	}
	if status != 0 {
		return interp.NewExitStatus(uint8(status))
	}
	return nil
}

// findWalk 深度优先遍历：条目按名升序（os.ReadDir 已排序），故输出确定。
// 谓词为真且 print 为真时输出（print 为「无显式动作」时的隐式 -print，见 cmdFind）；
// -prune 命中的目录不下探。
func findWalk(ctx context.Context, hc interp.HandlerContext, e findEntry, depth int, x *findExpr, lim findLimits, print bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth >= lim.mindepth && x.eval(&e) {
		if print {
			fmt.Fprintln(hc.Stdout, e.display)
		}
	}
	if !e.fi.IsDir() || e.pruned {
		return nil
	}
	if lim.maxdepth >= 0 && depth >= lim.maxdepth {
		return nil
	}
	entries, err := os.ReadDir(e.path)
	if err != nil {
		// 单个目录不可读（权限等）只报一行并继续其余分支，不丢弃整棵树的命中
		fmt.Fprintf(hc.Stderr, "find: %s: %s\n", e.display, slashErr(err))
		return nil
	}
	for _, ent := range entries {
		child := findJoin(e.path, ent.Name())
		fi, lerr := os.Lstat(child)
		if lerr != nil {
			fmt.Fprintf(hc.Stderr, "find: %s: %s\n", findJoin(e.display, ent.Name()), slashErr(lerr))
			continue
		}
		next := findEntry{display: findJoin(e.display, ent.Name()), path: child, name: ent.Name(), fi: fi}
		if err := findWalk(ctx, hc, next, depth+1, x, lim, print); err != nil {
			return err
		}
	}
	return nil
}

// ---------- 表达式解析（GNU find 子集） ----------

// parseFindExpr 解析表达式：隐式 AND、-a/-and、-o/-or、!/-not、括号；无表达式时
// 等价于恒真（即 -print 全部）。返回解析器本身（含 -maxdepth/-mindepth 限制、
// `{} +` 的待执行批次、是否已有显式动作），供 cmdFind 收尾。
func parseFindExpr(ctx context.Context, hc interp.HandlerContext, args []string) (*findExpr, *findParser, error) {
	lim := findLimits{maxdepth: -1}
	p := &findParser{args: args, lim: &lim, ctx: ctx, hc: hc}
	if len(args) == 0 {
		return &findExpr{kind: 't', test: func(*findEntry) bool { return true }}, p, nil
	}
	x, err := p.parseOr()
	if err != nil {
		return nil, p, err
	}
	if tok := p.peek(); tok != "" {
		return nil, p, fmt.Errorf("unexpected token: %s", tok)
	}
	return x, p, nil
}

type findParser struct {
	args []string
	pos  int
	lim  *findLimits
	ctx  context.Context
	hc   interp.HandlerContext

	// hasAction 表达式里出现过显式动作（-print/-exec）：为真则不再补隐式 -print。
	hasAction bool
	// batches `{} +` 形式的 -exec 动作，遍历结束后统一 flush。
	batches []*findExecAction
}

func (p *findParser) peek() string {
	if p.pos < len(p.args) {
		return p.args[p.pos]
	}
	return ""
}

func (p *findParser) next() string {
	s := p.peek()
	if s != "" {
		p.pos++
	}
	return s
}

func (p *findParser) accept(tok string) bool {
	if p.peek() == tok {
		p.pos++
		return true
	}
	return false
}

// startsPrimary 判断当前位置是否开启新的 primary（隐式 AND 的边界判定）。
func (p *findParser) startsPrimary() bool {
	switch tok := p.peek(); tok {
	case "", ")", "-o", "-or", "-a", "-and":
		return false
	default:
		return tok == "!" || tok == "-not" || tok == "(" || strings.HasPrefix(tok, "-")
	}
}

// value 取谓词参数（缺失即报错）。
func (p *findParser) value(pred string) (string, error) {
	v := p.next()
	if v == "" {
		return "", fmt.Errorf("%s: missing argument", pred)
	}
	return v, nil
}

func (p *findParser) parseOr() (*findExpr, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.accept("-o") || p.accept("-or") {
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &findExpr{kind: 'o', kids: []*findExpr{left, right}}
	}
	return left, nil
}

func (p *findParser) parseAnd() (*findExpr, error) {
	left, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	for {
		if p.accept("-a") || p.accept("-and") || p.startsPrimary() {
			right, err := p.parseNot()
			if err != nil {
				return nil, err
			}
			left = &findExpr{kind: 'a', kids: []*findExpr{left, right}}
			continue
		}
		return left, nil
	}
}

func (p *findParser) parseNot() (*findExpr, error) {
	if p.accept("!") || p.accept("-not") {
		kid, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		return &findExpr{kind: 'n', kids: []*findExpr{kid}}, nil
	}
	return p.parsePrimary()
}

func (p *findParser) parsePrimary() (*findExpr, error) {
	if p.accept("(") {
		x, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if !p.accept(")") {
			return nil, fmt.Errorf("missing ')'")
		}
		return x, nil
	}
	pred := p.next()
	if pred == "" {
		return nil, fmt.Errorf("missing expression after operator")
	}
	test, err := p.testFor(pred)
	if err != nil {
		return nil, err
	}
	return &findExpr{kind: 't', test: test}, nil
}

// testFor 把单个谓词编译成求值函数；未支持的谓词在此响亮报错。
func (p *findParser) testFor(pred string) (func(*findEntry) bool, error) {
	switch pred {
	case "-name", "-iname":
		pat, err := p.value(pred)
		if err != nil {
			return nil, err
		}
		fold := pred == "-iname"
		return func(e *findEntry) bool { return globMatch(pat, e.name, fold) }, nil
	case "-path":
		pat, err := p.value(pred)
		if err != nil {
			return nil, err
		}
		return func(e *findEntry) bool { return globMatch(pat, e.display, false) }, nil
	case "-type":
		typ, err := p.value(pred)
		if err != nil {
			return nil, err
		}
		for _, t := range strings.Split(typ, ",") {
			if len(t) != 1 || !strings.Contains("fdlcbps", t) {
				return nil, fmt.Errorf("invalid -type argument: %s", typ)
			}
		}
		return func(e *findEntry) bool { return strings.Contains(typ, findTypeChar(e.fi)) }, nil
	case "-maxdepth", "-mindepth":
		v, err := p.value(pred)
		if err != nil {
			return nil, err
		}
		n, convErr := strconv.Atoi(v)
		if convErr != nil || n < 0 {
			return nil, fmt.Errorf("invalid %s argument: %s", pred, v)
		}
		if pred == "-maxdepth" {
			p.lim.maxdepth = n
		} else {
			p.lim.mindepth = n
		}
		return func(*findEntry) bool { return true }, nil
	case "-print":
		// 显式 -print 必须自己输出：一旦表达式里有 -exec 等动作，cmdFind 就不再补
		// 隐式 -print，`-exec ... \; -print` 得靠这里出路径。
		p.hasAction = true
		return func(e *findEntry) bool {
			fmt.Fprintln(p.hc.Stdout, e.display)
			return true
		}, nil
	case "-exec":
		return p.compileExec()
	case "-prune":
		return func(e *findEntry) bool {
			e.pruned = true
			return true
		}, nil
	default:
		return nil, fmt.Errorf("unsupported predicate: %s (supported here: %s)", pred, findSupported)
	}
}

// ---------- -exec 动作 ----------

// findExecAction 一次 -exec 动作。batch 为 `{} +` 形式：不逐条执行，先累积路径，
// 遍历结束（或攒够一批）后再一次性传给子命令——模型最常用的 `-exec grep -l pat {} +`
// 靠它把开销从「每文件一进程」降到「一批一进程」。
type findExecAction struct {
	argv  []string // 模板参数（其中 {} 待替换）
	batch bool
	files []string // batch 模式下累积的路径（书写形态，与 -print 的输出一致）
}

// compileExec 解析 `-exec CMD ... {} \;` / `-exec CMD ... {} +`（shell 已把 `;` 作为
// 独立参数传入）。`+` 要求 {} 是最后一个参数（GNU 同样如此，否则无法确定插入位置）。
func (p *findParser) compileExec() (func(*findEntry) bool, error) {
	var argv []string
	term := ""
	for {
		tok := p.next()
		if tok == "" {
			return nil, fmt.Errorf("-exec: missing ';' or '+' terminator")
		}
		if tok == ";" || tok == "+" {
			term = tok
			break
		}
		argv = append(argv, tok)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("-exec: missing command")
	}
	a := &findExecAction{argv: argv, batch: term == "+"}
	if a.batch {
		if last := argv[len(argv)-1]; last != "{}" {
			return nil, fmt.Errorf("-exec ... + : '{}' must be the last argument")
		}
		p.batches = append(p.batches, a)
	}
	p.hasAction = true
	if a.batch {
		return func(e *findEntry) bool {
			a.files = append(a.files, e.display)
			return true // `{} +` 恒为真（对齐 GNU：结果取决于批量命令，而非单条）
		}, nil
	}
	return func(e *findEntry) bool {
		return a.runNow(p.ctx, p.hc, e.display)
	}, nil
}

// runNow 以单个路径执行子命令，退出码 0 即谓词为真。
func (a *findExecAction) runNow(ctx context.Context, hc interp.HandlerContext, path string) bool {
	argv := make([]string, len(a.argv))
	for i, s := range a.argv {
		argv[i] = strings.ReplaceAll(s, "{}", path)
	}
	return findRunCommand(ctx, hc, argv) == 0
}

// findExecBatch 单次 `{} +` 调用携带的路径上限。批量的本意是少起进程，但命令行长度
// 有硬上限（Windows CreateProcess 约 32K 字符），长路径工作区下必须截断分批。
const findExecBatch = 100

// flush 执行 `{} +` 累积的全部路径；无命中时一次也不执行（对齐 GNU）。
func (a *findExecAction) flush(ctx context.Context, hc interp.HandlerContext) {
	for start := 0; start < len(a.files); start += findExecBatch {
		end := start + findExecBatch
		if end > len(a.files) {
			end = len(a.files)
		}
		argv := make([]string, 0, len(a.argv)-1+end-start)
		argv = append(argv, a.argv[:len(a.argv)-1]...) // 末尾的 {} 由本批路径取代
		argv = append(argv, a.files[start:end]...)
		findRunCommand(ctx, hc, argv)
	}
	a.files = nil
}

// findExecBuiltins -exec 里允许调用的 shell 内建：只列「纯输出 / 纯判据、不触碰 shell
// 状态」的几个。exit/return/cd/exec/set/read/export 等一律不放行——GNU find 的 -exec
// 起的是外部进程，天然改不到父 shell；而这里若直接调内建，`-exec exit \;` 会掐掉整个
// 会话、`-exec cd` 会把 find 的遍历上下文搬走。未列出者按外部程序处理（通常报找不到，
// 模型据此改用可用的写法）。
var findExecBuiltins = map[string]bool{
	"echo":   true,
	"printf": true,
	"test":   true,
	"[":      true,
	"true":   true,
	"false":  true,
	"pwd":    true,
}

// findRunCommand 执行 -exec 的子命令，返回退出码。
//
// 分派次序与 shell 自身一致：内部命令表 → 安全内建 → PATH 外部程序。
//
// 前两级都必须走进程内：Windows 上插件子进程的 PATH 被宿主过滤，`-exec rm {} \;`
// 若按外部程序解析必然 127；而 grep/rm 与 echo/printf/test 恰恰是模型在 -exec 里
// 最常调的命令。Stdio/Dir 沿用当前 shell，使内建命令的相对路径与重定向语义一致。
func findRunCommand(ctx context.Context, hc interp.HandlerContext, argv []string) int {
	if len(argv) == 0 {
		fmt.Fprintln(hc.Stderr, "find: -exec: missing command")
		return 1
	}
	if fn, ok := internalCommands[argv[0]]; ok {
		// HandlerContext 是值类型，复制后仅替换 stdio 即可复用同一套内建实现
		// （内建命令只读 Dir/Env/Stdin/Stdout/Stderr）。
		return findExitCode(hc, argv[0], fn(ctx, hc, argv[1:]))
	}
	if findExecBuiltins[argv[0]] && interp.IsBuiltin(argv[0]) {
		return findExitCode(hc, argv[0], hc.Builtin(ctx, argv))
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	// 与其余派生点一致：Windows 上以 CREATE_NO_WINDOW 启动，避免 TUI 中弹出控制台
	// 窗口（core/proc_attr_regression_test.go 会扫描所有 exec 派生点，漏接即失败）。
	dsc.ConfigureChildProcessAttrs(cmd)
	cmd.Dir = hc.Dir
	if env := findEnvList(hc.Env); len(env) > 0 {
		cmd.Env = env
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = hc.Stdin, hc.Stdout, hc.Stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(hc.Stderr, "find: %s: %s\n", argv[0], slashErr(err))
		return 1
	}
	return 0
}

// findExitCode 把内建/内部命令的执行结果折算成退出码：退出状态原样透出，其余错误
// 报一行并记 1（-exec 只看退出码判定谓词真假）。
func findExitCode(hc interp.HandlerContext, name string, err error) int {
	if err == nil {
		return 0
	}
	if code, isExit := interp.IsExitStatus(err); isExit {
		return int(code)
	}
	fmt.Fprintf(hc.Stderr, "find: %s: %s\n", name, slashErr(err))
	return 1
}

// findEnvList 把解释器环境里已导出的变量转成 exec.Cmd 需要的 KEY=VALUE 列表。
func findEnvList(env expand.Environ) []string {
	if env == nil {
		return nil
	}
	var out []string
	env.Each(func(name string, vr expand.Variable) bool {
		if vr.IsSet() && vr.Exported {
			out = append(out, name+"="+vr.String())
		}
		return true
	})
	return out
}

// findTypeChar 返回 -type 判据的条目类型字符（对齐 GNU find）：f 普通文件、d 目录、
// l 符号链接、c 字符设备、b 块设备、p 命名管道、s socket；未知回 "?"。
func findTypeChar(fi os.FileInfo) string {
	m := fi.Mode()
	switch {
	case m.IsRegular():
		return "f"
	case m.IsDir():
		return "d"
	case m&os.ModeSymlink != 0:
		return "l"
	case m&os.ModeCharDevice != 0:
		return "c"
	case m&os.ModeDevice != 0:
		return "b"
	case m&os.ModeNamedPipe != 0:
		return "p"
	case m&os.ModeSocket != 0:
		return "s"
	}
	return "?"
}

// globMatch 按 fnmatch 语义匹配：* 任意串（**可跨路径分隔符**）、? 单字符、[...] 字符集
// （[!...]/[^...] 取反，支持 a-z 区间与 \ 转义）。GNU find 的 -name/-path 默认即此语义
// ——-path 的 * 能跨 / 是 `-path '*/src/*'` 之类惯用式的基础——与 path.Match /
// filepath.Match 的「* 不跨分隔符」不同，故不复用二者。
func globMatch(pattern, s string, foldCase bool) bool {
	if foldCase {
		pattern = strings.ToLower(pattern)
		s = strings.ToLower(s)
	}
	return globMatchAt(pattern, s)
}

func globMatchAt(pattern, s string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			for len(pattern) > 0 && pattern[0] == '*' {
				pattern = pattern[1:]
			}
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if globMatchAt(pattern, s[i:]) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		case '[':
			hit, rest, ok := matchClass(pattern, s)
			if !ok {
				// 非法字符集（未闭合/空集）：按字面 '[' 处理
				if s == "" || s[0] != '[' {
					return false
				}
				pattern, s = pattern[1:], s[1:]
				continue
			}
			if !hit {
				return false
			}
			pattern, s = rest, s[1:]
		case '\\':
			if len(pattern) > 1 {
				pattern = pattern[1:]
			}
			if s == "" || s[0] != pattern[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		default:
			if s == "" || s[0] != pattern[0] {
				return false
			}
			pattern, s = pattern[1:], s[1:]
		}
	}
	return s == ""
}

// matchClass 匹配 s 的首字符对 pattern 起始的 [...] 字符集：返回是否命中、剩余 pattern、
// 是否构成合法字符集（未闭合或空集视为非法，交调用方按字面 '[' 处理）。
func matchClass(pattern, s string) (hit bool, rest string, ok bool) {
	if s == "" {
		return false, pattern, false
	}
	i := 1
	neg := false
	if i < len(pattern) && (pattern[i] == '!' || pattern[i] == '^') {
		neg = true
		i++
	}
	matched := false
	first := true
	for i < len(pattern) {
		if pattern[i] == ']' && !first {
			break
		}
		first = false
		lo := pattern[i]
		if lo == '\\' && i+1 < len(pattern) {
			i++
			lo = pattern[i]
		}
		i++
		hi := lo
		if i+1 < len(pattern) && pattern[i] == '-' && pattern[i+1] != ']' {
			i++
			hi = pattern[i]
			if hi == '\\' && i+1 < len(pattern) {
				i++
				hi = pattern[i]
			}
			i++
		}
		if lo <= s[0] && s[0] <= hi {
			matched = true
		}
	}
	if i >= len(pattern) || i == 1 || (neg && i == 2) {
		return false, pattern, false
	}
	return matched != neg, pattern[i+1:], true
}
