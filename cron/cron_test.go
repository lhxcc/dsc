package cron

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// newTestStore 创建临时目录下的任务存储。
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestStorePersist(t *testing.T) {
	s := newTestStore(t)
	j := &Job{ID: "job-1", Name: "daily", Cron: "0 8 * * *", Prompt: "写日报", Enabled: true, CreatedAt: 100}
	if err := s.Save(j); err != nil {
		t.Fatalf("save: %v", err)
	}
	// 用同一目录重新打开（模拟重启），任务应从磁盘恢复
	s2, err := NewStore(filepath.Dir(s.path))
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := s2.Get("job-1")
	if !ok || got.Name != "daily" || got.Cron != "0 8 * * *" || !got.Enabled {
		t.Fatalf("restored job = %+v (ok=%v)", got, ok)
	}
}

func TestStoreRemoveMissing(t *testing.T) {
	s := newTestStore(t)
	if s.Remove("nope") {
		t.Fatal("removing missing job should return false")
	}
}

func TestSchedulerAddInvalidCron(t *testing.T) {
	s := newTestStore(t)
	sch := NewScheduler(s, nil, 0)
	err := sch.Add(&Job{Name: "bad", Cron: "not-a-cron", Prompt: "x", Enabled: true})
	if err == nil || !strings.Contains(err.Error(), "invalid cron") {
		t.Fatalf("invalid cron should fail, got %v", err)
	}
}

func TestSchedulerFireRunsRunner(t *testing.T) {
	s := newTestStore(t)
	var mu sync.Mutex
	ran := make(chan string, 1)
	sch := NewScheduler(s, func(ctx context.Context, j *Job) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		ran <- "ran:" + j.Prompt
		return "output-ok", nil
	}, time.Second)
	if err := sch.Add(&Job{Name: "daily", Cron: "0 8 * * *", Prompt: "do it", Enabled: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := sch.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop()

	// cron 条目应已注册（5 段表达式无法秒级触发，这里直接验证注册与执行路径）
	sch.mu.Lock()
	registered := len(sch.entries)
	sch.mu.Unlock()
	if registered != 1 {
		t.Fatalf("registered entries = %d, want 1", registered)
	}

	id := s.List()[0].ID
	sch.fire(id)

	select {
	case got := <-ran:
		if got != "ran:do it" {
			t.Fatalf("runner got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fire did not run the runner")
	}

	// 状态落盘异步，轮询等待
	deadline := time.Now().Add(2 * time.Second)
	for {
		list := sch.List()
		if len(list) == 1 && list[0].LastStatus == "success" {
			if list[0].LastOutput != "output-ok" {
				t.Fatalf("job output = %q", list[0].LastOutput)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job state did not settle: %+v", list)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestSchedulerSetEnabledPersists(t *testing.T) {
	s := newTestStore(t)
	sch := NewScheduler(s, func(ctx context.Context, j *Job) (string, error) {
		return "ok", nil
	}, time.Second)
	if err := sch.Add(&Job{Name: "every-sec", Cron: "* * * * *", Prompt: "x", Enabled: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	id := s.List()[0].ID
	if err := sch.SetEnabled(id, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ := sch.Get(id)
	if got.Enabled {
		t.Fatal("job should be disabled")
	}
	// 重启后仍停用（持久化生效）
	sch.Stop()
	if err := sch.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer sch.Stop()
	got, _ = sch.Get(id)
	if got.Enabled {
		t.Fatal("job should stay disabled after restart")
	}
}

func TestSchedulerRemove(t *testing.T) {
	s := newTestStore(t)
	sch := NewScheduler(s, nil, 0)
	if err := sch.Add(&Job{Name: "x", Cron: "0 8 * * *", Prompt: "x", Enabled: true}); err != nil {
		t.Fatalf("add: %v", err)
	}
	id := s.List()[0].ID
	if err := sch.Remove(id); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := sch.Get(id); ok {
		t.Fatal("job should be gone")
	}
	if err := sch.Remove(id); err == nil {
		t.Fatal("removing missing job should error")
	}
}

func TestTruncate(t *testing.T) {
	long := strings.Repeat("a", maxOutputLen+100)
	if got := truncate(long); len(got) != maxOutputLen {
		t.Fatalf("truncate len = %d, want %d", len(got), maxOutputLen)
	}
	if truncate("short") != "short" {
		t.Fatal("short string should be unchanged")
	}
}

// blockingRunner 让任务卡在执行中，便于在「运行期间」修改任务表。
func blockingRunner(started, release chan struct{}) Runner {
	return func(ctx context.Context, j *Job) (string, error) {
		close(started)
		<-release
		return "done", nil
	}
}

// TestRemovedWhileRunningStaysRemoved 运行期间删除的任务不得被落定写回：
// 旧实现落定时 Save 的是开始前取的副本，Save 会把它重新插回 map 并写到 cron.json
// ——任务「删了又活」，且不带任何调度条目（重启后继续跑）。
func TestRemovedWhileRunningStaysRemoved(t *testing.T) {
	s := newTestStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	sch := NewScheduler(s, blockingRunner(started, release), time.Second)
	if err := s.Save(&Job{ID: "j1", Name: "n", Cron: "0 8 * * *", Prompt: "p", Enabled: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	sch.fire("j1")
	<-started
	if !s.Remove("j1") {
		t.Fatal("remove 失败")
	}
	close(release)
	waitSettled(t, s, "j1")

	if got, ok := s.Get("j1"); ok {
		t.Fatalf("已删除的任务被落定写回: %+v", got)
	}
}

// TestDisabledWhileRunningStaysDisabled 运行期间停用的任务不得被旧副本改回启用：
// 旧实现会用开始前的 Enabled=true 覆盖磁盘，表现为「停用了，重启后又开始跑」。
func TestDisabledWhileRunningStaysDisabled(t *testing.T) {
	s := newTestStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	sch := NewScheduler(s, blockingRunner(started, release), time.Second)
	if err := s.Save(&Job{ID: "j2", Name: "n", Cron: "0 8 * * *", Prompt: "p", Enabled: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}

	sch.fire("j2")
	<-started
	if err := sch.SetEnabled("j2", false); err != nil {
		t.Fatal(err)
	}
	// 同时改 prompt：落定不得用旧副本把它覆盖回去
	cur, _ := s.Get("j2")
	cur.Prompt = "new prompt"
	if err := s.Save(cur); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitSettled(t, s, "j2")

	got, ok := s.Get("j2")
	if !ok {
		t.Fatal("任务不应消失")
	}
	if got.Enabled {
		t.Fatal("运行中被停用的任务被落定改回 Enabled=true")
	}
	if got.Prompt != "new prompt" {
		t.Fatalf("运行期间修改的 prompt 被落定覆盖: %q", got.Prompt)
	}
	if got.LastStatus != "success" || got.LastOutput != "done" {
		t.Fatalf("运行状态应写入: %+v", got)
	}
}

// TestStoreReturnsCopies Save 入表、List 出表都必须是副本：调度器在后台改状态，
// 外部持有内部指针就会绕过锁（数据竞态），也能无声改掉内部状态。
func TestStoreReturnsCopies(t *testing.T) {
	s := newTestStore(t)
	j := &Job{ID: "c1", Name: "orig", Cron: "0 8 * * *", Prompt: "p", Enabled: true, CreatedAt: 1}
	if err := s.Save(j); err != nil {
		t.Fatal(err)
	}
	j.Name = "mutated-after-save" // 调用方继续改自己的对象
	if got, _ := s.Get("c1"); got.Name != "orig" {
		t.Fatalf("Save 存了调用方指针（别名）: store Name=%q", got.Name)
	}
	list := s.List()
	list[0].Name = "mutated-via-list"
	if got, _ := s.Get("c1"); got.Name != "orig" {
		t.Fatalf("List 返回了内部指针: store Name=%q", got.Name)
	}
}

// TestTruncateKeepsRunesValid 输出截断不能把多字节字符劈开（落盘会变成 U+FFFD）。
func TestTruncateKeepsRunesValid(t *testing.T) {
	long := strings.Repeat("中", maxOutputLen/3+50) // 每字 3 字节，必然落在字符中间
	got := truncate(long)
	if !utf8.ValidString(got) {
		t.Fatal("截断结果不是合法 UTF-8（字符被劈开）")
	}
	if len(got) > maxOutputLen {
		t.Fatalf("截断后长度 %d 超过上限 %d", len(got), maxOutputLen)
	}
	if strings.Trim(got, "中") != "" {
		t.Fatalf("截断处产生了非预期内容: %q", got[len(got)-9:])
	}
}

// waitSettled 等待任务落定（或确认任务已不存在）。
func waitSettled(t *testing.T, s *Store, id string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		j, ok := s.Get(id)
		if !ok || j.LastStatus != "running" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("任务 %s 未在期限内落定", id)
}

// TestStartResetsStaleRunning 上次进程异常退出会留下 LastStatus="running"；
// 启动时必须复位，否则 /crons 与 cron_list 永远显示「运行中」，
// 模型会以为任务还活着、等一个永远不会到的结果。
func TestStartResetsStaleRunning(t *testing.T) {
	s := newTestStore(t)
	if err := s.Save(&Job{ID: "stale", Name: "n", Cron: "0 8 * * *", Prompt: "p",
		Enabled: true, CreatedAt: 1, LastStatus: "running", LastRunAt: 123}); err != nil {
		t.Fatal(err)
	}
	sch := NewScheduler(s, func(ctx context.Context, j *Job) (string, error) { return "", nil }, time.Second)
	if err := sch.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer sch.Stop()

	got, _ := s.Get("stale")
	if got.LastStatus == "running" {
		t.Fatal("启动后仍停留在 running：上次异常退出的残留状态未复位")
	}
	if got.LastStatus != "error" || !strings.Contains(got.LastError, "interrupted") {
		t.Fatalf("应记为 interrupted error，got status=%q err=%q", got.LastStatus, got.LastError)
	}
	// 任务本身仍是启用的（复位的是运行状态，不是调度开关）
	if !got.Enabled {
		t.Fatal("不应改变 enabled")
	}
}

// TestOnDoneFiresWithSettledSnapshot 落定回调必须带**落定后**的快照（状态与输出都是
// 最新的），它是「完成通知」的唯一事实来源——回调里若拿到开始前的状态，通知就会说
// 「运行中」。
func TestOnDoneFiresWithSettledSnapshot(t *testing.T) {
	s := newTestStore(t)
	done := make(chan *Job, 1)
	sch := NewScheduler(s, func(ctx context.Context, j *Job) (string, error) { return "out-ok", nil }, time.Second)
	sch.OnDone(func(j *Job) { done <- j })
	if err := s.Save(&Job{ID: "n1", Name: "daily", Cron: "0 8 * * *", Prompt: "p", Enabled: true, Notify: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	sch.fire("n1")

	select {
	case j := <-done:
		if j.LastStatus != "success" || j.LastOutput != "out-ok" {
			t.Fatalf("回调应拿到落定快照，got %+v", j)
		}
		if !j.Notify {
			t.Fatal("notify 标记应随快照带出（消费方据此决定是否叫醒模型）")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("落定后未回调")
	}
}

// TestOnDoneSkippedWhenRemovedDuringRun 运行期间被删除的任务不再回调：
// 通知一条「已完成」的任务，模型只会去 cron_list 找不到它。
func TestOnDoneSkippedWhenRemovedDuringRun(t *testing.T) {
	s := newTestStore(t)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan *Job, 1)
	sch := NewScheduler(s, blockingRunner(started, release), time.Second)
	sch.OnDone(func(j *Job) { done <- j })
	if err := s.Save(&Job{ID: "n2", Name: "n", Cron: "0 8 * * *", Prompt: "p", Enabled: true, Notify: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	sch.fire("n2")
	<-started
	s.Remove("n2")
	close(release)

	select {
	case j := <-done:
		t.Fatalf("已删除任务不应回调，got %+v", j)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSetNotifyPersists notify 是任务属性，必须落盘（重启后仍生效）。
func TestSetNotifyPersists(t *testing.T) {
	s := newTestStore(t)
	sch := NewScheduler(s, nil, 0)
	if err := s.Save(&Job{ID: "n3", Name: "n", Cron: "0 8 * * *", Prompt: "p", Enabled: true, CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := sch.SetNotify("n3", true); err != nil {
		t.Fatalf("set notify: %v", err)
	}
	got, _ := s.Get("n3")
	if !got.Notify {
		t.Fatal("notify 未写入存储")
	}
	// 重新打开（模拟重启）
	s2, err := NewStore(filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if j, _ := s2.Get("n3"); !j.Notify {
		t.Fatal("notify 未持久化到 cron.json")
	}
	if err := sch.SetNotify("nope", true); err == nil {
		t.Fatal("设置不存在的任务应报错")
	}
}
