package main

// 内建 sort / uniq：进程内实现（纯 Go，跨全平台、不依赖 PATH）。
//
// 动机：二者是模型做文本统计的标配（`sort | uniq -c | sort -rn` 统计出现次数排前几），
// 但 Windows 上 `sort` 会命中 C:\Windows\System32\sort.exe——那是**非 POSIX** 程序：只认
// `/R`、`/+n` 开关，`sort -n`/`-r`/`-u`/`-k`/`-t` 一律被当成文件名而报错（实测报
// 「系统找不到指定的文件」），排序口径也随系统 locale；`uniq` 则根本没有同名程序
// （exit 127）。故按 POSIX 语义进程内实现，使同一条管道在 Windows 上也能跑通。
//
// 语义口径（细节均以 GNU coreutils 实测核对过）：
//   - 默认按 **C locale 字节序**（不随系统 locale 漂移，输出确定）；
//   - 键相等时以**整行字节比较兜底**（GNU 的 last-resort 比较），保证「同一输入 → 同一
//     输出」——结果会进入模型上下文，顺序必须确定；
//   - `-u` 的去重判据是**键相等**（故 `sort -n -u` 下 1 与 01 视为相等；实测 GNU 亦然）；
//   - `-k N` 省略末字段时键**至行尾**；`-k N,M` 取第 N..M 字段（1 基）；
//   - `-n` 取数值前缀（忽略前导空白、可选正负号、十进制整数/小数；无数字前缀按 0）；
//   - 行切分对齐 bufio.ScanLines（以 \n 分行、剥行尾 \r、末行无换行也算一行），但**不受
//     Scanner 默认 64 KiB 行长上限**约束（排序对象可能是 minified JS 之类超长单行）。
//
// 已声明的简化（不模拟 POSIX 的对应细节，避免误以为等价）：
//   - 未给 `-t` 时以**连续空白**切分字段（等价于把「一段空白」当分隔符），故字段不含
//     前导/尾随空白；POSIX「第 2 个字段起含其前导空白」的细节不模拟；
//   - `-k` 的字段内偏移（`-k2.3`）不支持，遇到即报错；
//   - `-b`（忽略前导空白）接受但为空操作——本实现的字段切分本就不含前导空白。
//
// 未支持选项一律响亮报错并列出支持清单：sort/uniq 被 interp.ExecHandler 无条件拦截，
// 不存在回落到外部程序的可能，静默失败只会让模型空转。

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/interp"
)

// sortSupported / uniqSupported 错误文案里列出的受支持选项清单，供模型自我纠正。
const (
	sortSupported = "-n -r -u -f -b -t SEP -k N[,N][n|r|f]"
	uniqSupported = "-c -d -u -i"
)

// sortSpec 排序规格（全局选项 + 若干键定义）。
type sortSpec struct {
	numeric bool
	reverse bool
	unique  bool
	fold    bool
	sep     string
	hasSep  bool
	keys    []sortKey
}

// sortKey 单个键定义：第 start..end 字段（1 基）；end=0 表示至行尾。
// 修饰为「开语义」：键级 n/r/f 打开即生效，未给时沿用全局档。
type sortKey struct {
	start, end       int
	numeric, reverse bool
	fold             bool
}

// parseSortOption 解析一个选项参数：支持组合短选项（`-rn`），`-t`/`-k` 的取值既可紧随
// （`-t:`、`-k2`）也可为下一参数（`-t :`、`-k 2`）。返回本次消费的参数个数。
func (s *sortSpec) parseSortOption(arg string, args []string, i int) (int, error) {
	chars := arg[1:]
	consumed := 1
	for j := 0; j < len(chars); j++ {
		switch c := chars[j]; c {
		case 'n':
			s.numeric = true
		case 'r':
			s.reverse = true
		case 'u':
			s.unique = true
		case 'f':
			s.fold = true
		case 'b':
			// 忽略前导空白：本实现的字段切分本就不含前导空白，等价空操作
		case 't', 'k':
			val := chars[j+1:]
			if val == "" {
				if i+consumed >= len(args) {
					return 0, fmt.Errorf("option -%c requires an argument", c)
				}
				val = args[i+consumed]
				consumed++
			}
			if c == 't' {
				if val == "" {
					return 0, fmt.Errorf("option -t requires a non-empty separator")
				}
				s.sep, s.hasSep = val, true
			} else {
				key, err := parseSortKey(val)
				if err != nil {
					return 0, err
				}
				s.keys = append(s.keys, key)
			}
			j = len(chars) // 该参数剩余字符已作为取值消费
		default:
			return 0, fmt.Errorf("unsupported option: -%c (supported here: %s)", c, sortSupported)
		}
	}
	return consumed, nil
}

// parseSortKey 解析 -k 取值：N[,M][n|r|f]，如 2、2,2、2,2n、3r。字段内偏移（.C）不支持。
func parseSortKey(val string) (sortKey, error) {
	var k sortKey
	parseField := func(s string, isStart bool) (int, error) {
		digits := 0
		for digits < len(s) && s[digits] >= '0' && s[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return 0, fmt.Errorf("invalid -k key: %s", val)
		}
		n, err := strconv.Atoi(s[:digits])
		if err != nil || n < 1 {
			return 0, fmt.Errorf("invalid -k key: %s (fields are 1-based)", val)
		}
		for _, c := range s[digits:] {
			switch c {
			case 'n':
				k.numeric = true
			case 'r':
				k.reverse = true
			case 'f':
				k.fold = true
			case '.':
				return 0, fmt.Errorf("unsupported -k key (field offset .C): %s", val)
			default:
				return 0, fmt.Errorf("unsupported -k modifier: %c (supported here: %s)", c, sortSupported)
			}
		}
		_ = isStart
		return n, nil
	}
	parts := strings.SplitN(val, ",", 2)
	start, err := parseField(parts[0], true)
	if err != nil {
		return k, err
	}
	k.start = start
	if len(parts) == 2 {
		end, err := parseField(parts[1], false)
		if err != nil {
			return k, err
		}
		k.end = end
	}
	return k, nil
}

// sortFields 按分隔符切分一行：给了 -t 按其切（相邻分隔符产生空字段）；未给则以连续空白
// 切分（字段不含前导/尾随空白，见文件头「已声明的简化」）。
func sortFields(line, sep string, hasSep bool) []string {
	if hasSep {
		return strings.Split(line, sep)
	}
	return strings.Fields(line)
}

// keyText 抽取键文本：第 start..end 字段以分隔符连接；起始字段超出该行字段数时键为 ""
// （故字段少的行排在有效键之前）；end 为 0 或超出字段数时取至末字段。
func (s *sortSpec) keyText(line string, k sortKey) string {
	fs := sortFields(line, s.sep, s.hasSep)
	if k.start > len(fs) {
		return ""
	}
	end := k.end
	if end == 0 || end > len(fs) {
		end = len(fs)
	}
	if end < k.start {
		end = k.start
	}
	sep := s.sep
	if !s.hasSep {
		sep = " "
	}
	return strings.Join(fs[k.start-1:end], sep)
}

// cmp 比较两行：先按各键（无键则整行），键全等时以整行字节比较兜底（GNU 的 last-resort
// 比较）——键相等而整行不同时仍有确定顺序。
func (s *sortSpec) cmp(a, b string) int {
	if len(s.keys) == 0 {
		return compareText(a, b, s.numeric, s.fold, s.reverse)
	}
	for _, k := range s.keys {
		ka, kb := s.keyText(a, k), s.keyText(b, k)
		if c := compareText(ka, kb, k.numeric || s.numeric, k.fold || s.fold, k.reverse || s.reverse); c != 0 {
			return c
		}
	}
	return compareText(a, b, false, false, s.reverse)
}

// equalKeys 是 -u 的去重判据：各键（无键则整行）比较相等即视为同一条，不看兜底比较
// （对齐 GNU：-u 按键相等去重，故 -n 下 1 与 01 视为相等）。
func (s *sortSpec) equalKeys(a, b string) bool {
	if len(s.keys) == 0 {
		return compareText(a, b, s.numeric, s.fold, false) == 0
	}
	for _, k := range s.keys {
		ka, kb := s.keyText(a, k), s.keyText(b, k)
		if compareText(ka, kb, k.numeric || s.numeric, k.fold || s.fold, false) != 0 {
			return false
		}
	}
	return true
}

// compareText 单段文本比较：numeric 时按数值前缀，否则按字节序（C locale；fold 时忽略
// 大小写），reverse 时取反。
func compareText(a, b string, numeric, fold, reverse bool) int {
	var c int
	if numeric {
		c = compareNum(a, b)
	} else {
		if fold {
			a, b = strings.ToLower(a), strings.ToLower(b)
		}
		c = strings.Compare(a, b)
	}
	if reverse {
		c = -c
	}
	return c
}

// compareNum 按 -n 口径比较两行的数值前缀。
func compareNum(a, b string) int {
	av, bv := numPrefix(a), numPrefix(b)
	switch {
	case av < bv:
		return -1
	case av > bv:
		return 1
	}
	return 0
}

// numPrefix 取数值前缀（POSIX sort -n 口径）：忽略前导空白、可选正负号、十进制整数与
// 小数部分；无数字前缀按 0；超出 float64 范围取 ±Inf（保持排序语义，不因溢出报错）。
func numPrefix(s string) float64 {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start := i
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits, end := 0, i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits++
	}
	end = i
	if i < len(s) && s[i] == '.' {
		j, frac := i+1, 0
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
			frac++
		}
		if digits > 0 || frac > 0 { // ".5" 亦为有效数值前缀；"1." 不算小数点
			digits += frac
			end = j
		}
	}
	if digits == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(s[start:end], 64)
	if err != nil {
		if strings.HasPrefix(s[start:end], "-") {
			return math.Inf(-1)
		}
		return math.Inf(1)
	}
	return v
}

// readLines 读取全部行：以 \n 分行、剥行尾 \r、末行无换行也算一行（对齐
// bufio.ScanLines），但不受 Scanner 默认 64 KiB 行长上限约束。
func readLines(r io.Reader) ([]string, error) {
	br := bufio.NewReader(r)
	var lines []string
	for {
		s, err := br.ReadString('\n')
		if len(s) > 0 {
			s = strings.TrimSuffix(strings.TrimSuffix(s, "\n"), "\r")
			lines = append(lines, s)
		}
		if err != nil {
			if err == io.EOF {
				return lines, nil
			}
			return lines, err
		}
	}
}

func cmdSort(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var spec sortSpec
	var files []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			files = append(files, args[i+1:]...)
			i = len(args)
		case a == "-":
			files = append(files, a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			consumed, err := spec.parseSortOption(a, args, i)
			if err != nil {
				fmt.Fprintf(hc.Stderr, "sort: %s\n", err)
				return interp.NewExitStatus(2)
			}
			i += consumed - 1
		default:
			files = append(files, a)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	lines, status := readSortInput(hc, files)
	sort.SliceStable(lines, func(i, j int) bool { return spec.cmp(lines[i], lines[j]) < 0 })
	for i, ln := range lines {
		if spec.unique && i > 0 && spec.equalKeys(lines[i-1], ln) {
			continue
		}
		fmt.Fprintln(hc.Stdout, ln)
	}
	if status != 0 {
		return interp.NewExitStatus(uint8(status))
	}
	return nil
}

// readSortInput 读取全部输入行：多文件按给出顺序拼接（对齐 POSIX sort）；`-` 或未给文件时
// 读 stdin。单个文件失败只报一行并继续其余文件（与 grep/head/tail 的多文件处理一致），
// 返回退出码 2 由调用方在输出后回填。
func readSortInput(hc interp.HandlerContext, files []string) ([]string, int) {
	if len(files) == 0 {
		files = []string{"-"}
	}
	var lines []string
	status := 0
	for _, f := range files {
		r, closeFn, err := openReader(hc, f)
		if err != nil {
			fmt.Fprintf(hc.Stderr, "sort: %s: %s\n", f, slashErr(err))
			status = 2
			continue
		}
		part, readErr := readLines(r)
		closeFn()
		lines = append(lines, part...)
		if readErr != nil {
			fmt.Fprintf(hc.Stderr, "sort: %s: %s\n", f, slashErr(readErr))
			status = 2
		}
	}
	return lines, status
}

// cmdUniq 过滤**相邻**重复行（POSIX uniq 语义，非全局去重；全局去重先 sort）：
// -c 计数前缀（GNU 同为右对齐 7 列 + 一个空格）、-d 仅输出重复行、-u 仅输出唯一次出现的
// 行、-i 忽略大小写比较。
func cmdUniq(ctx context.Context, hc interp.HandlerContext, args []string) error {
	var count, dupOnly, uniqOnly, ignoreCase bool
	var files []string
	for _, a := range args {
		switch {
		case a == "--":
		case a == "-":
			files = append(files, a)
		case strings.HasPrefix(a, "-") && len(a) > 1:
			for _, c := range a[1:] {
				switch c {
				case 'c':
					count = true
				case 'd':
					dupOnly = true
				case 'u':
					uniqOnly = true
				case 'i':
					ignoreCase = true
				default:
					fmt.Fprintf(hc.Stderr, "uniq: unsupported option: -%c (supported here: %s)\n", c, uniqSupported)
					return interp.NewExitStatus(2)
				}
			}
		default:
			files = append(files, a)
		}
	}
	if len(files) > 1 {
		// POSIX 的第二个操作数是输出文件；本实现只吃一个输入，输出走 stdout 重定向
		fmt.Fprintf(hc.Stderr, "uniq: extra operand: %s (only one input file is supported; redirect stdout for output)\n", files[1])
		return interp.NewExitStatus(2)
	}
	file := "-"
	if len(files) == 1 {
		file = files[0]
	}
	r, closeFn, err := openReader(hc, file)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "uniq: %s: %s\n", file, slashErr(err))
		return interp.NewExitStatus(2)
	}
	lines, readErr := readLines(r)
	closeFn()
	if readErr != nil {
		fmt.Fprintf(hc.Stderr, "uniq: %s: %s\n", file, slashErr(readErr))
		return interp.NewExitStatus(2)
	}
	for i := 0; i < len(lines); {
		if err := ctx.Err(); err != nil {
			return err
		}
		j := i + 1
		for j < len(lines) && uniqSame(lines[i], lines[j], ignoreCase) {
			j++
		}
		n := j - i
		if (!dupOnly || n > 1) && (!uniqOnly || n == 1) {
			if count {
				fmt.Fprintf(hc.Stdout, "%7d %s\n", n, lines[i])
			} else {
				fmt.Fprintln(hc.Stdout, lines[i])
			}
		}
		i = j
	}
	return nil
}

// uniqSame 相邻两行是否为同一条（-i 时忽略大小写）。
func uniqSame(a, b string, ignoreCase bool) bool {
	if ignoreCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}
