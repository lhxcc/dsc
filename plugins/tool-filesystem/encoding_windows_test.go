//go:build windows

package main

// 码页相关的测试只对 Windows 有意义（解码走 kernel32 的 MultiByteToWideChar）。
//
// 这里刻意**不依赖本机码页**：直接按显式码页（950/932/936）解码固定字节夹具，
// 用来证明「解码路径与码页无关，繁体系统上就是 Big5」——本机是 zh-CN(936) 也照样
// 能验证 Big5 与 Shift-JIS 走的是同一条路径。

import (
	"strings"
	"testing"
)

func TestDecodeWithCodePageBig5(t *testing.T) {
	// "繁體中文" 的 Big5 字节
	got, ok := decodeWithCodePage(950, "\xc1\x63\xc5\xe9\xa4\xa4\xa4\xe5")
	if !ok {
		t.Fatal("950 码页解码失败")
	}
	if got != "繁體中文" {
		t.Fatalf("Big5 解码 = %q, want 繁體中文", got)
	}
}

func TestDecodeWithCodePageShiftJIS(t *testing.T) {
	// "ファイル" 的 CP932 字节
	got, ok := decodeWithCodePage(932, "\x83\x74\x83\x40\x83\x43\x83\x8b")
	if !ok {
		t.Fatal("932 码页解码失败")
	}
	if got != "ファイル" {
		t.Fatalf("Shift-JIS 解码 = %q", got)
	}
}

func TestDecodeWithCodePageGBK(t *testing.T) {
	got, ok := decodeWithCodePage(936, "\xba\xcf\xb2\xa2: v7 \xcf\xdf\xb3\xcc")
	if !ok || got != "合并: v7 线程" {
		t.Fatalf("936 解码 = %q, ok=%v", got, ok)
	}
}

// TestCrossCodePageDecodesWrongButValid 记录一个必须承认的边界：GBK 与 Big5 都是
// 双字节、编码区间重叠，**交叉解码会「成功」但给出错字**（不是 U+FFFD）。
//
// 也就是说：繁体系统上若某个程序硬输出 GBK，我们会按 Big5 解出错字而不是拒绝。
// 单凭字节无法判断码页，唯一的确定性办法是 DSC_OUTPUT_CODEPAGE 显式指定。
func TestCrossCodePageDecodesWrongButValid(t *testing.T) {
	got, ok := decodeWithCodePage(950, "\xba\xcf\xb2\xa2") // GBK "合并"
	if !ok {
		t.Fatalf("交叉解码居然失败（行为可能已变化，需重新评估）：%q", got)
	}
	if got == "合并" {
		t.Fatal("交叉解码不该解对，夹具或码页有误")
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("双字节码页交叉解码通常不会产生 U+FFFD，实际 %q", got)
	}
}

// TestCodePageCandidatesOverride DSC_OUTPUT_CODEPAGE 显式钉死码页（不再试探）。
func TestCodePageCandidatesOverride(t *testing.T) {
	t.Setenv("DSC_OUTPUT_CODEPAGE", "950")
	got := codePageCandidates()
	if len(got) != 1 || got[0] != 950 {
		t.Fatalf("override 应只留 950，实际 %v", got)
	}

	// 非法值忽略，回到系统码页候选（OEM + ANSI）
	for _, bad := range []string{"", "abc", "0", "-3", "70000"} {
		t.Setenv("DSC_OUTPUT_CODEPAGE", bad)
		got := codePageCandidates()
		if len(got) != 2 || got[0] != codePageOEMCP || got[1] != codePageACP {
			t.Fatalf("非法值 %q 应回退系统码页，实际 %v", bad, got)
		}
	}
}

// TestEnsureUTF8UsesOverride 端到端：钉死 950 后，Big5 字节能被还原成繁体中文。
func TestEnsureUTF8UsesOverride(t *testing.T) {
	t.Setenv("DSC_OUTPUT_CODEPAGE", "950")
	got := ensureUTF8("\xc1\x63\xc5\xe9\xa4\xa4\xa4\xe5\n")
	if !strings.Contains(got, "繁體中文") {
		t.Fatalf("钉死 950 后应还原 Big5，实际 %q", got)
	}
}
