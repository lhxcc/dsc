package main

import (
	"strings"
	"testing"
)

// TestBrowserHeadedDefaults 默认无头（不弹窗）；HEADLESS / HEADED / OFFSCREEN 可覆盖，
// 优先级 HEADLESS > HEADED > OFFSCREEN。
func TestBrowserHeadedDefaults(t *testing.T) {
	clear := func() {
		t.Setenv("DSC_BROWSER_HEADLESS", "")
		t.Setenv("DSC_BROWSER_HEADED", "")
		t.Setenv("DSC_BROWSER_OFFSCREEN", "")
	}

	clear()
	if browserHeaded() {
		t.Fatal("默认应无头（实测无头与有头在常用指纹上一致，且不弹窗）")
	}

	clear()
	t.Setenv("DSC_BROWSER_OFFSCREEN", "1")
	if !browserHeaded() || !browserOffscreen() {
		t.Fatal("OFFSCREEN 应有头且离屏")
	}

	clear()
	t.Setenv("DSC_BROWSER_HEADED", "1")
	if !browserHeaded() || browserOffscreen() {
		t.Fatal("HEADED 应有头且可见（不离屏）")
	}

	clear()
	t.Setenv("DSC_BROWSER_HEADED", "1")
	t.Setenv("DSC_BROWSER_HEADLESS", "1")
	if browserHeaded() {
		t.Fatal("HEADLESS 优先级最高")
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
