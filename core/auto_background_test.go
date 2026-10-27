package core

import (
	"context"
	"encoding/json"
	"strings"
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

// withGrace 临时改小前台宽限期，让测试不必真等 30 秒。
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

// TestAutoBackgroundWithoutCapabilityStaysSync 未声明能力的工具不受影响，
// 即便执行时间超过宽限期也不转后台。
func TestAutoBackgroundWithoutCapabilityStaysSync(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	withGrace(t, 10*time.Millisecond)
	if err := m.toolRegistry.Register(&graceTool{
		name: "plain_cmd", delay: 40 * time.Millisecond, out: "sync",
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
