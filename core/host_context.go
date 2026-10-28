package core

import "sync"

// 宿主侧的上下文计量实例（每会话一个），对齐 DSH 的 token-meter 插件：
// **实例归宿主**，agent 与各前端都读同一份数字，不再各自另算。
//
// 与 agent 的关系：agent 是独立进程，它自己也持有一个 TokenMeter 用于**决策**
// （裁剪/压缩必须与度量和在同一个进程里），并把投影值随 usage 帧（Usage.ContextTokens）
// 上报；宿主侧实例据此锚定并对外提供读数，供 TUI、cron、将来的 Web 前端读取。
// 两边跑的是**同一份实现**（core.TokenMeter）与同一套归一后的 usage，所以数字一致。
//
// 为什么不把 agent 的实例搬过来：跨进程无法共享内存，agent 必须在本地判断
// 「这一发塞不塞得进窗口」，否则每步都要往返一次宿主。
type HostContextMeters struct {
	mu     sync.Mutex
	window int
	bySess map[string]*hostMeterState
}

type hostMeterState struct {
	meter *TokenMeter
	// reported 是 agent 随帧上报的投影值（Usage.ContextTokens）。
	// 决策发生在 agent 进程，那个值才是"下一次请求会占多少"的权威估计；
	// 宿主只做保管与对外发布，不重新算一遍。
	reported int
	// actual 是本步 pre-step 改写后**真正发出去**的表面（消息 + 工具目录）。
	// 0 = 本步没有插件改写请求，展示沿用 reported。
	//
	// 为什么需要它：压缩后端改写请求后（900k → 摘要 + 尾部 ≈ 40k），宿主与 agent
	// 看到的仍是「未改写的全量历史」，于是界面上一直显示 90%，而压缩插件按自己的
	// 真实表面判断「还没到阈值」——用户看到的现象就是「都 90% 了还不压缩」。
	// 展示口径必须跟真正发出去的东西走，否则数字只是在描述一个不存在的请求。
	actual int
}

// Seed 用会话日志里的历史读数预置展示值（启动时、还没有任何 usage 帧）。
//
// 存在的理由：恢复一个有历史的项目会话时，第一帧 usage 要等到用户发出请求之后才有，
// 此前标题栏的「已用」一直是 0——明明上下文里已经躺着几十万 token 的历史。品牌新会话
// 显示 0 是对的（确实空），有历史时显示 0 就是在说假话。
//
// 只填「兜底」那一层：不写 reported，也不写 actual，第一帧真实用量到达后被自然替换。
func (h *HostContextMeters) Seed(sessionID string, tokens int) {
	if sessionID == "" || tokens <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stateLocked(sessionID).meter.SampleUsage(tokens, 0)
}

// NoteActualSurface 记录本步改写后实际发出的表面大小（消息 + 工具目录，同一个
// core 估算口径）；传 0 表示本步没有改写，展示退回 agent 上报的投影值。
func (h *HostContextMeters) NoteActualSurface(sessionID string, n int) {
	if sessionID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stateLocked(sessionID).actual = n
}

// NewHostContextMeters 创建宿主侧计量集合；window 为上下文窗口（0 = 未知）。
func NewHostContextMeters(window int) *HostContextMeters {
	return &HostContextMeters{window: window, bySess: make(map[string]*hostMeterState)}
}

// SetWindow 更新上下文窗口（模型切换后可能变化）。
func (h *HostContextMeters) SetWindow(window int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.window = window
	for _, st := range h.bySess {
		st.meter.UpdateContextWindow(window)
	}
}

func (h *HostContextMeters) stateLocked(sessionID string) *hostMeterState {
	st, ok := h.bySess[sessionID]
	if !ok {
		st = &hostMeterState{meter: NewTokenMeter(h.window)}
		h.bySess[sessionID] = st
	}
	return st
}

// NoteUsage 记录一帧 usage：以服务端实测的 prompt 侧用量锚定本会话的计量，
// 并保管 agent 上报的投影值（reportedContextTokens <= 0 表示本帧未提供）。
func (h *HostContextMeters) NoteUsage(sessionID string, u *Usage, reportedContextTokens int) {
	if u == nil || sessionID == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	st := h.stateLocked(sessionID)
	// 无增量基线（baseline=0）：Project 退化为实测值，作为"没有上报值时的下界"。
	st.meter.SampleUsage(PromptSideTokens(u), 0)
	if reportedContextTokens > 0 {
		st.reported = reportedContextTokens
	}
}

// ContextTokens 返回展示用的上下文占用：
//   - 本步有插件改写过请求 → 用实际发出的表面（否则压缩后数字永远虚高）
//   - 否则用 agent 上报的投影值（与决策同源）
//   - 都没有 → 退回实测 prompt 侧用量
//   - 全无 → 0（前端显示"未知"而不是 0%）
//
// 兜底必须读「实测用量」本身，不能用 meter.Project(0)：那算的是
// `实测 − 该次请求的估算`（启发式误差），在小窗口长会话里会是一个几万的小常数，
// 而且几乎不随会话增长——界面上的「已用」因此永远钉在一个假数字上。
func (h *HostContextMeters) ContextTokens(sessionID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.bySess[sessionID]
	if !ok {
		return 0
	}
	if st.actual > 0 {
		return st.actual
	}
	if st.reported > 0 {
		return st.reported
	}
	return st.meter.MeasuredPromptTokens()
}

// Reset 清空某会话的计量（切会话 / 压缩改写历史后调用）。
func (h *HostContextMeters) Reset(sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.bySess, sessionID)
}
