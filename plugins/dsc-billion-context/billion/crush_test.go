package billion

import (
	"encoding/json"
	"strings"
	"testing"
)

// crushConfig 造一个「上下文已满、absorb+crush 全开」的配置，方便直接命中门控。
func crushConfig() Config {
	cfg := DefaultConfig(10_000)
	cfg.Absorb.Enabled = true
	cfg.Absorb.MinToolTokens = 500
	cfg.Absorb.ContextThresholdPct = 0.5
	cfg.Crush.Enabled = true
	cfg.Crush.MinReduction = 0.1
	return cfg
}

// toolMsg 造一条「工具结果」消息：IsAbsorbCandidate 要求 ContentType 为 tool-result
// 且带 ToolCallID（配对完整性），缺一项就会被门控直接挡掉。
func toolMsg(id, tool, text string) CoreMessage {
	return CoreMessage{
		ID: id, Role: RoleTool, ToolName: tool, Text: text,
		ContentType: ContentTypeToolResult, ToolCallID: "call-" + id,
	}
}

// TestCrushDisabledByDefault 默认关闭：不开开关时字节级不变（内核的 same-bytes 承诺）。
func TestCrushDisabledByDefault(t *testing.T) {
	cfg := DefaultConfig(10_000)
	if cfg.Crush.Enabled {
		t.Fatal("crush 应默认关闭")
	}
	body := strings.Repeat(`{"a":1,"b":2}`+"\n", 200)
	msgs := []CoreMessage{toolMsg("t1", "shell", body)}
	out, stats := ApplyCrush(msgs, CreateInitialState(), cfg, 9_000)
	if stats.Crushed+stats.Distilled != 0 || out[0].Text != body {
		t.Fatal("关闭时不得改动任何字节")
	}
}

// TestCrushJSONFoldIsStructural 对象数组被因子化：相同字段提升、相同行折叠，
// 且结果仍是合法 JSON（模型可直接解析/判断）。
func TestCrushJSONFoldIsStructural(t *testing.T) {
	// 60 行、每行字段相同（status=ok / level=INFO），只有 msg 的少量取值
	var rows []string
	for i := 0; i < 60; i++ {
		rows = append(rows, `{"status":"ok","level":"INFO","i":1,"msg":"retrying"}`)
	}
	body := "[\n" + strings.Join(rows, ",\n") + "\n]"
	cfg := crushConfig()
	eval := EvaluateCrush(toolMsg("t1", "shell", body), CreateInitialState(), cfg, 9_000)
	if eval.Kind != CrushCrushed {
		t.Fatalf("60 行完全相同应压到阈值下, kind=%s strategy=%s (%d→%d)",
			eval.Kind, eval.Strategy, eval.Before, eval.After)
	}
	if eval.Strategy != strategyJSONFold {
		t.Fatalf("策略 = %s, want %s", eval.Strategy, strategyJSONFold)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(eval.Text), &v); err != nil {
		t.Fatalf("压缩结果必须仍是合法 JSON: %v\n%s", err, eval.Text)
	}
	if v["__billion_crush"] != "rows" {
		t.Fatalf("应带 __billion_crush 信封: %v", v["__billion_crush"])
	}
	// 相同行必须被折叠成一条带计数的信封（60 行 => 1 条 count=60）
	out := eval.Text
	if !strings.Contains(out, `"count":60`) {
		t.Fatalf("相同行应折叠为 count=60: %s", out)
	}
}

// TestCrushJSONFoldFailsOpen 畸形 JSON 必须原样放行（fail open）。
func TestCrushJSONFoldFailsOpen(t *testing.T) {
	cfg := crushConfig()
	for _, body := range []string{
		`[{"a":1}, {"a":2`,      // 截断
		strings.Repeat("[", 40), // 超深/畸形
		"[1,2,3]",               // 非对象数组
		"not json at all " + strings.Repeat("x", 900),
	} {
		eval := EvaluateCrush(toolMsg("t1", "shell", body), CreateInitialState(), cfg, 9_000)
		if eval.Kind != CrushSkip || eval.Text != body {
			t.Fatalf("畸形输入应原样放行: kind=%s body=%q", eval.Kind, body[:minOf(20, len(body))])
		}
	}
}

// TestCrushLogKeepsEveryErrorLine 日志压缩的硬不变量：所有 ERROR/FAIL 行原样保留。
func TestCrushLogKeepsEveryErrorLine(t *testing.T) {
	var lines []string
	for i := 0; i < 400; i++ {
		switch {
		case i%37 == 0:
			lines = append(lines, "2026-10-25T10:00:00Z ERROR failed to connect to db-"+itoa(i))
		case i%11 == 0:
			lines = append(lines, "2026-10-25T10:00:00Z WARN retrying request")
		default:
			lines = append(lines, "2026-10-25T10:00:00Z INFO processed item "+itoa(i))
		}
	}
	body := strings.Join(lines, "\n")
	cfg := crushConfig()
	out, stats := ApplyCrush([]CoreMessage{toolMsg("t1", "shell", body)},
		CreateInitialState(), cfg, 9_000)
	if stats.Crushed+stats.Distilled == 0 {
		t.Fatal("长日志应被压缩")
	}
	// 每条不同的 ERROR 行都必须逐字可见
	for _, l := range lines {
		if strings.Contains(l, "ERROR") && !strings.Contains(out[0].Text, l) {
			t.Fatalf("ERROR 行被吞掉: %q", l)
		}
	}
	// 落线文本必须带策略标记（下一轮据此跳过，不再二次压）
	if !strings.Contains(out[0].Text, crushMarkerPrefix+strategyLogSel) {
		t.Fatalf("应带策略标记: %q", out[0].Text[:minOf(80, len(out[0].Text))])
	}
	// 且确实变小了
	if estimateTokensForText(out[0].Text) >= estimateTokensForText(body) {
		t.Fatal("压缩后应更小")
	}
}

// TestCrushCodePreservesStrings code-trim 不得吃掉字符串里的 //（URL、正则）。
func TestCrushCodePreservesStrings(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 60; i++ {
		b.WriteString("// 注释行，应被剥离\n")
		b.WriteString(`url := "https://example.com/path"` + "\n")
		b.WriteString("x := 1\n")
	}
	body := b.String()
	cfg := crushConfig()
	cfg.Absorb.MinToolTokens = 100
	eval := EvaluateCrush(toolMsg("t1", "shell", body), CreateInitialState(), cfg, 9_000)
	if eval.Kind == CrushSkip {
		t.Fatalf("注释占比高时应被压缩（before=%d）", eval.Before)
	}
	if !strings.Contains(eval.Text, `"https://example.com/path"`) {
		t.Fatalf("字符串里的 // 不得当作注释剥离: %s", eval.Text[:minOf(200, len(eval.Text))])
	}
	if strings.Contains(eval.Text, "注释行，应被剥离") {
		t.Fatal("注释行应被剥离")
	}
}

// TestCrushRespectsExcludedTools 排除表生效（支持通配）。
func TestCrushRespectsExcludedTools(t *testing.T) {
	cfg := crushConfig()
	cfg.Crush.ExcludeTools = []string{"browser_*"}
	body := strings.Repeat(`{"a":1}`+"\n", 400)
	eval := EvaluateCrush(toolMsg("t1", "browser_shot", body), CreateInitialState(), cfg, 9_000)
	if eval.Kind != CrushSkip || eval.Text != body {
		t.Fatal("排除表命中的工具不应被压缩")
	}
}

// TestCrushIdempotent crush 过的文本不会被二次压（幂等，防止每轮啃一遍）。
func TestCrushIdempotent(t *testing.T) {
	cfg := crushConfig()
	body := "[\n" + strings.Join(func() []string {
		out := make([]string, 60)
		for i := range out {
			out[i] = `{"status":"ok","level":"INFO","i":1,"msg":"retrying the connection now"}`
		}
		return out
	}(), ",\n") + "\n]"
	state := CreateInitialState()
	msgs := []CoreMessage{toolMsg("t1", "shell", body)}
	out1, stats := ApplyCrush(msgs, state, cfg, 9_000)
	if stats.Crushed != 1 {
		t.Fatalf("首次应压缩 1 条, got %d", stats.Crushed)
	}
	out2, stats2 := ApplyCrush(out1, state, cfg, 9_000)
	if stats2.Crushed+stats2.Distilled != 0 || out2[0].Text != out1[0].Text {
		t.Fatal("已压缩过的文本不应被再次压缩")
	}
}

// TestCrushNeedsPressure 上下文宽裕时不做压缩（省下的 token 没价值，只损失保真）。
func TestCrushNeedsPressure(t *testing.T) {
	cfg := crushConfig()
	body := "[\n" + strings.Repeat(`{"a":1},`+"\n", 200) + `{"a":1}]`
	// tokenCount 低于 ContextThresholdPct(0.5)×10000 = 5000
	eval := EvaluateCrush(toolMsg("t1", "shell", body), CreateInitialState(), cfg, 1_000)
	if eval.Kind != CrushSkip {
		t.Fatalf("上下文宽裕时不应压缩, kind=%s", eval.Kind)
	}
}

// minOf 取较小值（测试辅助）。
func minOf(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestStatusReportsSavings 状态报告要说清「省了多少」：分层块数 + crush/CCR 明细。
// 这些过程用户看不见，只有这组累计数字能回答"压缩到底有没有用"。
func TestStatusReportsSavings(t *testing.T) {
	cfg := DefaultConfig(1_000_000)
	state := CreateInitialState()
	state.Blocks = []CompressionBlock{
		{BlockID: "b0", Tier: Tier1, Active: true, CompressedTokens: 40_000},
		{BlockID: "b1", Tier: Tier1, Active: true, CompressedTokens: 30_000},
		{BlockID: "b2", Tier: Tier2, Active: true, CompressedTokens: 70_000},
		{BlockID: "b3", Tier: Tier1, Active: false, CompressedTokens: 10_000},
	}
	state.Stats.TokensCompressed = 140_000
	state.Stats.CompressionCount = 3
	state.Stats.CrushCount = 2
	state.Stats.CrushTokensSaved = 8_000
	state.Stats.CCRStored = 5
	state.Stats.CCRStoredTokens = 60_000
	state.Stats.CCRRetrieved = 4
	state.Stats.CCRExported = 1

	r := BuildStatus(nil, state, cfg, 300_000)
	if r.ActiveBlocks != 3 || r.TotalBlocks != 4 {
		t.Fatalf("块统计不对: active=%d total=%d", r.ActiveBlocks, r.TotalBlocks)
	}
	if r.Breakdown["tier1"] != 2 || r.Breakdown["tier2"] != 1 || r.Breakdown["tier3"] != 0 {
		t.Fatalf("分层统计应只数 active: %v", r.Breakdown)
	}
	if r.Breakdown["crushTokensSaved"] != 8_000 || r.Breakdown["ccrStored"] != 5 ||
		r.Breakdown["ccrRetrieved"] != 4 || r.Breakdown["ccrExported"] != 1 {
		t.Fatalf("节省明细丢失: %v", r.Breakdown)
	}
	if r.TokenCount != 300_000 {
		t.Fatalf("占用应取传入的真实值: %d", r.TokenCount)
	}

	text := FormatStatus(r)
	for _, want := range []string{"T1=2 T2=1 T3=0", "Crush: 2 deterministic compactions, 8000 tokens saved",
		"Stored: 5 payloads (60000 tokens)", "Retrievals: 4 (1 exported to files)"} {
		if !strings.Contains(text, want) {
			t.Fatalf("状态文本应包含 %q，实际：\n%s", want, text)
		}
	}
}
