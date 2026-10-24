package core

import (
	"context"
	"strings"
	"testing"

	"dsc/proto"
)

// TestTokenMeterUpdateUsage 校验：UpdateUsage 正确存储用量并计算提示缓存感知的总量。
func TestTokenMeterUpdateUsage(t *testing.T) {
	tm := NewTokenMeter(10000)

	// 无 usage 时应回退到 estimate
	msgs := []*Message{{Role: "user", Content: "hello world"}}
	m := tm.Measure(msgs)
	if m.Source != "estimate" {
		t.Errorf("无 usage 时 Source 应为 estimate，got %q", m.Source)
	}
	if m.TotalTokens <= 0 {
		t.Error("estimate 应返回 > 0 tokens")
	}

	// 更新 usage
	tm.UpdateUsage(&Usage{
		PromptTokens:         5000,
		CompletionTokens:     200,
		CacheReadInputTokens: 3000, // 缓存命中 3000 tokens
	})

	m = tm.Measure(msgs)
	if m.Source != "usage" {
		t.Errorf("有 usage 时 Source 应为 usage，got %q", m.Source)
	}
	// 口径契约：PromptTokens 已是完整 prompt（含命中缓存的部分），不再加 cache_read
	// 总量 = prompt(5000) + completion(200) = 5200
	if m.TotalTokens != 5200 {
		t.Errorf("TotalTokens 应为 5200（prompt+completion），got %d", m.TotalTokens)
	}
	if m.Pressure < 0.51 || m.Pressure > 0.53 {
		t.Errorf("Pressure 应约 0.52，got %.4f", m.Pressure)
	}
}

// TestTokenMeterShouldCompact 校验：压力超过阈值时 ShouldCompact 返回 true。
func TestTokenMeterShouldCompact(t *testing.T) {
	tm := NewTokenMeter(10000)

	// 无锚点：退回纯估算。估算 4000 → 40%，不触发。
	if tm.ShouldCompact(4000, 0.80) {
		t.Error("40% 不应压缩")
	}
	if !tm.ShouldCompact(9000, 0.80) {
		t.Error("90% 应压缩")
	}

	// 有锚点：实测 prompt 侧 8000 → 80%
	tm.SampleUsage(8000, 8000)
	if !tm.ShouldCompact(8000, 0.80) {
		t.Error("压力 80% 应触发压缩")
	}
	if tm.ShouldCompact(8000, 0.90) {
		t.Error("压力 80% 不应触发 90% 阈值")
	}
	// 锚点 + 增量：估算涨到 9500 表示本轮又多了 1500 → 95%
	if !tm.ShouldCompact(9500, 0.90) {
		t.Error("实测 8000 + 增量 1500 = 95% 应触发 90% 阈值")
	}
}

// TestTokenMeterEstimateTextTokens 校验文本估算：按字符分类累加——宽字符 1 token/字，
// 其余 4 字节/token。
//
// 回归背景：旧实现取「整串字节/4」与「整串 rune 数」的较大者，纯 ASCII 时 runes==bytes，
// 于是按 1 token/字符计（真实约 0.25），代码类会话被高估 4~5 倍，进而触发毫无必要的
// 上下文裁剪/压缩。此处的期望值就是按正确口径重算的：
func TestTokenMeterEstimateTextTokens(t *testing.T) {
	cases := []struct {
		in   string
		want int
		why  string
	}{
		{"hello", 2, "5 字节英文 → ceil(5/4)"},
		{"你好", 2, "两个 CJK 字 → 2"},
		{"hi你好", 3, "ceil(2/4)=1 + 2 CJK 字"},
		{"", 0, "空串"},
		{"if (x < 10) { y = z; }", 6, "18 字节代码 → ceil(18/4)，不得按字符数算成 19"},
		{"全角（），。", 6, "全角标点按 1 token/字"},
		{"abc你好def", 4, "窄字节 3+3 合并后 ceil(6/4)=2 + 2 CJK 字（宽窄边界不另计）"},
	}
	for _, c := range cases {
		if got := EstimateTextTokens(c.in); got != c.want {
			t.Errorf("EstimateTextTokens(%q) = %d, want %d（%s）", c.in, got, c.want, c.why)
		}
	}
	// 纯 ASCII 长文本不得再退化成「1 token/字符」（旧 bug 的判据）
	ascii := strings.Repeat("a", 4000)
	if got := EstimateTextTokens(ascii); got != 1000 {
		t.Errorf("4000 字节纯 ASCII 应约 1000 tokens，got %d（按字符数算会是 4000）", got)
	}
}

// TestTokenMeterEstimateMessageTokens 校验消息估算含结构开销。
func TestTokenMeterEstimateMessageTokens(t *testing.T) {
	msg := &Message{Role: "user", Content: "hello"}
	got := EstimateMessageTokens(msg)
	// content = ceil(5/4) = 2 tokens + 4 (role+delimiter) = 6
	if got != 6 {
		t.Errorf("EstimateMessageTokens = %d, want 6 (2 content + 4 overhead)", got)
	}

	// tool 消息有不同开销
	toolMsg := &Message{Role: "tool", Content: "result"}
	got = EstimateMessageTokens(toolMsg)
	// content = ceil(6/4) = 2 + 6 (tool overhead) = 8
	if got != 8 {
		t.Errorf("tool message tokens = %d, want 8", got)
	}
}

// TestTokenMeterEstimateMessagesTokens 校验多条消息总估算。
func TestTokenMeterEstimateMessagesTokens(t *testing.T) {
	msgs := []*Message{
		{Role: "user", Content: "hello"},   // ceil(5/4)=2 + 4 = 6
		{Role: "assistant", Content: "hi"}, // ceil(2/4)=1 + 4 = 5
		{Role: "user", Content: "bye"},     // ceil(3/4)=1 + 4 = 5
	}
	got := EstimateMessagesTokens(msgs)
	if got != 16 {
		t.Errorf("EstimateMessagesTokens = %d, want 16", got)
	}
}

// TestEstimateProtoMessageTokens 校验 proto 版公用启发式与 core.Message 版口径一致：
// CJK 感知（rune 计数）、角色开销、图像结构引用、工具调用名称+参数；供宿主 pre-step
// 与压缩插件共用同一估算（回归「各持私有字节/4 估算导致压力判定漂移」）。
func TestEstimateProtoMessageTokens(t *testing.T) {
	// 纯 ASCII 文本：ceil(5/4)=2 + 4 = 6（同 core.Message 版；旧口径按字符数算成 9）
	got := EstimateProtoMessageTokens(&proto.Message{Role: "user", Content: "hello"})
	if got != 6 {
		t.Errorf("ascii user msg = %d, want 6", got)
	}
	// CJK：每字 1 token（rune 计数），3 字 + 4 = 7
	got = EstimateProtoMessageTokens(&proto.Message{Role: "user", Content: "你好啊"})
	if got != 7 {
		t.Errorf("cjk user msg = %d, want 7", got)
	}
	// tool 角色 +6；工具调用 name+args 估算 +8
	got = EstimateProtoMessageTokens(&proto.Message{
		Role:    "tool",
		Content: "ok", // ceil(2/4)=1 + 6 = 7
	})
	if got != 7 {
		t.Errorf("tool msg = %d, want 7", got)
	}
	// 图像按结构引用计价（对齐 DSH estimateStructuralBlock，当时有效/过期无效）：
	// "dsc-shot://a"（12 字符）→ 4 + ceil(12/4) = 7；1 + 4 + 7 = 12
	got = EstimateProtoMessageTokens(&proto.Message{
		Role:    "user",
		Content: "x",
		Images:  []string{"dsc-shot://a"},
	})
	if got != 12 {
		t.Errorf("image msg = %d, want 12", got)
	}
	got = EstimateProtoMessageTokens(&proto.Message{
		Role:      "assistant",
		ToolCalls: []*proto.ToolCall{{Name: "shell", ArgumentsJson: `{"cmd":"ls"}`}},
	}) // content 空 0 + 4 + EstimateTextTokens(`shell{"cmd":"ls"}`)(17 字节 → 5) + 8 = 17
	if got != 17 {
		t.Errorf("tool-call msg = %d, want 17", got)
	}
	// 列表求和
	msgs := []*proto.Message{
		{Role: "user", Content: "hello"}, // 6
		{Role: "tool", Content: "ok"},    // 7
	}
	if got := EstimateProtoMessagesTokens(msgs); got != 13 {
		t.Errorf("EstimateProtoMessagesTokens = %d, want 13", got)
	}
}

// TestTokenMeterContextWindowUpdate 校验动态更新窗口大小。
func TestTokenMeterContextWindowUpdate(t *testing.T) {
	tm := NewTokenMeter(10000)
	tm.UpdateUsage(&Usage{PromptTokens: 8000, CompletionTokens: 0})

	m := tm.Measure(nil)
	if m.ContextWindow != 10000 {
		t.Errorf("ContextWindow = %d, want 10000", m.ContextWindow)
	}
	if m.Pressure < 0.79 || m.Pressure > 0.81 {
		t.Errorf("Pressure should be ~0.80, got %.4f", m.Pressure)
	}

	// 更新窗口
	tm.UpdateContextWindow(20000)
	m = tm.Measure(nil)
	if m.ContextWindow != 20000 {
		t.Errorf("ContextWindow after update = %d, want 20000", m.ContextWindow)
	}
	if m.Pressure != 0.4 {
		t.Errorf("Pressure after window update should be 0.40, got %.4f", m.Pressure)
	}
}

// TestTokenMeterNoCacheRead 校验：无缓存命中时正常计算。
func TestTokenMeterNoCacheRead(t *testing.T) {
	tm := NewTokenMeter(10000)
	tm.UpdateUsage(&Usage{
		PromptTokens:     6000,
		CompletionTokens: 500,
		// CacheReadInputTokens = 0 (默认零值)
	})

	m := tm.Measure(nil)
	// 无缓存命中：total = prompt(6000) + completion(500) = 6500
	if m.TotalTokens != 6500 {
		t.Errorf("TotalTokens without cache = %d, want 6500", m.TotalTokens)
	}
	if m.CacheReadTokens != 0 {
		t.Errorf("CacheReadTokens should be 0, got %d", m.CacheReadTokens)
	}
}

// TestTokenMeterNilUsage 校验：nil usage 不崩溃。
func TestTokenMeterNilUsage(t *testing.T) {
	tm := NewTokenMeter(10000)
	tm.UpdateUsage(nil) // 不应 panic

	m := tm.Measure([]*Message{{Role: "user", Content: "test"}})
	if m.Source != "estimate" {
		t.Errorf("nil usage 后应回退到 estimate，got %q", m.Source)
	}
}

// TestTokenMeterFormatPressure 校验压力格式化。
func TestTokenMeterFormatPressure(t *testing.T) {
	if got := FormatPressure(0.78); got != "78%" {
		t.Errorf("FormatPressure(0.78) = %q, want '78%%'", got)
	}
	if got := FormatPressure(1.0); got != "100%" {
		t.Errorf("FormatPressure(1.0) = %q, want '100%%'", got)
	}
}

// TestTokenMeterEstimateToolTokens 校验工具定义 token 估算。
func TestTokenMeterEstimateToolTokens(t *testing.T) {
	tools := []Tool{
		{Name: "shell", Description: "Execute a shell command", ParametersJSON: `{"type":"object","properties":{"command":{"type":"string"}}}`},
	}
	got := EstimateToolTokens(tools)
	if got <= 0 {
		t.Error("EstimateToolTokens should return > 0")
	}
	// 验证包含 name + description 的 token 估算
	nameTokens := EstimateTextTokens("shell")
	descTokens := EstimateTextTokens("Execute a shell command")
	if got < nameTokens+descTokens {
		t.Errorf("tool tokens %d should >= name(%d) + desc(%d) = %d", got, nameTokens, descTokens, nameTokens+descTokens)
	}
}

// 消除未使用 import 警告
var _ = context.Background
var _ = strings.Contains

// TestTokenMeterProjectAnchored 锁定「实测锚点 + 启发式增量」的投影语义（对齐 DSH
// tokenMeter 的 projectedTokens = pressureTokens + surfaceTokens − sampledSurfaceTokens）：
// 启发式只承担增量，绝对误差在锚点处被实测值抵消。
func TestTokenMeterProjectAnchored(t *testing.T) {
	tm := NewTokenMeter(1_000_000)

	// 无锚点（首次请求 / 重启恢复）：退回纯估算
	if got := tm.Project(120000); got != 120000 {
		t.Fatalf("无锚点应退回估算 120000，got %d", got)
	}

	// 实测 332000，采样时消息列表估算 320000 → 现在估算 325000（涨 5000）
	tm.SampleUsage(332000, 320000)
	if got := tm.Project(325000); got != 337000 {
		t.Fatalf("锚定后投影 = 332000 + (325000-320000) = 337000，got %d", got)
	}
	// 估算下降（例如压缩/裁剪后）：投影随之下降，但不低于 0
	if got := tm.Project(20000); got != 32000 {
		t.Fatalf("减量同样生效：332000 + (20000-320000) = 32000，got %d", got)
	}
	if got := tm.Project(0); got != 12000 {
		t.Fatalf("332000 - 320000 = 12000，got %d", got)
	}

	// 无基线（只知道用量、不知道当次消息估算）：投影退化为实测值，不应把整段估算加上去
	tm2 := NewTokenMeter(1_000_000)
	tm2.UpdateUsage(&Usage{PromptTokens: 90000})
	if got := tm2.Project(500000); got != 90000 {
		t.Fatalf("无基线时投影应为实测值 90000（而非 90000+500000），got %d", got)
	}

	// Reset（切会话 / 压缩后）：回到纯估算
	tm.Reset()
	if got := tm.Project(70000); got != 70000 {
		t.Fatalf("Reset 后应退回估算 70000，got %d", got)
	}
}

// TestPromptSideTokens 上下文占用只算输入侧：输出已随历史进入下一发 prompt，计进来会重复。
//
// 口径契约：PromptTokens 是**完整** prompt（含缓存），cache_read/cache_write 只是其中
// 的构成，**不得相加**。曾经的 bug 正是这里相加，把缓存命中 99.95% 的会话（实测 498K）
// 算成 ~1.05M，误报「单轮内容超出上下文窗口」。
func TestPromptSideTokens(t *testing.T) {
	u := &Usage{PromptTokens: 5000, CompletionTokens: 200, CacheReadInputTokens: 3000, CacheCreationInputTokens: 500}
	if got := PromptSideTokens(u); got != 5000 {
		t.Fatalf("PromptSideTokens = %d, want 5000（PromptTokens 已含缓存，不得相加）", got)
	}
	if got := PromptSideTokens(nil); got != 0 {
		t.Fatalf("nil usage 应为 0，got %d", got)
	}
}
