package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchFormats 真浏览器跑三种格式，确认它们确实是三种东西而不是同一种换了名字：
//   - layout：并排的导航项在同一行（保留版面）；
//   - text  ：innerText，导航项各自成行（紧凑）；
//   - html  ：原始标记，能看到 <div class="nav">。
//
// 需要本机 chromium，默认跳过：DSC_BROWSER_E2E=1 go test -run TestFetchFormats -v
func TestFetchFormats(t *testing.T) {
	if os.Getenv("DSC_BROWSER_E2E") != "1" {
		t.Skip("需要真实浏览器：设置 DSC_BROWSER_E2E=1 后运行")
	}
	t.Setenv("DSC_BROWSER_HEADLESS", "1") // 测试不需要窗口（TestMain 已兜底，这里写明意图）
	// 用临时文件而不是 data: URL：QueryEscape 会把空格编码成 "+"，而 data: URL 里 "+"
	// 不是空格，HTML 标签属性之间的空格会被吃掉，标签直接失效（样例页就查不到 .nav 了）。
	dir := t.TempDir()
	htmlPath := filepath.Join(dir, "sample.html")
	if err := os.WriteFile(htmlPath, []byte(layoutSampleHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	pageURL := "file:///" + filepath.ToSlash(htmlPath)

	fetch := func(format fetchFormat) WebFetchResult {
		t.Helper()
		raw, err := webFetchImpl("fmt-e2e", pageURL, format, false)
		if err != nil {
			t.Fatalf("%s fetch: %v", format, err)
		}
		var r WebFetchResult
		if err := json.Unmarshal([]byte(raw), &r); err != nil {
			t.Fatalf("%s unmarshal: %v", format, err)
		}
		if !r.Success {
			t.Fatalf("%s 失败: %s", format, r.Error)
		}
		return r
	}
	defer GetBrowserSessionManager().CloseSession("fmt-e2e")

	layout := fetch(formatLayout).Content
	text := fetch(formatText).Content
	html := fetch(formatHTML).Content

	sameLine := func(content, a, b string) bool {
		for _, ln := range strings.Split(content, "\n") {
			if strings.Contains(ln, a) && strings.Contains(ln, b) {
				return true
			}
		}
		return false
	}

	if !sameLine(layout, "首页", "产品") {
		t.Fatalf("layout 应把并排的导航项放在同一行：\n%s", layout)
	}
	if sameLine(text, "首页", "产品") {
		t.Fatalf("text(innerText) 本应把并排的块拆成多行，实际同一行：\n%s", text)
	}
	if !strings.Contains(html, `<div class="nav">`) {
		t.Fatalf("html 应返回原始标记：\n%.200s", html)
	}
	if layout == text || text == html {
		t.Fatal("三种格式不该产出相同内容")
	}
}
