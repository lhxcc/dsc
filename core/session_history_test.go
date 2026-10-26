package core

import (
	"path/filepath"
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

// TestDisplayableMessages 只保留有正文的 user/assistant，过滤 system/tool/空正文。
func TestDisplayableMessages(t *testing.T) {
	msgs := []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "u"},
		{Role: "tool", Content: "t"},
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: "   "},
	}
	got := displayableMessages(msgs)
	if len(got) != 2 || got[0].Role != "user" || got[1].Role != "assistant" {
		t.Fatalf("displayableMessages = %+v, want [user, assistant]", got)
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

// TestSeedSessionPromptFromLog 重启恢复时从会话日志恢复上次成功请求的 prompt
// 用量，供 pre-step 的 LastUsageTokens 使用（避免首发把全量估算当已用）。
func TestSeedSessionPromptFromLog(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	s := session.New()
	s.Append(session.LLMAttempt, &session.LLMAttemptData{Turn: 1, Step: 1,
		Usage: &proto.Usage{PromptTokens: 40347, TotalTokens: 40864}}, nil)
	// 之后一次失败：无 prompt 用量，不应覆盖
	s.Append(session.LLMAttempt, &session.LLMAttemptData{Turn: 2, Step: 1,
		Error: "context_window_exceeded"}, nil)

	const key = "sess-x"
	m.seedSessionPromptFromLog(key, s)
	if got := m.lastSessionPrompt(key); got != 40347 {
		t.Fatalf("lastSessionPrompt = %d, want 40347", got)
	}
}
