//go:build windows

package main

// Windows：按系统 OEM/ANSI 码页把非 UTF-8 字节流严格解码为 UTF-8 文本。
// 中文 Windows 的控制台工具（tree.com、find.exe 等原生程序）以系统 OEM 码页
// （zh-CN 为 GBK/936）输出；先 OEM 后 ANSI，均以 MB_ERR_INVALID_CHARS 严格
// 校验——解不出合法文本即返回 false，交由调用方走 U+FFFD 兜底，绝不猜码页
// 猜出假文本。

import (
	"os"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

const (
	codePageACP   = 0 // 系统 ANSI 码页
	codePageOEMCP = 1 // 系统 OEM 码页（控制台工具的实际输出码页）

	mbErrInvalidChars = 0x00000008 // 遇非法字节序列即报错（严格模式）
)

// systemOEMCodePage 返回系统 OEM 码页（控制台工具的实际输出码页），供测试按
// 实际系统能力断言（如 zh-CN 的 936/GBK）。经 kernel32.GetOEMCP 惰性调用。
func systemOEMCodePage() uint16 {
	r, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetOEMCP").Call()
	return uint16(r)
}

// decodeSystemCodePage 按候选码页逐个严格解码 s；任一码页成功即返回。
func decodeSystemCodePage(s string) (string, bool) {
	for _, cp := range codePageCandidates() {
		if dec, ok := decodeWithCodePage(cp, s); ok {
			return dec, true
		}
	}
	return "", false
}

// decodeWithCodePage 以**指定**码页严格解码（0 = 系统 ANSI，1 = 系统 OEM，
// 其它为显式码页如 950/936/932）。
func decodeWithCodePage(cp uint32, s string) (string, bool) {
	if len(s) == 0 {
		return "", false
	}
	u16, ok := multibyteToUTF16(cp, []byte(s))
	if !ok {
		return "", false
	}
	return string(utf16.Decode(u16)), true
}

// codePageCandidates 解码候选码页。
//
// **码页在运行时取自系统，不写死**：CP_OEMCP / CP_ACP 由 Windows 换成本机的实际值
// （zh-CN 936/GBK、zh-TW 950/Big5、ja-JP 932、en-US 437 与 1252……），所以繁体系统
// 上走的自然就是 Big5，不需要我们判断「这是不是简体」。
//
// 先 OEM 后 ANSI 的理由：管道里直连的原生控制台程序（dir/tree/findstr）按控制台码页
// （OEM）输出，CRT 程序多按 ANSI；两者在东亚系统上通常相同，在单字节系统上才有差别。
//
// 需要确定性时用 DSC_OUTPUT_CODEPAGE 显式钉死（如 950 或 65001），此时不再试探——
// 「这台机器上就是这个码页」比我们猜更可信。
func codePageCandidates() []uint32 {
	if v := strings.TrimSpace(os.Getenv("DSC_OUTPUT_CODEPAGE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 65535 {
			return []uint32{uint32(n)}
		}
	}
	return []uint32{codePageOEMCP, codePageACP}
}

// multibyteToUTF16 调 MultiByteToWideChar（严格模式）把多字节缓冲转为 UTF-16。
// 第一次调用传 nil 目标取所需长度，再分配后取实际结果（Win32 惯例两段式）。
func multibyteToUTF16(codePage uint32, b []byte) ([]uint16, bool) {
	pin := &b[0]
	n, err := windows.MultiByteToWideChar(codePage, mbErrInvalidChars, pin, int32(len(b)), nil, 0)
	if err != nil || n <= 0 {
		return nil, false
	}
	u16 := make([]uint16, n)
	n, err = windows.MultiByteToWideChar(codePage, mbErrInvalidChars, pin, int32(len(b)), &u16[0], n)
	if err != nil || n <= 0 {
		return nil, false
	}
	return u16[:n], true
}
