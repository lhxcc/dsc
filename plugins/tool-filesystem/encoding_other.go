//go:build !windows

package main

// 非 Windows：没有「系统码页」这回事——子进程的编码取决于它自己的 locale，
// 没有可问的操作系统接口。因此默认不做任何试探（非法字节老实给 U+FFFD），
// 只有 DSC_OUTPUT_CODEPAGE 显式指定时才用 mahonia 解码（数字或字符集名都行）。
//
// 为什么默认不猜：Linux 上最常见的非 UTF-8 是 Latin-1 系与 GBK，而 GBK 与 Big5
// 交叉解码会「成功但出错字」——猜错比显示替换字符更难发现。

import (
	"os"
	"strconv"
	"strings"
)

// decodeCandidates 非 Windows 的候选：只有显式覆盖。
func decodeCandidates() []decodeCandidate {
	v := strings.TrimSpace(os.Getenv("DSC_OUTPUT_CODEPAGE"))
	if v == "" {
		return nil
	}
	if n, err := strconv.Atoi(v); err == nil {
		// 数字：越界视为写坏了（不猜），非数字才当字符集名
		if n <= 0 || n > 65535 {
			return nil
		}
		return []decodeCandidate{{charset: charsetForCodePage(uint32(n))}}
	}
	return []decodeCandidate{{charset: v}}
}

// decodeWindowCodePage 非 Windows 没有码页 API。
func decodeWindowCodePage(uint32, string) (string, bool) { return "", false }

// systemOEMCodePage 非 Windows 无系统码页概念；测试据此跳过码页断言。
func systemOEMCodePage() uint16 { return 0 }
