package billion

import (
	"strings"
	"testing"
)

// 本文件覆盖 ProcessTurn 新接入的三个 pipeline 节点（此前只跑 4 步）：
// sync-blocks（块可见性校正）、hide-compress-calls（折叠已消费压缩调用）、
// emergency-truncate（最后安全阀）。

// TestProcessTurnSyncsBlocks 视图里已不存在的块必须停用，否则它的 summary 会作为
// 「幽灵摘要」继续注入上下文（宿主裁剪历史 / 会话 fork / 块被消费后都可能发生）。
func TestProcessTurnSyncsBlocks(t *testing.T) {
	state := CreateInitialState()
	config := DefaultConfig(100000)

	msgs := []CoreMessage{
		{ID: "u1", Role: RoleUser, Text: "hello"},
		{ID: "a1", Role: RoleAssistant, Text: "hi"},
	}
	AssignRefs(msgs, state)
	state.Blocks = append(state.Blocks, CompressionBlock{
		BlockID: "b0", Tier: Tier1, Active: true, Summary: "early talk",
		StartRef: "m00000", EndRef: "m00001", EffectiveMessageIDs: []string{"u1", "a1"},
	})

	// 视图里已完全没有该块的消息（等价于历史被裁剪 / history_injection 只带最近 N 条）
	gone := []CoreMessage{{ID: "x1", Role: RoleUser, Text: "fresh start"}}
	res := ProcessTurn(gone, state, config, 100)

	if len(state.Blocks) != 1 {
		t.Fatalf("块不应被删除，got %d", len(state.Blocks))
	}
	// 缺席 ≠ 消失：停用会写回状态，用户把配置改回来（或换回大窗口）也复原不了
	// 这些压缩收益。视图是临时的，状态必须留着。
	if !state.Blocks[0].Active {
		t.Fatal("块覆盖的消息只是本轮缺席，不得永久停用")
	}
	// 但摘要也不能注入：模型看不到原文，一段「幽灵摘要」白占前缀。
	for _, m := range res.Messages {
		if strings.Contains(m.Text, "early talk") {
			t.Fatalf("本轮视图不含该块的消息，不应注入其摘要：%q", m.Text)
		}
	}
}

// TestProcessTurnHidesConsumedCompressCalls 已被消费（对应块不再 active）的 compress
// 调用对必须从视图移除：否则 summary 在「块」与「调用参数」里各存一份，压缩收益被抵消。
func TestProcessTurnHidesConsumedCompressCalls(t *testing.T) {
	state := CreateInitialState()
	config := DefaultConfig(100000)

	msgs := []CoreMessage{
		{ID: "u1", Role: RoleUser, Text: "start"},
		{ID: "c1", Role: RoleAssistant, ContentType: ContentTypeToolCall, ToolName: "compress", ToolCallID: "call_c1", Text: `{"content":[]}`},
		{ID: "r1", Role: RoleTool, ContentType: ContentTypeToolResult, ToolCallID: "call_c1", Text: "compressed 3 ranges"},
		{ID: "u2", Role: RoleUser, Text: "next"},
	}
	AssignRefs(msgs, state)
	// 真实场景：b0 被蒸馏进 b1（b1.DirectBlockIDs 引用 b0）→ b0 被消费、保持 inactive。
	// 只写一个 inactive 块是不够的：sync-blocks 会把「原始消息仍在视图里」的块复活。
	state.Blocks = append(state.Blocks,
		CompressionBlock{
			BlockID: "b0", Tier: Tier1, Active: false, Summary: "early",
			CompressCallID: "call_c1", StartRef: "m00000", EndRef: "m00001",
			EffectiveMessageIDs: []string{"u1"},
		},
		CompressionBlock{
			BlockID: "b1", Tier: Tier2, Active: true, Summary: "distilled",
			DirectBlockIDs: []string{"b0"}, EffectiveMessageIDs: []string{"u1"},
		},
	)

	out := ProcessTurn(msgs, state, config, 100).Messages
	for _, m := range out {
		if m.ToolCallID == "call_c1" {
			t.Fatalf("已消费的 compress 调用对不应留在视图里: %+v", m)
		}
	}
}

// TestProcessTurnEmergencyTruncates 上下文超过截断线（默认 0.95）且存在超大工具输出时，
// 必须截断它以避开撞窗口——这是 DESIGN 的最后安全阀。
func TestProcessTurnEmergencyTruncates(t *testing.T) {
	state := CreateInitialState()
	config := DefaultConfig(1000) // 小窗口便于触发

	huge := strings.Repeat("x", 8000) // 约 2000 token，远超窗口
	msgs := []CoreMessage{
		{ID: "t0", Role: RoleTool, ContentType: ContentTypeToolResult, ToolCallID: "call_big", Text: huge},
	}
	// 远端保护：最后 PreserveRecentMessages 条不参与，故补足短消息把大输出推到近端之外
	for i := 0; i < config.PreserveRecentMessages+1; i++ {
		msgs = append(msgs, CoreMessage{ID: "n" + string(rune('a'+i)), Role: RoleUser, Text: "ok"})
	}

	out := ProcessTurn(msgs, state, config, 990).Messages // 990/1000 = 0.99 > 0.95
	var found *CoreMessage
	for i := range out {
		if out[i].ID == "t0" {
			found = &out[i]
		}
	}
	if found == nil {
		t.Fatal("截断不应删除消息本身")
	}
	if len(found.Text) >= len(huge) {
		t.Fatalf("超大工具输出应被截断，长度仍为 %d", len(found.Text))
	}
}

// TestProcessTurnHidesAbsorbedMessages 已吸收的 tool-call/result 对必须从视图移除：
// 此前 state.Absorbed 在渲染链路里零引用，工具回报"已隐藏"但原文照旧发送。
func TestProcessTurnHidesAbsorbedMessages(t *testing.T) {
	config := DefaultConfig(100000)
	config.Absorb.Enabled = true
	state := CreateInitialState()

	msgs := []CoreMessage{
		{ID: "c1", Role: RoleAssistant, ContentType: ContentTypeToolCall, ToolName: "shell", ToolCallID: "call_1", Text: `{"command":"ls"}`},
		{ID: "r1", Role: RoleTool, ContentType: ContentTypeToolResult, ToolName: "shell", ToolCallID: "call_1", Text: "huge output"},
		{ID: "u2", Role: RoleUser, Text: "next"},
	}
	AssignRefs(msgs, state)
	state.Absorbed = append(state.Absorbed, AbsorbRecord{
		ToolCallID: "call_1", CallMessageID: "c1", ResultMessageID: "r1", Summary: "distilled",
	})

	out := ProcessTurn(msgs, state, config, 100).Messages
	for _, m := range out {
		if m.ID == "c1" || m.ID == "r1" {
			t.Fatalf("已吸收的消息对不应出现在视图里: %+v", m)
		}
	}
}

// TestProcessTurnAppendsAbsorbPrompt 达标的大工具结果应被追加 [billion absorb] 提示，
// 否则模型无从知道可以吸收（工具描述本身就要求"看到标记才调用"）。
func TestProcessTurnAppendsAbsorbPrompt(t *testing.T) {
	config := DefaultConfig(100000)
	config.Absorb.Enabled = true
	config.Absorb.MinToolTokens = 10
	state := CreateInitialState()

	msgs := []CoreMessage{
		{ID: "r1", Role: RoleTool, ContentType: ContentTypeToolResult, ToolName: "shell", ToolCallID: "call_1",
			Text: strings.Repeat("y", 400)},
		{ID: "u2", Role: RoleUser, Text: "next"},
	}
	AssignRefs(msgs, state)

	out := ProcessTurn(msgs, state, config, 100).Messages
	found := false
	for _, m := range out {
		if m.ID == "r1" && strings.Contains(m.Text, AbsorbPromptMarker) {
			found = true
		}
	}
	if !found {
		t.Fatal("候选工具结果应追加 absorb 提示")
	}

	// 未启用时不得追加（避免默认开启）
	config.Absorb.Enabled = false
	out = ProcessTurn(msgs, state, config, 100).Messages
	for _, m := range out {
		if m.ID == "r1" && strings.Contains(m.Text, AbsorbPromptMarker) {
			t.Fatal("未启用 absorb 时不应追加提示")
		}
	}
}
