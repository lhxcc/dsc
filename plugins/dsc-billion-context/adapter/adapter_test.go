package adapter

import (
	"os"
	"strings"
	"testing"

	bcacp "dsc-billion-context/acp"
	dsc "dsc-sdk"
	"dsc/proto"
)

// TestRoundTripPreservesToolCallArguments 改写往返必须保住 tool-call 的**参数**。
//
// 背景：pre-step 会把整个消息列表经 ToCoreMessages → RenderMessages →
// FromCoreMessages 转一圈再发给模型。此前 CoreToolCall 只有 id/name，参数被静默丢弃，
// 于是模型在第二轮起看到的自己历史是「调过 shell，参数未知」——它无法判断自己跑过
// 什么命令，只能反复重跑，并把「空输出」（git status 干净工作区）当成工具坏了。
//
// 这条属于「返回子集适配器」的字段保真往返：喂全字段，逐字段断言存活。
func TestRoundTripPreservesToolCallArguments(t *testing.T) {
	args := `{"command":"git status --short","description":"Show working tree status"}`
	msgs := []*proto.Message{
		{Role: "user", Content: "看一下工作区"},
		{
			Role:    "assistant",
			Content: "",
			ToolCalls: []*proto.ToolCall{
				{Id: "call_a", Name: "shell", ArgumentsJson: args},
				{Id: "call_b", Name: "read_file", ArgumentsJson: `{"path":"a.go"}`},
			},
		},
		{Role: "tool", Content: "[exit_code: 0]", ToolCallId: "call_a"},
		{Role: "tool", Content: "package main", ToolCallId: "call_b"},
		{Role: "assistant", Content: "干净", Images: []string{"dsc-img://abc"}},
	}

	back := FromCoreMessages(ToCoreMessages(msgs, nil))
	if len(back) != len(msgs) {
		t.Fatalf("消息条数应保持 %d，实际 %d", len(msgs), len(back))
	}
	for i := range msgs {
		if back[i].Role != msgs[i].Role {
			t.Fatalf("#%d role 丢失: %q → %q", i, msgs[i].Role, back[i].Role)
		}
		if back[i].Content != msgs[i].Content {
			t.Fatalf("#%d content 丢失: %q → %q", i, msgs[i].Content, back[i].Content)
		}
		if back[i].ToolCallId != msgs[i].ToolCallId {
			// assistant 消息带工具调用时，往返会补上「首个调用的 id」——那是压缩侧把
			// tool-call 与 tool-result 配对用的键（stripOrphaned* 依赖它），属派生值，
			// 不是丢字段；provider 侧只对 role=tool 读 tool_call_id。
			derived := len(msgs[i].ToolCalls) > 0 && back[i].ToolCallId == msgs[i].ToolCalls[0].Id
			if !derived {
				t.Fatalf("#%d tool_call_id 丢失: %q → %q", i, msgs[i].ToolCallId, back[i].ToolCallId)
			}
		}
		if len(back[i].ToolCalls) != len(msgs[i].ToolCalls) {
			t.Fatalf("#%d tool_calls 条数丢失: %d → %d", i, len(msgs[i].ToolCalls), len(back[i].ToolCalls))
		}
		for j, want := range msgs[i].ToolCalls {
			got := back[i].ToolCalls[j]
			if got.Id != want.Id || got.Name != want.Name {
				t.Fatalf("#%d.%d 调用身份丢失: %+v → %+v", i, j, want, got)
			}
			if got.ArgumentsJson != want.ArgumentsJson {
				t.Fatalf("#%d.%d 参数丢失（模型将看不到自己执行过什么）: %q → %q",
					i, j, want.ArgumentsJson, got.ArgumentsJson)
			}
		}
		if len(back[i].Images) != len(msgs[i].Images) {
			t.Fatalf("#%d 图像引用丢失: %v → %v", i, msgs[i].Images, back[i].Images)
		}
	}
}

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

// TestStateStoreUsesSessionsDirWithSuffix state 必须与宿主会话同目录、靠后缀区分：
// 不再单独占用 ExecDir/billion-context/，且文件名带 .acp.json 以免被 session.Store
// （只扫描 *.jsonl）或人工清理会话时误认。
func TestStateStoreUsesSessionsDirWithSuffix(t *testing.T) {
	dir := t.TempDir()
	store := NewStateStore(dir)
	st := bcacp.CreateInitialState()
	if err := store.Save("session-7", st); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	want := dsc.PJoin(dir, "session-7"+StateFileSuffix)
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("应写入 %s: %v", want, err)
	}
	// 不得产生裸 <id>.json（旧命名），否则与新版并存时行为不确定
	if _, err := os.Stat(dsc.PJoin(dir, "session-7.json")); err == nil {
		t.Fatalf("不应再写旧命名 session-7.json")
	}
}

// TestStateStorePathUsesSessionIDVerbatim state 文件名必须与会话 id 一一对应：
// 旧实现把非法字符替换为 '_'（"a b" 与 "a_b" 折成同一文件，两个会话串状态），
// 现在只用 SDK 的统一校验，不做字符替换；非法 id 直接报错而非静默改名。
func TestStateStorePathUsesSessionIDVerbatim(t *testing.T) {
	dir := t.TempDir()
	store := NewStateStore(dir)

	for _, id := range []string{"a b", "a_b", "O=--Agents--dsc", "x~db"} {
		p, err := store.path(id)
		if err != nil {
			t.Fatalf("%q 应可作文件名: %v", id, err)
		}
		if want := dsc.PJoin(dir, id+StateFileSuffix); p != want {
			t.Fatalf("path(%q) = %q, want %q", id, p, want)
		}
	}

	for _, bad := range []string{"", "..", "a/b", `a\b`} {
		if _, err := store.path(bad); err == nil {
			t.Fatalf("%q 应被拒绝（防路径穿越）", bad)
		}
	}
}

// TestStateFileNamedAfterPlugin 状态文件名必须带插件名（<会话 id>.<插件名>.json）：
// 宿主核心不该知道任何插件的落盘后缀，同名会话的多种插件状态同目录并存而不冲突。
func TestStateFileNamedAfterPlugin(t *testing.T) {
	if !strings.Contains(StateFileSuffix, PluginName) {
		t.Fatalf("后缀应含插件名: %q（插件名 %q）", StateFileSuffix, PluginName)
	}
	p, err := NewStateStore(t.TempDir()).path("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(p, "session-1."+PluginName+".json") {
		t.Fatalf("文件名规则不符: %q", p)
	}
	if strings.HasSuffix(p, dsc.SessionFileSuffix) {
		t.Fatalf("不得与会话日志同名: %q", p)
	}
}

// TestPruneOrphans 会话已删除时，其状态文件由插件自行清理（宿主不再知道这些文件）。
func TestPruneOrphans(t *testing.T) {
	dir := t.TempDir()
	store := NewStateStore(dir)

	// 两个会话：一个仍有日志，一个已被删除
	if err := os.WriteFile(dsc.PJoin(dir, "session-1"+dsc.SessionFileSuffix), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"session-1", "session-2"} {
		if err := store.Save(id, bcacp.CreateInitialState()); err != nil {
			t.Fatalf("保存 %s 失败: %v", id, err)
		}
	}
	// 无关文件不得被误删
	other := dsc.PJoin(dir, "session-1.other-plugin.json")
	if err := os.WriteFile(other, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}

	if n := PruneOrphans(dir); n != 1 {
		t.Fatalf("应清理 1 个孤儿状态，got %d", n)
	}
	if _, err := os.Stat(dsc.PJoin(dir, "session-1"+StateFileSuffix)); err != nil {
		t.Fatalf("仍有会话日志的状态不应被删: %v", err)
	}
	if _, err := os.Stat(dsc.PJoin(dir, "session-2"+StateFileSuffix)); !os.IsNotExist(err) {
		t.Fatalf("孤儿状态应被删除: err=%v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("其它插件的文件不得被误删: %v", err)
	}
}
