package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"dsc/jobs"
)

// waitJobStatus 轮询等待后台任务落定为指定状态。
func waitJobStatus(t *testing.T, m *Manager, id string, want jobs.JobStatus) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snap, err := m.jobs.Get(id, ""); err == nil && snap.Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s did not reach %s", id, want)
}

// ctxBlockingTool 阻塞到 ctx 取消为止，用于验证后台任务取消真正传导到执行体。
type ctxBlockingTool struct {
	started chan struct{}
}

func (t *ctxBlockingTool) Name() string                      { return "ctx_blocking" }
func (t *ctxBlockingTool) Description() string               { return "blocks until ctx is cancelled" }
func (t *ctxBlockingTool) ParametersSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (t *ctxBlockingTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	close(t.started)
	<-ctx.Done()
	return "", ctx.Err()
}

// TestBackgroundJobKillCancelsExecution 回归后台任务取消：job_kill 必须取消
// 执行体的 ctx 并以 killed 落定；且发起回合的 ctx 取消不应影响后台任务。
func TestBackgroundJobKillCancelsExecution(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	tool := &ctxBlockingTool{started: make(chan struct{})}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	turnCtx, cancelTurn := context.WithCancel(context.Background())
	out, _, _, _, err := m.ExecuteToolWithView(turnCtx, "ctx_blocking", json.RawMessage(`{"run_in_background":true}`))
	if err != nil || !strings.Contains(out, "Background job started") {
		t.Fatalf("start background = %q, %v", out, err)
	}
	select {
	case <-tool.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background tool did not start")
	}

	jobsList := m.jobs.ListAll()
	if len(jobsList) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobsList))
	}
	id := jobsList[0].ID

	// 回合 ctx 取消：后台任务必须继续运行。
	cancelTurn()
	time.Sleep(50 * time.Millisecond)
	if snap, err := m.jobs.Get(id, ""); err != nil || snap.Status != jobs.StatusRunning {
		t.Fatalf("background job should survive turn ctx cancel, got %+v, %v", snap, err)
	}

	// job_kill：取消传导到执行体，任务以 killed 落定。
	if _, err := m.jobs.KillHost(id, "test kill"); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitJobStatus(t, m, id, jobs.StatusKilled)
	if snap, _ := m.jobs.Get(id, ""); !strings.Contains(snap.Detail, "test kill") {
		t.Fatalf("kill detail = %q, want reason propagated", snap.Detail)
	}
}

func TestJobToolsRegistered(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	for _, name := range []string{"job_output", "job_list", "job_kill"} {
		tool, ok := m.toolRegistry.Get(name)
		if !ok || tool.Name() != name {
			t.Fatalf("%s should be registered", name)
		}
		if len(tool.ParametersSchema()) == 0 {
			t.Fatalf("%s should have a schema", name)
		}
	}
}

func TestJobTools(t *testing.T) {
	// 这里断言的是「不等」那条路径（立刻回报当前快照）；等首块输出的行为见
	// jobs_output_wait_test.go，单独用短预算去测。
	t.Setenv("DSC_JOB_OUTPUT_WAIT_SECS", "0")
	m := NewManager(&ManagerConfig{})
	get := func(name string) *jobTool {
		tool, _ := m.toolRegistry.Get(name)
		return tool.(*jobTool)
	}

	// 空列表
	out, err := get("job_list").Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || out != "(no background jobs)" {
		t.Fatalf("empty list = %q, %v", out, err)
	}

	// 后台启动一个运行中的任务（无 owner）
	id, _ := m.jobs.Start(jobs.StartSpec{
		Kind:  "workflow",
		Label: "tally",
		Start: func() (jobs.JobHooks, error) {
			done := make(chan jobs.JobOutcome, 1)
			go func() {
				time.Sleep(30 * time.Millisecond)
				done <- jobs.JobOutcome{Status: jobs.StatusCompleted, Output: "done"}
			}()
			return jobs.JobHooks{Done: done}, nil
		},
	})

	// job_list 显示运行中的任务
	out, err = get("job_list").Execute(context.Background(), json.RawMessage(`{}`))
	if err != nil || !strings.Contains(out, id) || !strings.Contains(out, "[workflow] running — tally") {
		t.Fatalf("list = %q, %v", out, err)
	}

	// job_output 非阻塞：运行中 → 无输出 + 存活状态
	out, err = get("job_output").Execute(context.Background(), []byte(`{"job_id":"`+id+`"}`))
	if err != nil || !strings.Contains(out, "(no output yet)") || !strings.Contains(out, "[status: running]") {
		t.Fatalf("output running = %q, %v", out, err)
	}

	// 等待完成 → job_output 返回最终输出
	waitJobStatus(t, m, id, jobs.StatusCompleted)
	out, err = get("job_output").Execute(context.Background(), []byte(`{"job_id":"`+id+`"}`))
	if err != nil || !strings.Contains(out, "done") || !strings.Contains(out, "[status: completed]") {
		t.Fatalf("output done = %q, %v", out, err)
	}

	// 已结束的任务 kill → already finished
	out, err = get("job_kill").Execute(context.Background(), []byte(`{"job_id":"`+id+`"}`))
	if err != nil || !strings.Contains(out, "already finished [status: completed]") {
		t.Fatalf("kill finished = %q, %v", out, err)
	}

	// 未知 job → 错误
	if _, err := get("job_output").Execute(context.Background(), json.RawMessage(`{"job_id":"nope"}`)); err == nil {
		t.Fatal("unknown job should fail")
	}
	if _, err := get("job_kill").Execute(context.Background(), json.RawMessage(`{"job_id":"nope"}`)); err == nil {
		t.Fatal("kill unknown job should fail")
	}

	// 运行中的任务 kill → 请求取消 → 落定 killed
	id2, _ := m.jobs.Start(jobs.StartSpec{
		Kind:  "workflow",
		Label: "hang",
		Start: func() (jobs.JobHooks, error) {
			done := make(chan jobs.JobOutcome, 1)
			cancel := func(reason string) {
				go func() { done <- jobs.JobOutcome{Status: jobs.StatusKilled} }()
			}
			return jobs.JobHooks{Cancel: cancel, Done: done}, nil
		},
	})
	out, err = get("job_kill").Execute(context.Background(), []byte(`{"job_id":"`+id2+`"}`))
	if err != nil || !strings.Contains(out, "requested cancellation of job "+id2) {
		t.Fatalf("kill running = %q, %v", out, err)
	}
	waitJobStatus(t, m, id2, jobs.StatusKilled)
	out, err = get("job_output").Execute(context.Background(), []byte(`{"job_id":"`+id2+`"}`))
	if err != nil || !strings.Contains(out, "[status: killed]") {
		t.Fatalf("output killed = %q, %v", out, err)
	}
}

// TestJobDoneEventBus 验证通用送达：jobs 落定 → 宿主事件总线 JobDoneEvent，
// 任何订阅者（TUI/web/novelforge）都能收到。
func TestJobDoneEventBus(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	got := make(chan jobs.JobSnapshot, 1)
	unsub := m.OnEvent(JobDoneEvent, func(ctx EventContext) (any, error) {
		if s, ok := ctx.Data.(jobs.JobSnapshot); ok {
			got <- s
		}
		return nil, nil
	})
	defer unsub()

	id, _ := m.jobs.Start(jobs.StartSpec{
		Kind: "workflow", Label: "x",
		Start: func() (jobs.JobHooks, error) {
			done := make(chan jobs.JobOutcome, 1)
			go func() { done <- jobs.JobOutcome{Status: jobs.StatusCompleted, Output: "ok"} }()
			return jobs.JobHooks{Done: done}, nil
		},
	})
	select {
	case s := <-got:
		if s.ID != id || s.Status != jobs.StatusCompleted {
			t.Fatalf("event snapshot = %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("JobDoneEvent should be delivered")
	}
}

// TestJobToolsOwnerIsolation 验证工具层 owner 隔离：caller 经 ctx 注入，
// 外来会话被拒绝、无 owner 任务开放。
func TestJobToolsOwnerIsolation(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	get := func(name string) *jobTool {
		tool, _ := m.toolRegistry.Get(name)
		return tool.(*jobTool)
	}
	// 有 owner 的任务（owner=sess-a）
	id, _ := m.jobs.Start(jobs.StartSpec{
		Kind: "workflow", Label: "private", Owner: "sess-a",
		Start: func() (jobs.JobHooks, error) {
			done := make(chan jobs.JobOutcome, 1)
			go func() { done <- jobs.JobOutcome{Status: jobs.StatusCompleted, Output: "secret"} }()
			return jobs.JobHooks{Done: done}, nil
		},
	})
	// owner 可读（先等落定，避免立即完成竞态）
	if _, err := m.jobs.Wait(id, 2*time.Second, "sess-a"); err != nil {
		t.Fatalf("owner wait: %v", err)
	}
	out, err := get("job_output").Execute(WithCaller(context.Background(), "sess-a"), []byte(`{"job_id":"`+id+`"}`))
	if err != nil || !strings.Contains(out, "secret") {
		t.Fatalf("owner read = %q, %v", out, err)
	}
	// 外来会话（含无会话）被拒绝
	for _, caller := range []string{"sess-b", ""} {
		ctx := context.Background()
		if caller != "" {
			ctx = WithCaller(ctx, caller)
		}
		if _, err := get("job_output").Execute(ctx, []byte(`{"job_id":"`+id+`"}`)); err == nil {
			t.Fatalf("caller %q should be denied", caller)
		}
		if _, err := get("job_kill").Execute(ctx, []byte(`{"job_id":"`+id+`"}`)); err == nil {
			t.Fatalf("caller %q should be denied kill", caller)
		}
	}
	// job_list：外来会话看不到私有任务
	out, err = get("job_list").Execute(WithCaller(context.Background(), "sess-b"), json.RawMessage(`{}`))
	if err != nil || strings.Contains(out, id) {
		t.Fatalf("foreign list = %q, %v", out, err)
	}
}

// TestDetachJobContextSurvivesTurnCancel 后台任务不得随回合 ctx 一起被取消：
// 现场表现为 LLM 流中断后 job 变 failed、job_output 返回 context canceled。
func TestDetachJobContextSurvivesTurnCancel(t *testing.T) {
	parent, cancel := context.WithCancel(WithCaller(WithApprovalPolicy(context.Background(), "ask"), "session-7"))
	jobCtx := detachJobContext(parent)
	cancel()

	select {
	case <-jobCtx.Done():
		t.Fatal("后台任务 ctx 不应随回合取消")
	case <-time.After(50 * time.Millisecond):
	}
	// 归属信息必须保留（工具流水线据此判定审批策略与会话）
	if got := CallerFromContext(jobCtx); got != "session-7" {
		t.Fatalf("caller = %q, want session-7", got)
	}
	if got := ApprovalPolicyFromContext(jobCtx); got != "ask" {
		t.Fatalf("approval policy = %q, want ask", got)
	}
}

// TestRenderJobOutputMarksRepeatRead 第二次读同一个任务要明说「内容未变」：
// 最终输出任务是幂等的，模型却不知道（也不记得）自己已经读过，于是反复读——
// 界面上表现为两张一模一样的输出卡，看起来像渲染重复。
func TestRenderJobOutputMarksRepeatRead(t *testing.T) {
	rd := jobs.Read{Text: "boom", Snapshot: jobs.JobSnapshot{Status: jobs.StatusCompleted}, ReadCount: 1}
	first := renderJobOutput(rd, 0)
	if strings.Contains(first, "read #") {
		t.Fatalf("首次读取不应带重复提示: %q", first)
	}
	rd.ReadCount = 2
	second := renderJobOutput(rd, 0)
	if !strings.Contains(second, "read #2") || !strings.Contains(second, "not changed") {
		t.Fatalf("第二次读取应标注重复: %q", second)
	}
	if !strings.Contains(second, "boom") || !strings.Contains(second, "[status: completed]") {
		t.Fatalf("输出与状态行不得被提示挤掉: %q", second)
	}
}
