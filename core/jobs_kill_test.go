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

// blockingTool 一直阻塞到 ctx 被取消——模拟「后台命令正在跑」。
type blockingTool struct {
	started  chan struct{}
	canceled chan struct{}
	tail     string // 非空则实现 OutputTail（模拟能流式上报的 shell）

	onceStart sync.Once
	onceEnd   sync.Once
}

func (t *blockingTool) Name() string        { return "blocker" }
func (t *blockingTool) Description() string { return "blocks until canceled" }
func (t *blockingTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}

// OutputTail 让该工具看起来「运行期能读到最近输出」（宿主按接口探测）。
func (t *blockingTool) OutputTail(context.Context, string) (string, int64, bool) {
	if t.tail == "" {
		return "", 0, false
	}
	return t.tail, int64(len(t.tail)), true
}

func (t *blockingTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	t.onceStart.Do(func() { close(t.started) })
	<-ctx.Done()
	t.onceEnd.Do(func() { close(t.canceled) })
	return "", ctx.Err()
}

// TestJobKillActuallyCancelsTool 现场：`job_kill` 只往一个没人读的 channel 里塞了
// reason，工具照旧跑到底——模型见状改用 taskkill 之类的外部手段，而外部终止不会
// 更新任务记录，JOB 一直挂在 running。
//
// 这里钉住三件事：job_kill 让工具的 ctx 真的被取消、任务落定为 killed、原因带上。
func TestJobKillActuallyCancelsTool(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	tool := &blockingTool{started: make(chan struct{}), canceled: make(chan struct{})}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	msg, _, err := m.startBackgroundTool(context.Background(), tool.Name(), json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	// 提示里两个动作都要带 job_id：否则模型会去 shell 里 taskkill
	if !strings.Contains(msg, "job_output (job_id: ") || !strings.Contains(msg, "job_kill (job_id: ") {
		t.Fatalf("启动提示应同时给出 job_output 与 job_kill 的 job_id：%q", msg)
	}
	id := jobIDFromMessage(t, msg)

	select {
	case <-tool.started:
	case <-time.After(2 * time.Second):
		t.Fatal("工具未启动")
	}

	kill, ok := m.toolRegistry.Get("job_kill")
	if !ok {
		t.Fatal("job_kill 未注册")
	}
	out, err := kill.Execute(context.Background(), json.RawMessage(`{"job_id":"`+id+`","reason":"模型要求停止"}`))
	if err != nil {
		t.Fatalf("job_kill: %v", err)
	}
	if !strings.Contains(out, "requested cancellation") {
		t.Fatalf("job_kill 输出 = %q", out)
	}

	// 工具必须真的收到取消（而不是继续跑）
	select {
	case <-tool.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("job_kill 之后工具的 ctx 仍未取消（Cancel 没有接上可取消上下文）")
	}

	waitJobStatus(t, m, id, jobs.StatusKilled)
	snap, _ := m.jobs.Get(id, "")
	if !strings.Contains(snap.Detail, "killed by caller") || !strings.Contains(snap.Detail, "模型要求停止") {
		t.Fatalf("落定原因应说明是调用方终止并带上 reason，实际 %q", snap.Detail)
	}
}

// jobIDFromMessage 从启动提示里取 job id（"job_id: <id>" 首次出现）。
func jobIDFromMessage(t *testing.T, msg string) string {
	t.Helper()
	const mark = "job_id: "
	i := strings.Index(msg, mark)
	if i < 0 {
		t.Fatalf("提示里没有 job_id：%q", msg)
	}
	rest := msg[i+len(mark):]
	if j := strings.IndexAny(rest, ")., "); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		t.Fatalf("解析不出 job id：%q", msg)
	}
	return rest
}

// TestJobKillKeepsWhatWasPrinted 终止不该抹掉已经打印出来的内容：kill 之后
// 工具结果通常是空的（ctx 取消），若原样落定，模型再读就是 (no output yet)，
// 前面看到过的输出凭空消失。这里要求落定文本取自终止前的尾部快照。
func TestJobKillKeepsTailOutput(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	tool := &blockingTool{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
		tail:     "tick-1\ntick-2\n",
	}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	// 真实链路上工具调用的 ctx 带 call id（宿主按它去插件问活动与尾部）；
	// 这里补上，否则 jobProbes 直接返回 nil、尾部通道根本不会装。
	callCtx := WithToolCallID(context.Background(), "call-kill-tail")
	msg, _, err := m.startBackgroundTool(callCtx, tool.Name(), json.RawMessage(`{}`), "")
	if err != nil {
		t.Fatalf("start background: %v", err)
	}
	id := jobIDFromMessage(t, msg)
	<-tool.started

	// 先读一次让宿主缓存尾部（真实链路上 job_output 会先读，模型才知道有输出）
	if _, err := jobOutputOf(t, m, id); err != nil {
		t.Fatalf("job_output: %v", err)
	}

	kill, _ := m.toolRegistry.Get("job_kill")
	if _, err := kill.Execute(context.Background(), json.RawMessage(`{"job_id":"`+id+`"}`)); err != nil {
		t.Fatalf("job_kill: %v", err)
	}
	waitJobStatus(t, m, id, jobs.StatusKilled)

	out, err := jobOutputOf(t, m, id)
	if err != nil {
		t.Fatalf("job_output after kill: %v", err)
	}
	if !strings.Contains(out, "tick-1") || !strings.Contains(out, "[status: killed]") {
		t.Fatalf("终止后应保留已打印的输出，实际 %q", out)
	}
	if strings.Contains(out, "(no output yet)") {
		t.Fatalf("不该退化成 (no output yet)：%q", out)
	}
}
