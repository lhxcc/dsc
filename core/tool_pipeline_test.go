package core

import (
	"errors"
	"testing"

	"dsc/jobs"
)

// TestExitCodeFromResult 退出码标记解析：命中返回数值，未命中返回 0（含负数）。
func TestExitCodeFromResult(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"\n[exit_code: 0]\n(no output)\n", 0},
		{"\n[exit_code: 1]\n(no output)\n", 1},
		{"some output\n[exit_code: 2]\n", 2},
		{"[exit_code: -1]", -1},
		{"plain output with no marker", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := exitCodeFromResult(c.in); got != c.want {
			t.Fatalf("exitCodeFromResult(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestSettleBackgroundOutcome 后台任务终态：流水线报错或命令非零退出都判 failed，
// 且失败原因写进 Detail；命令成功（exit 0 或结果无标记）才 completed。
func TestSettleBackgroundOutcome(t *testing.T) {
	runErr := errors.New("killed by timeout policy")
	cases := []struct {
		name   string
		result string
		err    error
		status jobs.JobStatus
		detail string
	}{
		{"流水线报错 → failed", "out", runErr, jobs.StatusFailed, "killed by timeout policy"},
		{"exit 0 → completed", "\n[exit_code: 0]\n(no output)\n", nil, jobs.StatusCompleted, ""},
		{"exit 1 → failed 带退出码", "\n[exit_code: 1]\n(no output)\n", nil, jobs.StatusFailed, "exit code: 1"},
		{"exit 2 有输出 → failed 带退出码", "log line\n[exit_code: 2]\n", nil, jobs.StatusFailed, "exit code: 2"},
		{"无标记 → completed", "plain output", nil, jobs.StatusCompleted, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, d := settleBackgroundOutcome(c.result, c.err)
			if st != c.status {
				t.Fatalf("status = %q, want %q", st, c.status)
			}
			if d != c.detail {
				t.Fatalf("detail = %q, want %q", d, c.detail)
			}
		})
	}
}
