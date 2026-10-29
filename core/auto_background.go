package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// CapabilityAutoBackground 声明式能力标签：声明该能力的工具，宿主在执行前台调用时
// 先按后台任务登记，再给一个「前台宽限期」等结果。
//
// 为什么要这么做：一个 step = 一次 LLM 请求 + 该请求要求的全部工具执行。模型在工具
// 执行期间不在推理，任何注入（用户插话、后台任务完成通知）都插不进去，只能等这一步
// 跑完。一条 `sleep 90 && curl` 就能把整个 step 占死 90 秒——通知于是「迟迟不来」，
// 看起来像插话机制坏了，实际是这一步根本没有可插入的推理在跑。
//
// 自动后台让长命令不再占死 step：宽限期内跑完就当作普通工具结果返回（模型无感），
// 超时则返回 job_id、任务转入后台继续跑，完成时照常发通知——插入点重新回到
// 「两次请求之间」，也就是真正的步插入。
//
// 与 sdk 侧 dsc.CapabilityAutoBackground 同值（对齐同一 wire token）。
const CapabilityAutoBackground = "auto-background"

// defaultAutoBackgroundGrace 前台**空闲预算**：命令前端等待期间，超过这么久没有
// 任何输出（活动）就转入后台。
//
// 这不是「最多等多久」：只要有输出，预算就重新计时（活跃续命），命令可以一直在
// 前台跑完；只有真的安静下来（编译中、阻塞在网络、卡在交互）才把控制权交回模型。
// 60s 是实测定下的经验值：30s 会把「安静的编译/安装/网络等待」误判成卡死，模型隔
// 一步就去看 job，来回切反而更慢；而真正卡死的命令多等 30 秒的代价可以接受。
const defaultAutoBackgroundGrace = 60 * time.Second

// maxAutoBackgroundForeground 前台等待的硬上限：即便命令一直在输出，也不能让模型
// 无限期被占住——到点同样转后台（任务继续跑，只是先把它交出去）。
const maxAutoBackgroundForeground = 10 * time.Minute

// autoBackgroundGrace 生效的前台空闲预算（0 = 关闭自动后台，工具永远同步执行）。
// 可用 DSC_AUTO_BACKGROUND_SECS 覆盖（正数秒数；0 或负数表示关闭）。
var autoBackgroundGrace = autoBackgroundGraceFromEnv()

func autoBackgroundGraceFromEnv() time.Duration {
	v := os.Getenv("DSC_AUTO_BACKGROUND_SECS")
	if v == "" {
		return defaultAutoBackgroundGrace
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultAutoBackgroundGrace
	}
	if n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// activityProbe 可选接口：工具能报告「本次调用最近一次活动时间」。
// *RemoteTool 实现它（转发插件侧 GetActivity）；旧插件不实现 → 退回固定预算。
type activityProbe interface {
	LastActivity(ctx context.Context, callID string) (time.Time, bool)
}

// autoBackgroundable 报告该工具是否声明了自动后台能力。
// 按能力标签识别，不认工具名/插件名——新增同类工具只需在自己的声明里加这个标签。
func (m *Manager) autoBackgroundable(toolName string) bool {
	tool, ok := m.toolRegistry.Get(toolName)
	if !ok {
		return false
	}
	ct, ok := tool.(CapabilityTool)
	return ok && ct.HasCapability(CapabilityAutoBackground)
}

// bgResult 承接后台管道交回的附属物（结构化视图 + 管道错误）。
// 宽限期路径把任务登记在 job 表里，但结果要像同步调用一样返回给调用方：
// jobs.JobOutcome 只带输出文本，视图与「工具/策略错误」这两样只能走这里——
// 尤其错误必须回到 err 通道（resp.Error），否则策略拒绝会被降级成一段普通
// 结果文本，模型和单轮退出码都看不出这次调用其实失败了。
// 同步性由「写结果 → 发 Done → 注册表 close(settled) → AwaitBudget 返回」这条
// happens-before 链保证；锁负责跨 goroutine 的可见性。
type bgResult struct {
	mu     sync.Mutex
	view   string
	runErr error
}

func (b *bgResult) set(view string, runErr error) {
	if b == nil {
		return
	}
	b.mu.Lock()
	b.view, b.runErr = view, runErr
	b.mu.Unlock()
}

func (b *bgResult) get() (string, error) {
	if b == nil {
		return "", nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.view, b.runErr
}

// runWithGrace 以「后台登记 + 前台宽限期」执行一次工具调用。
//
// 返回 handled=false 表示本次没有走自动后台（能力未声明、宽限期关闭、或登记失败），
// 调用方应当回退到普通同步路径。
func (m *Manager) runWithGrace(ctx context.Context, toolName string, argsJSON json.RawMessage, caller string) (result, view string, handled bool, err error) {
	if autoBackgroundGrace <= 0 || m.jobs == nil {
		return "", "", false, nil
	}
	sink := &bgResult{}
	jobID, err := m.startBackgroundJob(ctx, toolName, argsJSON, caller, sink)
	if err != nil {
		// 登记失败（理论上是预检错误）：回退同步执行，别让工具调用因此失败
		m.logger.Warn("auto background unavailable, running synchronously", "tool", toolName, "error", err)
		return "", "", false, nil
	}

	// Claim：宽限期内完成时，结果由这里直接交回模型，落定不再另行通知（否则同一次
	// 调用会既返回结果又弹一条「job finished」，模型被喂两遍）。
	_ = m.jobs.Claim(jobID)

	// 前台等待：按「距上次活动」计时（活跃续命）。有输出就重新记时，命令可以一直
	// 在前台跑完；持续安静超过预算，或到了硬顶，就把任务转入后台、控制权还给模型。
	// 活动来自插件侧的 GetActivity（旧插件不支持时 probe 为 nil → 退化为固定预算）。
	var probe activityProbe
	if tool, ok := m.toolRegistry.Get(toolName); ok {
		probe, _ = tool.(activityProbe)
	}
	callID := ToolCallIDFromContext(ctx)
	lastActivity := time.Now()
	foregroundDeadline := lastActivity.Add(maxAutoBackgroundForeground)

	for {
		// 单次等待不超过「剩余预算」与 250ms 的较小者：否则短预算会被一次长等待
		// 整个吞掉（命令在预算之外又多跑了一会儿才返回，导致本该转后台的没转）。
		wait := autoBackgroundGrace - time.Since(lastActivity)
		if wait <= 0 {
			wait = 0
		}
		if wait > 250*time.Millisecond {
			wait = 250 * time.Millisecond
		}
		snap, out, done := m.jobs.AwaitBudget(jobID, wait)
		if done {
			// 前台跑完 = 一次普通工具调用：结果返回，任务记录一并丢弃，
			// 不让 job_list 里堆满一大堆 finished 的瞬时命令。
			m.jobs.Discard(jobID)
			view, runErr := sink.get()
			// 管道错误（策略拒绝、执行失败）原样回到 err 通道，与同步路径完全一致；
			// 命令非零退出不是 Go 错误（退出码已在结果文本里），照常返回结果。
			return out, view, true, runErr
		}
		if probe != nil {
			if ts, ok := probe.LastActivity(ctx, callID); ok && ts.After(lastActivity) {
				lastActivity = ts
			}
		}
		idle := time.Since(lastActivity)
		if idle < autoBackgroundGrace && time.Now().Before(foregroundDeadline) {
			continue
		}

		// 预算耗尽（或到硬顶）：任务留在后台继续跑。必须 Release——不放开的话它落定
		// 时一声不响，正是「长任务完成了没人知道」的老毛病。
		m.jobs.Release(jobID)
		reason := fmt.Sprintf("no output for %s", idle.Round(time.Second))
		if idle < autoBackgroundGrace {
			reason = "foreground budget exhausted"
		}
		m.logger.Info("auto background: moved to job", "tool", toolName, "job", jobID, "reason", reason)
		result = fmt.Sprintf(
			"Command still running (%s); moved to background (job_id: %s, status: %s). "+
				"It keeps running on its own — read its output with job_output (job_id: %s) and stop it with job_kill if needed.",
			reason, jobID, snap.Status, jobID)
		return result, "", true, nil
	}
}
