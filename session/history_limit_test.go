package session

import (
	"testing"

	"dsc/proto"
)

// TestDeriveMessagesLimitedOff 历史注入关闭（0）：恢复来的历史一条不带，
// 但本次运行内产生的轮次完整保留——否则「注入多少历史」这个设置会删掉
// 同一会话里刚发生的对话（实测第二轮直接失忆）。
func TestDeriveMessagesLimitedOff(t *testing.T) {
	s := New()
	appendSystemPrompt(t, s, "sys")
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "q0", Source: "user"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "a0"})
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "q1", Source: "user"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "a1"})
	// 到此为止是从存档恢复来的历史
	s.restoredThrough = s.Len()

	// 本次运行内新产生的一轮
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "q2", Source: "user"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "a2"})

	msgs := s.DeriveMessagesLimited(0)
	if len(msgs) != 1+2 {
		t.Fatalf("off 应保留 system+本次运行的两条, 实际 %d 条: %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "user" || msgs[1].Content != "q2" {
		t.Fatalf("首条应为本次运行的消息, 实际 (%s, %q)", msgs[1].Role, msgs[1].Content)
	}
}

// TestDeriveMessagesLimitedIgnoresLimitWithoutHistory 没有恢复历史时（全新会话、
// 或同一进程的后续轮次），注入上限不生效：全部消息都是本次运行产生的。
//
// 回归：历史注入曾按整个 surface 倒数，于是设成 0 时连当前会话已发生的轮次
// 一起被砍掉，模型在第二轮就看不到第一轮做了什么。
func TestDeriveMessagesLimitedIgnoresLimitWithoutHistory(t *testing.T) {
	s := New()
	appendSystemPrompt(t, s, "sys")
	for _, q := range []string{"q0", "q1", "q2"} {
		appendSurface(t, s, UserMessage, &UserMessageData{Content: q, Source: "user"})
		appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "a-" + q})
	}
	for _, limit := range []int{0, 1, 2} {
		msgs := s.DeriveMessagesLimited(limit)
		if len(msgs) != 1+6 {
			t.Fatalf("limit=%d 无恢复历史时应全量派生, 实际 %d 条", limit, len(msgs))
		}
	}
}

// TestDeriveMessagesLimitedCount 按条数截取恢复历史中最近 N 条，并回退到 user 边界。
func TestDeriveMessagesLimitedCount(t *testing.T) {
	s := New()
	appendSystemPrompt(t, s, "sys")
	for i := 0; i < 5; i++ {
		appendSurface(t, s, UserMessage, &UserMessageData{Content: "q", Source: "user"})
		appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "a"})
	}
	s.restoredThrough = s.Len()

	// 10 条历史；limit=3 → 最近 3 条 [a3,q4,a4] 起点是 assistant，回退到 q3 起，
	// 保留 [q3,a3,q4,a4] = 4 条（完整轮次，工具配对完整）。
	msgs := s.DeriveMessagesLimited(3)
	if len(msgs) != 1+4 {
		t.Fatalf("limit=3 应保留 system+4 条（回退到 user 边界）, 实际 %d 条", len(msgs))
	}
	if msgs[1].Role != "user" {
		t.Fatalf("首条应为 user 边界, 实际 role=%q", msgs[1].Role)
	}
	if msgs[len(msgs)-1].Role != "assistant" {
		t.Fatalf("末条应为最近消息(assistant), 实际 role=%q", msgs[len(msgs)-1].Role)
	}
}

// TestDeriveMessagesLimitedUserBoundary 截断回退到 user 边界，避免落在孤立的 tool
// 结果或未配对 tool_calls 的 assistant 上（Anthropic 要求 tool_use 与 tool_result 成对）。
func TestDeriveMessagesLimitedUserBoundary(t *testing.T) {
	s := New()
	appendSystemPrompt(t, s, "sys")
	// q1 → assistant(tool_call) → tool 结果 → q2 → assistant
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "q1", Source: "user"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "", ToolCalls: []*proto.ToolCall{{Id: "c1", Name: "x"}}})
	appendSurface(t, s, ToolResult, &ToolResultData{CallID: "c1", Content: "r1"})
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "q2", Source: "user"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "a2"})
	s.restoredThrough = s.Len()

	// limit=2 → 最近 2 条 [q2,a2] 起点恰为 user，保留完整
	msgs := s.DeriveMessagesLimited(2)
	if len(msgs) != 1+2 {
		t.Fatalf("期望保留 system+user(q2)+assistant(a2), 实际 %d 条", len(msgs))
	}
	if msgs[1].Role != "user" || msgs[1].Content != "q2" {
		t.Fatalf("首条应为 user(q2), 实际 (%s, %q)", msgs[1].Role, msgs[1].Content)
	}
	if msgs[2].Content != "a2" {
		t.Fatalf("第二条应为 assistant(a2), 实际 %q", msgs[2].Content)
	}
}

// TestDeriveMessagesLimitedKeepsCurrentTurn 当前轮（本次运行新增）永远完整保留：
// limit 只裁剪恢复来的历史，绝不切进正在进行的这一轮。
func TestDeriveMessagesLimitedKeepsCurrentTurn(t *testing.T) {
	s := New()
	appendSystemPrompt(t, s, "sys")
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "q0", Source: "user"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "a0"})
	s.restoredThrough = s.Len()

	// 本次运行：q1 → assistant(tool_call) → tool 结果 → assistant
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "q1", Source: "user"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "", ToolCalls: []*proto.ToolCall{{Id: "c1", Name: "x"}}})
	appendSurface(t, s, ToolResult, &ToolResultData{CallID: "c1", Content: "r1"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Content: "a1"})

	// limit=1 只作用于恢复历史 → 保留历史的最后 1 条并回退到 user(q0)，
	// 当前轮 4 条一条不少
	msgs := s.DeriveMessagesLimited(1)
	if len(msgs) != 1+2+4 {
		t.Fatalf("历史按 limit 裁剪、当前轮完整保留，应为 system+2+4 条，实际 %d 条", len(msgs))
	}
	if msgs[1].Role != "user" || msgs[1].Content != "q0" {
		t.Fatalf("历史首条应回退到 user(q0), 实际 (%s, %q)", msgs[1].Role, msgs[1].Content)
	}
	if msgs[len(msgs)-1].Content != "a1" {
		t.Fatalf("末条应为当前轮最后一条, 实际 %q", msgs[len(msgs)-1].Content)
	}
}

// TestFoldHistoryLimit 折叠 history/limit 事件：最后一条生效；无记录返回 (-1, false)。
func TestFoldHistoryLimit(t *testing.T) {
	s := New()
	limit, found := FoldHistoryLimit(s.Events())
	if found || limit != -1 {
		t.Fatalf("无记录: limit=%d found=%v, 期望 -1/false", limit, found)
	}

	s.Append(HistoryLimit, &HistoryLimitData{Count: 10}, nil)
	s.Append(HistoryLimit, &HistoryLimitData{Count: 0}, nil)
	s.Append(HistoryLimit, &HistoryLimitData{Count: 5}, nil)
	limit, found = FoldHistoryLimit(s.Events())
	if !found || limit != 5 {
		t.Fatalf("最后一条生效: limit=%d found=%v, 期望 5/true", limit, found)
	}
}
