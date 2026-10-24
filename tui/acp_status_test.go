package tui

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// TestRenderACPStatus 面板要把三件事说清楚：占了多少、省了多少、回取顺不顺。
func TestRenderACPStatus(t *testing.T) {
	payload := map[string]any{
		"usage": 0.42, "tokens": float64(420_000), "limit": float64(1_000_000),
		"blocks": float64(6), "blocksTotal": float64(9),
		"tiers":      []any{float64(4), float64(2), float64(0)},
		"compressed": float64(180_000), "compressions": float64(6),
		"absorbed": float64(12_000),
		"crush":    float64(3), "crushSaved": float64(9_500),
		"stored": float64(4), "storedTokens": float64(31_000),
		"retrieved": float64(2), "exported": float64(1), "misses": float64(0),
		"compressibleCount": float64(3),
	}
	out := ansi.Strip(renderACPStatus(payload))
	for _, want := range []string{
		"上下文压缩", "420K / 1.00M", "42%",
		"T1=4 T2=2 T3=0",
		"180K", "absorb", "确定性压缩: 3 次，省 9K",
		"已卸载可回取: 4 条", "回取: 2 次（其中导出 1 次），落空 0 次",
		"可压范围: 3 段",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("面板应包含 %q，实际：\n%s", want, out)
		}
	}
}

// TestSlashACPWithoutStatus 没有状态时给提示，不显示空白面板。
func TestSlashACPWithoutStatus(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	if handled, _ := m.runSlashCommand("/acp"); !handled {
		t.Fatal("/acp 应被识别为命令")
	}
	view := ansi.Strip(strings.Join(m.lines, ""))
	if !strings.Contains(view, "暂无压缩状态") {
		t.Fatalf("无状态时应给提示，实际视图：%s", view)
	}
}

// TestACPStatusMsgStored 事件到达后状态被记下，且 /acp 能读出来。
func TestACPStatusMsgStored(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(acpStatusMsg{payload: map[string]any{
		"tokens": float64(1000), "limit": float64(10000),
		"blocks": float64(1), "blocksTotal": float64(1),
	}})
	if m.acpStatus == nil {
		t.Fatal("状态应被记录")
	}
	if _, cmd := m.runSlashCommand("/acp"); cmd != nil {
		_ = cmd
	}
	view := ansi.Strip(strings.Join(m.lines, ""))
	if !strings.Contains(view, "1K / 10K") {
		t.Fatalf("面板应显示占用，实际：%s", view)
	}
}
