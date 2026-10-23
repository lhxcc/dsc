package main

import "testing"

// TestDefaultOutDirBesideTarget 默认产物目录必须落在处理目标旁边的具名子目录：
// <源目录>/<源主名>-images[/sub]。旧行为丢到 <工作区根>/pdf-images/<文件名>/，
// 处理工作区外的 PDF 时落点无关，且不同目录的同名 PDF 会互相覆盖。
func TestDefaultOutDirBesideTarget(t *testing.T) {
	cases := []struct {
		name string
		src  string
		sub  string
		want string
	}{
		{"workdir pdf", "/data/docs/report.pdf", "", "/data/docs/report-images"},
		{"render subdir", "/data/docs/report.pdf", "render", "/data/docs/report-images/render"},
		{"no extension", "/data/docs/scan", "", "/data/docs/scan-images"},
		{"dots in name", "/data/a.b/c.pdf", "", "/data/a.b/c-images"},
		{"root file", "/report.PDF", "", "/report-images"},
	}
	for _, c := range cases {
		if got := defaultOutDir(c.src, c.sub); got != c.want {
			t.Errorf("%s: defaultOutDir(%q,%q) = %q, want %q", c.name, c.src, c.sub, got, c.want)
		}
	}
	// 同名 PDF 位于不同目录时产物不得共享一个目录（旧实现会撞在一起）
	if defaultOutDir("/a/x.pdf", "") == defaultOutDir("/b/x.pdf", "") {
		t.Fatal("不同目录的同名 PDF 不应共用一个输出目录")
	}
}
