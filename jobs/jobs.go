// Package jobs 后台任务注册表（对齐 DSH jobs 完整契约）。
//
// 生产方声明 kind/label/owner，经 Start 预检后同步启动并登记；运行时拥有
// 身份、访问授权与生命周期状态。访问按 owner 会话隔离：有 owner 的任务仅
// 其 owner（或空调用方仅限无 owner 任务）可见/可读/可取消。
//
// 输出模型二选一（由生产方决定）：
//   - 流式任务（提供 ReadOutput）：read 消费"自上次以来的增量"，单一游标；
//   - 最终输出任务（不提供 ReadOutput）：终止后 read 幂等返回最终输出。
//
// 状态机对齐 DSH：running → (stopping) → completed/killed/failed；settlement
// 首次结果优先。reported 标志在 kill/read/wait 触及终态时置位，供完成通知
// 抑制重复（v1 无自动唤醒，字段保留）。
package jobs

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
)

// JobStatus 任务生命周期（对齐 DSH：running、可选 stopping，随后恰好一个终态）。
type JobStatus string

const (
	StatusRunning   JobStatus = "running"
	StatusStopping  JobStatus = "stopping"
	StatusCompleted JobStatus = "completed"
	StatusKilled    JobStatus = "killed"
	StatusFailed    JobStatus = "failed"
)

// KillResult kill 的结果（对齐 DSH：requested / already-finished）。
type KillResult string

const (
	KillRequested       KillResult = "requested"
	KillAlreadyFinished KillResult = "already-finished"
)

// JobHooks 生产方钩子（对齐 DSH JobHooks）。Done 在生产方释放资源后交付终态。
type JobHooks struct {
	// Cancel 请求终止：同步、幂等，最终 settle Done；reason 原样转发。
	Cancel func(reason string)
	// Done 终态结果（completed/killed/failed）。
	Done <-chan JobOutcome
	// ReadOutput 消费自上次调用以来的输出增量；nil 标记最终输出任务。
	ReadOutput func() string
	// TailOutput 运行期可读的**最近输出快照**（末尾若干 KB）与累计字节数；可选。
	//
	// 与 ReadOutput 的区别是「消费 vs 快照」：增量读一次少一段（适合真流式生产方），
	// 快照每次都给同一段尾巴（适合「工具在别的进程里跑、只能问到最近状态」）。
	// 提供它的任务仍以 Done.Output 为权威终态输出——尾部只用于任务未落定时的观察。
	TailOutput func() (tail string, totalBytes int64)
	// Activity 最近一次活动时间（有输出即算活动）；可选。
	//
	// 用来回答「它还在动吗」：final-output 任务运行期没有可读输出，模型唯一能问的
	// 就是「上次动是多久之前」。返回 false 表示生产方不提供。
	Activity func() (time.Time, bool)
}

// JobOutcome 生产方终态结果（对齐 DSH JobOutcome）。
type JobOutcome struct {
	Status JobStatus // completed | killed | failed
	Detail string    // kind 特有事实（如 "exit code: 3"）
	Output string    // final-output 任务的最终输出；stream 任务不设
}

// StartSpec 启动声明（对齐 DSH JobStart）。
type StartSpec struct {
	Kind  string // 生产方 kind，亦为 id 前缀（如 "workflow"）
	Label string
	// OutputLimitBytes 生产方拥有的模型呈现上限（每次完整读取/通知）；<=0 不设限。
	// 注册表不重写生产方输出，由生产方自行应用。
	OutputLimitBytes int
	// Owner owner 会话标识；空 = 无 owner（开放给所有调用方，存续到服务释放）。
	Owner string
	// Start 预检后同步启动工作并返回钩子；返回错误则不登记、不启动。
	Start func() (JobHooks, error)
}

// Read 一次读取结果（对齐 DSH JobRead）。
type Read struct {
	Text     string
	Snapshot JobSnapshot
	// Streaming 该任务的输出在**运行期就可读**（生产方提供了 ReadOutput 增量，
	// 或 TailOutput 尾部快照），还是**只在落定时一次性可读**（final-output）。
	// 调用方据此决定「等首块输出」值不值得等：可读任务随时可能有新内容，
	// final-output 任务在结束前永远是空的。
	Streaming bool
	// Tailable 生产方提供了 TailOutput（运行期可读到「最近输出快照」）。
	//
	// 调用方据此决定等多久：真增量任务值得等着看下一块，而尾部快照随时可读——
	// 等只是「接住刚好正在打印的那几行」，不该按分钟级预算阻塞。
	Tailable bool
	// Partial 任务**未落定**时能看到的最近输出快照（来自 TailOutput）；落定后为空，
	// 完整输出走 Text。它不消费、不推进任何游标，连读两次拿到的是同一段尾巴。
	Partial string
	// PartialBytes 已产生的输出总字节数（Partial 只是其中最后一段）。0 表示未知。
	PartialBytes int64
	// LastActivity 上次活动时间（生产方提供 Activity 时）；零值表示未知。
	LastActivity time.Time
	// ReadCount 该任务终态输出被读取的次数（1 = 第一次读到终态）。
	// 最终输出任务读起来是幂等的，模型却不知道自己已经读过——同一个任务被连读
	// 两三次、每次都拿到同样一段输出，界面看起来就像"重复渲染"。这个计数让
	// 调用方能在返回里点明"这是第 N 次，内容没变"。
	ReadCount int
}

// JobSnapshot 任务只读快照（每次 fresh 投影，非活跃注册表状态）。
type JobSnapshot struct {
	ID               string
	Kind             string
	Label            string
	OutputLimitBytes int
	Owner            string // owner 会话标识；空 = 无 owner
	Status           JobStatus
	Detail           string
	StartedAt        time.Time
	FinishedAt       time.Time // 零值 = 未落定
	// Reported kill/read/wait 触及终态时置位，供完成通知抑制重复。
	Reported bool
	// Awaited 有调用方正阻塞在 Wait 上。落定后由该调用方直接取回输出，
	// 再发一次「任务完成」唤醒纯属重复（模型会收到自己刚读过的内容）。
	Awaited bool
}

// Registry 后台任务注册表（线程安全）。
type Registry struct {
	mu            sync.Mutex
	jobs          map[string]*Job
	order         []string                  // 启动顺序（List 按此返回）
	doneListeners map[int]func(JobSnapshot) // 完成监听器（contained，落定时通知）
	nextListener  int
	logger        hclog.Logger // 监听器 panic 等内部错误的日志出口；nil 则静默
}

// ShortID 生成 Git 风格的短哈希标识：<kind>-<7 位小写十六进制>。
//
// 所有「需要人类可读唯一标识」的地方共用它：后台任务（Registry.Start）、
// 定时任务、workflow 运行。熵取自 crypto/rand 的 4 字节（展示 28 位），
// 与进程、会话长度都无关——旧的「进程内自增序号」每重启一次就从 1 重来，
// 超长会话里会攒出十几个同名标识，通知与查询都无法指认是哪一个。
//
// 需要同表查重时（如后台任务表）用 newJobID；单次生成用它。
func ShortID(kind string) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 不可用（极罕见）：退回纳秒时间熵，仍带低位噪声
		n := time.Now().UnixNano()
		b = [4]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	}
	return kind + "-" + hex.EncodeToString(b[:])[:7]
}

// newJobID 生成后台任务标识（同表内查重，撞了就重生成）。
func newJobID(kind string, exists func(string) bool) string {
	for i := 0; i < 16; i++ {
		if id := ShortID(kind); !exists(id) {
			return id
		}
	}
	// 极端兜底：连撞 16 次也给一个唯一标识，绝不返回空 id
	return fmt.Sprintf("%s-%d", kind, time.Now().UnixNano())
}

// SetLogger 注入注册表内部日志出口（监听器 panic 等），便于问题可追溯。
func (r *Registry) SetLogger(l hclog.Logger) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logger = l
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry {
	return &Registry{jobs: make(map[string]*Job)}
}

// maxRetainedJobs 保留的终态任务数上限；超过则淘汰最旧的终态任务，
// 防止已完成 job 在 r.jobs/order 无限累积。
const maxRetainedJobs = 256

// pruneLocked 淘汰最旧的终态 job，直到终态数量不超过 maxRetainedJobs（需已持有 r.mu）。
func (r *Registry) pruneLocked() {
	terminal := 0
	for _, id := range r.order {
		if isTerminalStatus(r.jobs[id].snapshot.Status) {
			terminal++
		}
	}
	over := terminal - maxRetainedJobs
	if over <= 0 {
		return
	}
	kept := make([]string, 0, len(r.order))
	for _, id := range r.order {
		if over > 0 && isTerminalStatus(r.jobs[id].snapshot.Status) {
			delete(r.jobs, id)
			over--
			continue
		}
		kept = append(kept, id)
	}
	r.order = kept
}

// isTerminalStatus 判断是否终态（可被淘汰）。
func isTerminalStatus(s JobStatus) bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusKilled
}

// Job 注册表内部记录（公开快照经 Snapshot 投影）。
type Job struct {
	snapshot      JobSnapshot
	output        string // final-output 终态输出（幂等读）
	terminalReads int    // 终态输出被读次数（见 Read.ReadCount）
	cancel        func(reason string)
	readOutput    func() string            // nil = 非增量
	tailOutput    func() (string, int64)   // 可选：运行期输出快照
	activity      func() (time.Time, bool) // 可选：最近活动时间
	settled       chan struct{}            // 落定后 close
}

// Start 预检 + 同步启动 + 登记（对齐 DSH）：预检失败或 Start 抛错都不产生
// 任务；成功返回后登记不可失败。done 在后台落定。
func (r *Registry) Start(spec StartSpec) (string, error) {
	if strings.TrimSpace(spec.Kind) == "" {
		return "", fmt.Errorf("jobs: kind is required")
	}
	if spec.Start == nil {
		return "", fmt.Errorf("jobs: starter is required")
	}
	hooks, err := spec.Start()
	if err != nil {
		return "", err
	}
	r.mu.Lock()
	id := newJobID(spec.Kind, func(cand string) bool {
		_, exists := r.jobs[cand]
		return exists
	})
	j := &Job{
		snapshot: JobSnapshot{
			ID: id, Kind: spec.Kind, Label: spec.Label,
			OutputLimitBytes: spec.OutputLimitBytes,
			Owner:            spec.Owner,
			Status:           StatusRunning,
			StartedAt:        time.Now(),
		},
		cancel:     hooks.Cancel,
		readOutput: hooks.ReadOutput,
		tailOutput: hooks.TailOutput,
		activity:   hooks.Activity,
		settled:    make(chan struct{}),
	}
	r.jobs[id] = j
	r.order = append(r.order, id)
	r.mu.Unlock()

	go func() {
		outcome := <-hooks.Done
		r.mu.Lock()
		j.snapshot.FinishedAt = time.Now()
		switch outcome.Status {
		case StatusCompleted:
			j.snapshot.Status = StatusCompleted
			j.output = outcome.Output
		case StatusKilled:
			j.snapshot.Status = StatusKilled
			// 被终止的任务也可能携带「终止前已产生的输出」（生产方从尾部快照兜底），
			// 与 completed 一样保存下来——否则事后读取只剩 (no output yet)。
			j.output = outcome.Output
		case StatusFailed:
			j.snapshot.Status = StatusFailed
		default:
			j.snapshot.Status = StatusFailed
			outcome.Detail = "producer settled with invalid status"
		}
		j.snapshot.Detail = outcome.Detail
		close(j.settled)
		// 终态任务淘汰：防已完成 job 在 r.jobs/order 无限累积
		r.pruneLocked()
		// 完成监听器（contained）：在锁外逐个调用，异常隔离，不阻塞落定
		listeners := make([]func(JobSnapshot), 0, len(r.doneListeners))
		for _, fn := range r.doneListeners {
			listeners = append(listeners, fn)
		}
		snap := j.snapshot
		if snap.Awaited {
			listeners = nil // 等待方会直接取回输出，无需唤醒
		}
		r.mu.Unlock()
		for _, fn := range listeners {
			func() {
				defer func() {
					if p := recover(); p != nil {
						if r.logger != nil {
							r.logger.Error("job done listener panic", "job", snap.ID, "panic", p)
						}
					}
				}()
				fn(snap)
			}()
		}
	}()
	return id, nil
}

// OnJobDone 注册完成监听器（对齐 DSH onJobDone）：每个落定记录都会通知所有
// 监听器，携带 fresh 快照。监听器异常被隔离（contained），不影响落定与其他
// 监听器。返回取消函数（幂等）。
func (r *Registry) OnJobDone(fn func(snapshot JobSnapshot)) func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.doneListeners == nil {
		r.doneListeners = make(map[int]func(JobSnapshot))
	}
	id := r.nextListener
	r.nextListener++
	r.doneListeners[id] = fn
	return func() {
		r.mu.Lock()
		delete(r.doneListeners, id)
		r.mu.Unlock()
	}
}

// authorize 校验任务存在且调用方被授权（owner 隔离：空调用方仅限无 owner 任务）。
func (r *Registry) authorize(id, caller string) (*Job, error) {
	j, ok := r.jobs[id]
	if !ok {
		return nil, r.unknownJobLocked(id)
	}
	if j.snapshot.Owner != "" && j.snapshot.Owner != caller {
		return nil, fmt.Errorf("jobs: job %q belongs to another session", id)
	}
	return j, nil
}

// unknownJobLocked 生成「任务不存在」的错误（需已持有锁）。
//
// 为什么要把原因写进错误里：job id 只是**本进程**注册表里的键，从不落盘。会话记录
// 里留着的 id（上一次运行打印的、或父进程打印而被子进程读到的）在新进程里必然查
// 不到。光秃秃一句 "unknown job" 会让模型以为是自己记错，于是原地重试、或改成相近
// 的 id 再试，整轮就耗在这上面；明确说「换进程就失效，用 job_list 看现有的」它才有
// 可行动的下一步。
//
// 这条路径是**模型可见**的（job_output/job_kill 经 authorize）；宿主管理视图
// （ReadHost/KillHost）保持短句，TUI 自己就有任务列表，不需要这串解释。
func (r *Registry) unknownJobLocked(id string) error {
	running := 0
	for _, j := range r.jobs {
		if j.snapshot.Status == StatusRunning || j.snapshot.Status == StatusStopping {
			running++
		}
	}
	return fmt.Errorf("jobs: unknown job %q (job ids live only in this process and are never persisted; "+
		"an id printed by an earlier run or by another process no longer exists. "+
		"Use job_list to see current jobs — %d running now)", id, running)
}

// Get 返回非消费快照（不改变游标与 reported）。
func (r *Registry) Get(id, caller string) (JobSnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, err := r.authorize(id, caller)
	if err != nil {
		return JobSnapshot{}, err
	}
	return j.snapshot, nil
}

// List 返回调用方可见任务（自己拥有 + 无 owner），按启动顺序。
func (r *Registry) List(caller string) []JobSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]JobSnapshot, 0, len(r.order))
	for _, id := range r.order {
		j := r.jobs[id]
		if j.snapshot.Owner == "" || j.snapshot.Owner == caller {
			out = append(out, j.snapshot)
		}
	}
	return out
}

// ListAll 返回全部任务快照（宿主管理视图：TUI /jobs 使用，不做 owner 隔离）。
func (r *Registry) ListAll() []JobSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]JobSnapshot, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.jobs[id].snapshot)
	}
	return out
}

// readLocked 读取单个任务输出（需已持有锁）：流式任务消费自上次以来的增量；
// 最终输出任务在终止后幂等返回终态输出（运行中为空）。终止读取标记 reported。
//
// 运行期还有两条「观察」通道（都不消费）：TailOutput 给最近输出快照、Activity 给
// 上次活动时间。它们只对未落定任务有意义——落定后完整输出就在 Text 里，尾部是冗余。
func (r *Registry) readLocked(j *Job) Read {
	rd := Read{Snapshot: j.snapshot}
	if j.readOutput != nil {
		rd.Text = j.readOutput()
		rd.Streaming = true
	}
	running := j.snapshot.Status == StatusRunning || j.snapshot.Status == StatusStopping
	if j.tailOutput != nil {
		rd.Tailable = true
		if running {
			rd.Partial, rd.PartialBytes = j.tailOutput()
		}
		// 尾部快照也算「运行期可读」：调用方据此决定等不等首块内容。
		rd.Streaming = true
	}
	if j.activity != nil {
		if ts, ok := j.activity(); ok {
			rd.LastActivity = ts
		}
	}
	if j.readOutput != nil || running {
		return rd
	}
	// 终态：幂等返回最终输出，并计数（见 Read.ReadCount）
	j.snapshot.Reported = true
	j.terminalReads++
	rd.Snapshot = j.snapshot // 上面动过 snapshot（reported），快照要跟着更新
	rd.Text = j.output
	rd.Partial = ""
	rd.ReadCount = j.terminalReads
	return rd
}

// Read 读取输出：流式任务消费自上次以来的增量（单一游标）；最终输出任务在
// 终止后幂等返回终态输出（运行中为空）。终止读取标记 reported。
func (r *Registry) Read(id, caller string) (Read, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, err := r.authorize(id, caller)
	if err != nil {
		return Read{}, err
	}
	return r.readLocked(j), nil
}

// ReadHost 读取任意任务输出与状态（宿主管理视图：TUI /jobs output，跳过 owner 隔离）。
func (r *Registry) ReadHost(id string) (Read, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return Read{}, fmt.Errorf("jobs: unknown job %q", id)
	}
	return r.readLocked(j), nil
}

// killLocked 请求取消单个任务（需已持有锁）：先标记 stopping + reported，再调用
// 生产方 cancel（reason 转发）。已终态返回 already-finished（幂等，不重复取消）。
func (r *Registry) killLocked(j *Job, reason string) KillResult {
	if j.snapshot.Status != StatusRunning && j.snapshot.Status != StatusStopping {
		return KillAlreadyFinished
	}
	j.snapshot.Status = StatusStopping
	j.snapshot.Reported = true
	if j.cancel != nil {
		j.cancel(reason)
	}
	return KillRequested
}

// Kill 请求取消：先标记 stopping + reported，再调用生产方 cancel（reason 转发）。
// 已终态返回 already-finished（幂等，不重复取消）。
func (r *Registry) Kill(id, caller, reason string) (KillResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, err := r.authorize(id, caller)
	if err != nil {
		return "", err
	}
	return r.killLocked(j, reason), nil
}

// KillHost 请求取消任意任务（宿主管理视图：TUI /jobs kill，跳过 owner 隔离）。
func (r *Registry) KillHost(id, reason string) (KillResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return "", fmt.Errorf("jobs: unknown job %q", id)
	}
	return r.killLocked(j, reason), nil
}

// Claim 声明「有调用方正同步等待这个任务的结果」：落定时不发完成通知
// （等待方自己就会把输出交出去，再通知一次纯属重复）。
//
// 与 Wait 内部的 Awaited 同一语义，区别是**可以撤销**：前台宽限期场景里，
// 宽限内跑完就 Claim 着收结果，超时转后台时必须 Release 回来，否则这条任务
// 落定时会一声不响——恰好是「长任务完成没人知道」的老毛病。
func (r *Registry) Claim(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return fmt.Errorf("jobs: unknown job %q", id)
	}
	j.snapshot.Awaited = true
	return nil
}

// Release 撤销 Claim（任务落定时恢复完成通知）。未知 id 静默忽略（幂等）。
func (r *Registry) Release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if j, ok := r.jobs[id]; ok {
		j.snapshot.Awaited = false
	}
}

// AwaitBudget 有界等待落定：期限内完成则返回终态快照与最终输出，
// 到期未完成则返回当前（running）快照并报告未完成——**不取消任务**。
//
// 与 Wait 的差别：不认领结果（Awaited 由调用方经 Claim 决定），超时后任务
// 继续在后台跑、落定时仍走正常完成通知。用于「前台执行宽限期」：
//   - 宽限内跑完 → 当作一次普通同步工具调用，结果直接返回给模型；
//   - 超时 → 把 job id 交给模型，任务转入后台，完成时照常通知。
func (r *Registry) AwaitBudget(id string, d time.Duration) (JobSnapshot, string, bool) {
	r.mu.Lock()
	j, ok := r.jobs[id]
	if !ok {
		r.mu.Unlock()
		return JobSnapshot{}, "", false
	}
	if isTerminalStatus(j.snapshot.Status) {
		snap, out := j.snapshot, j.output
		r.mu.Unlock()
		return snap, out, true
	}
	settled := j.settled
	r.mu.Unlock()

	select {
	case <-settled:
		r.mu.Lock()
		snap, out := j.snapshot, j.output
		r.mu.Unlock()
		return snap, out, true
	case <-time.After(d):
		r.mu.Lock()
		snap := j.snapshot
		r.mu.Unlock()
		return snap, "", false
	}
}

// Discard 删除一个任务记录（宿主内部瞬时任务用：宽限期内跑完的前台命令
// 就是一次普通工具调用，没有理由在 job_list 里留一条 finished 记录）。
func (r *Registry) Discard(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.jobs[id]; !ok {
		return
	}
	delete(r.jobs, id)
	for i, x := range r.order {
		if x == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

// Wait 有界等待落定（不取消任务）：超时返回存活快照；落定后返回终态快照并
// 标记 reported（该等待方已获知终态）。
func (r *Registry) Wait(id string, timeout time.Duration, caller string) (JobSnapshot, error) {
	r.mu.Lock()
	j, err := r.authorize(id, caller)
	if err != nil {
		r.mu.Unlock()
		return JobSnapshot{}, err
	}
	if j.snapshot.Status != StatusRunning && j.snapshot.Status != StatusStopping {
		r.mu.Unlock()
		return j.snapshot, nil
	}
	settled := j.settled
	// 必须在解锁/阻塞**之前**登记：落定广播在 close(settled) 之后立刻触发，
	// 等到 <-settled 再置位就已经晚了一步（通知已经发出去了）。
	j.snapshot.Awaited = true
	r.mu.Unlock()

	select {
	case <-settled:
		r.mu.Lock()
		defer r.mu.Unlock()
		j.snapshot.Reported = true
		return j.snapshot, nil
	case <-time.After(timeout):
		r.mu.Lock()
		defer r.mu.Unlock()
		// 等待超时 = 这次等待没拿到结果，必须把认领撤掉：否则任务稍后落定时
		// 会因为 Awaited 仍然为真而不发完成通知，等待方白白错过「它已经完了」
		// ——于是只能再 wait 一次，又一次阻塞。没消费成功，就不算送达。
		if j.snapshot.Status == StatusRunning || j.snapshot.Status == StatusStopping {
			j.snapshot.Awaited = false
		} else {
			j.snapshot.Reported = true // 恰好在超时瞬间落定：这次等待已经读到终态
		}
		return j.snapshot, nil
	}
}
