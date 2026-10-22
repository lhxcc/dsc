package adapter

import (
	"os"
	"strings"
	"testing"

	bcacp "dsc-billion-context/acp"
	"dsc/proto"
)

// TestCoreMessageIDsUniqueByPosition 内容相同的两条消息必须得到不同 ID。
//
// 旧实现按 (role+content+toolIds) 做 FNV hash：两条"继续"、两次相同 compile 输出
// 会撞成同一个 ID，MessageRefs.ByRaw 互相覆盖、CoveredMessageIDs 判定错位，
// 压缩就会命中错误的消息。改为按位置取身份后必须唯一且跨轮稳定。
func TestCoreMessageIDsUniqueByPosition(t *testing.T) {
	msgs := []*proto.Message{
		{Role: "user", Content: "继续"},
		{Role: "assistant", Content: "好的"},
		{Role: "user", Content: "继续"}, // 与第一条完全相同
		{Role: "tool", Content: "ok", ToolCallId: "call_1"},
		{Role: "tool", Content: "ok", ToolCallId: "call_1"}, // 完全重复
	}

	cores := ToCoreMessages(msgs, nil)
	seen := map[string]bool{}
	for i, cm := range cores {
		if cm.ID == "" {
			t.Fatalf("#%d ID 为空", i)
		}
		if seen[cm.ID] {
			t.Fatalf("#%d ID 重复: %s（内容相同的消息必须按位置区分）", i, cm.ID)
		}
		seen[cm.ID] = true
	}

	// 同一输入重复转换 → ID 必须稳定（跨轮 ref 映射依赖它）
	again := ToCoreMessages(msgs, nil)
	for i := range cores {
		if cores[i].ID != again[i].ID {
			t.Fatalf("#%d 重复转换 ID 不稳定: %s vs %s", i, cores[i].ID, again[i].ID)
		}
	}
}

// TestStateStoreSaveAtomic 覆盖写后落盘内容必须完整可解析、且不残留临时文件。
// 旧实现直接 WriteFile：崩溃/断电会留下半截 JSON，Load 解析失败即重建，
// 压缩记录全丢（压缩收益一次性归零）。
func TestStateStoreSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	store := NewStateStore(dir)

	st := bcacp.CreateInitialState()
	st.Stats.CompressionCount = 1
	if err := store.Save("sess-1", st); err != nil {
		t.Fatalf("首次保存失败: %v", err)
	}

	st.Stats.CompressionCount = 2
	if err := store.Save("sess-1", st); err != nil {
		t.Fatalf("覆盖保存失败: %v", err)
	}

	// 新 store 直接读磁盘（绕过缓存）→ 证明落盘是完整 JSON
	reloaded, err := NewStateStore(dir).Load("sess-1")
	if err != nil {
		t.Fatalf("落盘内容应完整可解析: %v", err)
	}
	if reloaded.Stats.CompressionCount != 2 {
		t.Fatalf("覆盖写未生效: count=%d", reloaded.Stats.CompressionCount)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("不应残留临时文件: %s", e.Name())
		}
	}
}
