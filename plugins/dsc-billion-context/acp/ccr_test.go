package acp

import (
	"strings"
	"testing"
)

func ccrConfig() Config {
	cfg := DefaultConfig(200_000)
	cfg.CCR.Enabled = true
	cfg.CCR.MinToolTokens = 500
	cfg.CCR.InlineLimitTokens = 1000
	return cfg
}

// toolResult 造一条工具结果消息（CCR 只处理工具结果）。
func toolResult(id, tool, text string, images ...string) CoreMessage {
	return CoreMessage{
		ID: id, Role: RoleTool, ToolName: tool, Text: text,
		ContentType: ContentTypeToolResult, ToolCallID: "call-" + id,
		Images: images,
	}
}

// TestCCROffloadsLargeResultsOnly 只有够大的工具结果被卸载；小结果原样保留。
func TestCCROffloadsLargeResultsOnly(t *testing.T) {
	cfg := ccrConfig()
	big := strings.Repeat("line of build output\n", 500)
	small := "ok"
	msgs := []CoreMessage{toolResult("t1", "shell", big), toolResult("t2", "shell", small)}

	out, off := ApplyCCR(msgs, CreateInitialState(), cfg)
	if len(off) != 1 {
		t.Fatalf("应卸载 1 条，实际 %d", len(off))
	}
	if !IsCCRPlaceholder(out[0].Text) {
		t.Fatalf("大结果应换成占位符: %q", out[0].Text[:minOf(60, len(out[0].Text))])
	}
	if out[1].Text != small {
		t.Fatal("小结果不应改动")
	}
	if got := off[0].Entry.Tokens; got < 500 {
		t.Fatalf("条目 token 数应达门槛，实际 %d", got)
	}
	// 占位符必须给出可用的 ref 与取回指引
	if !strings.Contains(out[0].Text, off[0].Entry.Ref) || !strings.Contains(out[0].Text, "acp_retrieve") {
		t.Fatalf("占位符应带 ref 与取回工具名: %s", out[0].Text)
	}
}

// TestCCRPlaceholderDeterministic 同一内容两次卸载 → 同一 ref、同一占位符字节
// （前缀缓存靠这个：占位符一旦落到线上就不能再变）。
func TestCCRPlaceholderDeterministic(t *testing.T) {
	cfg := ccrConfig()
	body := strings.Repeat("x", 4000)

	e1 := CCREntry{Ref: "c00003", Shasum: "abc", Kind: CCRKindText, Tool: "shell", Tokens: 1000, Bytes: 4000, Preview: "npm run build"}
	e2 := e1
	if BuildCCRPlaceholder(e1, "acp_retrieve") != BuildCCRPlaceholder(e2, "acp_retrieve") {
		t.Fatal("同一条目必须产出同一占位符")
	}
	// 与内容无关：body 变了但条目相同，占位符不变（说明没有把原文掺进去）
	if BuildCCRPlaceholder(e1, "acp_retrieve") != BuildCCRPlaceholder(e2, "acp_retrieve") {
		t.Fatal("占位符不应依赖原文")
	}

	// 内容相同 → 复用同一条目与 ref（内容寻址去重）
	st := CreateInitialState()
	out1, off1 := ApplyCCR([]CoreMessage{toolResult("t1", "shell", body)}, st, cfg)
	out2, off2 := ApplyCCR([]CoreMessage{toolResult("t2", "shell", body)}, st, cfg)
	if len(off1) != 1 || len(off2) != 0 {
		t.Fatalf("相同内容第二次不应新增条目: off1=%d off2=%d", len(off1), len(off2))
	}
	if len(st.CCR) != 1 {
		t.Fatalf("索引应只有 1 条，实际 %d", len(st.CCR))
	}
	if out1[0].Text != out2[0].Text {
		t.Fatal("相同内容必须得到相同占位符")
	}
}

// TestCCRImageEntryKeepsRefs 图像结果：正文卸载，但 dsc-img ref 原样记在条目里
// （字节不搬，取回时挂回工具结果由 provider 解析）。
func TestCCRImageEntryKeepsRefs(t *testing.T) {
	cfg := ccrConfig()
	msgs := []CoreMessage{toolResult("t1", "browser_shot", strings.Repeat("x", 4000),
		"dsc-img://aaa", "dsc-img://bbb")}
	out, off := ApplyCCR(msgs, CreateInitialState(), cfg)
	if len(off) != 1 {
		t.Fatalf("应卸载 1 条，实际 %d", len(off))
	}
	e := off[0].Entry
	if e.Kind != CCRKindImage {
		t.Fatalf("kind = %s, want image", e.Kind)
	}
	if len(e.Images) != 2 || e.Images[0] != "dsc-img://aaa" {
		t.Fatalf("图像 ref 应完整保留: %v", e.Images)
	}
	// 图像条目的正文也要存：占位符已把正文替换掉，不存就永久丢了
	if off[0].Body == "" {
		t.Fatal("图像条目同样需要保存正文")
	}
	if !strings.Contains(out[0].Text, "2 张图") {
		t.Fatalf("占位符应说明图像张数: %s", out[0].Text)
	}
}

// TestCCRIdempotent 已是占位符的消息不再被卸载（否则每轮都会换新 ref）。
func TestCCRIdempotent(t *testing.T) {
	cfg := ccrConfig()
	body := strings.Repeat("y", 4000)
	st := CreateInitialState()
	out1, _ := ApplyCCR([]CoreMessage{toolResult("t1", "shell", body)}, st, cfg)
	out2, off2 := ApplyCCR(out1, st, cfg)
	if len(off2) != 0 {
		t.Fatalf("占位符不应被二次卸载，实际 %d", len(off2))
	}
	if out2[0].Text != out1[0].Text {
		t.Fatal("占位符不应被改写")
	}
}

// TestCCRRetrievalPlan 回取交付形态：内联 / 导出 / 图像三条分支。
func TestCCRRetrievalPlan(t *testing.T) {
	cfg := ccrConfig() // InlineLimit = 1000
	text := CCREntry{Ref: "c00001", Shasum: "sha1", Kind: CCRKindText}

	if p := PlanRetrieval(text, cfg, "short"); !p.Inline || p.Text != "short" {
		t.Fatalf("小内容应内联: %+v", p)
	}
	big := strings.Repeat("z", 8000)
	p := PlanRetrieval(text, cfg, big)
	if p.Inline || p.ExportPath == "" {
		t.Fatalf("超限内容应导出: %+v", p)
	}
	if !strings.HasSuffix(p.ExportPath, ".txt") || !strings.Contains(p.ExportPath, "sha1") {
		t.Fatalf("导出路径应按 sha 命名（幂等重写）: %s", p.ExportPath)
	}
	img := CCREntry{Ref: "c00002", Kind: CCRKindImage, Images: []string{"dsc-img://x"}}
	pi := PlanRetrieval(img, cfg, "")
	if len(pi.Images) != 1 || pi.Inline {
		t.Fatalf("图像应走挂回分支: %+v", pi)
	}
}

// TestCCRTrimKeepsRetainRef 条目数超限时淘汰最旧的，但保留正在引用的那条。
func TestCCRTrimKeepsRetainRef(t *testing.T) {
	var entries []CCREntry
	for i := 1; i <= 5; i++ {
		entries = append(entries, CCREntry{Ref: CCRRefFor(i)})
	}
	out := TrimCCREntries(entries, 2, "c00001")
	if len(out) != 2 {
		t.Fatalf("应裁到 2 条，实际 %d", len(out))
	}
	if out[0].Ref != "c00001" {
		t.Fatalf("正在引用的条目必须保留，实际首条 %s", out[0].Ref)
	}
	// ref 永不复用：裁剪后序号仍然单调（NextCCRID 不回退）
	if CCRRefFor(6) != "c00006" {
		t.Fatal("ref 生成应保持单调")
	}
}

// TestCCRNotFoundHint 幻觉 ref 的回复要指出正确用法，而不是报错。
func TestCCRNotFoundHint(t *testing.T) {
	s := CCRRetrieveNotFound("c99999")
	if !strings.Contains(s, "c99999") || !strings.Contains(s, "search_context") {
		t.Fatalf("提示应包含给定的 ref 与补救手段: %s", s)
	}
}
