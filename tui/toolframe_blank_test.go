package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"dsc/core"
	"github.com/charmbracelet/x/ansi"
)

// 工具调用卡与它的结果之间不得出现空行。三条路径都要一致：
//   - 实时：调用帧 → 结果帧（紧贴）；
//   - 实时：调用帧 → 空帧 → 结果帧（空帧不得落下一个纯换行）；
//   - 回放：调用条目 → 结果条目（tight，不补分隔空行）。
//
// 现场：git commit 的结果卡上方多出一个空行（箭头所指），就是这三条里某一条漏了。

func stripJoin(lines []string) string {
	return ansi.Strip(strings.Join(lines, "\n"))
}

func TestLiveToolResultHugsCallCard(t *testing.T) {
	m := New(&stubAgent{frames: []*core.RunStreamResponse{
		{Status: "tool", ToolName: "shell", ToolArgs: `{"command":"git commit --amend","description":"Drop v6-specific script"}`},
		{Status: "tool", ToolResult: "[main 7362fc22] disk: keep the page-aligned prefix\n 2 files changed"},
		{Status: "success"},
	}}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m2 := submitAndDrain(m, "hi", 20).(*Model)
	if joined := stripJoin(m2.lines); strings.Contains(joined, "Drop v6-specific script\n\n") {
		t.Fatalf("调用卡与结果之间多出空行:\n%v", m2.lines)
	}
}

// TestEmptyToolFrameDoesNotEmitBlankLine 空白工具帧（无名字、无结果、输出只有换行）
// 此前会走 appendToolFrame(dimSty.Render(""))——注意 Render("") 不是空串（套了 ANSI
// 转义），于是追加的是一行"看似有内容"的空白，表现为调用卡与结果之间凭空多一行。
func TestEmptyToolFrameDoesNotEmitBlankLine(t *testing.T) {
	m := New(&stubAgent{frames: []*core.RunStreamResponse{
		{Status: "tool", ToolName: "shell", ToolArgs: `{"command":"git commit --amend","description":"Drop it"}`},
		{Status: "tool", Output: "\n"}, // 空白帧（只带换行的状态帧，实际会出现）
		{Status: "tool", ToolResult: "[main 7362fc22] disk: keep\ntail"},
		{Status: "success"},
	}}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m2 := submitAndDrain(m, "hi", 20).(*Model)
	joined := stripJoin(m2.lines)
	if strings.Contains(joined, "Drop it\n\n") {
		t.Fatalf("空帧在调用卡与结果之间插入了空行: %q", joined)
	}
	// 结果仍应紧贴调用卡（空帧不能把 toolCallOpen 打断）
	if !strings.Contains(joined, "Drop it\n") {
		t.Fatalf("结果未紧跟调用卡:\n%v", m2.lines)
	}
	for _, ln := range m2.lines {
		if ln == "\n" || ln == "" {
			t.Fatalf("出现了纯空行元素: %#v", m2.lines)
		}
	}
}

// TestReplayCallAndResultAreTight 回放里「调用卡 → 结果卡」必须 tight：seedHistory 与
// 向上翻页（prependBlocks）都按该标记省略分隔空行。
func TestReplayCallAndResultAreTight(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	entries := []*core.AgentDebugMessage{
		{Role: "user", Content: "跑一下"},
		{Role: "tool", Kind: core.DebugKindToolCall, ToolName: "shell", ToolArgs: `{"command":"git commit","description":"Drop it"}`},
		{Role: "tool", Kind: core.DebugKindToolResult, Content: "[main 7362fc22] disk: keep"},
		{Role: "assistant", Content: "好了"},
	}
	items := m.renderHistoryItems(entries)
	if len(items) != 4 {
		t.Fatalf("条目数 = %d, want 4", len(items))
	}
	if items[0].tight || items[1].tight || items[3].tight {
		t.Fatalf("只有工具结果应 tight: %+v", items)
	}
	if !items[2].tight {
		t.Fatal("工具结果应紧贴它的调用标题（tight）")
	}

	// 走真实回放拼接：调用卡与结果之间不得有空行
	m.historyAll = entries
	m.historyPage = len(entries)
	m.historyLoaded = 0
	m.seedHistory()
	joined := stripJoin(m.lines)
	if strings.Contains(joined, "Drop it\n\n") {
		t.Fatalf("回放里调用卡与结果之间多出空行:\n%v", m.lines)
	}
	if !strings.Contains(joined, "Drop it\n") {
		t.Fatalf("回放里结果未紧跟调用卡:\n%v", m.lines)
	}
	// 其余块之间仍保留空行（可读性）
	if !strings.Contains(joined, "跑一下") || !strings.Contains(joined, "好了") {
		t.Fatalf("回放缺块:\n%v", m.lines)
	}
}

// TestAppendMessageSkipsEmptyBlocks 空块不入列：空串 / 只有换行 / 只有 ANSI 转义
// 的渲染结果都没有可读内容，写入只会在消息之间多出一个空行。用可见宽度判定，
// 而不是比较空串（dimSty.Render("") 就不是空串）。
func TestAppendMessageSkipsEmptyBlocks(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	before := len(m.lines)
	for _, empty := range []string{"", "\n", dimSty.Render("")} {
		m.appendMessage(empty)
	}
	if len(m.lines) != before {
		t.Fatalf("空块被写入: %d → %d", before, len(m.lines))
	}
	// 有内容的照旧追加（并且补一个分隔空行）
	m.appendMessage("正文")
	if len(m.lines) != before+1 {
		t.Fatalf("正常消息未写入: %d → %d", before, len(m.lines))
	}
	if before > 0 && !strings.HasPrefix(m.lines[len(m.lines)-1], "\n") {
		t.Fatalf("非首条消息应带分隔空行: %q", m.lines[len(m.lines)-1])
	}
}
