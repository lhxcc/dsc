package main

import (
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestEnsureUTF8ValidPassthrough 合法 UTF-8 原样返回（快速路径，不做任何改写）。
func TestEnsureUTF8ValidPassthrough(t *testing.T) {
	for _, s := range []string{"", "plain ascii", "中文 UTF-8 ✓ emoji 🎉", "line1\nline2\r\n"} {
		got := ensureUTF8(s)
		if got != s {
			t.Fatalf("ensureUTF8(%q) = %q, want unchanged", s, got)
		}
	}
}

// TestEnsureUTF8InvalidBytes 非法 UTF-8 字节必须被净化：结果合法 UTF-8，不再触发
// gRPC marshal 失败。净化形式与系统码页有关：Windows 上若 0x80 恰能被系统码页
// 解出字符（如 CP1252 的 €、CP437 的 Ç）则还原为对应字符，否则落到 U+FFFD——
// 契约是「绝不留非法字节、结果恒为合法 UTF-8」，不断言具体占位符。
func TestEnsureUTF8InvalidBytes(t *testing.T) {
	got := ensureUTF8("abc\x80def")
	if !utf8.ValidString(got) {
		t.Fatalf("ensureUTF8 output must be valid UTF-8, got %q", got)
	}
	if strings.Contains(got, "\x80") {
		t.Fatalf("非法字节不应残留, got %q", got)
	}
	if got == "abc\x80def" {
		t.Fatalf("非法输入不得原样返回, got %q", got)
	}
}

// TestEnsureUTF8GBKOnWindows 中文 Windows 控制台工具输出 GBK（OEM 936 码页）字节，
// 在 GBK 系统上应被还原为正确中文（而非 U+FFFD 乱码）；系统码页非 936 时跳过
// （本机非 zh-CN 时 GBK 字节本就不属于系统码页，U+FFFD 才是正确行为）。
// 夹具须为纯 GBK 字节流（真实工具输出整体按 OEM 码页编码，不会混入 UTF-8）：
// "目录列表：你好.txt" 的 GBK 编码。
func TestEnsureUTF8GBKOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("GBK 码页还原仅 Windows 有意义")
	}
	if systemOEMCodePage() != 936 {
		t.Skipf("系统 OEM 码页 %d ≠ 936（GBK），本机不解 GBK，跳过", systemOEMCodePage())
	}
	got := ensureUTF8("\xc4\xbf\xc2\xbc\xc1\xd0\xb1\xed\xa3\xba\xc4\xe3\xba\xc3.txt")
	if !utf8.ValidString(got) {
		t.Fatalf("输出必须为合法 UTF-8, got %q", got)
	}
	if !strings.Contains(got, "你好") {
		t.Fatalf("GBK 字节应还原为「你好」, got %q", got)
	}
}

// TestEnsureUTF8MixedEncodings 混编码输出：同一段里既有 GBK 行、又有 UTF-8 行。
//
// 现场（2026-10-29 三张截图）：`python3 - <<PY` 打印的中文按系统码页（GBK）出来，
// 而同一份结果里还有我们按 UTF-8 写的行；整段严格解码因一行不合法而全盘放弃，
// 于是 `合并: v7 线程` 显示成 `�ϲ�: v7 �߳�`。逐行重试后两种编码各归各位。
func TestEnsureUTF8MixedEncodings(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("系统码页还原仅 Windows 有意义")
	}
	if systemOEMCodePage() != 936 {
		t.Skipf("系统 OEM 码页 %d ≠ 936（GBK），跳过", systemOEMCodePage())
	}
	// 第 1 行 GBK：合并: v7 线程；第 2 行 UTF-8：已解决
	mixed := "\xba\xcf\xb2\xa2: v7 \xcf\xdf\xb3\xcc\n已解决\n"
	got := ensureUTF8(mixed)
	if !utf8.ValidString(got) {
		t.Fatalf("结果必须为合法 UTF-8, got %q", got)
	}
	if !strings.Contains(got, "合并: v7 线程") {
		t.Fatalf("GBK 行应还原为中文, got %q", got)
	}
	if !strings.Contains(got, "已解决") {
		t.Fatalf("UTF-8 行应原样保留, got %q", got)
	}
	if strings.Contains(got, "\uFFFD") {
		t.Fatalf("不应残留占位符, got %q", got)
	}
}

// TestDecodeMixedLinesKeepsValidLines 逐行解码的边界：全合法输入不标记 changed，
// 全非法且解不出时逐行兜底为 U+FFFD（不返回错误，结果是合法 UTF-8）。
func TestDecodeMixedLinesKeepsValidLines(t *testing.T) {
	if got, changed := decodeMixedLines("完全合法\nstill ascii\n"); changed || got != "完全合法\nstill ascii\n" {
		t.Fatalf("全合法输入不应改动：changed=%v got=%q", changed, got)
	}
	got, changed := decodeMixedLines("abc\xffdef\n")
	if !changed || !utf8.ValidString(got) {
		t.Fatalf("非法行必须被净化：changed=%v got=%q", changed, got)
	}
}
