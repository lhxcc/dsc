package tui

import (
	"context"
	"strings"
	"testing"

	core "dsc/core"
)

// TestRenderHistoryBlocksToolEntries 历史回放里的工具过程必须渲染成卡片
// （与实时视图同一套 renderToolCall / renderToolResult），否则重启后只剩结论、
// 看不到模型改代码与跑命令的过程。
func TestRenderHistoryBlocksToolEntries(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.width = 80

	// 调用卡：str_replace_editor 的 command/path 会被解析成 StrReplaceEditor(StrReplace, a.go)
	blocks := m.renderHistoryBlocks([]*core.AgentDebugMessage{{
		Role: "tool", Kind: core.DebugKindToolCall,
		ToolName: "str_replace_editor",
		ToolArgs: `{"command":"str_replace","path":"a.go"}`,
	}})
	if len(blocks) != 1 {
		t.Fatalf("调用卡应为 1 块, got %d: %#v", len(blocks), blocks)
	}
	if !strings.Contains(blocks[0], "StrReplace") || !strings.Contains(blocks[0], "a.go") {
		t.Fatalf("调用卡应含工具动词与路径, got %q", blocks[0])
	}

	// 结果卡：内容可见
	blocks = m.renderHistoryBlocks([]*core.AgentDebugMessage{{
		Role: "tool", Kind: core.DebugKindToolResult, Content: "patched a.go\n[exit_code: 0]",
	}})
	if len(blocks) != 1 || !strings.Contains(blocks[0], "patched a.go") {
		t.Fatalf("结果卡应含结果正文, got %#v", blocks)
	}

	// 空工具名 / 纯空白结果：不产生空块（否则回放会多出空行）
	if blocks = m.renderHistoryBlocks([]*core.AgentDebugMessage{{
		Role: "tool", Kind: core.DebugKindToolCall, ToolName: "  ",
	}}); len(blocks) != 0 {
		t.Fatalf("空工具名不应产生块, got %#v", blocks)
	}
	if blocks = m.renderHistoryBlocks([]*core.AgentDebugMessage{{
		Role: "tool", Kind: core.DebugKindToolResult, Content: "   \n",
	}}); len(blocks) != 0 {
		t.Fatalf("空白结果不应产生块, got %#v", blocks)
	}
}
