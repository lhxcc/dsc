package adapter

import (
	"testing"

	bcacp "dsc-billion-context/acp"
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

// TestContentIdenticalMessagesKeepDistinctIDs 内容完全相同的两条消息必须是两个身份。
//
// 旧实现按 (role+content+toolIds) 做 hash：两条「继续」、两次相同编译输出会撞成同一个
// ID，MessageRefs.ByRaw 互相覆盖、覆盖判定错位，压缩命中错误的消息。现在身份由宿主
// 按事件 seq 下发，内容重复完全不影响唯一性——这里连同「逐字相同」的极端情形一起钉住。
func TestContentIdenticalMessagesKeepDistinctIDs(t *testing.T) {
	msgs := []*proto.Message{
		{Id: "event-1", Role: "user", Content: "继续"},
		{Id: "event-2", Role: "assistant", Content: "好的"},
		{Id: "event-3", Role: "user", Content: "继续"}, // 与第一条完全相同
		{Id: "event-4", Role: "tool", Content: "ok", ToolCallId: "call_1"},
		{Id: "event-5", Role: "tool", Content: "ok", ToolCallId: "call_1"}, // 完全重复
	}

	cores := ToCoreMessages(msgs, nil)
	seen := map[string]bool{}
	for i, cm := range cores {
		if cm.ID != msgs[i].Id {
			t.Fatalf("#%d id = %q，期望宿主 id %q", i, cm.ID, msgs[i].Id)
		}
		if seen[cm.ID] {
			t.Fatalf("#%d id 重复: %s", i, cm.ID)
		}
		seen[cm.ID] = true
	}

	// 同一输入重复转换 → id 必须稳定（跨轮 ref 映射依赖它）
	again := ToCoreMessages(msgs, nil)
	for i := range cores {
		if cores[i].ID != again[i].ID {
			t.Fatalf("#%d 重复转换 id 不稳定: %s vs %s", i, cores[i].ID, again[i].ID)
		}
	}
}

// TestMessageIDIsStableAcrossListShift 宿主下发的稳定 id 必须原样成为压缩后端的
// 消息身份，且**不随列表位置变化**——这是「配置随时会改」的兜底：history_injection
// 只带最近 N 条、宿主裁剪、fork 都会让列表起点平移。
//
// 此前用列表下标当身份，理由是「pre-step 收到的是 append-only 原始历史」；那个前提
// 被裁剪与注入开关打破后，同一个 mNNNNN 会指到别的消息，压缩块遮掉模型当前的工作
// 内容（实测渲染从 765K 塌到 1.9K）。
func TestMessageIDIsStableAcrossListShift(t *testing.T) {
	full := []*proto.Message{
		{Id: "event-10", Role: "user", Content: "最早的需求"},
		{Id: "event-11", Role: "assistant", Content: "好的"},
		{Id: "event-12", Role: "user", Content: "第二步"},
		{Id: "event-13", Role: "assistant", Content: "继续"},
	}
	core := ToCoreMessages(full, nil)
	for i, cm := range core {
		if cm.ID != full[i].Id {
			t.Fatalf("第 %d 条 core id = %q，期望宿主 id %q", i, cm.ID, full[i].Id)
		}
	}

	// 只发后两条（模拟只注入最近 N 条）：id 不变，位置变了
	shifted := ToCoreMessages(full[2:], nil)
	if shifted[0].ID != "event-12" || shifted[1].ID != "event-13" {
		t.Fatalf("列表起点平移后 id 漂移：%q, %q", shifted[0].ID, shifted[1].ID)
	}

	// 往返也必须保住 id（改写链路上任何一环丢了，下一轮就退回按位置寻址）
	back := FromCoreMessages(core)
	for i, pm := range back {
		if pm.Id != full[i].Id {
			t.Fatalf("往返后第 %d 条 id = %q，期望 %q", i, pm.Id, full[i].Id)
		}
	}
}

// TestMissingMessageIDIsNotFabricated 没有稳定 id 的消息不得拿列表下标顶替。
//
// 「缺 id 就退回位置」等于把已经被证明会遮错内容的老方案留在仓库里：一旦某条链路
// 忘了填 id，症状不是报错而是压缩块指到别的消息。空 id 的后果是「这条消息不参与
// 压缩」——局部失效，不破坏别人的内容。
func TestMissingMessageIDIsNotFabricated(t *testing.T) {
	core := ToCoreMessages([]*proto.Message{
		{Role: "user", Content: "a"},
		{Role: "assistant", Content: "b"},
	}, nil)
	for i, cm := range core {
		if cm.ID != "" {
			t.Fatalf("第 %d 条不应被编造 id，实际 %q", i, cm.ID)
		}
	}
}

// TestLoadDiscardsStateWithUnknownSchema 状态文件格式版本不符时整体丢弃，不做逐字段
// 迁移：旧格式里记的消息 id 是列表下标（msg-<i>），与稳定 id 空间对不上，硬迁就等于
// 给已废弃的寻址方式留实现分支。丢弃后由事件日志重建（compress 调用带完整范围与摘要）。
func TestLoadDiscardsStateWithUnknownSchema(t *testing.T) {
	dir := t.TempDir()
	store := NewStateStore(dir)

	old := bcacp.CreateInitialState()
	old.Schema = bcacp.StateSchema - 1
	old.Blocks = []bcacp.CompressionBlock{{
		BlockID:             "b0",
		Active:              true,
		StartRef:            "m00001",
		EndRef:              "m00002",
		DirectMessageIDs:    []string{"msg-1", "msg-2"},
		EffectiveMessageIDs: []string{"msg-1", "msg-2"},
	}}
	old.MessageRefs.ByRaw["msg-1"] = "m00001"
	old.MessageRefs.ByRef["m00001"] = "msg-1"
	if err := store.Save("s1", old); err != nil {
		t.Fatalf("save: %v", err)
	}

	// 新 store 实例：绕开进程内 cache，走真实读盘路径
	store2 := NewStateStore(dir)
	got, err := store2.Load("s1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Schema != bcacp.StateSchema {
		t.Fatalf("应返回当前版本的空状态，实际 schema = %d", got.Schema)
	}
	if len(got.Blocks) != 0 || len(got.MessageRefs.ByRaw) != 0 {
		t.Fatalf("不兼容的旧状态应被丢弃，实际 blocks=%d refs=%d",
			len(got.Blocks), len(got.MessageRefs.ByRaw))
	}
}
