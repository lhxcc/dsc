package core

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsc/proto"
)

// TestUsageSamplesFeedCalibration 每一帧结算都要落两处：会话账本（可审计）与按模型的
// 校准表（跨会话、跨重启有效）。
func TestUsageSamplesFeedCalibration(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(&ManagerConfig{ExecDir: dir})

	for i := 0; i < 4; i++ {
		m.recordUsageSample("sess-1", UsageSample{
			Model: "model-x", Sent: 1000, Actual: 1200,
			Msgs: 10, MsgEst: 900, ToolEst: 100,
			Classes: [7]int{100, 20, 5, 30, 2, 0, 0},
		})
	}

	ledger := filepath.Join(m.sessionDir(), "sess-1"+usageLedgerSuffix)
	data, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("账本未写入: %v", err)
	}
	lines := strings.Count(strings.TrimSpace(string(data)), "\n") + 1
	if lines != 4 {
		t.Fatalf("账本应有 4 行，got %d:\n%s", lines, data)
	}
	var rec UsageSample
	if err := json.Unmarshal([]byte(strings.Split(string(data), "\n")[0]), &rec); err != nil {
		t.Fatalf("账本行应可解析: %v", err)
	}
	if rec.Sent != 1000 || rec.Actual != 1200 || rec.Model != "model-x" || rec.Classes[3] != 30 {
		t.Fatalf("账本记录不完整: %+v", rec)
	}

	// 校准表：4 帧比值都是 1.2 → 系数 1.2，样本数 4
	m.calibMu.RLock()
	c := m.calib["model-x"]
	m.calibMu.RUnlock()
	if c == nil || c.Samples != 4 {
		t.Fatalf("校准条目缺失或样本数不对: %+v", c)
	}
	if c.Factor < 1.19 || c.Factor > 1.21 {
		t.Fatalf("系数应约 1.2，got %.4f", c.Factor)
	}
	if got := m.calibrationFor("model-x"); got < 1.19 || got > 1.21 {
		t.Fatalf("calibrationFor 应返回 1.2，got %.4f", got)
	}

	// 落盘：新实例（同目录、删掉内存）能读回来
	raw, err := os.ReadFile(m.calibrationPath())
	if err != nil {
		t.Fatalf("校准表未落盘: %v", err)
	}
	var f calibrationFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Schema != calibrationSchema {
		t.Fatalf("校准表格式不对: %v\n%s", err, raw)
	}
}

// TestCalibrationSurvivesRestart 冷启动：新实例没有任何内存锚点，但校准表还在，
// 于是第一个请求就有依据——这正是「历史最厚、最容易撞墙」的那一刻。
func TestCalibrationSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	m1 := NewManager(&ManagerConfig{ExecDir: dir})
	for i := 0; i < 5; i++ {
		m1.recordUsageSample("sess-1", UsageSample{Model: "model-x", Sent: 1000, Actual: 1300})
	}

	m2 := NewManager(&ManagerConfig{ExecDir: dir})
	if got := m2.calibrationFor("model-x"); got < 1.28 || got > 1.32 {
		t.Fatalf("重启后应从校准表读回 1.3，got %.4f", got)
	}
	// 冷启动的新会话没有锚点 → 偏差系数直接用校准表，而不是 1
	if got := m2.estimateInflation("brand-new-session", "model-x"); got < 1.28 || got > 1.32 {
		t.Fatalf("冷启动首帧应使用校准表，got %.4f", got)
	}
	// 未知模型仍然退回 1（没有可借的东西）
	if got := m2.estimateInflation("brand-new-session", "model-y"); got != 1 {
		t.Fatalf("未知模型应为 1，got %.4f", got)
	}
}

// TestCalibrationRebuiltFromLedger 校准表被删（或首次升级上来）时，从账本重建：
// 不必从 1 重新学。
func TestCalibrationRebuiltFromLedger(t *testing.T) {
	dir := t.TempDir()
	m1 := NewManager(&ManagerConfig{ExecDir: dir})
	for i := 0; i < 6; i++ {
		m1.recordUsageSample("sess-1", UsageSample{Model: "model-x", Sent: 1000, Actual: 1400})
	}
	if err := os.Remove(m1.calibrationPath()); err != nil {
		t.Fatalf("删校准表: %v", err)
	}

	m2 := NewManager(&ManagerConfig{ExecDir: dir})
	if got := m2.calibrationFor("model-x"); got < 1.35 || got > 1.45 {
		t.Fatalf("应从账本重建出约 1.4，got %.4f", got)
	}
}

// TestCalibrationTracksDriftAndSpread 连续偏离要拉动系数；离散度要能算出来
// （它是「整体比例还够不够用」的判据）。
func TestCalibrationTracksDriftAndSpread(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	for i := 0; i < 10; i++ {
		m.recordUsageSample("s", UsageSample{Model: "m", Sent: 1000, Actual: 1000})
	}
	base := m.calibrationFor("m")
	if base < 0.99 || base > 1.01 {
		t.Fatalf("基线应约 1.0，got %.3f", base)
	}

	// 内容构成变了：比值稳定跳到 1.5，EWMA 应在若干帧内靠近它
	for i := 0; i < 15; i++ {
		m.recordUsageSample("s", UsageSample{Model: "m", Sent: 1000, Actual: 1500})
	}
	got := m.calibrationFor("m")
	if got < 1.35 {
		t.Fatalf("持续漂移后系数应靠近 1.5，got %.3f", got)
	}
	m.calibMu.RLock()
	spread := m.calib["m"].Spread
	m.calibMu.RUnlock()
	if spread <= 1.0 {
		t.Fatalf("窗口内比值从 1.0 变到 1.5，离散度应 >1，got %.3f", spread)
	}
}

// TestMeasureMatchesEstimator 测量与估算必须逐字节同口径：判超窗用估算、喂校准用测量，
// 两处一旦分叉就是「按 A 判、按 B 学」。
func TestMeasureMatchesEstimator(t *testing.T) {
	msgs := []*proto.Message{
		{Role: "system", Content: "You are an agent."},
		{Role: "user", Content: "帮我看看 O:/Agents/dsc 的 token 估算"},
		{Role: "assistant", Content: "", ToolCalls: []*proto.ToolCall{
			{Name: "shell", ArgumentsJson: `{"command":"go test ./core"}`},
		}},
		{Role: "tool", Content: "ok: 1.234s", ToolCallId: "c1"},
		{Role: "user", Content: strings.Repeat("x", 300), Images: []string{"dsc-shot://a"}},
	}
	want := EstimateProtoMessagesTokens(msgs)
	got := MeasureProtoMessages(msgs)
	if got.Estimate != want {
		t.Fatalf("测量与估算不一致: measure=%d estimate=%d", got.Estimate, want)
	}
	if got.Msgs != len(msgs) || got.ToolCalls != 1 {
		t.Fatalf("消息/工具调用计数不对: %+v", got)
	}
	tools := []*proto.Tool{{Name: "shell", Description: "run", ParametersJson: `{"type":"object"}`}}
	surf := MeasureRequestSurface(msgs, tools)
	if surf.Estimate != want+EstimateProtoToolTokens(tools) {
		t.Fatalf("表面估算应为消息+工具目录: %d vs %d", surf.Estimate, want+EstimateProtoToolTokens(tools))
	}
	// 构成计数必须真的分类，不是全零
	if surf.Classes[clsLetter] == 0 || surf.Classes[clsWide] == 0 {
		t.Fatalf("构成计数异常: %v", surf.Classes)
	}
}
