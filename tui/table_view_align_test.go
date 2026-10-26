package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"dsc/core"
)

// TestTableViewAlignsCJKHeaders ToolView 表格（cron_list 这类结构化卡片）的列必须按
// **显示宽度**对齐：中文表头每字占两列，若按字符数算宽，列头会比数据行逐列右移
// （与 /plugins 列表同一个坑，现场表现为「数据没跟表头对齐」）。
func TestTableViewAlignsCJKHeaders(t *testing.T) {
	v := &core.ToolView{
		Kind:  "table",
		Title: "CronJobs",
		Columns: []core.ViewColumn{
			{Key: "id", Title: "ID"},
			{Key: "name", Title: "名称"},
			{Key: "cron", Title: "表达式"},
			{Key: "enabled", Title: "启用"},
		},
		Rows: []core.ViewRow{
			{"id": "cron-1", "name": "日报", "cron": "0 8 * * *", "enabled": "true"},
			{"id": "cron-22", "name": "巡检任务", "cron": "*/5 * * * *", "enabled": "false"},
		},
	}
	out := renderTableView(v)
	lines := strings.Split(out, "\n")
	if len(lines) < 3 {
		t.Fatalf("表格行数不足: %q", out)
	}
	// 表头行 = 第二行（首行是 Title 头）
	header := lines[1]
	cols := []int{
		columnOf(header, "ID"),
		columnOf(header, "名称"),
		columnOf(header, "表达式"),
		columnOf(header, "启用"),
	}
	for i := 1; i < len(cols); i++ {
		if cols[i] <= cols[i-1] {
			t.Fatalf("表头列起点异常 %v:/n%s", cols, out)
		}
	}

	want := [][]string{
		{"cron-1", "日报", "0 8 * * *", "true"},
		{"cron-22", "巡检任务", "*/5 * * * *", "false"},
	}
	for ri, w := range want {
		ln := lines[2+ri]
		for ci, cell := range w {
			end := len(ln) // 末列取到行尾
			width := 0
			if ci+1 < len(cols) {
				width = cols[ci+1] - cols[ci]
			} else {
				width = lipgloss.Width(ln) - cols[ci]
			}
			if end == 0 {
				t.Fatal("unreachable")
			}
			if got := textAtColumn(ln, cols[ci], width); got != cell {
				t.Fatalf("第 %d 行第 %d 列 = %q, want %q（未与 CJK 表头对齐）:\n%s",
					ri+1, ci+1, got, cell, out)
			}
		}
	}
}

// TestTableViewTruncatesByDisplayWidth 超长单元格按显示宽度截断：结果总宽恰为
// tableColMax，且不会切出半个中文字。
func TestTableViewTruncatesByDisplayWidth(t *testing.T) {
	long := strings.Repeat("中", 30) // 60 列，远超 tableColMax
	v := &core.ToolView{
		Kind:    "table",
		Columns: []core.ViewColumn{{Key: "v", Title: "值"}},
		Rows:    []core.ViewRow{{"v": long}},
	}
	out := renderTableView(v)
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("表格行数不足: %q", out)
	}
	// 无 Title 时首行就是列头，数据行紧随其后；去掉 gutter 前缀（首行）与缩进（续行）
	last := lines[len(lines)-1]
	cell := strings.TrimRight(strings.TrimSpace(last), " ")
	if w := lipgloss.Width(cell); w > tableColMax {
		t.Fatalf("单元格宽 %d 超过上限 %d: %q", w, tableColMax, cell)
	}
	if !strings.HasSuffix(cell, "…") {
		t.Fatalf("超长单元格应以 … 结尾: %q", cell)
	}
	if strings.ContainsRune(cell, '\uFFFD') {
		t.Fatalf("截断产生了半字符: %q", cell)
	}
}
