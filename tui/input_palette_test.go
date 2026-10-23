package tui

import "testing"

// TestInputPaletteContrast 回归：输入区必须显式给出高对比前景色。
// textarea 默认的 Focused.Text 不带前景色、完全继承终端前景，在「深底深字」
// 的主题下会黑底黑字；这里守住「深色终端近白、浅色终端近黑」的映射方向。
func TestInputPaletteContrast(t *testing.T) {
	darkText, darkHint := inputPaletteFor(true)
	lightText, lightHint := inputPaletteFor(false)

	if darkText != "#F2F2F2" {
		t.Errorf("深色终端的输入文字应为近白色，got %q", darkText)
	}
	if lightText != "#1F1F1F" {
		t.Errorf("浅色终端的输入文字应为近黑色，got %q", lightText)
	}
	if darkText == darkHint {
		t.Errorf("占位提示色必须与正文不同（深色），got %q", darkText)
	}
	if lightText == lightHint {
		t.Errorf("占位提示色必须与正文不同（浅色），got %q", lightText)
	}
	if darkHint != "#7C7C7C" || lightHint != "#8A8A8A" {
		t.Errorf("占位提示色映射异常: dark=%q light=%q", darkHint, lightHint)
	}
}
