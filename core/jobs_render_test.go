package core

import (
	"strings"
	"testing"
	"time"

	"dsc/jobs"
)

// 运行中的任务：尾部快照 + 进度摘要（跑了多久、上次输出多久前）。
func TestRenderJobOutputShowsPartialAndProgress(t *testing.T) {
	now := time.Now()
	rd := jobs.Read{
		Partial:      "step 3/9: linking\nwarning: unused variable x\n",
		PartialBytes: 37 * 1024,
		Streaming:    true,
		LastActivity: now.Add(-3 * time.Second),
		Snapshot: jobs.JobSnapshot{
			ID: "shell-abc", Kind: "shell", Status: jobs.StatusRunning,
			StartedAt: now.Add(-4*time.Minute - 12*time.Second),
		},
	}
	out := renderJobOutput(rd, 0)

	for _, want := range []string{
		"[partial output",
		"last 45 B of 37.0 KB so far",
		"the full output arrives when the job finishes",
		"step 3/9: linking",
		"[status: running]",
		"running for 4m12s",
		"last activity 3s ago",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q：\n%s", want, out)
		}
	}
	// 尾部不是完整结果：不能出现「没有输出」或「只在结束时才有输出」的措辞
	for _, unwanted := range []string{"(no output yet)", "exposes output only when it finishes"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("不应出现 %q：\n%s", unwanted, out)
		}
	}
}

// 运行中且工具不上报活动：只说明「这个工具不报活动」，不编造静默时长。
func TestRenderJobOutputWithoutActivityProbe(t *testing.T) {
	now := time.Now()
	rd := jobs.Read{Snapshot: jobs.JobSnapshot{
		Status: jobs.StatusRunning, StartedAt: now.Add(-90 * time.Second),
	}}
	out := renderJobOutput(rd, 0)
	if !strings.Contains(out, "running for 1m30s") {
		t.Fatalf("应给出已跑时长：\n%s", out)
	}
	if !strings.Contains(out, "does not report activity") {
		t.Fatalf("应说明工具不上报活动：\n%s", out)
	}
	if strings.Contains(out, "(no output yet)") == false {
		t.Fatalf("没有可读输出时仍应如实说 (no output yet)：\n%s", out)
	}
}

// 落定任务不显示进度摘要（FinishedAt 才是时长口径，且没有「还在跑」的意思）。
func TestRenderJobOutputTerminalHasNoProgressNote(t *testing.T) {
	now := time.Now()
	rd := jobs.Read{
		Text: "done\n",
		Snapshot: jobs.JobSnapshot{
			Status: jobs.StatusCompleted, StartedAt: now.Add(-30 * time.Second), FinishedAt: now,
		},
		ReadCount: 1,
	}
	out := renderJobOutput(rd, 0)
	if strings.Contains(out, "running for") || strings.Contains(out, "last activity") {
		t.Fatalf("落定任务不应出现进度摘要：\n%s", out)
	}
	if !strings.Contains(out, "[status: completed]") {
		t.Fatalf("缺状态行：\n%s", out)
	}
}

// TestRenderJobOutputExplainsSilentJob 运行中、尾部通道可用、却 0 字节：
// 必须解释「为什么没有输出」并给出路，而不是只丢一行 (no output yet)。
//
// 现场：起一个加载超大模型的命令，job_output 在加载期间永远是 (no output yet)，
// 人工跑同一条命令却看得见打印。差别在子进程自己的 stdio 缓冲，模型不知道这件事，
// 只会反复读同一个空结果。
func TestRenderJobOutputExplainsSilentJob(t *testing.T) {
	now := time.Now()
	rd := jobs.Read{
		Streaming:    true,
		Tailable:     true,
		PartialBytes: 0,
		LastActivity: now.Add(-2 * time.Second),
		Snapshot: jobs.JobSnapshot{
			Status: jobs.StatusRunning, StartedAt: now.Add(-5*time.Minute - 12*time.Second),
		},
	}
	out := renderJobOutput(rd, 1)

	for _, want := range []string{
		"(no output yet)",
		"[status: running]",
		"running for 5m12s",
		"0 B written", // 0 字节是事实，不是「我们没读到」
		"block-buffers", // 解释原因：子进程自己的缓冲
		"0 bytes here means 0 bytes",
		"python -u",        // 给出路（Windows 上不带 stdbuf）
		"> log 2>&1",       // 重定向会同样静音——说清代价
		"completion notice", // 收手：完成会通知
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("缺 %q：\n%s", want, out)
		}
	}
}

// TestRenderJobOutputSilentRepeatReadIsShort 第二次读空只点明「还是 0 字节」：
// 整段缓冲解释只在第一次给，重复给等于每次轮询都烧一遍 token。
func TestRenderJobOutputSilentRepeatReadIsShort(t *testing.T) {
	now := time.Now()
	rd := jobs.Read{
		Streaming: true, Tailable: true,
		LastActivity: now.Add(-2 * time.Second),
		Snapshot: jobs.JobSnapshot{
			Status: jobs.StatusRunning, StartedAt: now.Add(-5 * time.Minute),
		},
	}
	out := renderJobOutput(rd, 3)
	if !strings.Contains(out, "read #3") || !strings.Contains(out, "still 0 bytes") {
		t.Fatalf("重复读空应点明次数与字节数：\n%s", out)
	}
	if strings.Contains(out, "block-buffers") {
		t.Fatalf("重复读空不该再贴一遍整段解释：\n%s", out)
	}
}

// TestRenderJobOutputFinalOnlyKeepsItsOwnNote 只在终态给输出的工具不能套用
// 「缓冲」那套说辞：它的 0 字节与子进程缓冲无关，开错药方会让模型白折腾。
func TestRenderJobOutputFinalOnlyKeepsItsOwnNote(t *testing.T) {
	rd := jobs.Read{
		LastActivity: time.Now().Add(-time.Minute),
		Snapshot:     jobs.JobSnapshot{Status: jobs.StatusRunning, StartedAt: time.Now().Add(-2 * time.Minute)},
	}
	out := renderJobOutput(rd, 1)
	if !strings.Contains(out, "exposes output only when it finishes") {
		t.Fatalf("应说明这类任务只在结束时才有输出：\n%s", out)
	}
	if strings.Contains(out, "block-buffers") {
		t.Fatalf("不该把缓冲这套解释套到不给运行期输出的工具上：\n%s", out)
	}
}

func TestHumanDurationAndBytes(t *testing.T) {
	durs := map[time.Duration]string{
		0:                              "0s",
		42 * time.Second:               "42s",
		4*time.Minute + 12*time.Second: "4m12s",
		time.Hour + 3*time.Minute:      "1h03m",
	}
	for d, want := range durs {
		if got := humanDuration(d); got != want {
			t.Fatalf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
	if got := humanDuration(-time.Second); got != "0s" {
		t.Fatalf("负时长应归零，得到 %q", got)
	}
	bytes := map[int64]string{0: "0 B", 812: "812 B", 4096: "4.0 KB", 38000: "37.1 KB", 3 * 1024 * 1024: "3.0 MB"}
	for n, want := range bytes {
		if got := humanBytes(n); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}
