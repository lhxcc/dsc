package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestToolNotFoundSuggestsClosest 工具名不存在时，错误要带上最相近的真实工具名，
// 让模型能自己改对（否则它会换一个同样不存在的名字继续试）。
func TestToolNotFoundSuggestsClosest(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	for _, name := range []string{"web_search", "search_context", "memory_search", "read_file"} {
		if err := m.toolRegistry.Register(&mockTool{name: name}); err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}

	_, err := m.ExecuteTool(context.Background(), "tool_search", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("未注册的工具应返回错误")
	}
	msg := err.Error()
	if !strings.Contains(msg, "tool not found: tool_search") {
		t.Fatalf("错误前缀变了: %q", msg)
	}
	if !strings.Contains(msg, "web_search") {
		t.Fatalf("应给出最相近的真实工具名: %q", msg)
	}
	if strings.Contains(msg, "read_file") {
		t.Fatalf("不相关的工具不应出现在候选里: %q", msg)
	}
}

// TestToolNotFoundWithoutCloseMatch 没有任何相近工具时，保持原来那句话，
// 不硬塞一堆无关名字。
func TestToolNotFoundWithoutCloseMatch(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	if err := m.toolRegistry.Register(&mockTool{name: "read_file"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	_, err := m.ExecuteTool(context.Background(), "zzzzzzzzzzzzzz", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("未注册的工具应返回错误")
	}
	if got := err.Error(); got != "tool not found: zzzzzzzzzzzzzz" {
		t.Fatalf("无相近候选时应保持简短文案, got %q", got)
	}
}

// TestEditDistance 基本正确性（用于候选排序）。
func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "abc", 3},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"tool_search", "web_search", 4}, // 3 处替换 + 1 处删除（"_" 位置不同）
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}
