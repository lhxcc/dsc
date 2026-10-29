package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"dsc/jobs"
)

// 本文件专测 job_output 的「先等一会儿再回报」行为：
//
// 后台任务刚转过来时必然还没有可读输出（final-output 模式只在落定后可读），
// 立刻回一行 (no output yet) 对模型没有任何用处——它会马上再读一次，空转烧 token。
// 现在默认等首块输出（或落定），最多 60s；等到就立刻返回。

func jobOutputOf(t *testing.T, m *Manager, id string) (string, error) {
	t.Helper()
	tool, ok := m.toolRegistry.Get("job_output")
	if !ok {
		t.Fatal("job_output not registered")
	}
	return tool.Execute(context.Background(), json.RawMessage(`{"job_id":"`+id+`"}`))
}

// TestJobOutputWaitsForFirstChunk 流式任务：一开始没有输出，稍后吐出第一块 →
// 一次调用就该拿到那块内容，而不是 (no output yet)。
func TestJobOutputWaitsForFirstChunk(t *testing.T) {
	t.Setenv("DSC_JOB_OUTPUT_WAIT_SECS", "5")
	m := NewManager(&ManagerConfig{})

	var produced atomic.Bool
	go func() {
		time.Sleep(300 * time.Millisecond)
		produced.Store(true)
	}()
	done := make(chan jobs.JobOutcome, 1)
	id, err := m.jobs.Start(jobs.StartSpec{
		Kind:  "stream",
		Label: "chunky",
		Start: func() (jobs.JobHooks, error) {
			return jobs.JobHooks{
				ReadOutput: func() string {
					if produced.Load() {
						return "first-chunk"
					}
					return ""
				},
				Done: done,
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { done <- jobs.JobOutcome{Status: jobs.StatusCompleted} }()

	start := time.Now()
	out, err := jobOutputOf(t, m, id)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("job_output: %v", err)
	}
	if !strings.Contains(out, "first-chunk") {
		t.Fatalf("应等到首块输出，得到 %q", out)
	}
	if elapsed < 200*time.Millisecond {
		t.Fatalf("等待时间 %v 太短，说明没有真的等（或轮询没生效）", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("拿到输出后应立即返回，实际等了 %v", elapsed)
	}
}

// TestJobOutputWaitsForSettlement 最终输出任务（shell 后台命令就是这种）：
// 落定前不可读 → 等它落定，一次调用直接把结果交出去。
func TestJobOutputWaitsForSettlement(t *testing.T) {
	t.Setenv("DSC_JOB_OUTPUT_WAIT_SECS", "5")
	m := NewManager(&ManagerConfig{})

	done := make(chan jobs.JobOutcome, 1)
	id, err := m.jobs.Start(jobs.StartSpec{
		Kind:  "shell",
		Label: "slow echo",
		Start: func() (jobs.JobHooks, error) {
			go func() {
				time.Sleep(300 * time.Millisecond)
				done <- jobs.JobOutcome{Status: jobs.StatusCompleted, Output: "hello from job"}
			}()
			return jobs.JobHooks{Done: done}, nil // ReadOutput=nil：只在落定后可读
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	out, err := jobOutputOf(t, m, id)
	if err != nil {
		t.Fatalf("job_output: %v", err)
	}
	if !strings.Contains(out, "hello from job") || !strings.Contains(out, "[status: completed]") {
		t.Fatalf("应等到落定并返回最终输出，得到 %q", out)
	}
}

// TestJobOutputGivesUpAfterBudget 预算内没输出：如实回报当前状态，不能无限等。
func TestJobOutputGivesUpAfterBudget(t *testing.T) {
	t.Setenv("DSC_JOB_OUTPUT_WAIT_SECS", "1")
	m := NewManager(&ManagerConfig{})

	done := make(chan jobs.JobOutcome, 1)
	id, err := m.jobs.Start(jobs.StartSpec{
		Kind:  "shell",
		Label: "quiet",
		Start: func() (jobs.JobHooks, error) {
			return jobs.JobHooks{Done: done}, nil
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { done <- jobs.JobOutcome{Status: jobs.StatusCompleted} }()

	start := time.Now()
	out, err := jobOutputOf(t, m, id)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("job_output: %v", err)
	}
	if !strings.Contains(out, "(no output yet)") || !strings.Contains(out, "[status: running]") {
		t.Fatalf("超预算后应如实回报运行中且无输出，得到 %q", out)
	}
	if elapsed < 900*time.Millisecond {
		t.Fatalf("等待 %v 短于预算 1s", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("超出预算太多：%v", elapsed)
	}
}

// TestJobOutputWaitDisabled 宿主禁止阻塞（DSC_JOB_WAIT_MAX_SECS=0）时立刻回报，
// 显式的输出等待预算也不能绕过它。
func TestJobOutputWaitDisabled(t *testing.T) {
	t.Setenv("DSC_JOB_WAIT_MAX_SECS", "0")
	t.Setenv("DSC_JOB_OUTPUT_WAIT_SECS", "5")
	m := NewManager(&ManagerConfig{})

	done := make(chan jobs.JobOutcome, 1)
	id, err := m.jobs.Start(jobs.StartSpec{
		Kind:  "shell",
		Label: "quiet",
		Start: func() (jobs.JobHooks, error) {
			return jobs.JobHooks{Done: done}, nil
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { done <- jobs.JobOutcome{Status: jobs.StatusCompleted} }()

	start := time.Now()
	if _, err := jobOutputOf(t, m, id); err != nil {
		t.Fatalf("job_output: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("宿主禁止阻塞时不应等待，实际 %v", elapsed)
	}
}

// TestJobOutputWaitStopsOnCancel ctx 取消（用户中断/回合结束）时不许把人卡在等待里。
func TestJobOutputWaitStopsOnCancel(t *testing.T) {
	t.Setenv("DSC_JOB_OUTPUT_WAIT_SECS", "30")
	m := NewManager(&ManagerConfig{})

	done := make(chan jobs.JobOutcome, 1)
	id, err := m.jobs.Start(jobs.StartSpec{
		Kind:  "shell",
		Label: "quiet",
		Start: func() (jobs.JobHooks, error) {
			return jobs.JobHooks{Done: done}, nil
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { done <- jobs.JobOutcome{Status: jobs.StatusCompleted} }()

	tool, _ := m.toolRegistry.Get("job_output")
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	out, err := tool.Execute(ctx, json.RawMessage(`{"job_id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("job_output: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ctx 取消后应立刻返回，实际 %v", elapsed)
	}
	if !strings.Contains(out, "(no output yet)") {
		t.Fatalf("取消后应返回当前快照，得到 %q", out)
	}
}

// TestJobOutputDoesNotWaitLongForFinalOutput 后台命令这类 final-output 任务：
// 运行期没有可读输出，等满 60s 也只是赌它马上结束。上限压到 3s，并明说「别轮询」。
//
// 现场（用户实测）：起一个长驻服务后调 job_output，干等 60s 仍是一行 (no output yet)；
// 而模型随手再跑一条 curl/grep 立刻就有结果——因为那条命令的输出是它自己的 stdout，
// 与后台任务无关。
func TestJobOutputDoesNotWaitLongForFinalOutput(t *testing.T) {
	t.Setenv("DSC_JOB_OUTPUT_WAIT_SECS", "60")
	m := NewManager(&ManagerConfig{})

	done := make(chan jobs.JobOutcome, 1)
	id, err := m.jobs.Start(jobs.StartSpec{
		Kind:  "shell",
		Label: "long server",
		Start: func() (jobs.JobHooks, error) {
			return jobs.JobHooks{Done: done}, nil // ReadOutput=nil：落定前无可读输出
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { done <- jobs.JobOutcome{Status: jobs.StatusCompleted} }()

	start := time.Now()
	out, err := jobOutputOf(t, m, id)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("job_output: %v", err)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("final-output 任务不该按 60s 预算等待，实际等了 %v", elapsed)
	}
	if !strings.Contains(out, "(no output yet)") || !strings.Contains(out, "[status: running]") {
		t.Fatalf("应如实回报运行中且无输出，得到 %q", out)
	}
	if !strings.Contains(out, "only when it finishes") {
		t.Fatalf("应说明这类任务只在结束时才有输出（避免模型反复轮询），得到 %q", out)
	}
}
