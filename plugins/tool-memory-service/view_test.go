package main

import (
	"encoding/json"
	"testing"

	"dsc-sdk"
)

// TestMemorySearchView memory_search 视图：表格版式、徽标命中数、行数据与分数列。
func TestMemorySearchView(t *testing.T) {
	result := `{"results":[{"id":"mem-aaaa000000000001","content":"今天天气？","source":"preset","project":"global","score":0.85},{"id":"mem-aaaa000000000002","content":"写周报","source":"tool","project":"w--proj-1A2B3C4D","score":0.5}],"total":2}`
	view, err := memorySearchView(result)
	if err != nil {
		t.Fatalf("memorySearchView err: %v", err)
	}
	if len(view) == 0 {
		t.Fatal("应返回非空视图")
	}
	var v dsc.View
	if err := json.Unmarshal(view, &v); err != nil {
		t.Fatalf("视图 JSON 非法: %v", err)
	}
	if v.Kind != "table" || v.Title != "Memory" || v.Badge == nil || v.Badge.Text != "2 hit(s)" || v.Badge.Tone != "teal" {
		t.Fatalf("view 头 = %+v", v)
	}
	if len(v.Columns) != 5 || len(v.Rows) != 2 {
		t.Fatalf("columns/rows = %d/%d", len(v.Columns), len(v.Rows))
	}
	// 归属列直接展示存值：global 或项目键，不做标签转换
	if v.Columns[4].Key != "project" || v.Rows[0]["project"] != "global" || v.Rows[1]["project"] != "w--proj-1A2B3C4D" {
		t.Fatalf("project 列展示不符: cols=%+v rows=%+v", v.Columns, v.Rows)
	}
	if v.Columns[2].Key != "score" || v.Columns[2].Tone != "green" {
		t.Fatalf("score 列应带绿色 tone: %+v", v.Columns)
	}
	if v.Rows[0]["id"] != "mem-aaaa000000000001" || v.Rows[0]["content"] != "今天天气？" || v.Rows[0]["score"] != "0.85" {
		t.Fatalf("row0 = %+v", v.Rows[0])
	}
	// 非法 JSON 回退（返回空视图，由 TUI 兜底）
	if view, _ := memorySearchView("not json"); len(view) != 0 {
		t.Fatalf("非法 JSON 应返回空视图: %q", view)
	}
}

// TestMemoryAddView memory_add 视图：卡片版式，dedup 时黄色徽标 + 跳过文案。
func TestMemoryAddView(t *testing.T) {
	view, _ := memoryAddView(`{"id":"mem-bbbb000000000007","dedup":false}`)
	var v dsc.View
	if err := json.Unmarshal(view, &v); err != nil {
		t.Fatalf("视图 JSON 非法: %v", err)
	}
	if v.Kind != "card" || v.Title != "Memory" || v.Badge == nil || v.Badge.Text != "saved" || v.Badge.Tone != "green" {
		t.Fatalf("view 头 = %+v", v)
	}
	if len(v.Fields) != 2 || v.Fields[0].Value != "mem-bbbb000000000007" || v.Fields[1].Value != "saved" {
		t.Fatalf("fields = %+v", v.Fields)
	}

	// 重复内容：黄色徽标 + 跳过状态
	view, _ = memoryAddView(`{"id":"mem-bbbb000000000007","dedup":true}`)
	if err := json.Unmarshal(view, &v); err != nil {
		t.Fatalf("视图 JSON 非法: %v", err)
	}
	if v.Badge.Text != "duplicate" || v.Badge.Tone != "yellow" || v.Fields[1].Value != "skipped (duplicate content)" || v.Fields[1].Tone != "yellow" {
		t.Fatalf("duplicate view = %+v", v)
	}
}
