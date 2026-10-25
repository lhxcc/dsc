package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// mkPayload 造一份测量结果：每项是 (文本, x, y, h)，charW 固定 8px（8px/字符）。
func mkPayload(charW float64, items ...layoutItem) string {
	b, _ := json.Marshal(layoutPage{CharW: charW, Items: items})
	return string(b)
}

func item(text string, x, y, h float64) layoutItem {
	return layoutItem{Text: text, X: x, Y: y, H: h}
}

// TestRenderLayoutKeepsSameRowTogether 同一视觉行里的几块必须落在同一行，并按 x 补白。
// 这是这次改动的核心：flex/grid 页面上，innerText 会把「首页 关于 联系」竖着拆成三行。
func TestRenderLayoutKeepsSameRowTogether(t *testing.T) {
	payload := mkPayload(8,
		item("首页", 0, 10, 20),
		item("关于", 160, 12, 18),
		item("联系", 320, 8, 24),
	)
	out, ok := renderLayout(payload)
	if !ok {
		t.Fatal("应能还原版面")
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 1 {
		t.Fatalf("同一行的三块应产出 1 行，实际 %d 行：%q", len(lines), out)
	}
	// 列位：0 / 20 / 40 列（x÷8）；"首页" 占 4 列（全角按 2 列计），故补 16 空格到第 20 列
	want := "首页" + strings.Repeat(" ", 16) + "关于" + strings.Repeat(" ", 16) + "联系"
	if lines[0] != want {
		t.Fatalf("行内容不对\n got=%q\nwant=%q", lines[0], want)
	}
}

// TestRenderLayoutSplitsDifferentRows 上下两行不能合并；高度不同也要分开。
func TestRenderLayoutSplitsDifferentRows(t *testing.T) {
	payload := mkPayload(8,
		item("标题", 0, 0, 24),
		item("正文第一段", 0, 40, 20),
	)
	out, _ := renderLayout(payload)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("两行文本应产出 2 行，实际 %d 行：%q", len(lines), out)
	}
	if lines[0] != "标题" || lines[1] != "正文第一段" {
		t.Fatalf("行内容不对：%q", out)
	}
}

// TestRenderLayoutJoinsTallAndShortBlocks 一行里高矮不齐（图标 / 链接 / 按钮）时，
// 只要垂直重叠过半就算同一行——这是「top 相等」判据做不到的。
func TestRenderLayoutJoinsTallAndShortBlocks(t *testing.T) {
	payload := mkPayload(8,
		item("按钮", 0, 10, 36),   // 高块
		item("图标", 100, 20, 16), // 落在高块中部
	)
	out, _ := renderLayout(payload)
	if strings.Count(out, "\n") != 0 {
		t.Fatalf("垂直重叠过半应视为同一行，实际：%q", out)
	}
}

// TestRenderLayoutCapsGap 绝对定位 / 浮动侧栏会拉出上百列空档，补白必须封顶。
func TestRenderLayoutCapsGap(t *testing.T) {
	payload := mkPayload(8,
		item("左", 0, 0, 20),
		item("右", 8000, 0, 20), // 1000 列
	)
	out, _ := renderLayout(payload)
	gap := strings.Count(out, " ") - 0
	if gap > maxColumnGap {
		t.Fatalf("补白 %d 列超过上限 %d：%q", gap, maxColumnGap, out)
	}
	if !strings.HasPrefix(out, "左") || !strings.HasSuffix(out, "右") {
		t.Fatalf("两端的文本都应保留：%q", out)
	}
}

// TestRenderLayoutTrimsTrailingSpaces 行尾不补白（否则模型读到一堆无意义空格）。
func TestRenderLayoutTrimsTrailingSpaces(t *testing.T) {
	payload := mkPayload(8, item("只有一块", 0, 0, 20))
	out, _ := renderLayout(payload)
	if strings.HasSuffix(out, " ") {
		t.Fatalf("行尾不该有空格：%q", out)
	}
}

// TestRenderLayoutTable 表格：每行一格，列位一致。
func TestRenderLayoutTable(t *testing.T) {
	payload := mkPayload(10,
		item("姓名", 0, 0, 20), item("年龄", 200, 0, 20), item("城市", 320, 0, 20),
		item("张三", 0, 30, 20), item("28", 200, 30, 20), item("广州", 320, 30, 20),
	)
	out, _ := renderLayout(payload)
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("表格应还原成 2 行，实际 %d：%q", len(lines), out)
	}
	// 两行的第二列、第三列应出现在同一列位（列 = x ÷ 10）
	for _, l := range lines {
		if idx := strings.Index(l, "年龄"); idx < 0 {
			continue
		}
	}
	colOf := func(line, want string) int {
		i := strings.Index(line, want)
		if i < 0 {
			t.Fatalf("%q 中找不到 %q", line, want)
		}
		return cellWidth(line[:i]) // 列位按显示宽度比，汉字占两列
	}
	if colOf(lines[0], "年龄") != colOf(lines[1], "28") {
		t.Fatalf("同一列应对齐：%q / %q", lines[0], lines[1])
	}
	if colOf(lines[0], "城市") != colOf(lines[1], "广州") {
		t.Fatalf("第三列应对齐：%q / %q", lines[0], lines[1])
	}
}

// TestRenderLayoutRejectsBadPayload 测量结果不可用时返回 false，让调用方退回 innerText：
// 取文功能不能因为排版失败而整体失败。
func TestRenderLayoutRejectsBadPayload(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
	}{
		{"空串", ""},
		{"坏 JSON", `{"items":[`},
		{"无 items", `{"charW":8,"items":[]}`},
		{"只有空白文本", mkPayload(8, item("   ", 0, 0, 10))},
	} {
		if out, ok := renderLayout(tc.payload); ok {
			t.Fatalf("%s：应返回 false，实际 %q", tc.name, out)
		}
	}
}

// TestRenderLayoutCharWFallback charW 缺失/异常时按 8px 处理，不能出现除零或全挤成一行。
func TestRenderLayoutCharWFallback(t *testing.T) {
	payload := mkPayload(0,
		item("甲", 0, 0, 20),
		item("乙", 80, 0, 20), // 按 8px → 10 列
	)
	out, ok := renderLayout(payload)
	if !ok {
		t.Fatal("charW 为 0 时应走默认值而非失败")
	}
	if !strings.Contains(out, "甲") || !strings.Contains(out, "乙") {
		t.Fatalf("两块都应保留：%q", out)
	}
	if strings.Count(out, " ") != 8 {
		t.Fatalf("按默认 8px、汉字占 2 列，应有 8 个空格，实际：%q", out)
	}
}

// TestRenderLayoutJoinsInlineRuns 同一位置起头的两块（<b>、<span> 把一个词拆成两个
// 文本节点）必须直接拼接，不能插空格——否则 "PDI" 会被写成 "PD I"。
func TestRenderLayoutJoinsInlineRuns(t *testing.T) {
	cont := item("I造假", 15, 0, 20) // 15px ÷ 8 ≈ 2 列，而 "PD" 已占 2 列
	cont.Cont = true               // 测量侧判定「接着上一段写的」
	payload := mkPayload(8, item("PD", 0, 0, 20), cont)
	out, _ := renderLayout(payload)
	if out != "PDI造假" {
		t.Fatalf("内联片段应直接拼接，实际：%q", out)
	}
}

// TestRenderLayoutSeparatesAdjacentBlocks 两个独立块即使矩形贴在一起（页面上没有栏距）
// 也必须隔开：把两条互不相干的标题粘成一句，比多一个空格危险得多。
func TestRenderLayoutSeparatesAdjacentBlocks(t *testing.T) {
	next := item("2027款坦克500正式上市", 108, 0, 20) // 紧贴上一块右缘
	payload := mkPayload(8, item("售价33.5万元", 0, 0, 20), next)
	out, _ := renderLayout(payload)
	if strings.Contains(out, "万元2027") {
		t.Fatalf("相邻的独立块被粘在一起了：%q", out)
	}
	// 具体补几个空格由列位决定（这里是 2 个），只要隔开且两块都完整即可
	if !strings.HasPrefix(out, "售价33.5万元 ") || !strings.HasSuffix(out, "2027款坦克500正式上市") {
		t.Fatalf("两块之间应有空格，实际：%q", out)
	}
}

// TestRenderLayoutDoesNotMergeBeyondPageWidth 超出页宽的块不参与行内合并：
// 两个隔着大半页的东西（绝对定位残余、横向溢出）并成一行只会得到一长串空格。
func TestRenderLayoutDoesNotMergeBeyondPageWidth(t *testing.T) {
	payload, _ := json.Marshal(map[string]any{
		"charW": 8.0,
		"cols":  12, // 页宽 12 列
		"items": []layoutItem{item("左", 0, 0, 20), item("右", 4000, 0, 20)},
	})
	out, ok := renderLayout(string(payload))
	if !ok {
		t.Fatal("应能还原版面")
	}
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("超出页宽的两块应各占一行，实际：%q", out)
	}
	if strings.Contains(out, "          ") {
		t.Fatalf("不该出现超长补白：%q", out)
	}
}

// TestRenderLayoutJoinsInlineRuns 同一位置起头的两块（<b>、<span> 把一个词拆成两个
// 文本节点）必须直接拼接，不能插空格——否则 "PDI" 会被写成 "PD I"。
