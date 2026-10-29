package jobs

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// tailBuf 可变的尾部快照，模拟「工具在别的进程里跑，宿主只能问到最近输出」。
type tailBuf struct {
	mu    sync.Mutex
	text  string
	total int64
}

func (b *tailBuf) append(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.text += s
	b.total += int64(len(s))
}

func (b *tailBuf) snap() (string, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.text, b.total
}

// 运行中：部分输出 + 活动时间是可读的，且不消费（连读两次一样）。
func TestReadExposesTailWhileRunning(t *testing.T) {
	r := NewRegistry()
	buf := &tailBuf{}
	buf.append("building... 42%")
	lastSeen := time.Now().Add(-3 * time.Second)
	done := make(chan JobOutcome, 1)

	id, err := r.Start(StartSpec{
		Kind:  "shell",
		Label: "build",
		Start: func() (JobHooks, error) {
			return JobHooks{
				Done:       done,
				TailOutput: buf.snap,
				Activity:   func() (time.Time, bool) { return lastSeen, true },
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	rd, err := r.Read(id, "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rd.Text != "" {
		t.Fatalf("运行中不应有最终输出，得到 %q", rd.Text)
	}
	if rd.Partial != "building... 42%" || rd.PartialBytes != int64(len("building... 42%")) {
		t.Fatalf("partial = %q / %d", rd.Partial, rd.PartialBytes)
	}
	if !rd.Streaming {
		t.Fatal("有尾部快照的任务应标记为运行期可读（否则调用方只会等 3 秒）")
	}
	if !rd.LastActivity.Equal(lastSeen) {
		t.Fatalf("last activity = %v, want %v", rd.LastActivity, lastSeen)
	}

	// 快照不是增量：追加后连读两次都是「最新那段」，不会因为读过就消失
	buf.append(" 43%")
	first, _ := r.Read(id, "")
	second, _ := r.Read(id, "")
	if !strings.Contains(first.Partial, "43%") || !strings.Contains(second.Partial, "43%") {
		t.Fatalf("尾部快照应为最新值且可重复读：%q / %q", first.Partial, second.Partial)
	}
	if second.Snapshot.Reported {
		t.Fatal("运行中读取不应把任务标记成 reported（否则完成通知会被抑制）")
	}

	// 落定后：完整输出覆盖尾部，Partial 清空
	done <- JobOutcome{Status: StatusCompleted, Output: "build done, 0 errors"}
	final := waitRead(t, r, id, StatusCompleted)
	if final.Text != "build done, 0 errors" {
		t.Fatalf("final text = %q", final.Text)
	}
	if final.Partial != "" {
		t.Fatalf("落定后不应再有 partial，得到 %q", final.Partial)
	}
}

// 没有 TailOutput 的任务保持原样：运行中只有空输出，不是「可读」。
func TestReadWithoutTailKeepsFinalOutputSemantics(t *testing.T) {
	r := NewRegistry()
	done := make(chan JobOutcome, 1)
	id, err := r.Start(StartSpec{Kind: "workflow", Start: func() (JobHooks, error) {
		return JobHooks{Done: done}, nil
	}})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	rd, err := r.Read(id, "")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if rd.Partial != "" || rd.Streaming || rd.Text != "" {
		t.Fatalf("无尾部通道的任务运行中应完全空且不可读：%+v", rd)
	}
}

// waitRead 轮询读到指定状态为止。
func waitRead(t *testing.T, r *Registry, id string, want JobStatus) Read {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rd, err := r.Read(id, "")
		if err == nil && rd.Snapshot.Status == want {
			return rd
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("job %s 未在期限内进入 %s", id, want)
	return Read{}
}
