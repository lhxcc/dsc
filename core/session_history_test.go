package core

import (
	"path/filepath"
	"testing"

	"dsc/proto"
	"dsc/session"
)

// TestDisplayCountForInjection 显示条数由 history_injection 单旋钮派生。
func TestDisplayCountForInjection(t *testing.T) {
	cases := []struct {
		name   string
		inject int
		want   int
	}{
		{"注入关闭时显示也关", -1, 0},
		{"任意负值关闭", -5, 0},
		{"未设置(不限制注入)取显示默认", 0, HistoryShowsDefault},
		{"显式 N", 5, 5},
		{"N=1", 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := displayCountForInjection(c.inject); got != c.want {
				t.Fatalf("displayCountForInjection(%d) = %d, want %d", c.inject, got, c.want)
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

// TestLoadDisplayHistoryCount 端到端：注入 N 条 → 回显最近 N 条可显示消息。
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

	// history_injection=4 → 回显最近 4 条：u2 a2 u3 a3
	got := m.LoadDisplayHistory(4)
	want := []string{"u2", "a2", "u3", "a3"}
	if len(got) != len(want) {
		t.Fatalf("LoadDisplayHistory(4) = %d 条 (%+v), want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Content != w {
			t.Fatalf("第 %d 条 = %q, want %q (全部: %+v)", i, got[i].Content, w, got)
		}
	}

	// 注入关闭（<0）→ 显示也关闭 → nil
	if got := m.LoadDisplayHistory(-1); got != nil {
		t.Fatalf("inject<0 应返回 nil, got %+v", got)
	}
}

// TestLoadDisplayHistoryDefaultCap history_injection=0（不限制注入）时，回显被
// 渲染上限 HistoryShowsDefault 截断，避免长会话刷屏。
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

	got := m.LoadDisplayHistory(0)
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
	got := m.LoadDisplayHistory(0)
	want := []string{"u3", "a3"}
	if len(got) != len(want) {
		t.Fatalf("LoadDisplayHistory(0) = %d 条 (%+v), want %d (会话内 limit=2)", len(got), got, len(want))
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
	if got := m.LoadDisplayHistory(0); got != nil {
		t.Fatalf("无会话应返回 nil, got %+v", got)
	}
	var nilMgr *Manager
	if got := nilMgr.LoadDisplayHistory(0); got != nil {
		t.Fatalf("nil manager 应返回 nil, got %+v", got)
	}
}
