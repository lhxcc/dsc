package core

import (
	"path/filepath"
	"strings"
	"testing"

	"dsc/proto"
	"dsc/session"
)

// TestResolveHistoryShowsCount 显示条数解析：受注入层门控 + 自身三值语义。
func TestResolveHistoryShowsCount(t *testing.T) {
	cases := []struct {
		name   string
		shows  int
		inject int
		want   int
	}{
		{"注入关闭时显示也关", 10, -1, 0},
		{"注入关闭且默认", 0, -1, 0},
		{"显式关闭显示", -1, 0, 0},
		{"未设置取默认", 0, 0, HistoryShowsDefault},
		{"显式 N", 5, 0, 5},
		{"注入限量不影响显示请求(最终由条数封顶)", 0, 3, HistoryShowsDefault},
		{"显示请求大于注入量(最终由条数封顶)", 100, 3, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveHistoryShowsCount(c.shows, c.inject); got != c.want {
				t.Fatalf("resolveHistoryShowsCount(%d,%d) = %d, want %d", c.shows, c.inject, got, c.want)
			}
		})
	}
}

// TestDisplayableMessages 回放条目的基本筛选：system 与空正文丢弃，
// user/assistant 文本保留，tool 结果保留为结果卡（过程必须可见，见
// TestDisplayableMessagesExpandsToolProcess）。
func TestDisplayableMessages(t *testing.T) {
	msgs := []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u"},
		{Role: "tool", Content: "t"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "   "},
	}
	got := displayableMessages(msgs)
	if len(got) != 3 {
		t.Fatalf("displayableMessages = %+v, want 3 条（user + 工具结果 + assistant）", got)
	}
	if got[0].Role != "user" || got[1].Kind != DebugKindToolResult || got[2].Role != "assistant" {
		t.Fatalf("条目类型不符: %+v", got)
	}
}

// stageDisplaySession 在临时 ExecDir 里造一个项目会话并落盘，返回 Manager。
func stageDisplaySession(t *testing.T, events func(sess *session.Session)) *Manager {
	t.Helper()
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	st, err := session.NewStore(filepath.Join(m.config.ExecDir, "sessions"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess, err := st.Ensure(session.SessionKeyForProject(WorkspaceRoot))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	events(sess)
	if err := st.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	return m
}

func appendUser(sess *session.Session, content string) {
	sess.Append(session.UserMessage, &session.UserMessageData{Content: content, Source: "user"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
}

func appendAssistant(sess *session.Session, content string) {
	sess.Append(session.AssistantMessage, &session.AssistantMessageData{Content: content},
		&session.SurfaceOp{Op: session.SurfaceAppend})
}

// TestLoadDisplayHistoryCount 显示条数由 history_shows 控制，与注入量独立。
func TestLoadDisplayHistoryCount(t *testing.T) {
	origRoot := WorkspaceRoot
	WorkspaceRoot = t.TempDir()
	defer func() { WorkspaceRoot = origRoot }()

	m := stageDisplaySession(t, func(sess *session.Session) {
		appendUser(sess, "u1")
		appendAssistant(sess, "a1")
		appendUser(sess, "u2")
		appendAssistant(sess, "a2")
		appendUser(sess, "u3")
		appendAssistant(sess, "a3")
	})

	cases := []struct {
		name        string
		inject      int
		shows       int
		wantContent []string
	}{
		// 注入不限制、显示最近 4 条
		{"显示4条", 0, 4, []string{"u2", "a2", "u3", "a3"}},
		// 注入最近 4 条、显示默认 20（被实际注入量截断为 4）
		{"注入4显示默认", 4, 0, []string{"u2", "a2", "u3", "a3"}},
		// 注入最近 2 条、显示 1 条（显示可独立调小）
		{"注入2显示1", 2, 1, []string{"a3"}},
		// 注入不限制、显示 2 条
		{"显示2条", 0, 2, []string{"u3", "a3"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := m.LoadDisplayHistory(c.inject, c.shows)
			if len(got) != len(c.wantContent) {
				t.Fatalf("LoadDisplayHistory(%d,%d) = %d 条 (%+v), want %d",
					c.inject, c.shows, len(got), got, len(c.wantContent))
			}
			for i, w := range c.wantContent {
				if got[i].Content != w {
					t.Fatalf("第 %d 条 = %q, want %q (全部: %+v)", i, got[i].Content, w, got)
				}
			}
		})
	}

	// 注入关闭（<0）→ 显示不起作用 → nil
	if got := m.LoadDisplayHistory(-1, 4); got != nil {
		t.Fatalf("inject<0 应返回 nil, got %+v", got)
	}
	// 显示显式关闭（<0）→ nil，且与注入是否开启无关
	if got := m.LoadDisplayHistory(0, -1); got != nil {
		t.Fatalf("shows<0 应返回 nil, got %+v", got)
	}
}

// TestLoadDisplayHistoryShowsCappedByInjection 显示请求大于注入量时，以实际注入量封顶。
func TestLoadDisplayHistoryShowsCappedByInjection(t *testing.T) {
	origRoot := WorkspaceRoot
	WorkspaceRoot = t.TempDir()
	defer func() { WorkspaceRoot = origRoot }()

	m := stageDisplaySession(t, func(sess *session.Session) {
		appendUser(sess, "u1")
		appendAssistant(sess, "a1")
		appendUser(sess, "u2")
		appendAssistant(sess, "a2")
		appendUser(sess, "u3")
		appendAssistant(sess, "a3")
	})

	// 注入最近 2 条、显示 20 条 → 只能显示实际注入的 2 条
	got := m.LoadDisplayHistory(2, 20)
	want := []string{"u3", "a3"}
	if len(got) != len(want) {
		t.Fatalf("LoadDisplayHistory(2,20) = %d 条 (%+v), want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Content != w {
			t.Fatalf("第 %d 条 = %q, want %q (全部: %+v)", i, got[i].Content, w, got)
		}
	}
}

// TestLoadDisplayHistoryDefaultCap history_shows=0（未设置）时回显被默认上限截断。
func TestLoadDisplayHistoryDefaultCap(t *testing.T) {
	origRoot := WorkspaceRoot
	WorkspaceRoot = t.TempDir()
	defer func() { WorkspaceRoot = origRoot }()

	m := stageDisplaySession(t, func(sess *session.Session) {
		for i := 1; i <= 15; i++ { // 15 对 = 30 条，超过默认上限 20
			appendUser(sess, "u")
			appendAssistant(sess, "a")
		}
	})

	got := m.LoadDisplayHistory(0, 0)
	if len(got) != HistoryShowsDefault {
		t.Fatalf("默认回显 %d 条, want %d", len(got), HistoryShowsDefault)
	}
}

// TestLoadDisplayHistoryRespectsSessionLimit 会话内 history/limit 覆盖配置缺省，
// 且显示不超过该注入量。
func TestLoadDisplayHistoryRespectsSessionLimit(t *testing.T) {
	origRoot := WorkspaceRoot
	WorkspaceRoot = t.TempDir()
	defer func() { WorkspaceRoot = origRoot }()

	m := stageDisplaySession(t, func(sess *session.Session) {
		appendUser(sess, "u1")
		appendAssistant(sess, "a1")
		appendUser(sess, "u2")
		appendAssistant(sess, "a2")
		appendUser(sess, "u3")
		appendAssistant(sess, "a3")
		// /settings history 2 持久化的会话级上限（log-only）
		sess.Append(session.HistoryLimit, &session.HistoryLimitData{Count: 2}, nil)
	})

	// 配置注入=0（不限制），但会话内 limit=2 优先；回显也被限制到实际注入的 2 条。
	got := m.LoadDisplayHistory(0, 0)
	want := []string{"u3", "a3"}
	if len(got) != len(want) {
		t.Fatalf("LoadDisplayHistory(0,0) = %d 条 (%+v), want %d (会话内 limit=2)", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Content != w {
			t.Fatalf("第 %d 条 = %q, want %q (全部: %+v)", i, got[i].Content, w, got)
		}
	}
}

// TestLoadDisplayHistoryNoSession 无会话文件时返回 nil（新项目首次启动）。
func TestLoadDisplayHistoryNoSession(t *testing.T) {
	origRoot := WorkspaceRoot
	WorkspaceRoot = t.TempDir()
	defer func() { WorkspaceRoot = origRoot }()

	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	if got := m.LoadDisplayHistory(0, 20); got != nil {
		t.Fatalf("无会话应返回 nil, got %+v", got)
	}
	var nilMgr *Manager
	if got := nilMgr.LoadDisplayHistory(0, 20); got != nil {
		t.Fatalf("nil manager 应返回 nil, got %+v", got)
	}
}

// TestLoadHistoryReplay 全量历史 + 初始条数：Messages 为全量可显示消息（供向上
// 翻页），Initial 受 history_shows 与注入量共同封顶。
func TestLoadHistoryReplay(t *testing.T) {
	origRoot := WorkspaceRoot
	WorkspaceRoot = t.TempDir()
	defer func() { WorkspaceRoot = origRoot }()

	m := stageDisplaySession(t, func(sess *session.Session) {
		appendUser(sess, "u1")
		appendAssistant(sess, "a1")
		appendUser(sess, "u2")
		appendAssistant(sess, "a2")
		appendUser(sess, "u3")
		appendAssistant(sess, "a3")
	})

	// 注入最近 2 条、显示默认 20 → Initial 被注入量封顶为 2；Messages 仍是全量 6。
	r := m.LoadHistoryReplay(2, 0)
	if r == nil {
		t.Fatal("LoadHistoryReplay(2,0) = nil")
	}
	if len(r.Messages) != 6 {
		t.Fatalf("Messages 应为全量 6 条, got %d", len(r.Messages))
	}
	if r.Initial != 2 {
		t.Fatalf("Initial 应被注入量封顶为 2, got %d", r.Initial)
	}

	// 注入不限制、显示 3 → Initial=3。
	r = m.LoadHistoryReplay(0, 3)
	if r == nil || r.Initial != 3 || len(r.Messages) != 6 {
		t.Fatalf("LoadHistoryReplay(0,3) = %+v, want Initial=3, Messages=6", r)
	}

	// 显示关闭 / 注入关闭 → nil。
	if r := m.LoadHistoryReplay(0, -1); r != nil {
		t.Fatalf("shows<0 应返回 nil, got %+v", r)
	}
	if r := m.LoadHistoryReplay(-1, 20); r != nil {
		t.Fatalf("inject<0 应返回 nil, got %+v", r)
	}
}

// TestSeedSessionPromptFromLog 重启恢复时从会话日志恢复上次成功请求的用量锚点
// （prompt 用量 + 信封指纹 + 当次估算），供 pre-step 计算投影压力。
func TestSeedSessionPromptFromLog(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	s := session.New()
	// 信封（request/header）与用量（llm/attempt）分开落，按 Turn/Step 配对。
	s.Append(session.RequestHeader, &session.RequestHeaderData{
		Turn: 1, Step: 1, ToolsHash: "abc123", Model: "deepseek-test",
		ContextWindow: 1048576, SentEstimate: 37790}, nil)
	s.Append(session.LLMAttempt, &session.LLMAttemptData{Turn: 1, Step: 1,
		Usage: &proto.Usage{PromptTokens: 40347, TotalTokens: 40864}}, nil)
	// 之后一次失败：无 prompt 用量，不应覆盖
	s.Append(session.LLMAttempt, &session.LLMAttemptData{Turn: 2, Step: 1,
		Error: "context_window_exceeded"}, nil)

	const key = "sess-x"
	m.seedSessionPromptFromLog(key, s)
	a, ok := m.lastSessionAnchor(key)
	if !ok || a.promptTokens != 40347 {
		t.Fatalf("lastSessionAnchor = %+v ok=%v, want promptTokens=40347", a, ok)
	}
	// 信封指纹必须一并恢复，否则重启后首发的锚点会被判为不可比。
	if a.toolsHash != "abc123" || a.sentEstimate != 37790 || a.contextWindow != 1048576 || a.model != "deepseek-test" {
		t.Fatalf("锚点信封字段未恢复: %+v", a)
	}
	// 同时预置展示读数：恢复有历史的会话时，「已用」不该在用户发出第一个请求之前
	// 一直显示 0——上下文里已经躺着这段历史了。
	if got := m.ContextTokens(key); got != 40347 {
		t.Fatalf("启动预置的已用 = %d，want 40347（历史已注入，不该显示 0）", got)
	}
}

// TestDisplayableMessagesExpandsToolProcess 回放必须带上工具过程：
// 助手正文 → 它的每个调用卡 → 对应的结果卡（失败按 exit_code 标红）。
// 此前只挑 user/assistant 正文，重启后只剩「结论」，看不到模型改代码/跑命令的过程。
func TestDisplayableMessagesExpandsToolProcess(t *testing.T) {
	msgs := []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "看看这个文件"},
		// 只有工具调用、没有正文的助手消息：不得产生空气泡，但调用卡必须在
		{Role: "assistant", ToolCalls: []*proto.ToolCall{{
			Name:          "str_replace_editor",
			ArgumentsJson: `{"command":"str_replace","path":"a.go"}`,
		}}},
		{Role: "tool", Content: "done\n[exit_code: 0]\n"},
		{Role: "tool", Content: "boom\n[exit_code: 2]\n"},
		{Role: "assistant", Content: "改完了"},
	}

	got := displayableMessages(msgs)
	if len(got) != 5 {
		t.Fatalf("条目数 = %d, want 5（user + 调用卡 + 两个结果卡 + assistant）: %+v", len(got), got)
	}
	if got[0].Role != "user" || got[0].Kind != "" {
		t.Fatalf("第 1 条应为 user 气泡: %+v", got[0])
	}
	if got[1].Role != "tool" || got[1].Kind != DebugKindToolCall || got[1].ToolName != "str_replace_editor" {
		t.Fatalf("第 2 条应为工具调用卡: %+v", got[1])
	}
	if !strings.Contains(got[1].ToolArgs, "str_replace") {
		t.Fatalf("调用卡应带参数 JSON: %+v", got[1])
	}
	if got[2].Kind != DebugKindToolResult || got[2].IsError {
		t.Fatalf("exit_code: 0 应判成功: %+v", got[2])
	}
	if got[3].Kind != DebugKindToolResult || !got[3].IsError {
		t.Fatalf("exit_code: 2 应判失败: %+v", got[3])
	}
	if got[4].Role != "assistant" || got[4].Content != "改完了" {
		t.Fatalf("末条应为 assistant 气泡: %+v", got[4])
	}
}

// TestReplayInitialKeepsToolsWithTheirMessage history_shows 数的是**对话条数**：
// 工具卡不占名额，但要随所属消息一起进入初始切片（否则重启后首屏只有一两句结论）。
func TestReplayInitialKeepsToolsWithTheirMessage(t *testing.T) {
	entries := []*AgentDebugMessage{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1"},
		{Role: "tool", Kind: DebugKindToolCall, ToolName: "x"},
		{Role: "tool", Kind: DebugKindToolResult, Content: "r1"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
		{Role: "tool", Kind: DebugKindToolCall, ToolName: "y"},
		{Role: "tool", Kind: DebugKindToolResult, Content: "r2"},
	}
	if n := countConversationMessages(entries); n != 4 {
		t.Fatalf("对话条数 = %d, want 4（工具卡不算）", n)
	}
	// 最近 2 条对话消息 = u2 + a2，连同它们的 2 张卡 = 4 个条目
	if n := tailEntryCountForMessages(entries, 2); n != 4 {
		t.Fatalf("最近 2 条消息覆盖 %d 个条目, want 4", n)
	}
	if n := tailEntryCountForMessages(entries, 1); n != 3 {
		t.Fatalf("最近 1 条消息应覆盖 a2 + 两张卡 = 3 个条目, got %d", n)
	}
	if n := tailEntryCountForMessages(entries, 99); n != len(entries) {
		t.Fatalf("请求超过总条数时应返回全部条目, got %d", n)
	}
	if n := tailEntryCountForMessages(entries, 0); n != 0 {
		t.Fatalf("0 条应返回 0, got %d", n)
	}
}
