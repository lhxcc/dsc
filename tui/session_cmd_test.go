package tui

import (
	"context"
	"strings"
	"testing"

	"dsc/core"
	"dsc/cron"
	"dsc/session"
)

// TestSessionCommandSwitch 校验 /session <id> 命令触发 agent 会话切换。
func TestSessionCommandSwitch(t *testing.T) {
	ag := &stubAgent{}
	m := New(ag, nil, context.Background(), "m", "minimal", 131072)

	handled, cmd := m.runSlashCommand("/session session-2")
	if !handled {
		t.Fatal("/session should be handled")
	}
	if cmd != nil {
		t.Fatalf("cmd = %v, want nil", cmd)
	}
	if len(ag.switchCalls) != 1 || ag.switchCalls[0] != "session-2" {
		t.Fatalf("switchCalls = %v, want [session-2]", ag.switchCalls)
	}
}

// TestSessionCommandSwitchWithoutArg 校验缺 id 时给出用法提示。
func TestSessionCommandSwitchWithoutArg(t *testing.T) {
	ag := &stubAgent{}
	m := New(ag, nil, context.Background(), "m", "minimal", 131072)

	handled, _ := m.runSlashCommand("/session")
	if !handled {
		t.Fatal("/session should be handled")
	}
	if len(ag.switchCalls) != 0 {
		t.Fatalf("switchCalls = %v, want none", ag.switchCalls)
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "用法") {
		t.Fatalf("should show usage hint, got: %q", full)
	}
}

// TestSessionsCommandLists 校验 /sessions 列出会话（manager 提供目录）。
func TestSessionsCommandLists(t *testing.T) {
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: t.TempDir()})
	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)

	handled, _ := m.runSlashCommand("/sessions")
	if !handled {
		t.Fatal("/sessions should be handled")
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "会话") {
		t.Fatalf("should show session list header, got: %q", full)
	}
}

// TestPlanCommandToggle 校验 /plan 与 /plan off 触发 agent 的 SetPlanMode。
func TestPlanCommandToggle(t *testing.T) {
	ag := &stubAgent{}
	m := New(ag, nil, context.Background(), "m", "minimal", 131072)

	handled, _ := m.runSlashCommand("/plan")
	if !handled {
		t.Fatal("/plan should be handled")
	}
	handled, _ = m.runSlashCommand("/plan off")
	if !handled {
		t.Fatal("/plan off should be handled")
	}
	if len(ag.planCalls) != 2 || ag.planCalls[0] != true || ag.planCalls[1] != false {
		t.Fatalf("planCalls = %v, want [true false]", ag.planCalls)
	}
}

// TestCronCommandAddAndList 校验 /job add 添加任务、/jobs 列出任务。
func TestCronCommandAddAndList(t *testing.T) {
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: t.TempDir()})
	if err := mgr.StartCron(); err != nil {
		t.Fatalf("start jobs: %v", err)
	}
	defer mgr.StopCron()
	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)

	handled, _ := m.runSlashCommand("/cron add 0 8 * * * 写今日日报")
	if !handled {
		t.Fatal("/cron add should be handled")
	}
	list := mgr.ListCronJobs()
	if len(list) != 1 || list[0].Cron != "0 8 * * *" || list[0].Prompt != "写今日日报" || !list[0].Enabled {
		t.Fatalf("jobs = %+v", list)
	}

	handled, _ = m.runSlashCommand("/crons")
	if !handled {
		t.Fatal("/crons should be handled")
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, list[0].ID) {
		t.Fatalf("should list job, got: %q", full)
	}

	// 无效 cron 应报错且不落盘
	if err := mgr.AddCronJob(&cron.Job{Name: "bad", Cron: "nope", Prompt: "x"}); err == nil {
		t.Fatal("invalid cron should fail")
	}
}

// TestSessionCommandNew 校验 /session new 新建会话并切换。
func TestSessionCommandNew(t *testing.T) {
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: t.TempDir()})
	ag := &stubAgent{}
	m := New(ag, mgr, context.Background(), "m", "minimal", 131072)

	if handled, _ := m.runSlashCommand("/session new"); !handled {
		t.Fatal("/session new should be handled")
	}
	if len(ag.switchCalls) != 1 {
		t.Fatalf("switchCalls = %v, want 1", ag.switchCalls)
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "已新建并切换到会话") {
		t.Fatalf("should confirm new session, got: %q", full)
	}
}

// TestSessionCommandDelete 校验 /session delete <id> 删除会话。
func TestSessionCommandDelete(t *testing.T) {
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: t.TempDir()})
	// 先建一个会话供删除
	id, err := mgr.CreateSession()
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)

	if handled, _ := m.runSlashCommand("/session delete " + id); !handled {
		t.Fatal("/session delete should be handled")
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "已删除会话") {
		t.Fatalf("should confirm delete, got: %q", full)
	}
	// 验证文件已被删除
	summaries, err := mgr.ListSessions()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, s := range summaries {
		if s.ID == id {
			t.Fatalf("session %s should be deleted", id)
		}
	}
}

// TestSandboxCommandToggle 校验 /sandbox 三档切换运行时沙箱策略（on/off 为兼容别名）。
func TestSandboxCommandToggle(t *testing.T) {
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: t.TempDir()})
	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)

	// 初始为缺省 workspace-write
	if got := mgr.GetSandboxPolicy(); got != core.SandboxWorkspaceWrite {
		t.Fatalf("initial policy = %v, want workspace", got)
	}
	if handled, _ := m.runSlashCommand("/sandbox read-only"); !handled {
		t.Fatal("/sandbox read-only should be handled")
	}
	if got := mgr.GetSandboxPolicy(); got != core.SandboxReadOnly {
		t.Fatalf("after read-only, policy = %v, want read-only", got)
	}
	// 切回 workspace-write 档（此前 TUI 无法切回的缺省档）
	if handled, _ := m.runSlashCommand("/sandbox workspace"); !handled {
		t.Fatal("/sandbox workspace should be handled")
	}
	if got := mgr.GetSandboxPolicy(); got != core.SandboxWorkspaceWrite {
		t.Fatalf("after workspace, policy = %v, want workspace", got)
	}
	if handled, _ := m.runSlashCommand("/sandbox full-access"); !handled {
		t.Fatal("/sandbox full-access should be handled")
	}
	if got := mgr.GetSandboxPolicy(); got != core.SandboxFullAccess {
		t.Fatalf("after full-access, policy = %v, want full", got)
	}
	// on/off 兼容别名
	if handled, _ := m.runSlashCommand("/sandbox on"); !handled {
		t.Fatal("/sandbox on should be handled")
	}
	if got := mgr.GetSandboxPolicy(); got != core.SandboxReadOnly {
		t.Fatalf("after on, policy = %v, want read-only", got)
	}
	if handled, _ := m.runSlashCommand("/sandbox off"); !handled {
		t.Fatal("/sandbox off should be handled")
	}
	if got := mgr.GetSandboxPolicy(); got != core.SandboxFullAccess {
		t.Fatalf("after off, policy = %v, want full", got)
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "沙箱") {
		t.Fatalf("should show sandbox messages, got: %q", full)
	}
}

// TestSandboxCommandWithoutManager 校验 manager 缺失时的错误分支。
func TestSandboxCommandWithoutManager(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	if handled, _ := m.runSlashCommand("/sandbox on"); !handled {
		t.Fatal("/sandbox on should be handled")
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "插件管理器不可用") {
		t.Fatalf("should show manager-unavailable error, got: %q", full)
	}
}

// TestApprovalCommandToggle 校验 /approval 切换审批策略（on/off 为 ask/never 别名）。
func TestApprovalCommandToggle(t *testing.T) {
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: t.TempDir()})
	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)

	if got := mgr.GetSessionApprovalPolicy(m.currentSessionID); got != core.ApprovalAsk {
		t.Fatalf("initial policy = %v, want ask", got)
	}
	if handled, _ := m.runSlashCommand("/approval never"); !handled {
		t.Fatal("/approval never should be handled")
	}
	if got := mgr.GetSessionApprovalPolicy(m.currentSessionID); got != core.ApprovalNever {
		t.Fatalf("after never, policy = %v, want never", got)
	}
	if handled, _ := m.runSlashCommand("/approval ask"); !handled {
		t.Fatal("/approval ask should be handled")
	}
	if got := mgr.GetSessionApprovalPolicy(m.currentSessionID); got != core.ApprovalAsk {
		t.Fatalf("after ask, policy = %v, want ask", got)
	}
	// off/on 兼容别名
	if handled, _ := m.runSlashCommand("/approval off"); !handled {
		t.Fatal("/approval off should be handled")
	}
	if got := mgr.GetSessionApprovalPolicy(m.currentSessionID); got != core.ApprovalNever {
		t.Fatalf("after off, policy = %v, want never", got)
	}
	if handled, _ := m.runSlashCommand("/approval on"); !handled {
		t.Fatal("/approval on should be handled")
	}
	if got := mgr.GetSessionApprovalPolicy(m.currentSessionID); got != core.ApprovalAsk {
		t.Fatalf("after on, policy = %v, want ask", got)
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "审批") {
		t.Fatalf("should show approval messages, got: %q", full)
	}
}

// TestApprovalCommandWithoutManager 校验 manager 缺失时的错误分支。
func TestApprovalCommandWithoutManager(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	if handled, _ := m.runSlashCommand("/approval on"); !handled {
		t.Fatal("/approval on should be handled")
	}
	full := strings.Join(m.lines, "\n")
	if !strings.Contains(full, "插件管理器不可用") {
		t.Fatalf("should show manager-unavailable error, got: %q", full)
	}
}

// TestSessionsCommandShowsTurnsTimeAndCurrent 会话列表应展示真实语义：
// 轮次（turn 计数）、最后活动时间、最近一条用户消息，并标记当前会话；
// 切换示例必须是真实 id（不再是虚构的 session-3）。
func TestSessionsCommandShowsTurnsTimeAndCurrent(t *testing.T) {
	dir := t.TempDir()
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: dir})
	st, err := session.NewStore(session.Dir(dir))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	s1, _ := st.Create()
	s1.Append(session.TurnStart, &session.TurnData{Turn: 1}, nil)
	s1.Append(session.UserMessage, &session.UserMessageData{Content: "first topic", Source: "user"}, &session.SurfaceOp{Op: session.SurfaceAppend})
	s1.Append(session.TurnStart, &session.TurnData{Turn: 2}, nil)
	s1.Append(session.UserMessage, &session.UserMessageData{Content: "latest question", Source: "user"}, &session.SurfaceOp{Op: session.SurfaceAppend})
	if err := st.Save(s1); err != nil {
		t.Fatalf("save s1: %v", err)
	}
	s2, _ := st.Create()
	s2.Append(session.TurnStart, &session.TurnData{Turn: 1}, nil)
	s2.Append(session.UserMessage, &session.UserMessageData{Content: "other", Source: "user"}, &session.SurfaceOp{Op: session.SurfaceAppend})
	if err := st.Save(s2); err != nil {
		t.Fatalf("save s2: %v", err)
	}

	m := New(&stubAgent{}, mgr, context.Background(), "m", "minimal", 131072)
	m.currentSessionID = s1.ID()
	if handled, _ := m.runSlashCommand("/sessions"); !handled {
		t.Fatal("/sessions should be handled")
	}
	full := strings.Join(m.lines, "\n")
	for _, want := range []string{
		"● " + s1.ID(),
		"2 轮",
		"latest question",
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("sessions output missing %q:\n%s", want, full)
		}
	}
	// 切换示例必须是列表里真实存在的 id（顺序按最后活动时间，故两条都接受）。
	if !strings.Contains(full, "/session "+s1.ID()) && !strings.Contains(full, "/session "+s2.ID()) {
		t.Fatalf("hint should reference a real session id:\n%s", full)
	}
	if strings.Contains(full, "session-3") {
		t.Fatalf("hint must not reference a fabricated id:\n%s", full)
	}
}
