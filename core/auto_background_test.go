package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"dsc/jobs"
)

// graceTool 模拟一个可能长时间阻塞、并声明了自动后台能力的工具。
type graceTool struct {
	name  string
	caps  []string
	delay time.Duration
	out   string
}

func (t *graceTool) Name() string                      { return t.name }
func (t *graceTool) Description() string               { return "auto background tool" }
func (t *graceTool) ParametersSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *graceTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	select {
	case <-time.After(t.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return t.out, nil
}

func (t *graceTool) HasCapability(cap string) bool {
	for _, c := range t.caps {
		if c == cap {
			return true
		}
	}
	return false
}

// withGrace 临时改小前台宽限期，让测试不必真等默认的 60 秒。
func withGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := autoBackgroundGrace
	autoBackgroundGrace = d
	t.Cleanup(func() { autoBackgroundGrace = old })
}

// TestAutoBackgroundCompletesWithinGrace 宽限期内跑完：结果当普通工具结果返回，
// 任务记录被丢弃（job_list 不留瞬时命令）。
func TestAutoBackgroundCompletesWithinGrace(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 200*time.Millisecond)
	if err := m.toolRegistry.Register(&graceTool{
		name: "slow_cmd", caps: []string{CapabilityAutoBackground},
		delay: 10 * time.Millisecond, out: "done-fast",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	out, err := m.ExecuteTool(context.Background(), "slow_cmd", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if out != "done-fast" {
		t.Fatalf("out = %q, 期望与同步执行完全一致", out)
	}
	if n := len(m.jobs.List("")); n != 0 {
		t.Fatalf("宽限内完成的任务应被丢弃，job_list 仍有 %d 条", n)
	}
}

// TestAutoBackgroundMovesToBackgroundAfterGrace 超时：返回 job id、任务继续跑，
// 且落定后仍然发出完成通知（Release 生效，不能被 Claim 永久静音）。
func TestAutoBackgroundMovesToBackgroundAfterGrace(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 20*time.Millisecond)
	if err := m.toolRegistry.Register(&graceTool{
		name: "long_cmd", caps: []string{CapabilityAutoBackground},
		delay: 120 * time.Millisecond, out: "done-later",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	notified := make(chan jobs.JobSnapshot, 1)
	m.jobs.OnJobDone(func(s jobs.JobSnapshot) { notified <- s })

	out, err := m.ExecuteTool(context.Background(), "long_cmd", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if !strings.Contains(out, "moved to background") || !strings.Contains(out, "job_id: long_cmd-") {
		t.Fatalf("超时结果应带 job id: %q", out)
	}

	select {
	case snap := <-notified:
		if snap.Status != jobs.StatusCompleted {
			t.Fatalf("完成通知状态 = %s", snap.Status)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("超时转后台的任务落定后没有发出完成通知（Claim 未 Release）")
	}
}

// TestAutoBackgroundWithoutCapabilityStaysSync 未声明能力的工具**不登记任务**：
// 它没有「交出去继续跑」的资格，所以预算内跑完就原样同步返回（job_list 里不留记录）。
func TestAutoBackgroundWithoutCapabilityStaysSync(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 200*time.Millisecond)
	if err := m.toolRegistry.Register(&graceTool{
		name: "plain_cmd", delay: 10 * time.Millisecond, out: "sync",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	out, err := m.ExecuteTool(context.Background(), "plain_cmd", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if out != "sync" {
		t.Fatalf("out = %q，未声明能力的工具应原样同步返回", out)
	}
	if n := len(m.jobs.List("")); n != 0 {
		t.Fatalf("未声明能力的工具不应登记任务，job_list = %d", n)
	}
}

// TestAutoBackgroundWithoutCapabilityTerminates 未声明 auto-background 能力的工具：
// 安静到预算耗尽就**终止**本次调用，而不是转后台（它没声明适合脱离本轮继续跑），
// 更不能无限期占着这一轮。
func TestAutoBackgroundWithoutCapabilityTerminates(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 30*time.Millisecond)
	if err := m.toolRegistry.Register(&graceTool{
		name: "plain_slow", delay: 10 * time.Second, out: "never",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	start := time.Now()
	_, err := m.ExecuteTool(context.Background(), "plain_slow", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("超过预算应终止并报错")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("应在预算附近终止，实际等了 %v", elapsed)
	}
	if !strings.Contains(err.Error(), "cannot be moved to background") ||
		!strings.Contains(err.Error(), "run_in_background") {
		t.Fatalf("错误文案应说明为何终止、以及长任务该怎么做，实际 %q", err)
	}
	if n := len(m.jobs.List("")); n != 0 {
		t.Fatalf("未声明能力的工具不应登记任务，job_list = %d", n)
	}
}

// chattyTool 持续产生「活动」（模拟有输出的长命令），并实现 activityProbe。
type chattyTool struct {
	name     string
	d        time.Duration
	interval time.Duration

	mu   sync.Mutex
	last time.Time
}

func (t *chattyTool) Name() string                      { return t.name }
func (t *chattyTool) Description() string               { return "chatty tool" }
func (t *chattyTool) ParametersSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *chattyTool) HasCapability(cap string) bool     { return cap == CapabilityAutoBackground }

func (t *chattyTool) touch() {
	t.mu.Lock()
	t.last = time.Now()
	t.mu.Unlock()
}

func (t *chattyTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	deadline := time.After(t.d)
	tick := time.NewTicker(t.interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			return "chatty-done", nil
		case <-tick.C:
			t.touch()
		}
	}
}

func (t *chattyTool) LastActivity(_ context.Context, _ string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last.IsZero() {
		return time.Time{}, false
	}
	return t.last, true
}

// TestAutoBackgroundExtendsOnActivity 活跃续命：只要还在产生输出，前台预算不断重置，
// 命令可以一直在前台跑完；只有真的安静下来超过预算才转后台。
func TestAutoBackgroundExtendsOnActivity(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 100*time.Millisecond)
	tool := &chattyTool{name: "chatty", d: 600 * time.Millisecond, interval: 40 * time.Millisecond}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	out, err := m.ExecuteTool(context.Background(), "chatty", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if out != "chatty-done" {
		t.Fatalf("持续有输出时应前台跑完，got %q（说明预算没有被活动重置）", out)
	}
	if n := len(m.jobs.List("")); n != 0 {
		t.Fatalf("前台跑完的任务应被丢弃，job_list = %d", n)
	}
}

// TestAutoBackgroundGoesBackgroundWhenSilent 同一个工具停止上报活动后，预算照常耗尽
// 转后台（续命不是永久的）。
func TestAutoBackgroundGoesBackgroundWhenSilent(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 60*time.Millisecond)
	tool := &chattyTool{name: "quiet-after-start", d: 500 * time.Millisecond, interval: 10 * time.Second}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	tool.touch() // 只有进入时那一次活动

	out, err := m.ExecuteTool(context.Background(), "quiet-after-start", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if !strings.Contains(out, "moved to background") {
		t.Fatalf("长时间无输出应转后台，got %q", out)
	}
}

// callIDProbeTool 记录执行时看到的工具调用 id（插件侧正是拿它当活跃上报的键）。
type callIDProbeTool struct {
	name    string
	started chan struct{}
	release chan struct{}
	mu      sync.Mutex
	seen    string
}

func (t *callIDProbeTool) Name() string        { return t.name }
func (t *callIDProbeTool) Description() string { return "call id probe" }
func (t *callIDProbeTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *callIDProbeTool) HasCapability(cap string) bool { return cap == CapabilityAutoBackground }

func (t *callIDProbeTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	t.mu.Lock()
	t.seen = ToolCallIDFromContext(ctx)
	t.mu.Unlock()
	select {
	case <-t.started:
	default:
		close(t.started)
	}
	select {
	case <-t.release:
	case <-time.After(3 * time.Second):
	}
	return "probe-done", nil
}

func (t *callIDProbeTool) seenID() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.seen
}

// TestBackgroundPipelinePreservesToolCallID 后台管道必须把「工具调用 id」带进执行 ctx。
//
// 这不是内部细节：插件侧的活跃上报（sdk.TouchActivity → GetActivity）以这个 id 为键，
// 宿主的前台宽限期轮询的也是它。曾经 detachJobContext 只复制会话与审批策略，id 被丢成
// 空串——插件于是把活动记在 ""（recordActivity 直接 return）→ 宿主永远读到 0 →
// 任何有输出的长命令都会在固定预算（默认 60s）后被判 "no output" 转后台。实测现场就是
// 「几乎所有长命令都无谓转后台，然后模型又不得不反复查 job_output」。
func TestBackgroundPipelinePreservesToolCallID(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 5*time.Millisecond)
	tool := &callIDProbeTool{name: "call-id-probe", started: make(chan struct{}), release: make(chan struct{})}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := WithToolCallID(context.Background(), "call-abc")
	out, err := m.ExecuteTool(ctx, "call-id-probe", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if !strings.Contains(out, "moved to background") {
		t.Fatalf("夹具无效：应在宽限后转后台，got %q", out)
	}

	select {
	case <-tool.started:
	case <-time.After(2 * time.Second):
		t.Fatal("后台执行没有开始")
	}
	if got := tool.seenID(); got != "call-abc" {
		t.Fatalf("后台执行看到的 call id = %q，期望 call-abc（丢了它，插件侧活动上报就落在空键上，宿主永远读不到）", got)
	}
	close(tool.release)
}

// chattyRemoteTool 模拟「在插件进程里跑、但持续有输出」的工具：执行期间持续上报
// 活动（activityProbe），自身调不到宿主的看门狗句柄。
type chattyRemoteTool struct {
	name     string
	run      time.Duration
	started  chan struct{}
	lastSeen time.Time
	mu       sync.Mutex
}

func (t *chattyRemoteTool) Name() string        { return t.name }
func (t *chattyRemoteTool) Description() string { return "chatty remote tool" }
func (t *chattyRemoteTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

func (t *chattyRemoteTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if t.started != nil {
		close(t.started)
	}
	deadline := time.Now().Add(t.run)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
		t.mu.Lock()
		t.lastSeen = time.Now()
		t.mu.Unlock()
	}
	return "chatty done", nil
}

// LastActivity 实现 activityProbe：宿主的续命轮询读的就是它。
func (t *chattyRemoteTool) LastActivity(context.Context, string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lastSeen, !t.lastSeen.IsZero()
}

// TestDefaultIdleRenewsFromRemoteActivity 远程工具的活动信号过不了 gRPC（看门狗句柄在
// 宿主 ctx 里），所以宿主必须轮询 GetActivity 替它续命；否则「活跃续命」对插件工具是
// 假话——一条持续输出的命令照样在预算到点时被杀，文案还写着 no output。
func TestDefaultIdleRenewsFromRemoteActivity(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 300*time.Millisecond) // 预算远小于总耗时
	tool := &chattyRemoteTool{name: "chatty_remote", run: 900 * time.Millisecond}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := WithToolCallID(context.Background(), "call-chatty")
	out, err := m.ExecuteTool(ctx, "chatty_remote", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("有持续活动时不该被终止：%v", err)
	}
	if out != "chatty done" {
		t.Fatalf("out = %q", out)
	}
}
