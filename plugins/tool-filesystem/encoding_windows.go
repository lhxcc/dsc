//go:build windows

package main

// Windows：按系统码页把非 UTF-8 字节流严格解码为 UTF-8 文本。
//
// 走 Win32 的 MultiByteToWideChar（MB_ERR_INVALID_CHARS 严格校验），因为它是唯一
// 覆盖**全部**系统码页的解码器——包括 mahonia 不收的韩文 949。码页号不写死：传
// CP_OEMCP / CP_ACP 由系统解析（zh-CN 936、zh-TW 950/Big5、ja-JP 932、ko-KR 949…）。
//
// 非 Windows 平台没有「系统码页」这个概念，那条路由 mahonia 承担
// （见 encoding_charset.go）。

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

// systemANSICodePage 返回系统 ANSI 码页（CRT 程序写管道时的默认编码）。
func systemANSICodePage() uint16 {
	r, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetACP").Call()
	return uint16(r)
}

// decodeCandidates 解码候选。
//
// **码页在运行时取自系统，不写死**：CP_OEMCP / CP_ACP 交给 Windows 解析，于是
// zh-CN 得到 936、zh-TW 得到 950（Big5）、ja-JP 得到 932、ko-KR 得到 949，不需要
// 判断「这是不是简体系统」。先 OEM 后 ANSI 的理由：管道直连的原生控制台程序
// （dir/tree/findstr）按控制台码页（OEM）输出，CRT 程序多按 ANSI；东亚系统上两者
// 通常相同，单字节系统上才有差别。
//
// DSC_OUTPUT_CODEPAGE 可显式钉死：数字（950）或字符集名（big5）都行，此时不再试探。
func decodeCandidates() []decodeCandidate {
	if v := strings.TrimSpace(os.Getenv("DSC_OUTPUT_CODEPAGE")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			// 数字：越界视为写坏了，回退系统码页（不能当成字符集名 "0"）
			if n <= 0 || n > 65535 {
				return systemCandidates()
			}
			if n == codePageOEMCP || n == codePageACP {
				return []decodeCandidate{{cp: uint32(n)}}
			}
			return []decodeCandidate{{cp: uint32(n), charset: charsetForCodePage(uint32(n))}}
		}
		// 非数字：当字符集名（big5 / gb18030 / windows-1252 …）
		return []decodeCandidate{{charset: v}}
	}
	return systemCandidates()
}

// systemCandidates 系统码页候选：OEM（控制台程序）+ ANSI（CRT 程序）。
func systemCandidates() []decodeCandidate {
	return []decodeCandidate{{cp: codePageOEMCP}, {cp: codePageACP}}
}

// decodeWindowCodePage 以指定码页严格解码（0 = 系统 ANSI，1 = 系统 OEM）。
func decodeWindowCodePage(cp uint32, s string) (string, bool) {
	u16, ok := multibyteToUTF16(cp, []byte(s))
	if !ok {
		return "", false
	}
	return string(utf16.Decode(u16)), true
}

// multibyteToUTF16 调 MultiByteToWideChar（严格模式）把多字节缓冲转为 UTF-16。
// 第一次调用传 nil 目标取所需长度，再分配后取实际结果（Win32 惯例两段式）。
func multibyteToUTF16(codePage uint32, b []byte) ([]uint16, bool) {
	if len(b) == 0 {
		return nil, false
	}
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
