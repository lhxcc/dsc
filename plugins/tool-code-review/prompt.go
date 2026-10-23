// prompt.go：评审提示词与带行号的代码渲染。
//
// 关键约束都写在提示词里并用代码兜底：评论只能落在「+」行（新增/修改行），
// 行号使用新文件行号；输出必须是纯 JSON 数组。行号由渲染阶段逐行标注，
// 从源头降低模型「数错行」的概率。
package main

import (
	"fmt"
	"strings"
)

const reviewSystemPrompt = `你是一名严谨的代码评审员。你只输出能被直接落到代码行上的问题清单。

评审原则：
- 精确优先于覆盖：只报你在给定代码中能确证的问题；误报比漏报更有害。
- 只评审后面列出文件的新增/修改行（前缀 +）与被删除的代码（前缀 -）；
  不要对未改动的上下文行提问题。
- 删除行在新文件中已不存在：若删除本身有问题（如删掉了校验），把评论锚定到
  紧随其后那一行（块尾则用前一行）的新文件行号。
- 不报格式、命名、import 顺序、纯风格偏好——这些由 formatter/linter 覆盖。
- 不做没有证据的推测（"可能并发"、"可能为空"）：先确认调用上下文，确认不了就不报。
- 同一问题只报一次；宁缺毋滥。没有问题时输出 []。

严重级别 severity：critical（必然故障/安全漏洞）、high（明确缺陷）、
medium（可疑且需确认）、low（可改进）。

分类 category：bug / security / performance / maintainability / test /
documentation / style / other。

严格只输出 JSON 数组，不要 markdown 代码围栏，不要任何解释文字。元素格式：
[{"path":"相对路径","start_line":123,"end_line":123,"severity":"high","category":"bug","content":"问题是什么、为什么错、怎么改","suggestion_code":"可选：替换 start_line..end_line 的代码"}]
`

const scanSystemExtra = `
注意：本次是整文件扫描（没有 diff）。行号是文件内真实行号；只对你能在文件片段中
确证的问题提评论，不要假设片段之外还有隐藏逻辑。
`

// systemWithRules 拼接系统提示词与适用规则文档。
func systemWithRules(docs []ruleDoc, mode string) string {
	var b strings.Builder
	b.WriteString(reviewSystemPrompt)
	if mode == "scan" {
		b.WriteString(scanSystemExtra)
	}
	for _, d := range docs {
		b.WriteString("\n## 适用规则：" + d.Name + "\n")
		b.WriteString(strings.TrimSpace(d.Content))
		b.WriteString("\n")
	}
	return b.String()
}

// buildDiffPrompt 生成 diff 评审的用户提示词。
func buildDiffPrompt(files []*fileDiff, docs []ruleDoc) (system, user string) {
	var b strings.Builder
	fmt.Fprintf(&b, "请评审以下 %d 个文件的改动。\n", len(files))
	b.WriteString("每行前缀格式为「行号|标记」：`+` 是新增行，`-` 是被删除的旧行，空格是上下文；")
	b.WriteString("行号是各自文件版本的真实行号。评论的 start_line/end_line 用新文件行号：")
	b.WriteString("`+` 行直接用本行号；若问题出在被删除的行（`-`），用紧随其后那一行（块尾用前一行）的新行号。\n")
	b.WriteString("path 必须逐字使用下面标题里的相对路径。\n\n")

	for _, f := range files {
		fmt.Fprintf(&b, "### %s（规则：%s）\n", f.path(), ruleNameForPath(f.path()))
		b.WriteString(renderAnnotatedDiff(f))
		b.WriteString("\n")
	}
	return systemWithRules(docs, "diff"), b.String()
}

// buildScanPrompt 生成整文件扫描的用户提示词。
func buildScanPrompt(path string, lines []string, start, end int, docs []ruleDoc) (system, user string) {
	var b strings.Builder
	fmt.Fprintf(&b, "请扫描文件 %s 的第 %d-%d 行（完整文件片段，行号已标注）。\n", path, start, end)
	b.WriteString("评论的 start_line/end_line 必须落在该区间内，path 必须逐字使用上面的路径。\n\n")
	for i, line := range lines {
		fmt.Fprintf(&b, "%5d|%s\n", start+i, line)
	}
	return systemWithRules(docs, "scan"), b.String()
}

// renderAnnotatedDiff 把 hunk 渲染为「行号|标记内容」的形式。
func renderAnnotatedDiff(f *fileDiff) string {
	var b strings.Builder
	for _, h := range f.Hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@ %s\n", h.OldStart, h.OldLines, h.NewStart, h.NewLines, h.Section)
		for _, l := range h.Lines {
			switch l.Kind {
			case '+':
				fmt.Fprintf(&b, "%5d|+%s\n", l.NewNo, l.Text)
			case '-':
				fmt.Fprintf(&b, "%5d|-%s\n", l.OldNo, l.Text)
			default:
				fmt.Fprintf(&b, "%5d| %s\n", l.NewNo, l.Text)
			}
		}
	}
	return b.String()
}
