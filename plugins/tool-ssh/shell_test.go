package main

import (
	"testing"
	"unicode/utf8"
)

// TestShellExtract 解析层的边界：marker 分片到达、退出码、UTF-8、缓冲裁剪标记。
func TestShellExtract(t *testing.T) {
	const marker = "__DSC_E_abc__:"

	t.Run("marker 未到齐时继续等", func(t *testing.T) {
		sh := &persistentShell{}
		sh.buf = []byte("partial output\n" + marker)
		if _, _, ok, _ := sh.extract(marker); ok {
			t.Fatal("退出码还没到就不该判定完成")
		}
		sh.buf = append(sh.buf, []byte("0\n")...)
		text, code, ok, _ := sh.extract(marker)
		if !ok || code != 0 || text != "partial output" {
			t.Fatalf("text=%q code=%d ok=%v", text, code, ok)
		}
		if len(sh.buf) != 0 {
			t.Fatalf("已消费的缓冲应清空，剩 %q", sh.buf)
		}
	})

	t.Run("非零退出码与尾部空行", func(t *testing.T) {
		sh := &persistentShell{buf: []byte("a\nb\n\n" + marker + "2\n")}
		text, code, ok, _ := sh.extract(marker)
		if !ok || code != 2 || text != "a\nb" {
			t.Fatalf("text=%q code=%d ok=%v", text, code, ok)
		}
	})

	t.Run("非法 UTF-8 不污染结果", func(t *testing.T) {
		sh := &persistentShell{buf: append(append([]byte{0xff, 0xfe}, []byte("ok\n")...), []byte(marker+"0\n")...)}
		text, _, ok, _ := sh.extract(marker)
		if !ok || !utf8.ValidString(text) {
			t.Fatalf("ok=%v text=%q（必须合法 UTF-8，否则整个工具结果 marshal 失败）", ok, text)
		}
	})

	t.Run("截断标记只报一次", func(t *testing.T) {
		sh := &persistentShell{trunc: true, buf: []byte("tail\n" + marker + "0\n")}
		text, _, ok, trunc := sh.extract(marker)
		if !ok || !trunc || text != "tail" {
			t.Fatalf("text=%q trunc=%v", text, trunc)
		}
		// 第二次（下一条命令）不该还带着上一条的截断标记
		sh.buf = []byte("x\n" + marker + "0\n")
		if _, _, _, trunc2 := sh.extract(marker); trunc2 {
			t.Fatal("截断标记应只对那一条命令生效")
		}
	})

	t.Run("marker 在中间也切得对", func(t *testing.T) {
		sh := &persistentShell{buf: []byte("head\n" + marker + "0\nleftover\n")}
		text, _, ok, _ := sh.extract(marker)
		if !ok || text != "head" {
			t.Fatalf("text=%q", text)
		}
		if string(sh.buf) != "leftover\n" {
			t.Fatalf("剩余缓冲 = %q", sh.buf)
		}
	})
}
