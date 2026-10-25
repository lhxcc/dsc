package main

import (
	"os"
	"strings"
	"testing"
)

// TestBrowserHeadedDefaults 默认策略：复用用户 profile 时有头（反爬），隔离 profile 时无头；
// 环境变量可以各自强制。
func TestBrowserHeadedDefaults(t *testing.T) {
	t.Setenv("DSC_BROWSER_HEADLESS", "")
	t.Setenv("DSC_BROWSER_HEADED", "")

	if !browserHeaded(true) {
		t.Fatal("用户模式默认应有头")
	}
	if browserHeaded(false) {
		t.Fatal("隔离模式默认应无头")
	}

	t.Setenv("DSC_BROWSER_HEADLESS", "1")
	if browserHeaded(true) {
		t.Fatal("DSC_BROWSER_HEADLESS=1 应强制无头（含用户模式）")
	}

	os.Unsetenv("DSC_BROWSER_HEADLESS")
	t.Setenv("DSC_BROWSER_HEADED", "true")
	if !browserHeaded(false) {
		t.Fatal("DSC_BROWSER_HEADED=true 应强制有头（含隔离模式）")
	}
}

// TestTruthy 开关解析要宽松（1/true/yes/on），别因为写成大写就不认。
func TestTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !truthy(v) {
			t.Fatalf("%q 应判为开", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "no"} {
		if truthy(v) {
			t.Fatalf("%q 应判为关", v)
		}
	}
}

// TestCandidateBrowsersPreferExplicitBin DSC_BROWSER_BIN 必须排第一（显式指定的优先），
// 其余候选按「系统 Chrome 优先于 Edge」排列，且都指向可执行文件路径。
func TestCandidateBrowsersPreferExplicitBin(t *testing.T) {
	t.Setenv("DSC_BROWSER_BIN", "/opt/custom/chrome")
	list := candidateBrowsers()
	if len(list) == 0 || list[0] != "/opt/custom/chrome" {
		t.Fatalf("DSC_BROWSER_BIN 应排第一：%v", list)
	}
	chromeAt, edgeAt := -1, -1
	for i, p := range list {
		if strings.Contains(p, "Chrome") || strings.Contains(p, "chrome") {
			if chromeAt < 0 {
				chromeAt = i
			}
		}
		if strings.Contains(p, "Edge") || strings.Contains(p, "msedge") || strings.Contains(p, "edge") {
			if edgeAt < 0 {
				edgeAt = i
			}
		}
	}
	if chromeAt < 0 {
		t.Fatalf("候选里应有 Chrome：%v", list)
	}
	if edgeAt >= 0 && edgeAt < chromeAt {
		t.Fatalf("Chrome 应排在 Edge 之前（profile 目录按 Chrome 找）：%v", list)
	}
}
