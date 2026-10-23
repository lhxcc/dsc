package tui

import (
	"context"
	"os"
	"strings"
	"testing"

	"dsc/core"
	"dsc/session"
)

// newExportFixture 建一个含真实会话文件的 manager，返回 manager、会话 id 与 ExecDir。
func newExportFixture(t *testing.T) (*core.Manager, string, string) {
	t.Helper()
	dir := t.TempDir()
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: dir})
	st, err := session.NewStore(session.Dir(dir))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	sess, err := st.Create()
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sess.Append(session.TurnStart, &session.TurnData{Turn: 1}, nil)
	sess.Append(session.UserMessage, &session.UserMessageData{Content: "hello", Source: "user"}, &session.SurfaceOp{Op: session.SurfaceAppend})
	sess.Append(session.AssistantMessage, &session.AssistantMessageData{Content: "hi"}, &session.SurfaceOp{Op: session.SurfaceAppend})
	if err := st.Save(sess); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return mgr, sess.ID(), dir
}

// TestExportCommandUsesResolvedSessionID 校验 /export 无参时自动纠正会话 id：
// TUI 启动时按工作区路径推导的 id 可能与实际存档名不一致（历史命名、进程早于
// 插件重建等），直接拿推导值会报 session not found。此时应回退到 store 中实际
// 存在（最近活动）的会话并导出。
func TestExportCommandUsesResolvedSessionID(t *testing.T) {
	mgr, id, dir := newExportFixture(t)
	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)
	// 模拟启动推导值与实际存档不一致
	m.currentSessionID = "Users--stale-default-4A0A88E7"

	if handled, _ := m.runSlashCommand("/export"); !handled {
		t.Fatal("/export should be handled")
	}
	full := strings.Join(m.lines, "\n")
	if strings.Contains(full, "导出会话失败") {
		t.Fatalf("export should succeed via agent session id, got: %s", full)
	}
	if !strings.Contains(full, id) {
		t.Fatalf("output should mention resolved session id %q:\n%s", id, full)
	}
	if _, err := os.Stat(core.PJoin(dir, "exports", id+".md")); err != nil {
		t.Fatalf("export file missing: %v", err)
	}
	// 解析结果回写 currentSessionID，后续会话级命令保持一致
	if m.currentSessionID != id {
		t.Fatalf("currentSessionID = %q, want %q", m.currentSessionID, id)
	}
}

// TestExportCommandWithExplicitID 校验 /export <id> 直接导出指定会话。
func TestExportCommandWithExplicitID(t *testing.T) {
	mgr, id, dir := newExportFixture(t)
	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)

	if handled, _ := m.runSlashCommand("/export " + id); !handled {
		t.Fatal("/export <id> should be handled")
	}
	full := strings.Join(m.lines, "\n")
	if strings.Contains(full, "导出会话失败") {
		t.Fatalf("explicit export failed: %s", full)
	}
	if _, err := os.Stat(core.PJoin(dir, "exports", id+".md")); err != nil {
		t.Fatalf("export file missing: %v", err)
	}
}

// TestExportCommandUnknownID 校验未知 id 给出可操作提示且不误报成功。
func TestExportCommandUnknownID(t *testing.T) {
	mgr, _, _ := newExportFixture(t)
	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)

	if handled, _ := m.runSlashCommand("/export ghost-session"); !handled {
		t.Fatal("/export <id> should be handled")
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "导出会话失败") || !strings.Contains(full, "/sessions") {
		t.Fatalf("should report failure with a hint, got: %s", full)
	}
}
