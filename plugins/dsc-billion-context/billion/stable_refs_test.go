package billion

import (
	"encoding/json"
	"strings"
	"testing"
)

// stableMsgs 造 10 条带**稳定 id** 的消息（对应宿主下发的 event-<seq>）。
func stableMsgs() []CoreMessage {
	out := make([]CoreMessage, 0, 10)
	for i := 0; i < 10; i++ {
		out = append(out, CoreMessage{
			ID:   "event-" + string(rune('0'+i)),
			Role: RoleUser,
			Text: "text" + string(rune('0'+i)),
		})
	}
	return out
}

// TestRefsAddressSameMessagesUnderHeadTrim 稳定 id 下，列表被截短不会让引用错位。
//
// 这是「用户随时改配置」的核心保障：history_injection 只带最近 N 条、宿主做请求级
// 裁剪，都会让列表起点平移。旧的按位置寻址（msg-<idx>）在那一刻整体错位——块的
// mNNNNN 指到别的消息，遮掉的是模型当前的工作内容（实测渲染塌到 1.9K，整段对话
// 只剩两条）。稳定 id 下同一个 mNNNNN 永远指同一条消息。
func TestRefsAddressSameMessagesUnderHeadTrim(t *testing.T) {
	config := DefaultConfig(1000000)
	config.PreserveRecent = 0

	full := stableMsgs()
	fullState := CreateInitialState()
	assignAndCompress(t, full, fullState, config)

	// 压缩 e1..e4（refs m00001..m00004）
	covered := fullState.CoveredMessageIDs()
	for _, id := range []string{"event-1", "event-2", "event-3", "event-4"} {
		if !covered[id] {
			t.Fatalf("夹具无效：%s 应被覆盖，实际 %v", id, covered)
		}
	}

	// 模拟宿主裁剪：只发后 6 条（e4..e9）
	trimmed := full[4:]
	trimState := cloneState(t, fullState)
	AssignRefs(trimmed, trimState)

	// 同一个 ref 必须仍指向同一条消息
	for _, id := range []string{"e4", "e5", "e9"} {
		if got := RefForRaw(id, trimState); got != RefForRaw(id, fullState) {
			t.Fatalf("%s 的 ref 变了：全量 %q，裁剪后 %q（引用错位会让块遮错内容）",
				id, RefForRaw(id, fullState), got)
		}
	}

	rendered := RenderMessages(trimmed, trimState, config, false)
	var kept []string
	for _, m := range rendered {
		kept = append(kept, m.Text)
	}
	joined := strings.Join(kept, ",")
	// e4 仍被同一个块覆盖 → 内容必须已隐藏；e5..e9 不在覆盖范围 → 必须原样保留
	if strings.Contains(joined, "text4") {
		t.Fatalf("e4 仍应在覆盖范围内（被摘要替代），实际渲染 = %s", joined)
	}
	for _, want := range []string{"text5", "text6", "text9"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%s 不在覆盖范围内，不应被隐藏；实际渲染 = %s", want, joined)
		}
	}
}

// TestPositionalIDsWouldShiftUnderHeadTrim 反证：id 若仍是列表下标，同一次裁剪会让
// 块遮掉**别的消息**——这正是要迁移掉的那种状态，不是「压不准」而是「遮错」。
func TestPositionalIDsWouldShiftUnderHeadTrim(t *testing.T) {
	config := DefaultConfig(1000000)
	config.PreserveRecent = 0

	full := stableMsgs()
	positional := make([]CoreMessage, len(full))
	for i, m := range full {
		positional[i] = CoreMessage{ID: "msg-" + string(rune('0'+i)), Role: m.Role, Text: m.Text}
	}
	state := CreateInitialState()
	assignAndCompress(t, positional, state, config) // 压缩下标 1..4（text1..text4）
	if covered := state.CoveredMessageIDs(); !covered["msg-1"] || !covered["msg-4"] {
		t.Fatalf("夹具无效：covered = %v", covered)
	}

	// 裁掉前 4 条后，「msg-4」这个 id 承载的内容已经不是 text4 而是 text8
	trimmed := positional[4:]
	if trimmed[4].Text != "text8" {
		t.Fatalf("夹具无效：trimmed[4] = %q", trimmed[4].Text)
	}
	rendered := RenderMessages(trimmed, state, config, false)
	var kept []string
	for _, m := range rendered {
		kept = append(kept, m.Text)
	}
	joined := "," + strings.Join(kept, ",") + ","
	// 本意是遮 text1..text4，实际遮掉的是 text5..text8：模型的当前工作内容被吃掉
	if !strings.Contains(joined, ",text9,") {
		t.Fatalf("text9 应保留，实际 %s", joined)
	}
	for _, wronglyHidden := range []string{"text5", "text6", "text7", "text8"} {
		if !strings.Contains(joined, ","+wronglyHidden+",") {
			t.Fatalf("按位置寻址时 %s 会被误遮（块覆盖的是 msg-5..msg-8，不是它建立时的 msg-1..msg-4）；实际 %s",
				wronglyHidden, joined)
		}
	}
}

// TestRebuildFromHistoryUsesStableIDs 状态文件不可用（格式版本不符/损坏）时，
// 唯一的恢复路径是拿事件日志重建：重放历史的 compress 调用，块覆盖的必须是
// **稳定 id**，而不是旧的位置 id——否则重建出来的块同样一条也盖不住。
//
// 这是「不留兼容分支」的可行性前提：不做旧格式迁移，就必须保证重建足够还原。
func TestRebuildFromHistoryUsesStableIDs(t *testing.T) {
	msgs := []CoreMessage{
		{ID: "event-0", Role: RoleUser, Text: "最早的需求"},
		{ID: "event-1", Role: RoleAssistant, Text: "a1"},
		{ID: "event-2", Role: RoleUser, Text: "u2"},
		{ID: "event-3", Role: RoleAssistant, Text: "a3"},
		{ID: "event-4", Role: RoleAssistant, ContentType: ContentTypeToolCall, ToolName: "compress", ToolCallID: "c1",
			Text: `{"content":[{"startId":"m00001","endId":"m00003","summary":"压缩摘要","topic":"t"}]}`},
		{ID: "event-5", Role: RoleTool, ContentType: ContentTypeToolResult, ToolCallID: "c1",
			Text: `{"ok":true,"blocksCreated":1}`},
	}
	state, n := RebuildStateFromMessages(msgs, DefaultConfig(1000000))
	if n != 1 {
		t.Fatalf("重放出 %d 个块，期望 1", n)
	}
	covered := state.CoveredMessageIDs()
	for _, want := range []string{"event-1", "event-2", "event-3"} {
		if !covered[want] {
			t.Fatalf("重建后应覆盖 %s，实际 %v", want, covered)
		}
	}
	if covered["event-0"] || covered["event-4"] || covered["msg-1"] {
		t.Fatalf("重建后覆盖集合不对：%v", covered)
	}
	if state.Schema != StateSchema {
		t.Fatalf("重建出的状态 schema = %d，期望 %d", state.Schema, StateSchema)
	}
}

// TestStableIDsSurviveJSONRoundTrip 状态落盘/加载不得改变语义（RefScheme 随状态持久化）。
func TestStableIDsSurviveJSONRoundTrip(t *testing.T) {
	state := CreateInitialState()
	AssignRefs(stableMsgs(), state)
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back CompressionState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Schema != StateSchema {
		t.Fatalf("Schema 未持久化：%d", back.Schema)
	}
	if len(back.MessageRefs.ByRaw) != 10 {
		t.Fatalf("refs 丢失：%d", len(back.MessageRefs.ByRaw))
	}
}

// assignAndCompress 压缩 msgs[1:5]（保留首条与尾部），返回建立后的 state。
func assignAndCompress(t *testing.T, msgs []CoreMessage, state *CompressionState, config Config) {
	t.Helper()
	AssignRefs(msgs, state)
	ranges := []PruneRange{{StartRef: "m00001", EndRef: "m00004", Summary: "summary of 1..4"}}
	_, res := ApplyCompression(ranges, msgs, state, config)
	if res.BlocksCreated != 1 {
		t.Fatalf("夹具无效：blocksCreated = %d", res.BlocksCreated)
	}
}

func cloneState(t *testing.T, state *CompressionState) *CompressionState {
	t.Helper()
	b, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var c CompressionState
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &c
}
