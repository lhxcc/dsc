package tui

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"dsc/core"
)

// TestThinkingLineShowsElapsedHintAndDownstream 工作行显示动画文字、耗时、取消快捷键与下行数据。
// 文案随本轮随机选中的动画语种变化，故断言「两者之一 + 尾部提示」，不锁死某一语种。
func TestThinkingLineShowsElapsedHintAndDownstream(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.thinking = true
	m.armAnim() // 工作行只在时钟推进时出现（见 workingLineVisible）
	m.elapsed = 5
	m.turnTokens = 2048

	// 动画文字随帧变化（第 0 帧中文全是占位下划线），故固定到已揭示若干字的帧
	m.thinkAnim = thinkAnim{kind: thinkAnimZh, frame: 4 * zhFramesPerStep}
	content := ansi.Strip(m.View().Content)
	if !strings.Contains(content, "(5 秒 · Ctrl+C 取消)") {
		t.Fatalf("工作行应含耗时与取消提示: %q", content)
	}
	if !strings.Contains(content, "深度求索") {
		t.Fatalf("中文动画应显示已揭示的文字: %q", content)
	}

	// 英文语种走同一行：全大写帧应出现整句
	m.thinkAnim = thinkAnim{kind: thinkAnimEn, frame: enWaveFrames}
	content = ansi.Strip(m.View().Content)
	if !strings.Contains(content, "DEEP SEEKING..") {
		t.Fatalf("英文动画应显示整句: %q", content)
	}
	if !strings.Contains(content, "↓2K") {
		t.Fatalf("工作行应含下行数据: %q", content)
	}
}

// TestThinkingLineWithoutDownstream 尚无下行数据时不显示 ↓。
func TestThinkingLineWithoutDownstream(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.thinking = true
	m.armAnim()

	content := ansi.Strip(m.View().Content)
	if strings.Contains(content, "↓") {
		t.Fatalf("无下行数据时不应显示 ↓: %q", content)
	}
}

// TestElapsedTickIncrements 运行中 elapsedTick 按 runStart 刷新耗时并继续 tick。
func TestElapsedTickIncrements(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.thinking = true
	m.armAnim()
	m.runStart = time.Now().Add(-3 * time.Second)

	model, cmd := m.Update(elapsedTickMsg{})
	m2 := model.(*Model)
	if m2.elapsed < 3 {
		t.Fatalf("elapsed = %d, want >= 3", m2.elapsed)
	}
	if cmd == nil {
		t.Fatal("运行中 elapsedTick 应继续返回下一个 tick")
	}
}

// TestRunInfoLineCacheRate 服务端报告缓存字段时显示命中率（两位小数精度）；否则不显示。
func TestRunInfoLineCacheRate(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// 无缓存字段：不显示
	if strings.Contains(m.runInfoLine(), "缓存命中") {
		t.Fatalf("无缓存数据时不应显示缓存命中: %q", m.runInfoLine())
	}

	// 有缓存字段：显示命中率（两位小数）
	m.cacheHit = 90
	m.cacheMiss = 10
	if !strings.Contains(m.runInfoLine(), "缓存命中 90.00%") {
		t.Fatalf("runInfoLine 应显示缓存命中 90.00%%: %q", m.runInfoLine())
	}

	// 非整值：909/1000 → 90.90%
	m.cacheHit = 909
	m.cacheMiss = 91
	if !strings.Contains(m.runInfoLine(), "缓存命中 90.90%") {
		t.Fatalf("runInfoLine 应显示缓存命中 90.90%%: %q", m.runInfoLine())
	}
}

// TestRunInfoLineUsedPercent 已用容量百分比两位小数精度（小于 0.01% 时保底显示）。
func TestRunInfoLineUsedPercent(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// 12.5%：两位小数精确显示（131072 容量、16384 已用）
	m.usedTokens = 16384
	if !strings.Contains(m.runInfoLine(), "已用 12.50%") {
		t.Fatalf("runInfoLine 应显示已用 12.50%%: %q", m.runInfoLine())
	}

	// 极小占比：保底 0.01%
	m.usedTokens = 1
	if !strings.Contains(m.runInfoLine(), "已用 0.01%") {
		t.Fatalf("极小占比应保底显示 0.01%%: %q", m.runInfoLine())
	}
}

// TestStepStartFrameAnchorsTTFT step_start 帧：以请求发出时刻打点（晚于打点时
// 到达的内容帧不再重置），随后内容帧测得 TTFT、结算帧算出每秒/初速。
func TestStepStartFrameAnchorsTTFT(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// 步开始帧（Turn=1, Step=1）：打点请求发出时刻
	m.Update(streamFrame{frame: &core.RunStreamResponse{Status: "step_start", Turn: 1, Step: 1}})
	if m.stepStart.IsZero() || !m.firstTokenAt.IsZero() {
		t.Fatalf("step_start 帧应打点 stepStart 并重置 firstTokenAt")
	}
	anchor := m.stepStart

	// 同编号内容帧：不得触发「编号变化」误重置（打点保持 step_start 时刻）
	m.Update(streamFrame{frame: &core.RunStreamResponse{Status: "reasoning", Reasoning: "思考", Turn: 1, Step: 1}})
	if !m.stepStart.Equal(anchor) {
		t.Fatal("内容帧不应重置 stepStart（step_start 帧已同步 lastSeen 编号）")
	}
	if m.firstTokenAt.IsZero() {
		t.Fatal("内容帧应打点 firstTokenAt")
	}

	// 模拟真实时序：帧驱动打点为墙钟时刻，测试内人为拉开窗口跨过
	// minReliableMetricWindow 可信地板（250ms），否则结算会被突发防护跳过
	m.stepStart = time.Now().Add(-2 * time.Second)
	m.firstTokenAt = time.Now().Add(-500 * time.Millisecond)

	// 结算帧（success 带 Usage）：算出每秒（纯解码）与初速（含首响等待）
	m.Update(streamFrame{frame: &core.RunStreamResponse{
		Status: "success", Turn: 1, Step: 1,
		Usage: &core.Usage{PromptTokens: 100, CompletionTokens: 500},
	}})
	if m.decodeTPS <= 0 || m.startTPS <= 0 {
		t.Fatalf("success 帧应结算速率: decode=%v start=%v", m.decodeTPS, m.startTPS)
	}
	if m.startTPS >= m.decodeTPS {
		t.Fatalf("初速（含首响等待）应低于每秒（纯解码）: start=%v decode=%v", m.startTPS, m.decodeTPS)
	}
}

// TestTrackTurnUsage 累计下行生成 token；缓存命中率两侧按最近一次请求覆盖。
//
// 命中率的分母口径：miss = PromptTokens - CacheReadInputTokens（PromptTokens 按契约
// 已含缓存部分，见 core.PromptSideTokens）。不再拿 CacheCreationInputTokens 当 miss——
// 那是「写入缓存」的原义，OpenAI 兼容端点为 0，会让命中率恒显示 100%。
func TestTrackTurnUsage(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)

	m.trackTurnUsage(&core.Usage{PromptTokens: 1000, CompletionTokens: 100, CacheReadInputTokens: 800}, 1, 0)
	if m.turnTokens != 100 || m.cacheHit != 800 || m.cacheMiss != 200 {
		t.Fatalf("trackTurnUsage 后: turn=%d hit=%d miss=%d（期望 100/800/200）",
			m.turnTokens, m.cacheHit, m.cacheMiss)
	}

	// 第二步：下行累计，缓存覆盖
	m.trackTurnUsage(&core.Usage{PromptTokens: 2000, CompletionTokens: 50, CacheReadInputTokens: 1900}, 2, 0)
	if m.turnTokens != 150 {
		t.Fatalf("turnTokens 应累计为 150, got %d", m.turnTokens)
	}
	if m.cacheHit != 1900 || m.cacheMiss != 100 {
		t.Fatalf("cache 应覆盖为 hit=1900 miss=100, got hit=%d miss=%d", m.cacheHit, m.cacheMiss)
	}
}

// TestRunInfoLineTurnStepFormat 指标行轮步段采用「N 轮 M 步」紧凑格式（对齐 DSH
// 「N turns M steps」）；无速率数据时不出现每秒/初速段。
func TestRunInfoLineTurnStepFormat(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.curTurn = 4
	m.curStep = 12

	line := ansi.Strip(m.runInfoLine())
	if !strings.Contains(line, "4 轮 12 步") {
		t.Fatalf("指标行应含「4 轮 12 步」: %q", line)
	}
	if strings.Contains(line, "每秒") || strings.Contains(line, "初速") {
		t.Fatalf("无速率数据时不应显示每秒/初速: %q", line)
	}
}

// TestRunInfoLineThroughput 有速率快照时显示「每秒 X 词元 · 初速 Y 词元」；
// 初速（含首 token 延迟）应低于每秒值（纯解码吞吐）。
func TestRunInfoLineThroughput(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	// 1160 词元：解码 10s → 每秒 116；全程 11.6s → 初速 100
	m.decodeTPS = 116
	m.startTPS = 100

	line := ansi.Strip(m.runInfoLine())
	if !strings.Contains(line, "每秒 116 词元") {
		t.Fatalf("指标行应含「每秒 116 词元」: %q", line)
	}
	if !strings.Contains(line, "初速 100 词元") {
		t.Fatalf("指标行应含「初速 100 词元」: %q", line)
	}
	if strings.Index(line, "初速") < strings.Index(line, "每秒") {
		t.Fatalf("初速应位于每秒之后: %q", line)
	}
}

// TestSettleStepMetrics 速率结算：TTFT 越大初速越低；同步多帧不重复结算；
// 无流式内容帧的步跳过保持上一次读数。
func TestSettleStepMetrics(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)

	// 无打点（步开始/首 token 缺失）：跳过
	m.settleStepMetrics(&core.Usage{CompletionTokens: 100})
	if m.decodeTPS != 0 || m.startTPS != 0 {
		t.Fatalf("无打点时不应结算: decode=%v start=%v", m.decodeTPS, m.startTPS)
	}

	// 正常结算：900 词元，首 token 在 9s 前（解码 9s → 每秒 100），步开始 10s 前（全程 10s → 初速 90）
	// time.Since 受调度抖动影响带微小浮点误差，用 0.01 容差断言。
	m.stepStart = time.Now().Add(-10 * time.Second)
	m.firstTokenAt = time.Now().Add(-9 * time.Second)
	m.settleStepMetrics(&core.Usage{CompletionTokens: 900})
	if math.Abs(m.decodeTPS-100) > 0.01 || math.Abs(m.startTPS-90) > 0.01 {
		t.Fatalf("结算后 decodeTPS=%v startTPS=%v, want ~100/~90", m.decodeTPS, m.startTPS)
	}

	// 同步的 tool 结果帧与 success 帧重复携带 Usage：不重复结算（读数不变）
	m.settleStepMetrics(&core.Usage{CompletionTokens: 99999})
	if math.Abs(m.decodeTPS-100) > 0.01 || math.Abs(m.startTPS-90) > 0.01 {
		t.Fatalf("重复结算应被忽略: decodeTPS=%v startTPS=%v", m.decodeTPS, m.startTPS)
	}

	// 新步开始（编号变化）：打点重置，无内容帧的步不结算，读数保持
	m.stepStart = time.Now()
	m.firstTokenAt = time.Time{}
	m.stepSettled = false
	m.settleStepMetrics(&core.Usage{CompletionTokens: 500})
	if math.Abs(m.decodeTPS-100) > 0.01 || math.Abs(m.startTPS-90) > 0.01 {
		t.Fatalf("无首 token 的步不应结算: decodeTPS=%v startTPS=%v", m.decodeTPS, m.startTPS)
	}
}

// TestSettleStepMetricsBurstGuard 回归「每秒 408814 词元」爆炸读数：流式帧整批
// 同刻到达 TUI 时，首个内容帧与结算帧的处理间隔仅数毫秒（实测 846 词元 ÷
// 2.07ms = 每秒 408814），解码窗口塌缩后直接相除得到物理不可能读数。
// 防护两级：
//   - 全程窗口 ≥ 地板但解码窗口 < 地板 → 两读数一并退化为全程口径（保守低估）；
//   - 全程窗口 < 地板（整步皆突发）→ 跳过结算保持上次读数。
func TestSettleStepMetricsBurstGuard(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "Agentic-Turbo-Coder", "minimal", 131072)

	// 预置一次正常读数（后续突发步应保持它）
	m.stepStart = time.Now().Add(-10 * time.Second)
	m.firstTokenAt = time.Now().Add(-9 * time.Second)
	m.settleStepMetrics(&core.Usage{CompletionTokens: 900})

	// 突发步：846 词元，内容帧与结算帧同批到达（解码窗口 2ms），但步开始
	// 帧 25s 前已到（全程窗口可信）→ 退化为全程口径：846/25 ≈ 33.8，
	// 每秒与初速一致，不再出现 40 万级的爆炸读数。
	m.stepSettled = false
	m.stepStart = time.Now().Add(-25 * time.Second)
	m.firstTokenAt = time.Now().Add(-2 * time.Millisecond)
	m.settleStepMetrics(&core.Usage{CompletionTokens: 846})
	if m.decodeTPS > 100 || m.startTPS > 100 {
		t.Fatalf("突发步应退化为全程口径: decodeTPS=%v startTPS=%v", m.decodeTPS, m.startTPS)
	}
	if math.Abs(m.decodeTPS-m.startTPS) > 0.5 {
		t.Fatalf("退化后两读数应一致: decodeTPS=%v startTPS=%v", m.decodeTPS, m.startTPS)
	}

	// 整步突发：步开始帧与结算帧也在同批（全程窗口 < 250ms）→ 跳过结算，
	// 保持上一次读数（后续帧到达时全程窗口自然增长，仍可正常结算）
	m.stepSettled = false
	prevDecode, prevStart := m.decodeTPS, m.startTPS
	m.stepStart = time.Now()
	m.firstTokenAt = time.Now().Add(-1 * time.Millisecond)
	m.settleStepMetrics(&core.Usage{CompletionTokens: 99999})
	if m.decodeTPS != prevDecode || m.startTPS != prevStart {
		t.Fatalf("整步突发应跳过结算: decodeTPS=%v startTPS=%v", m.decodeTPS, m.startTPS)
	}
	if m.stepSettled {
		t.Fatalf("跳过结算时不应标记 stepSettled（后续帧仍可结算）")
	}
}

// TestFormatTokensPerSecond 速率格式化：≥10 取整，<10 保留一位小数（对齐 DSH）。
func TestFormatTokensPerSecond(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{116.4, "116"},
		{9.96, "10"},
		{9.54, "9.5"},
		{0.5, "0.5"},
	}
	for _, c := range cases {
		if got := formatTokensPerSecond(c.in); got != c.want {
			t.Fatalf("formatTokensPerSecond(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNoteContextUsageUsesHostMeter TUI 的「已用」必须来自宿主计量实例（与决策同源），
// agent 上报了投影值（Usage.ContextTokens）就用它，没有才退回实测 prompt。
func TestNoteContextUsageUsesHostMeter(t *testing.T) {
	manager := core.NewManager(&core.ManagerConfig{})
	m := New(&stubAgent{}, manager, context.Background(), "Agentic-Turbo-Coder", "minimal", 1000000)

	// agent 上报投影值 512000（实测 prompt 498000）：已用取投影
	m.noteContextUsage(&core.Usage{PromptTokens: 498000, ContextTokens: 512000})
	if m.usedTokens != 512000 {
		t.Fatalf("已用应取宿主计量实例的投影值 512000，got %d", m.usedTokens)
	}
	if got := manager.ContextTokens(m.currentSessionID); got != 512000 {
		t.Fatalf("宿主实例应保管 512000，got %d", got)
	}

	// 旧 agent 不带上报值：退回实测 prompt
	m2 := New(&stubAgent{}, manager, context.Background(), "Agentic-Turbo-Coder", "minimal", 1000000)
	m2.currentSessionID = "other-session"
	m2.noteContextUsage(&core.Usage{PromptTokens: 1234})
	if m2.usedTokens != 1234 {
		t.Fatalf("无上报值时应退回实测 prompt 1234，got %d", m2.usedTokens)
	}
}

// TestNewSeedsUsedTokensFromHostMeter 启动即有读数：恢复有历史的会话时，宿主在
// tui.New 之前已把上一次请求的用量预置进计量实例（core.LoadHistoryReplay →
// seedSessionPromptFromLog），界面应在构造时就把它读进来。
//
// 此前 usedTokens 只在 noteContextUsage 里赋值，于是「已用」在用户发出第一个请求
// 之前一直显示 0——新会话显示 0 是对的，载入几十万 token 历史后仍是 0 就是在说假话。
func TestNewSeedsUsedTokensFromHostMeter(t *testing.T) {
	manager := core.NewManager(&core.ManagerConfig{})
	manager.SeedContextUsage(manager.DefaultSessionID(), 40347)

	m := New(&stubAgent{}, manager, context.Background(), "Agentic-Turbo-Coder", "minimal", 1048576)
	if m.usedTokens != 40347 {
		t.Fatalf("启动时应读到预置的已用 40347，got %d", m.usedTokens)
	}
	if !strings.Contains(m.capacityTag(), "39K") {
		t.Fatalf("容量标签应显示 39K/1.0M，got %q", m.capacityTag())
	}

	// 真·新会话：没有预置 → 仍是 0（前端据此显示「未知」而不是 0%）
	fresh := New(&stubAgent{}, core.NewManager(&core.ManagerConfig{}), context.Background(), "Agentic-Turbo-Coder", "minimal", 1048576)
	if fresh.usedTokens != 0 {
		t.Fatalf("无历史的会话应为 0，got %d", fresh.usedTokens)
	}
}
