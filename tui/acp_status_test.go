package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"dsc/core"
)

// TestRenderContextStatus 面板要把三件事说清楚：占了多少、压了多少、还有什么可压。
// 输入是宿主协议结构（core.ContextStatusEvent），不是某个插件的私有 kv——
// 这正是这条测试要守住的：前端只认协议。
func TestRenderContextStatus(t *testing.T) {
	st := core.ContextStatusEvent{
		Tokens:             420_000,
		Limit:              1_000_000,
		BlocksActive:       6,
		BlocksTotal:        9,
		BlocksNote:         "T1=4 T2=2 T3=0",
		TokensReclaimed:    180_000,
		Compressions:       6,
		CompressibleRanges: 3,
		Metrics: []core.ContextMetric{
			{Label: "蒸馏", Tokens: 12_000},
			{Label: "确定性压缩", Value: 3, Tokens: 9_500},
			{Label: "已卸载可回取", Value: 4, Tokens: 31_000},
			{Label: "回取", Value: 2, Note: "导出 1 · 落空 0"},
		},
	}
	out := ansi.Strip(renderContextStatus(st))
	for _, want := range []string{
		"上下文压缩", "420K / 1.00M", "42%",
		"6 active / 9 total", "T1=4 T2=2 T3=0",
		"180K", "6 次压缩",
		"蒸馏: 12K",
		"确定性压缩: 3 次，9K",
		"已卸载可回取: 4 次，31K",
		"回取: 2 次（导出 1 · 落空 0）",
		"可压范围: 3 段",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("面板应包含 %q，实际：\n%s", want, out)
		}
	}
}

// TestRenderContextStatusEmptyMetrics 后端什么都不报时也不能崩、不能打出空标签。
func TestRenderContextStatusEmptyMetrics(t *testing.T) {
	out := ansi.Strip(renderContextStatus(core.ContextStatusEvent{Tokens: 1000}))
	if !strings.Contains(out, "占用: 1K") {
		t.Fatalf("至少要显示占用: %s", out)
	}
	if strings.Contains(out, "metrics") || strings.Contains(out, ": \n") {
		t.Fatalf("没有指标时不应打出空行: %q", out)
	}
}

// TestSlashContextWithoutStatus 没有状态时给提示，不显示空白面板。
func TestSlashContextWithoutStatus(t *testing.T) {
	for _, cmd := range []string{"/context", "/acp"} {
		m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
		if handled, _ := m.runSlashCommand(cmd); !handled {
			t.Fatalf("%s 应被识别为命令", cmd)
		}
		view := ansi.Strip(strings.Join(m.lines, ""))
		if !strings.Contains(view, "暂无压缩状态") {
			t.Fatalf("%s 无状态时应给提示，实际视图：%s", cmd, view)
		}
	}
}

// TestContextStatusMsgStored 事件到达后状态被记下，且命令能读出来。
func TestContextStatusMsgStored(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(ctxStatusMsg{status: core.ContextStatusEvent{
		Tokens: 1000, Limit: 10000, BlocksActive: 1, BlocksTotal: 1,
	}})
	if !m.hasCtxStatus {
		t.Fatal("状态应被记录")
	}
	m.runSlashCommand("/context")
	view := ansi.Strip(strings.Join(m.lines, ""))
	if !strings.Contains(view, "1K / 10K") {
		t.Fatalf("面板应显示占用，实际：%s", view)
	}
}

// TestNoACPStatusText 三种"没有状态"的原因要给不同的话：
// 配置了但插件没加载 / 根本没配 / 配好了只是还没跑。
func TestNoACPStatusText(t *testing.T) {
	if got := noACPStatusText(nil); !strings.Contains(got, "未连接宿主管理器") {
		t.Fatalf("nil 的说明不对: %s", got)
	}
	// 没配后端：说明当前走内联压缩
	if got := noACPStatusText(fakeBackend{}); !strings.Contains(got, "未指定 compaction 后端") {
		t.Fatalf("未配置后端时应说明走内联压缩: %s", got)
	}
	// 配了但没生效：用户实际踩到的场景（plugins 列表里被注释掉）。
	// 名字只是配置里写的字符串，这里用任意名字验证"点名"行为，不绑定具体插件。
	got := noACPStatusText(fakeBackend{name: "backend-x"})
	if !strings.Contains(got, "已配置但未生效") || !strings.Contains(got, "backend-x") {
		t.Fatalf("配置未生效时应点名后端并说明原因: %s", got)
	}
	// 后端在跑但还没上报
	got = noACPStatusText(fakeBackend{name: "backend-x", active: true})
	if !strings.Contains(got, "还没有状态上报") {
		t.Fatalf("已生效时应说明等一轮: %s", got)
	}
}

// fakeBackend 假的压缩后端报告器。
type fakeBackend struct {
	name   string
	active bool
}

func (f fakeBackend) CompactionBackendState() (string, bool) { return f.name, f.active }
