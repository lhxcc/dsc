package main

import (
	"strings"
	"testing"

	"dsc/proto"
)

// 现场：模型把换行写成 JSON 里的 `\\n`，解码后是字面量 `\n\n`，提问卡片上原样显示。
func TestNormalizeModelTextExpandsStrayEscapes(t *testing.T) {
	in := `日志已定位到：\n\n主线程在 COMMIT-LAUNCHED 之后立刻 join；\n第二段。\n\t缩进行`
	got := normalizeModelText(in)
	if strings.Contains(got, `\n`) || strings.Contains(got, `\t`) {
		t.Fatalf("字面量转义没被展开: %q", got)
	}
	if !strings.Contains(got, "\n\n主线程") {
		t.Fatalf("空行没恢复: %q", got)
	}
	if !strings.Contains(got, "\t缩进行") {
		t.Fatalf("制表符没恢复: %q", got)
	}
}

func TestNormalizeModelTextLeavesNormalTextAlone(t *testing.T) {
	// 有真换行的文本一律不动（模型会写换行，剩下的反斜杠按有意处理）
	withReal := "第一段\n第二段，提到正则 `\\n` 的写法"
	if got := normalizeModelText(withReal); got != withReal {
		t.Fatalf("含真换行的文本被改了: %q", got)
	}
	// 纯文本无转义：一字不动
	plain := "短问题，没有换行"
	if got := normalizeModelText(plain); got != plain {
		t.Fatalf("普通文本被改了: %q", got)
	}
	// 空串
	if got := normalizeModelText(""); got != "" {
		t.Fatalf("空串变成 %q", got)
	}
}

// 没写换行的路径必须原样保留：`C:\new` 不能被当成换行吃掉。
func TestNormalizeModelTextKeepsBackslashPaths(t *testing.T) {
	// 注意：这种文本里若不出现字面量 `\n` 序列就不会进展开分支；
	// 但一旦同时出现（如列表里提到 \n），展开是整体行为，故这里只钉住「没有 \n 序列」的情形。
	in := `打开 C:\temp\new 目录`
	if got := normalizeModelText(in); got != in {
		t.Fatalf("路径被改了: %q", got)
	}
}

func TestNormalizeQuestionCoversAllProseFields(t *testing.T) {
	q := &proto.AskQuestion{
		Header:   `修复方向`,
		Question: `第一段\n\n第二段`,
		Detail:   `补充\n说明`,
		Options: []*proto.AskOption{
			{Label: `方案一`, Description: `小改动\n可验证`},
			nil, // 防御：畸形选项不能 panic
		},
	}
	normalizeQuestion(q)
	if q.Question != "第一段\n\n第二段" {
		t.Fatalf("question = %q", q.Question)
	}
	if q.Detail != "补充\n说明" {
		t.Fatalf("detail = %q", q.Detail)
	}
	if q.Options[0].Description != "小改动\n可验证" {
		t.Fatalf("option description = %q", q.Options[0].Description)
	}
	if q.Header != `修复方向` || q.Options[0].Label != `方案一` {
		t.Fatalf("无转义字段被改了: header=%q label=%q", q.Header, q.Options[0].Label)
	}
	normalizeQuestion(nil) // 不能 panic
}
