package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"dsc/jobs"
)

// ctxCallerKey context 键：从工具调用链路解析出的调用方会话标识。
type ctxCallerKey struct{}

// WithCaller 把调用方会话标识注入 ctx。
func WithCaller(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, ctxCallerKey{}, sessionID)
}

// CallerFromContext 读取 ctx 中的调用方会话标识（空 = 无会话调用方）。
// toolCallIDKey 保存本次工具调用 id（与 caller 会话一起跨层转发）。
type toolCallIDKey struct{}

// WithToolCallID 把工具调用 id 注入 ctx。
func WithToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

// ToolCallIDFromContext 读取工具调用 id（无则空串）。
func ToolCallIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(toolCallIDKey{}).(string); ok {
		return v
	}
	return ""
}

func CallerFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxCallerKey{}).(string); ok {
		return v
	}
	return ""
}

// JobDoneEvent 后台任务完成事件名（宿主事件总线发布；任何 UI/插件均可订阅，
// 对齐 DSH onJobDone 的通用送达——TUI 唤醒、web 界面插件、novelforge 等）。
const JobDoneEvent EventName = "job/done"

// ListJobs 返回宿主侧全部后台任务快照（TUI /jobs 管理视图：宿主是任务所有者，
// 不做 owner 会话隔离）。
func (m *Manager) ListJobs() []jobs.JobSnapshot {
	if m.jobs == nil {
		return nil
	}
	return m.jobs.ListAll()
}

// ReadJob 读取任务输出与状态（TUI /jobs output；宿主管理视图）。
func (m *Manager) ReadJob(id string) (jobs.Read, error) {
	if m.jobs == nil {
		return jobs.Read{}, fmt.Errorf("jobs registry not available")
	}
	return m.jobs.ReadHost(id)
}

// KillJob 请求取消任务（TUI /jobs kill；宿主管理视图）。
func (m *Manager) KillJob(id, reason string) (jobs.KillResult, error) {
	if m.jobs == nil {
		return "", fmt.Errorf("jobs registry not available")
	}
	return m.jobs.KillHost(id, reason)
}

// OnEvent 订阅宿主事件总线（如 JobDoneEvent 完成通知；TUI/web 插件等复用）。
// 返回取消函数。
func (m *Manager) OnEvent(name EventName, fn Listener) func() {
	return m.events.On(name, fn)
}

// jobTool 面向模型的 job 工具族（对齐 DSH tool-jobs：job_output/job_list/job_kill）。
// 后台任务由 Manager.jobs 注册表承载（owner 隔离 + 消费式游标；v1 生产方：
// workflow background）。caller（调用方会话）经工具调用链路注入 ctx。

// defaultWaitTimeout 与 maxWaitTimeout job_output 的 wait 默认/上限等待时间。
//
// 上限刻意压到 60s（对齐「不阻塞模型」的语义）：wait 是同步工具调用，等多久
// 模型就多久不能动——期间任何通知、任何用户插话都进不来。等待超时会把认领撤回，
// 任务落定时照常发完成通知，所以「等到一半放弃」不会丢结果。
const (
	defaultWaitTimeout = 30 * time.Second
	maxWaitTimeout     = 60 * time.Second
)

// outputWaitTimeout 没有任何输出时，job_output 先等多久再回报 (no output yet)。
//
// 为什么要有这段等待：后台任务的结果只在**落定后**才可读（final-output 模式），
// 所以「刚转后台就立刻读」必然拿到空串。模型拿到一行「(no output yet)」什么也做不了
// ——它要么立刻再读一次（空转烧 token），要么以为任务没起来。等一小会儿，绝大多数
// 命令都能在预算内跑完并把真结果交出去；真等不到再如实说没输出。
//
// 上限与 maxWaitTimeout 同源：这仍是一次同步工具调用，不能无限占着模型。
// DSC_JOB_OUTPUT_WAIT_SECS 可覆盖（0 = 不等，立刻回报；负数视同 0）。
const outputWaitTimeout = 60 * time.Second

// outputWaitPoll 等待首块输出时的轮询间隔。
//
// 为什么轮询而不是事件：注册表只为「落定」提供了广播通道（settled），没有为
// 「追加了输出」提供；流式任务（ReadOutput）更没有。120ms 的轮询对 60s 预算来说
// 是 500 次内存读，代价可以忽略，换来的是两条路径（final / stream）行为一致。
const outputWaitPoll = 120 * time.Millisecond

// finalOutputWait final-output 任务（绝大多数后台命令）的默认等待上限。
//
// 这类任务的输出**只在落定时**才可读，运行期永远是空的：等它等于赌「它就快结束了」。
// 任务结束时宿主会主动送完成通知，所以赌错的代价就是白占一首歌的时间——实测用户
// 起一个长驻服务后，`job_output` 干等满 60s 仍只有一行 (no output yet)。
// 留 3s 只为接住「刚转后台就收尾」的短命令；真要阻塞等结果请显式 wait:true。
const finalOutputWait = 3 * time.Second

// tailOutputWait 尾部快照任务的等待上限。
//
// 它比 final-output 长一点（接住「刚启动、插件还没吐字」的那几百毫秒），又远短于
// 增量任务的预算：快照类任务的价值在于「马上告诉我现在是什么样」，而不是赌它结束。
const tailOutputWait = 2 * time.Second

// jobWaitCap 生效的 wait 上限。DSC_JOB_WAIT_MAX_SECS 可覆盖：
// 设为 0（或负数）即彻底禁止阻塞式等待——此时 wait:true 会被直接拒绝并提示
// 「完成会通知你」，适合完全不允许工具调用占死模型的场景。
func jobWaitCap() time.Duration {
	v := os.Getenv("DSC_JOB_WAIT_MAX_SECS")
	if v == "" {
		return maxWaitTimeout
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return maxWaitTimeout
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// jobOutputWait 生效的「等首块输出」预算；0 表示不等（立刻回报）。
//
// 两个闸门都尊重：显式的 DSC_JOB_OUTPUT_WAIT_SECS，以及宿主级的 DSC_JOB_WAIT_MAX_SECS
// ——部署方既然禁掉了阻塞式等待，这里也不该偷偷阻塞一分钟。
func jobOutputWait() time.Duration {
	cap := jobWaitCap()
	if cap == 0 {
		return 0 // 宿主明确禁止阻塞式等待
	}
	budget := outputWaitTimeout
	if cap < budget {
		budget = cap
	}
	if v := strings.TrimSpace(os.Getenv("DSC_JOB_OUTPUT_WAIT_SECS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return budget // 写坏了按默认走，别把「等」悄悄变成「不等」
		}
		if n <= 0 {
			return 0
		}
		budget = time.Duration(n) * time.Second
		if budget > cap {
			budget = cap
		}
	}
	return budget
}

type jobTool struct {
	m    *Manager
	name string
}

func (t *jobTool) Name() string { return t.name }

func (t *jobTool) Description() string {
	switch t.name {
	case "job_output":
		return "Read a background job's output and status. While the job runs you get whatever is observable at that moment: tools that stream report a partial tail ('[partial output — last 4.0 KB of 37.0 KB so far]'), and every running job reports how long it has been running plus when it last produced output — that is how you tell 'still working' from 'stuck'. A running job with nothing to show answers '(no output yet)' and tells you which case it is: the tool cannot stream (output only arrives when it ends), or nothing has been written yet — usually a program block-buffering its own stdout because it is a pipe rather than a terminal, which is why a manual run shows prints and this does not; read that note before you retry, because it says what would actually help (run unbuffered, keep output attached) and how many times you have already asked. Polling does not change any of this, and a completion notice reaches you automatically when the job ends. This call waits a few seconds first (to catch jobs that are about to finish or that just started printing), then reports the current state. Do not poll in a loop; if you need a fresh signal, one re-read after a short separate command (e.g. a curl/health check) is enough. Only use wait:true when you cannot take another step without the settled result: it blocks until the job ends (max 60s, and an expired wait still notifies you later). Responses end with [status: ...]."
	case "job_list":
		return "List background jobs visible to this caller as '<id> [<kind>] <status> — <label>', one per line."
	case "job_kill":
		return "Stop a background job: pass its job_id (the one printed when the job started) and an optional reason. " +
			"This cancels the running tool call, so the process it started is terminated and the job settles as killed. " +
			"Kill it this way rather than through the shell (taskkill/kill): external termination bypasses the job record and leaves it stuck at running. " +
			"A finished job returns its terminal status instead."
	}
	return ""
}

func (t *jobTool) ParametersSchema() json.RawMessage {
	switch t.name {
	case "job_output":
		return json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"wait":{"type":"boolean"},"timeout_ms":{"type":"integer"}},"required":["job_id"],"additionalProperties":false}`)
	case "job_list":
		return json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	case "job_kill":
		return json.RawMessage(`{"type":"object","properties":{"job_id":{"type":"string"},"reason":{"type":"string"}},"required":["job_id"],"additionalProperties":false}`)
	}
	return json.RawMessage(`{}`)
}

func (t *jobTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	caller := CallerFromContext(ctx)
	switch t.name {
	case "job_output":
		return t.execOutput(ctx, args, caller)
	case "job_list":
		return t.execList(caller)
	case "job_kill":
		return t.execKill(args, caller)
	}
	return "", fmt.Errorf("job: unknown tool %q", t.name)
}

// execOutput 读取任务输出与状态（对齐 DSH job_output）：非阻塞默认；
// wait=true 时先有界等待落定，再消费读取。
func (t *jobTool) execOutput(ctx context.Context, args json.RawMessage, caller string) (string, error) {
	var p struct {
		JobID     string `json:"job_id"`
		Wait      bool   `json:"wait"`
		TimeoutMS int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("job_output: invalid args: %w", err)
	}
	deadline := time.Duration(p.TimeoutMS) * time.Millisecond
	if deadline <= 0 {
		deadline = defaultWaitTimeout
	}
	waitDropped := false
	if waitCap := jobWaitCap(); waitCap <= 0 {
		if p.Wait {
			// 禁止阻塞：把「等」变成「现在读 + 完成会通知」。先返回当前快照，
			// 让模型立刻拿回控制权，而不是卡在这里。
			p.Wait = false
			waitDropped = true
		}
	} else if deadline > waitCap {
		deadline = waitCap
	}
	if p.Wait {
		if _, err := t.m.jobs.Wait(p.JobID, deadline, caller); err != nil {
			return "", fmt.Errorf("job_output: %w", err)
		}
	}
	rd, err := t.m.jobs.Read(p.JobID, caller)
	if err != nil {
		return "", fmt.Errorf("job_output: %w", err)
	}
	// 还没有任何输出、任务也还在跑：先等一小会儿再回报，别把「刚转后台，来不及出东西」
	// 当成「这个任务没有输出」。等到了就立刻返回（不等满预算），等不到才如实说没有。
	if !p.Wait && rd.Text == "" && jobRunning(rd.Snapshot.Status) {
		rd = t.awaitFirstOutput(ctx, p.JobID, caller, rd)
	}
	// 运行中读到空：记一次，供返回文本点明「这是第 N 次、还是 0 字节」。
	// 只数 job_output 调用本身，不数 awaitFirstOutput 内部的轮询读（那会数成几百次）。
	emptyReads := 0
	if rd.Text == "" && rd.Partial == "" && jobRunning(rd.Snapshot.Status) {
		emptyReads = t.m.jobs.NoteEmptyRead(p.JobID, caller)
	}
	out := renderJobOutput(rd, emptyReads)
	if waitDropped {
		out += "\n[note: blocking wait is disabled on this host; returned the current state instead. You will be notified when the job finishes.]"
	}
	return out, nil
}

// jobRunning 任务是否仍在跑（running / stopping 都算）。
func jobRunning(status jobs.JobStatus) bool {
	return status == jobs.StatusRunning || status == jobs.StatusStopping
}

// awaitFirstOutput 等「第一块输出」或任务落定，最多 jobOutputWait()；超时返回最后一次读到的快照。
//
// 退出条件刻意有三条：拿到输出（立刻走）、任务落定（读完就可能拿到最终输出）、
// ctx 取消（用户中断 / 回合结束，不能把人卡在这里）。
//
// 轮询读流式任务会消费增量——那正是我们想要的：谁先读到就归谁，返回的就是这次的
// 第一块输出；没读到的部分留给下一次 job_output。超时那次读到的空结果不会丢任何
// 内容（流式任务本来就还没产生，最终输出任务在落定后仍可幂等读）。
func (t *jobTool) awaitFirstOutput(ctx context.Context, id, caller string, last jobs.Read) jobs.Read {
	budget := jobOutputWait()
	switch {
	case last.Tailable:
		// 尾部快照随时可读：等只是接住「刚巧正在打印的那几行」。没有内容就直接
		// 报进度（跑了多久、上次活动多久前），不该按分钟级预算把人卡住——这正是
		// 之前「等满一分钟还是 (no output yet)」被诟病的形态。
		if budget > tailOutputWait {
			budget = tailOutputWait
		}
	case !last.Streaming:
		// final-output 任务：运行期没有可读输出，长等只是赌它马上结束。
		// 催它快点没用，等满 60s 也一样是空——上限压到 finalOutputWait。
		if budget > finalOutputWait {
			budget = finalOutputWait
		}
	}
	if budget <= 0 {
		return last
	}
	deadline := time.Now().Add(budget)
	ticker := time.NewTicker(outputWaitPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return last
		case <-ticker.C:
		}
		rd, err := t.m.jobs.Read(id, caller)
		if err != nil {
			return last // 任务被剔掉/权限变化：退回上一次的结果，不把错误伪装成空输出
		}
		last = rd
		// 尾部快照也算拿到内容：这类任务（后台 shell 等）在落定前只能给这一段，
		// 继续等下去拿到的还是同一段尾巴。
		if rd.Text != "" || rd.Partial != "" || !jobRunning(rd.Snapshot.Status) {
			return rd
		}
		if !time.Now().Before(deadline) {
			return rd
		}
	}
}

// renderJobOutput 渲染 job_output 结果（对齐 DSH：输出 + [status: ...] 结尾；
// 空输出用 (no output yet)）。
//
// emptyReads 是「运行中读到空」的第几次（见 jobs.Registry.NoteEmptyRead）：
// 第一次给完整解释与出路，之后再读只点明「还是 0 字节」，不再重复整段说明。
//
// 重复读取要明说：最终输出任务是幂等的，模型连读两次拿到的是同一段文字，而它
// 无从知道自己已经读过（实测表现为同一个任务被反复读取、界面上出现两张一模一样
// 输出卡）。这里在第 2 次及以后的读取尾部标注次数与「内容未变」，模型据此收手。
func renderJobOutput(rd jobs.Read, emptyReads int) string {
	var b strings.Builder
	switch {
	case rd.Text != "":
		b.WriteString(rd.Text)
	case rd.Partial != "":
		// 任务还没结束，但执行方已经能给出最近一段输出（尾部快照）。
		// 明说这是尾部与总量：模型不该把「最后 4 KB」当成完整结果。
		fmt.Fprintf(&b, "[partial output — last %s of %s so far; the full output arrives when the job finishes]\n",
			humanBytes(int64(len(rd.Partial))), humanBytes(rd.PartialBytes))
		b.WriteString(rd.Partial)
	default:
		b.WriteString("(no output yet)")
	}
	empty := rd.Text == "" && rd.Partial == ""
	running := jobRunning(rd.Snapshot.Status)
	b.WriteString("\n[status: " + string(rd.Snapshot.Status) + "]")
	if rd.Snapshot.Detail != "" {
		b.WriteString(" " + rd.Snapshot.Detail)
	}
	// 运行中补一句「跑了多久 / 上次动是多久前」：final-output 任务这时没有可读输出，
	// 模型唯一能判断的是它还在动、还是已经安静很久（那才是该考虑 job_kill 的时候）。
	if running {
		b.WriteString("\n" + jobProgressNote(rd, empty))
	}
	// 「在跑但一个字节都没有」需要解释：不解释，模型只会反复读空串。
	if empty && running {
		b.WriteString("\n" + silentJobNote(rd, emptyReads))
	}
	if rd.ReadCount > 1 {
		fmt.Fprintf(&b, "\n[note: read #%d of this job's final output; it has not changed since your previous read — no need to read it again]", rd.ReadCount)
	}
	return b.String()
}

// silentJobNote 解释「任务在跑，却一个字节都没有」，并给出唯一可行的下一步。
//
// 这段文本存在的理由很具体：启动期加载（大模型、冷编译、慢磁盘）最常见的形态是
// 「进程活着、job_output 却永远是 (no output yet)」，而人工在终端里跑同一条命令
// 明明看得见打印。差别不在读取链路，在**子进程自己的缓冲**：stdout 是管道而不是
// 终端时按块（4–8 KB）缓冲，打印要等缓冲写满或进程退出才离开子进程——字节根本
// 还没到我们这里，所以它也不在任何「尾部快照」里。
//
// 因此这里明确三件事，缺一不可：0 字节是真的 0 字节（不是我们没读到）；为什么；
// 想看实时输出该怎么做。否则模型拿到的只是一句它无法行动的「没有输出」。
func silentJobNote(rd jobs.Read, emptyReads int) string {
	if !rd.Streaming {
		// 这类工具运行期根本不提供输出（final-output）：不是缓冲问题，别开错药方。
		if emptyReads > 1 {
			return fmt.Sprintf("[note: still no output — this is read #%d, and this job only reports output when it finishes. Stop polling; a completion notice reaches you.]", emptyReads)
		}
		return "[note: this job exposes output only when it finishes; it is still running. " +
			"Stop polling — a completion notice will reach you. Use wait:true only if you truly cannot proceed without the result.]"
	}
	if emptyReads > 1 {
		return fmt.Sprintf("[note: still nothing written — read #%d, still 0 bytes. Nothing changed since the previous read; "+
			"the reason and the way to get live output are in the first read's note. Stop polling — a completion notice reaches you.]", emptyReads)
	}
	return "[why no output: the process has not written anything yet. A child process whose stdout is a pipe rather than a " +
		"terminal block-buffers its own prints (typically 4–8 KB, or until it exits), so startup output can stay inside the " +
		"process for minutes even though the same command shows prints when run in a terminal. 0 bytes here means 0 bytes " +
		"arrived — nothing was dropped on this side.]\n" +
		"[what helps: " + unbufferedHint() + " A log redirect (`> log 2>&1`) silences this view as well, so keep output " +
		"attached if you want to watch progress here — redirect freely when the log file itself is what you are after. " +
		"Otherwise stop polling: a completion notice reaches you.]"
}

// unbufferedHint 按平台给出「让子进程别缓冲」的具体写法。
//
// 不写平台无关的一句话：`stdbuf` 在 Windows 上不存在，写上去等于让模型去撞墙；
// 而 python 的 -u / PYTHONUNBUFFERED 两边都有，是这类启动脚本最通用的开关。
func unbufferedHint() string {
	if runtime.GOOS == "windows" {
		return "restart it unbuffered (`python -u`, or PYTHONUNBUFFERED=1) and keep stdout/stderr attached."
	}
	return "restart it unbuffered (`python -u`, PYTHONUNBUFFERED=1, or `stdbuf -oL <cmd>`) and keep stdout/stderr attached."
}


// jobProgressNote 运行中任务的进度摘要（elapsed + 上次输出）。
//
// empty=true（运行中、还没有任何可读输出）时说清字节数：尾部通道可用却仍报 0 字节，
// 是「子进程还没吐字」的直接证据；只说「还在跑」会让模型以为再读一次就有了。
func jobProgressNote(rd jobs.Read, empty bool) string {
	parts := make([]string, 0, 3)
	if !rd.Snapshot.StartedAt.IsZero() {
		parts = append(parts, "running for "+humanDuration(time.Since(rd.Snapshot.StartedAt)))
	}
	switch {
	case !rd.LastActivity.IsZero():
		// 说 activity 而不是 output：SDK 在工具进入时就登记过一次活动，所以这个时间戳
		// 也可能表示「刚进工具、还什么都没打印」——写成 last output 会误导。
		parts = append(parts, "last activity "+humanDuration(time.Since(rd.LastActivity))+" ago")
	case !rd.Snapshot.StartedAt.IsZero():
		// 工具不上报活动（旧插件）：不编造「多久没输出」，只说明这一点。
		parts = append(parts, "this tool does not report activity")
	}
	if empty && rd.Tailable {
		parts = append(parts, humanBytes(rd.PartialBytes)+" written")
	}
	if len(parts) == 0 {
		return "[still running]"
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// humanDuration 人类可读的时长（42s / 4m12s / 1h03m）。
//
// 不复用 time.Duration.String()：那会给 "4m12.318s"，模型和人都只需要到秒。
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// humanBytes 人类可读的字节数（4.0 KB / 37.2 KB / 812 B）。
func humanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	kb := float64(n) / unit
	if kb < unit {
		return fmt.Sprintf("%.1f KB", kb)
	}
	return fmt.Sprintf("%.1f MB", kb/unit)
}

// execList 列出调用方可见任务（对齐 DSH job_list：<id> [<kind>] <status> — <label>）。
func (t *jobTool) execList(caller string) (string, error) {
	all := t.m.jobs.List(caller)
	if len(all) == 0 {
		return "(no background jobs)", nil
	}
	var b strings.Builder
	for _, j := range all {
		fmt.Fprintf(&b, "%s [%s] %s — %s\n", j.ID, j.Kind, j.Status, j.Label)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// execKill 请求取消任务（对齐 DSH job_kill：请求取消 / 已结束返回终态）。
func (t *jobTool) execKill(args json.RawMessage, caller string) (string, error) {
	var p struct {
		JobID  string `json:"job_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("job_kill: invalid args: %w", err)
	}
	res, err := t.m.jobs.Kill(p.JobID, caller, p.Reason)
	if err != nil {
		return "", fmt.Errorf("job_kill: %w", err)
	}
	switch res {
	case jobs.KillRequested:
		return fmt.Sprintf("requested cancellation of job %s", p.JobID), nil
	default: // already-finished
		snap, gerr := t.m.jobs.Get(p.JobID, caller)
		if gerr != nil {
			return "", fmt.Errorf("job_kill: %w", gerr)
		}
		return fmt.Sprintf("job %s already finished [status: %s]", p.JobID, snap.Status), nil
	}
}
