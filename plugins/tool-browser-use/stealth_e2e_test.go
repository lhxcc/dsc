package main

import (
	"os"
	"strings"
	"testing"
)

// TestStealthOnRealBrowser 真浏览器验证反爬指纹已清理：
//   - navigator.webdriver 必须是 false（rod 默认带 --enable-automation，它是 true）；
//   - UA 里不能出现 "HeadlessChrome"（等于自报无头）；
//   - UA 的平台必须与宿主一致（rod 默认把页面模拟成 MacBook）。
//
// 需要本机 chromium，默认跳过：
//
//	DSC_BROWSER_E2E=1 [DSC_BROWSER_BIN=<chrome 路径>] go test -run TestStealthOnRealBrowser -v
func TestStealthOnRealBrowser(t *testing.T) {
	if os.Getenv("DSC_BROWSER_E2E") != "1" {
		t.Skip("需要真实浏览器：设置 DSC_BROWSER_E2E=1 后运行")
	}
	t.Setenv("DSC_BROWSER_HEADLESS", "1") // 无头更苛刻（有头本来 UA 就是真的）

	mgr := GetBrowserSessionManager()
	sess, err := mgr.CreateSession("stealth-e2e", false) // 隔离会话，不碰用户 profile
	if err != nil {
		t.Fatalf("创建会话: %v", err)
	}
	defer mgr.CloseSession("stealth-e2e")

	page, err := sess.CreatePage("p", "data:text/html,<h1>hi</h1>")
	if err != nil {
		t.Fatalf("创建页面: %v", err)
	}
	wd, err := page.Eval("() => String(navigator.webdriver)")
	if err != nil {
		t.Fatalf("读 webdriver: %v", err)
	}
	if got := wd.Value.Str(); got != "false" {
		t.Fatalf("navigator.webdriver = %s，应为 false（--enable-automation 未清掉？）", got)
	}
	ua, _ := page.Eval("() => navigator.userAgent")
	if strings.Contains(ua.Value.Str(), "Headless") {
		t.Fatalf("UA 仍在自报无头: %s", ua.Value.Str())
	}
	plat, _ := page.Eval("() => navigator.platform")
	if !strings.Contains(ua.Value.Str(), "Windows") && plat.Value.Str() == "MacIntel" {
		t.Fatalf("UA/platform 与宿主不符（rod 默认设备模拟没关？）: %s / %s", ua.Value.Str(), plat.Value.Str())
	}
}
