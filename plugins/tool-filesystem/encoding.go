package main

// shell 输出的 UTF-8 净化：gRPC/proto 对 string 字段强制合法 UTF-8，原样透传
// 非法字节会使整个工具结果 marshal 失败（模型只收到 "grpc: error while
// marshaling: string field contains invalid UTF-8"，输出全部丢失）。真实案例：
// 中文 Windows 上 `tree` 命中 C:\Windows\tree.com，其输出为系统 OEM 码页（GBK）
// 编码，原样进入工具结果即触发 marshal 失败。
//
// 码页不是写死的常量：解码时传 CP_OEMCP / CP_ACP 给 Windows，由系统换成本机实际
// 码页（zh-CN 936、zh-TW 950/Big5、ja-JP 932……），可用 DSC_OUTPUT_CODEPAGE 钉死。
// 注意双字节码页交叉解码会「成功但错字」（GBK 的「合并」按 Big5 解出「磁甜」），
// 单凭字节无法判断码页——所以只做严格解码 + 显式覆盖，绝不按内容猜。
//
// 净化分两层：
//  1. Windows 上先尝试按系统 OEM/ANSI 码页严格解码（MB_ERR_INVALID_CHARS，
//     解不出合法文本即放弃）——中文 Windows 的控制台工具输出因此可还原为正确
//     中文而非乱码；
//  2. 兜底 strings.ToValidUTF8——任何平台、任何来源的非法字节最多退化为
//     U+FFFD 占位符，绝不再让整个工具结果被 gRPC 拒发。

import (
	"runtime"
	"strings"
	"unicode/utf8"
)

// ensureUTF8 把可能含非法 UTF-8 的命令输出净化为合法 UTF-8 字符串：
// 已合法则原样返回（零开销快速路径）；Windows 上先按系统码页尽力还原；
// 仍失败才以 U+FFFD 逐字节替换。
//
// 「尽力还原」分两级：先整段解一次（单一编码——绝大多数原生命令，一次成功），
// 失败再**逐行**重试。整段失败几乎只发生在混编码时：同一条命令里既有外部程序按
// 系统码页（GBK）输出的行，又有本解释器/内建命令按 UTF-8 输出的行（stdout 与
// stderr 还会被拼在一起）。严格解码要求整段合法，一行不合就全段放弃 —— 中文于是
// 整片退化成 U+FFFD（现场：`python3 - <<PY` 打印 GBK 中文 + 我们回写的 UTF-8 行，
// 结果 `合并: v7 线程` 显示成 `�ϲ�: v7 �߳�`）。
func ensureUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	if runtime.GOOS == "windows" {
		if dec, ok := decodeSystemCodePage(s); ok && utf8.ValidString(dec) {
			return dec
		}
		if dec, ok := decodeMixedLines(s); ok {
			return dec
		}
	}
	return strings.ToValidUTF8(s, string(utf8.RuneError))
}

// decodeCandidate 一种候选解码方式：码页号走平台原生解码（仅 Windows），字符集名
// 走 mahonia（全平台）。两者可并存——Windows 上钉死 950 时，原生解码是第一选择、
// mahonia 的 big5 是后端备份。
type decodeCandidate struct {
	cp      uint32 // 非 0：交给解码器按码页处理（0 = 系统 ANSI，1 = 系统 OEM）
	charset string // 非空：mahonia 字符集名
}

// decodeOne 用单个候选严格解码；解不出合法文本返回 false。
func decodeOne(c decodeCandidate, s string) (string, bool) {
	if c.cp != 0 {
		if dec, ok := decodeWindowCodePage(c.cp, s); ok {
			return dec, true
		}
	}
	if c.charset != "" {
		return decodeByCharset(c.charset, s)
	}
	return "", false
}

// decodeSystemCodePage 按候选（系统码页 / 显式覆盖）逐个严格解码。
func decodeSystemCodePage(s string) (string, bool) {
	for _, c := range decodeCandidates() {
		if dec, ok := decodeOne(c, s); ok {
			return dec, true
		}
	}
	return "", false
}

// decodeMixedLines 逐行解码混合编码输出：合法 UTF-8 行原样保留，非法行按系统码页
// 严格解码（解不出则局部 U+FFFD）。返回 changed=false 表示整段没有可取之处，
// 由调用方统一兜底。
//
// 按行切的理由：命令行程序的输出以行为单位产生，一行通常来自同一个写入方，
// 因此「同一行内部混编码」比「同一段里混编码」罕见得多。
func decodeMixedLines(s string) (string, bool) {
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	rest := s
	for len(rest) > 0 {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i+1], rest[i+1:]
		} else {
			rest = ""
		}
		if utf8.ValidString(line) {
			b.WriteString(line)
			continue
		}
		changed = true
		if dec, ok := decodeSystemCodePage(line); ok && utf8.ValidString(dec) {
			b.WriteString(dec)
			continue
		}
		b.WriteString(strings.ToValidUTF8(line, string(utf8.RuneError)))
	}
	return b.String(), changed
}
