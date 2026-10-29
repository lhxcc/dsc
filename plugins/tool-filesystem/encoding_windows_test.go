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
	got, ok := decodeWindowCodePage(950, "\xc1\x63\xc5\xe9\xa4\xa4\xa4\xe5")
	if !ok {
		t.Fatal("950 码页解码失败")
	}
	if got != "繁體中文" {
		t.Fatalf("Big5 解码 = %q, want 繁體中文", got)
	}
}

func TestDecodeWithCodePageShiftJIS(t *testing.T) {
	// "ファイル" 的 CP932 字节
	got, ok := decodeWindowCodePage(932, "\x83\x74\x83\x40\x83\x43\x83\x8b")
	if !ok {
		t.Fatal("932 码页解码失败")
	}
	if got != "ファイル" {
		t.Fatalf("Shift-JIS 解码 = %q", got)
	}
}

func TestDecodeWithCodePageGBK(t *testing.T) {
	got, ok := decodeWindowCodePage(936, "\xba\xcf\xb2\xa2: v7 \xcf\xdf\xb3\xcc")
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
	got, ok := decodeWindowCodePage(950, "\xba\xcf\xb2\xa2") // GBK "合并"
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
	got := decodeCandidates()
	if len(got) != 1 || got[0].cp != 950 || got[0].charset != "big5" {
		t.Fatalf("override 应只留 950/big5，实际 %+v", got)
	}

	// 字符集名同样接受（不依赖是否知道对应码页号）
	t.Setenv("DSC_OUTPUT_CODEPAGE", "big5")
	if got := decodeCandidates(); len(got) != 1 || got[0].cp != 0 || got[0].charset != "big5" {
		t.Fatalf("名字覆盖应为纯 charset 候选，实际 %+v", got)
	}

	// 非法数字值回退系统码页候选（OEM + ANSI）
	for _, bad := range []string{"", "0", "-3", "70000"} {
		t.Setenv("DSC_OUTPUT_CODEPAGE", bad)
		got := decodeCandidates()
		if len(got) != 2 || got[0].cp != codePageOEMCP || got[1].cp != codePageACP {
			t.Fatalf("非法值 %q 应回退系统码页，实际 %+v", bad, got)
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

// TestDecodeByCharsetGB18030 四字节区段只有 GB18030 解得开（mahonia 的 gbk 会吐
// U+FFFD）——所以 936 映射到 gb18030 而不是 gbk。
func TestDecodeByCharsetGB18030(t *testing.T) {
	got, ok := decodeByCharset("gb18030", "\x95\x32\x82\x36") // U+20000
	if !ok || got != "𠀀" {
		t.Fatalf("gb18030 四字节解码 = %q, ok=%v", got, ok)
	}
	if _, ok := decodeByCharset("gbk", "\x95\x32\x82\x36"); ok {
		t.Fatal("gbk 不该解开四字节区段（若解开了，说明映射到 gb18030 的判断可以简化）")
	}
}

// TestDecodeByCharsetStrictness 半个双字节 / 未知字符集都算失败（不产出假文本）。
func TestDecodeByCharsetStrictness(t *testing.T) {
	if _, ok := decodeByCharset("gb18030", "\xba\xcf\xb2"); ok {
		t.Fatal("截断的双字节序列应判失败")
	}
	if _, ok := decodeByCharset("no-such-charset", "abc"); ok {
		t.Fatal("未知字符集应判失败")
	}
	if got, ok := decodeByCharset("big5", "\xc1\x63\xc5\xe9\xa4\xa4\xa4\xe5"); !ok || got != "繁體中文" {
		t.Fatalf("big5 = %q, ok=%v", got, ok)
	}
}

// TestCharsetForCodePage 码页号 → 字符集名：认识的要映射，韩文 949 明确留空
// （mahonia 无对应字符集，Windows 上由 MultiByteToWideChar 兜住）。
func TestCharsetForCodePage(t *testing.T) {
	cases := map[uint32]string{936: "gb18030", 950: "big5", 932: "shift_jis", 65001: "utf-8", 949: ""}
	for cp, want := range cases {
		if got := charsetForCodePage(cp); got != want {
			t.Fatalf("charsetForCodePage(%d) = %q, want %q", cp, got, want)
		}
	}
}
