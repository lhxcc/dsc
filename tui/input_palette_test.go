package tui

import "testing"

// TestInputPaletteContrast 回归：探测到终端底色时，输入区必须显式给出高对比前景色
// （深色终端近白、浅色终端近黑）；探测不到时不得写死前景，必须保留继承终端默认
// 前景——否则浅色终端（如整屏白底）会被渲染成「白底浅字」，读不清。
func TestInputPaletteContrast(t *testing.T) {
	darkText, darkHint := inputPaletteFor(inputThemeDark)
	lightText, lightHint := inputPaletteFor(inputThemeLight)
	unknownText, unknownHint := inputPaletteFor(inputThemeUnknown)

	if darkText != "#F2F2F2" {
		t.Errorf("深色终端的输入文字应为近白色，got %q", darkText)
	}
	if lightText != "#1F1F1F" {
		t.Errorf("浅色终端的输入文字应为近黑色，got %q", lightText)
	}
	if unknownText != "" {
		t.Errorf("底色未知时不得写死输入文字颜色（应继承终端默认前景），got %q", unknownText)
	}
	if darkText == darkHint {
		t.Errorf("占位提示色必须与正文不同（深色），got %q", darkText)
	}
	if lightText == lightHint {
		t.Errorf("占位提示色必须与正文不同（浅色），got %q", lightText)
	}
	if unknownHint == "" {
		t.Error("底色未知时占位提示仍需给出可读的灰色")
	}
	if darkHint != "#7C7C7C" || lightHint != "#8A8A8A" {
		t.Errorf("占位提示色映射异常: dark=%q light=%q", darkHint, lightHint)
	}
}
