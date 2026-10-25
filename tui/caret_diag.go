package tui

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

// 输入行宽度诊断（/caret）。
//
// 真实光标的位置是算出来的：左侧 padding + 提示符 + 已输入文字到插入点的列宽
// （见 inputCursorAbs）。这个算式只在「终端把每个字形都画成我们量到的列宽」时
// 成立。一旦输入里出现「由别的字体绘制、实际列宽与 Unicode 宽度不一致」的字符，
// 屏幕上的文字就会相对模型整体偏移，光标于是压在相邻汉字的半个格子上——看起来
// 就是「中文只被选中一半」。
//
// 这类字符往往肉眼看不见：零宽空格、变体选择符 U+FE0F（把 ☺ 变成彩色 emoji 的
// 那个）、ZWJ 连字、组合附加符号，或罕见的星光平面汉字（字体缺字时回退绘制）。
// 用户在屏幕上无法发现它们，所以出问题时用 /caret 把输入行逐字符反查出来，直接
// 定位是哪一个字符、在第几个位置。
//
// 判据（任一命中即列为可疑）：
//   - Unicode 宽度与 wcwidth 宽度不一致（grapheme 连字 vs 逐 rune 计宽，emoji 序列
//     通常差好几列）；
//   - 零宽/控制类格式字符（Cf：ZWSP/ZWNJ/ZWJ/BOM 等）与变体选择符（FE0E/FE0F）；
//   - 组合附加符号（Mn/Me：会叠在前一个字形上，宽度按 0 计）；
//   - 星光平面字符（> U+FFFF：等宽字体普遍缺字，几乎一定走字体回退）。
func (m *Model) caretDiagnostic() string {
	value := m.input.Value()
	runes := []rune(value)

	li := m.input.LineInfo()
	cur := m.inputCursorAbs()
	curX, curY := -1, -1
	if cur != nil {
		curX, curY = cur.X, cur.Y
	}

	var b strings.Builder
	b.WriteString("输入行宽度诊断\n")
	fmt.Fprintf(&b, "  字符数 %d，模型宽度 %d 列（含提示符 %d + padding %d）\n",
		len(runes), ansi.StringWidth(value), composerPromptWidth, composerPadLeft)
	fmt.Fprintf(&b, "  光标：屏幕 X=%d Y=%d；行内第 %d 行、行内列 %d（CharOffset %d）\n",
		curX, curY, li.RowOffset, li.ColumnOffset, li.CharOffset)

	var suspicious []string
	for i, r := range runes {
		gw, ww := ansi.StringWidth(string(r)), ansi.StringWidthWc(string(r))
		reason := ""
		switch {
		case gw != ww:
			reason = fmt.Sprintf("宽度口径不一致：grapheme %d / wcwidth %d", gw, ww)
		case r == 0xFE0E || r == 0xFE0F:
			reason = "变体选择符（会把前一个字符切成 emoji 呈现）"
		case unicode.Is(unicode.Cf, r):
			reason = "零宽格式字符（屏幕上不可见）"
		case unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r):
			reason = "组合附加符号（叠在前一个字形上）"
		case r > 0xFFFF:
			reason = "星光平面字符（等宽字体普遍缺字，终端会回退字体绘制）"
		case r == '\t':
			reason = "制表符（终端按制表位展开，宽度不是 1）"
		}
		if reason == "" {
			continue
		}
		// 位置用「第几个字符」（从 1 数）更方便对照屏幕
		suspicious = append(suspicious, fmt.Sprintf("  #%d U+%04X %q：%s", i+1, r, r, reason))
		if len(suspicious) >= 20 {
			suspicious = append(suspicious, "  …（仅列前 20 个）")
			break
		}
	}
	if len(suspicious) == 0 {
		b.WriteString("  可疑字符：无（逐字符宽度与 wcwidth 一致）\n")
		b.WriteString("  说明：屏幕若仍偏移，问题在终端字体/列宽度量，而不在 DSC 的算式。\n")
	} else {
		b.WriteString("  可疑字符：\n")
		b.WriteString(strings.Join(suspicious, "\n"))
		b.WriteString("\n  这些字符的屏幕列宽可能与模型不同：光标偏移由它们引起，删掉即可恢复对齐。\n")
	}
	return b.String()
}

// 输入行几何常量：提示符 2 列（"❯ "）、composer 左侧 padding 1 列。
// 与 New() 里的 SetPromptFunc(2, …) 与 composerBoxSty 的 PaddingLeft(1) 同源，
// 改动时三处必须一起改（cursor_prompt_width_test.go 会盯住提示符那一侧）。
const (
	composerPromptWidth = 2
	composerPadLeft     = 1
)
