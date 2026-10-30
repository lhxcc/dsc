package core

import (
	"context"
	"errors"
	"sync"
	"time"
)

// 活跃续命执行域（idle deadline）——宿主通用执行原语，零领域语义：
//
//	WithIdleDeadline(ctx, idle)  安装看门狗（起步 idle 预算）
//	TouchActivity(ctx)           执行方报告一次活动（重置计时）
//	ErrIdleDeadline              看门狗触发的 ctx cause 标记
//
// 职责划分（对齐「policy 插件持有策略，宿主只派发与执行裁决」）：超时决策插件
// （timeout-policy）在 tool/execute 槽裁决「哪个工具、多大空闲预算、超时文案」，
// 宿主的工具流水线桥据此机械安装执行域；执行方（工具实现/子代理循环）只在
// 有活动（输出帧到达、工具结果返回）时调 TouchActivity 上报，不感知预算与策略。
// 未安装执行域时 TouchActivity 为 no-op——工具在无超时策略环境下照常工作。

// ErrIdleDeadline 活跃续命看门狗触发的取消原因（context.Cause 返回值）。
// 文案由裁决的 TimeoutSpec.message 提供（对齐 deny reason 的透传语义），
// 本标记只用于 cause 判定，不直接面向模型。
var ErrIdleDeadline = errors.New("idle deadline exceeded")

// activityHandle 一次执行域的活动句柄：记录空闲预算与看门狗定时器。
// touch 与停表可能并发（执行方活动 vs 调用方收尾），经 mu 串行化。
type activityHandle struct {
	mu    sync.Mutex
	timer *time.Timer
	idle  time.Duration
}

// touch 重置空闲看门狗（活动信号）。看门狗已触发后 touch 仍会重排定时器，
// 但首次 cancel 的 cause 已定格，重排只会产生一次无害的迟到取消（no-op）。
func (h *activityHandle) touch() {
	h.mu.Lock()
	h.timer.Reset(h.idle)
	h.mu.Unlock()
}

// stop 停止看门狗（正常完成路径收尾，杜绝迟到触发）。
func (h *activityHandle) stop() {
	h.mu.Lock()
	h.timer.Stop()
	h.mu.Unlock()
}

type activityKey struct{}

// WithIdleDeadline 返回带「活跃续命」看门狗的 ctx：起步 idle 预算，每次
// TouchActivity 重置计时；持续无活动达预算后 ctx 以 ErrIdleDeadline 为 cause
// 取消（执行方经 ctx 感知并协作退出）。返回的 cancel 停表并解除看门狗，
// 调用方在执行完成后必须调用（与 context.WithTimeout 的 cancel 纪律一致）。
// idle <= 0 时不安装看门狗（ctx 原样返回，cancel 为 no-op）。
func WithIdleDeadline(ctx context.Context, idle time.Duration) (context.Context, context.CancelFunc) {
	if idle <= 0 {
		return ctx, func() {}
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	h := &activityHandle{idle: idle}
	h.timer = time.AfterFunc(idle, func() { cancel(ErrIdleDeadline) })
	return context.WithValue(runCtx, activityKey{}, h), func() {
		h.stop()
		cancel(nil)
	}
}

// TouchActivity 报告一次活动：重置当前执行域的空闲看门狗。执行方每收到一帧
// 输出/进度即调用（shell 的每段输出、子代理的每个 LLM 帧与工具结果）。
// ctx 未安装执行域时为 no-op——活动上报不依赖策略在场。
func TouchActivity(ctx context.Context) {
	if h, _ := ctx.Value(activityKey{}).(*activityHandle); h != nil {
		h.touch()
	}
}

// hasIdleDeadline 报告 ctx 上是否已装看门狗。
//
// 用途：宿主给「没有策略裁决、也没有 auto-background 能力」的工具补一个默认空闲预算时，
// 要先确认没人在管——策略插件已经装了就别再套一层（两层计时器会互相抢先后，
// 模型看到的文案也会随机变成宿主的通用措辞）。
func hasIdleDeadline(ctx context.Context) bool {
	h, _ := ctx.Value(activityKey{}).(*activityHandle)
	return h != nil
}

// renewIdleFromActivity 用跨进程活动信号替看门狗续命（宿主侧轮询 GetActivity）。
//
// 起因：`WithIdleDeadline` 把句柄放在 ctx value 里，取消能跨 gRPC 传播，但 **value 不能**
// ——插件进程里的 `dsc.TouchActivity` 摸到的是另一个空句柄，等于 no-op。于是任何在插件
// 进程里跑的「长但有输出」的工具，看门狗都会在第 N 分钟准时开枪，文案还写着 no output。
//
// 这里由宿主按 GetActivity（正在跨进程的那条线）轮询执行方的最近活动时间，有推进就
// TouchActivity 续命。轮询间隔取预算的 1/4 并夹在 [100ms, 5s]：10 分钟预算 → 5 秒一次，
// 对 10 分钟的时间尺度足够灵敏，也不会给插件添明显的 RPC 负担。
//
// 返回的 stop 必须调用（defer），否则 goroutine 会活到 ctx 结束——快速工具上无所谓，
// 长任务里会白占一个协程。
func renewIdleFromActivity(ctx context.Context, probe activityProbe, callID string, budget time.Duration) (stop func()) {
	if probe == nil || callID == "" {
		return func() {}
	}
	interval := budget / 4
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	if interval > 5*time.Second {
		interval = 5 * time.Second
	}
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var last time.Time
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if ts, ok := probe.LastActivity(ctx, callID); ok && ts.After(last) {
				last = ts
				TouchActivity(ctx)
			}
		}
	}()
	return func() { close(done) }
}

// IdleDeadlineExceeded 报告 ctx 是否因空闲看门狗触发而取消。
// 供执行域安装方（工具流水线桥）在执行返回后判定：cause 为 ErrIdleDeadline
// 时以裁决文案替换执行错误。
func IdleDeadlineExceeded(ctx context.Context) bool {
	return context.Cause(ctx) == ErrIdleDeadline
}
