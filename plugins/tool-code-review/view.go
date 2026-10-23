// view.go：评审结果的结构化视图（TUI 统一渲染）。
package main

import (
	"encoding/json"
	"fmt"
	"strings"

	dsc "dsc-sdk"
)

// viewMaxRows 限制表格行数：完整清单仍在工具结果 JSON 里，视图只做概览。
const viewMaxRows = 25

// reviewView 构造评审结果视图：无问题时纯文本摘要，有问题时问题表格。
func reviewView(result string) (json.RawMessage, error) {
	var r reviewResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	if !r.OK {
		return dsc.PlainView("Code Review", &dsc.ViewBadge{Text: "error", Tone: "red"}, result), nil
	}

	badge := &dsc.ViewBadge{Text: "clean", Tone: "green"}
	if len(r.Comments) > 0 {
		badge = &dsc.ViewBadge{Text: fmt.Sprintf("%d findings", len(r.Comments)), Tone: severityTone(r.Counts)}
	}

	if len(r.Comments) == 0 {
		return dsc.PlainView("Code Review", badge, reviewSummary(r)+"\n无问题。"), nil
	}

	rows := make([]dsc.ViewRow, 0, min(len(r.Comments), viewMaxRows))
	for i, c := range r.Comments {
		if i >= viewMaxRows {
			rows = append(rows, dsc.ViewRow{
				"file":     "…",
				"line":     "",
				"severity": "",
				"category": "",
				"content":  fmt.Sprintf("另有 %d 条问题（完整清单见工具结果）", len(r.Comments)-viewMaxRows),
			})
			break
		}
		rows = append(rows, dsc.ViewRow{
			"file":     c.Path,
			"line":     lineRange(c.StartLine, c.EndLine),
			"severity": c.Severity,
			"category": c.Category,
			"content":  truncateRunes(c.Content, 90),
		})
	}
	return dsc.TableView("Code Review", badge, []dsc.ViewColumn{
		{Key: "file", Title: "file"},
		{Key: "line", Title: "line"},
		{Key: "severity", Title: "severity"},
		{Key: "category", Title: "category"},
		{Key: "content", Title: "content"},
	}, rows), nil
}

// rulesView 构造规则匹配视图。
func rulesView(result string) (json.RawMessage, error) {
	var r rulesResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	rows := make([]dsc.ViewRow, 0, len(r.Rules))
	for _, m := range r.Rules {
		rows = append(rows, dsc.ViewRow{"path": m.Path, "rule": m.Rule, "source": m.Source})
	}
	return dsc.TableView("Review Rules", &dsc.ViewBadge{Text: fmt.Sprintf("%d", len(rows)), Tone: "teal"}, []dsc.ViewColumn{
		{Key: "path", Title: "path"},
		{Key: "rule", Title: "rule"},
		{Key: "source", Title: "source"},
	}, rows), nil
}

// reviewSummary 生成一行摘要：模式、文件数、严重级别分布、错误与跳过原因。
func reviewSummary(r reviewResult) string {
	var parts []string
	parts = append(parts, "模式: "+r.Mode)
	if r.Base != "" {
		parts = append(parts, "基线: "+r.Base)
	}
	parts = append(parts, fmt.Sprintf("文件: %d", r.FilesReviewed))
	if len(r.Counts) > 0 {
		parts = append(parts, fmt.Sprintf("critical=%d high=%d medium=%d low=%d",
			r.Counts["critical"], r.Counts["high"], r.Counts["medium"], r.Counts["low"]))
	}
	if len(r.FilesSkipped) > 0 {
		parts = append(parts, fmt.Sprintf("跳过: %d", len(r.FilesSkipped)))
	}
	if r.Dropped > 0 {
		parts = append(parts, fmt.Sprintf("丢弃无效评论: %d", r.Dropped))
	}
	if r.Note != "" {
		parts = append(parts, r.Note)
	}
	if len(r.Errors) > 0 {
		parts = append(parts, "错误: "+strings.Join(r.Errors, "; "))
	}
	return strings.Join(parts, " · ")
}

// severityTone 按最高严重级别选择徽标颜色。
func severityTone(counts map[string]int) string {
	switch {
	case counts["critical"] > 0 || counts["high"] > 0:
		return "red"
	case counts["medium"] > 0:
		return "yellow"
	case counts["low"] > 0:
		return "teal"
	default:
		return "gray"
	}
}

func lineRange(start, end int) string {
	if end <= start {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
