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
	out := renderJobOutput(rd)

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
	out := renderJobOutput(rd)
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
	out := renderJobOutput(rd)
	if strings.Contains(out, "running for") || strings.Contains(out, "last activity") {
		t.Fatalf("落定任务不应出现进度摘要：\n%s", out)
	}
	if !strings.Contains(out, "[status: completed]") {
		t.Fatalf("缺状态行：\n%s", out)
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
