package cron

import (
	"context"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	robfigcron "github.com/robfig/cron/v3"
)

// Runner 任务执行器（宿主注入：Manager.RunSubagent 执行 prompt）。
// ctx 为本次触发的执行上下文（带超时）；返回任务输出文本与错误。
type Runner func(ctx context.Context, job *Job) (string, error)

// defaultTimeout 单次任务执行的默认超时。
const defaultTimeout = 10 * time.Minute

// maxOutputLen 记录到任务状态中的输出截断长度（防止 jobs.json 膨胀）。
const maxOutputLen = 4096

// Scheduler cron 任务调度器：以标准 5 段 cron 表达式调度启用的任务；
// 同一任务上次未跑完时跳过本次触发（SkipIfStillRunning，防重入）。
type Scheduler struct {
	mu      sync.Mutex
	cron    *robfigcron.Cron
	store   *Store
	runner  Runner
	timeout time.Duration
	entries map[string]robfigcron.EntryID // 任务 id -> 调度条目（启用且已注册时存在）
	// onDone 运行落定回调（宿主注入：发布 cron/done 事件）。收到的是**落定后的快照**，
	// 含 LastStatus/LastOutput，且一定是存储里的当前值（任务已被删除则不会回调）。
	onDone func(*Job)
}

// NewScheduler 创建调度器。runner 为任务执行回调；timeout 为单次执行超时
// （<=0 用默认 10 分钟）。
func NewScheduler(store *Store, runner Runner, timeout time.Duration) *Scheduler {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	return &Scheduler{
		store:   store,
		runner:  runner,
		timeout: timeout,
		entries: make(map[string]robfigcron.EntryID),
	}
}

// OnDone 注册运行落定回调（宿主用于发布 cron/done 事件）。回调在任务线程外执行，
// 参数是落定后的快照；任务若在运行期间被删除则不会回调。
func (s *Scheduler) OnDone(fn func(*Job)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onDone = fn
}

// doneHook 取当前回调（避免在任务 goroutine 里裸读 s.onDone）。
func (s *Scheduler) doneHook() func(*Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.onDone
}

// Start 启动调度：注册所有启用的任务。已启动时返回错误。
// 存量任务 cron 表达式无效时跳过该任务（不影响其余任务与调度器本身）。
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron != nil {
		return fmt.Errorf("cron: scheduler already started")
	}
	s.cron = robfigcron.New(robfigcron.WithChain(robfigcron.SkipIfStillRunning(robfigcron.DefaultLogger)))
	// 复位上次进程留下的「运行中」：启动这一刻不可能有任务在跑，若不清掉，
	// /crons 与 cron_list 会永远显示 running（模型据此以为任务还活着、等一个
	// 永远不会到的结果）。异常退出导致的运行没有输出，记 error 并说明原因。
	for _, j := range s.store.List() {
		if j.LastStatus != "running" {
			continue
		}
		_, _ = s.store.Update(j.ID, func(x *Job) {
			x.LastStatus = "error"
			x.LastError = "interrupted: the host exited while this job was running"
			x.LastOutput = ""
		})
	}
	for _, j := range s.store.List() {
		if !j.Enabled {
			continue
		}
		if eid, err := s.cron.AddFunc(j.Cron, func() { s.fire(j.ID) }); err == nil {
			s.entries[j.ID] = eid
		}
	}
	s.cron.Start()
	return nil
}

// Stop 停止调度（不等待运行中的任务：运行任务自带超时，独立于调度器）。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cron == nil {
		return
	}
	s.cron.Stop()
	s.cron = nil
	s.entries = make(map[string]robfigcron.EntryID)
}

// Add 新增任务：校验 cron 表达式并持久化；调度器运行中且任务启用时立即注册。
func (s *Scheduler) Add(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := robfigcron.ParseStandard(j.Cron); err != nil {
		return fmt.Errorf("cron: invalid cron %q: %w", j.Cron, err)
	}
	if j.ID == "" {
		j.ID = newID()
	}
	if j.CreatedAt == 0 {
		j.CreatedAt = time.Now().UnixMilli()
	}
	if err := s.store.Save(j); err != nil {
		return err
	}
	if j.Enabled && s.cron != nil {
		eid, err := s.cron.AddFunc(j.Cron, func() { s.fire(j.ID) })
		if err != nil {
			return fmt.Errorf("cron: schedule %q: %w", j.ID, err)
		}
		s.entries[j.ID] = eid
	}
	return nil
}

// Remove 删除任务并取消其调度；任务不存在返回错误。
func (s *Scheduler) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.store.Remove(id) {
		return fmt.Errorf("cron: job %q not found", id)
	}
	if eid, ok := s.entries[id]; ok {
		s.cron.Remove(eid)
		delete(s.entries, id)
	}
	return nil
}

// SetEnabled 启用/停用任务并同步调度（调度器未启动时仅持久化，启动时统一注册）。
func (s *Scheduler) SetEnabled(id string, enabled bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.store.Get(id)
	if !ok {
		return fmt.Errorf("cron: job %q not found", id)
	}
	job.Enabled = enabled
	if err := s.store.Save(job); err != nil {
		return err
	}
	if s.cron == nil {
		return nil
	}
	if enabled {
		if _, exists := s.entries[id]; !exists {
			eid, err := s.cron.AddFunc(job.Cron, func() { s.fire(id) })
			if err != nil {
				return fmt.Errorf("cron: schedule %q: %w", id, err)
			}
			s.entries[id] = eid
		}
	} else if eid, exists := s.entries[id]; exists {
		s.cron.Remove(eid)
		delete(s.entries, id)
	}
	return nil
}

// SetNotify 设置任务完成时是否即时通知模型（持久化）。任务不存在返回错误。
func (s *Scheduler) SetNotify(id string, notify bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, err := s.store.Update(id, func(j *Job) { j.Notify = notify })
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cron: job %q not found", id)
	}
	return nil
}

// List 返回全部任务（含运行状态快照）。
func (s *Scheduler) List() []*Job {
	return s.store.List()
}

// Get 返回指定任务。
func (s *Scheduler) Get(id string) (*Job, bool) {
	return s.store.Get(id)
}

// fire 任务触发点：由 cron 调度线程调用，标记 running 后异步执行。
func (s *Scheduler) fire(id string) {
	s.mu.Lock()
	job, ok := s.store.Get(id)
	runner := s.runner
	timeout := s.timeout
	s.mu.Unlock()
	if !ok || !job.Enabled || runner == nil {
		return
	}
	// 标记运行中：只更新运行状态字段（见 Store.Update 的说明）。
	if _, err := s.store.Update(id, func(j *Job) {
		j.LastRunAt = time.Now().UnixMilli()
		j.LastStatus = "running"
		j.LastError = ""
	}); err != nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		output, err := runner(ctx, job)
		// 落定同样只改运行状态：任务若在运行期间被删除/停用，这里不能把它写回去。
		updated, _ := s.store.Update(id, func(j *Job) {
			j.LastRunAt = time.Now().UnixMilli()
			if err != nil {
				j.LastStatus = "error"
				j.LastError = err.Error()
				j.LastOutput = ""
			} else {
				j.LastStatus = "success"
				j.LastOutput = truncate(output)
			}
		})
		// 通知用**落定后的快照**（状态与输出都要最新的），且任务已被删除时不再回调。
		if hook := s.doneHook(); hook != nil && updated {
			if snap, ok := s.store.Get(id); ok {
				hook(snap)
			}
		}
	}()
}

// truncate 截断任务输出到 maxOutputLen（按字节上限，但不切断 UTF-8 字符）。
// 直接 s[:n] 会把多字节字符劈开，落盘后变成 U+FFFD 乱码（CJK 输出很容易踩）。
func truncate(s string) string {
	if len(s) <= maxOutputLen {
		return s
	}
	cut := maxOutputLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
