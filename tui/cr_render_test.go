package tui

import (
	"context"
	"strings"
	"testing"

	core "dsc/core"
)

// 渲染层兜底：工具结果卡片里不应再有裸 CR（否则终端把光标拉回第 0 列、卡片缩进被覆盖），
// 且展示的是最终帧而不是某个中间帧。归一实现在 core（TUI 与 shell 工具共用）。
func TestRenderToolResultStripsCarriageReturns(t *testing.T) {
	in := "Updating files:  46% (2199/4780)\rUpdating files: 100% (4780/4780), done.\n[exit_code: 0]\n"
	out := renderToolResult(in, false)
	if strings.ContainsRune(out, '\r') {
		t.Fatalf("渲染结果含裸 CR（会覆盖左边距）: %q", out)
	}
	if !strings.Contains(out, "100% (4780/4780), done.") {
		t.Fatalf("应展示最终进度: %q", out)
	}
	if strings.Contains(out, "46%") {
		t.Fatalf("中间帧不应残留: %q", out)
	}
}

// 回放路径（renderHistoryBlocks → renderToolResult）同样归一：历史里的旧结果也带 CR。
func TestReplayToolResultStripsCarriageReturns(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.width = 80
	blocks := m.renderHistoryBlocks([]*core.AgentDebugMessage{
		{Role: "tool", Kind: core.DebugKindToolResult, Content: "1%\r100%, done.\n"},
	})
	if len(blocks) != 1 {
		t.Fatalf("应渲染 1 块, got %d", len(blocks))
	}
	if strings.ContainsRune(blocks[0], '\r') || !strings.Contains(blocks[0], "100%, done.") {
		t.Fatalf("回放结果应已归一: %q", blocks[0])
	}
}
