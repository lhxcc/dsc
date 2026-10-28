package core

import (
	"regexp"
	"strconv"
	"strings"
)

// 真实超窗（provider 4xx）之后对预算的收紧。
//
// 为什么不再走「插件紧急压缩」：那条路只能产出没有内容的占位摘要（一类后端写
// 「N 条消息已压缩」的固定模板，另一类 emergency=true 强制截断摘要、跳过 LLM），
// 一次就把几百条真实消息**永久**埋掉（会落盘成块）。滑窗只影响这一次请求——会话
// 日志一字不动，下一轮仍可正常压缩。所以 4xx 的处置权归宿主：收紧预算 → 重走 pre-step
// → 滑窗 → 重试。这是**确定性**的，而「压一个范围再重试」不保证收敛（实测三次重试
// 仍 1.07M）。
//
// 收紧的依据来自 provider 自己的话：错误文本里通常同时给出「实际用了多少」与「上限
// 多少」（`prompt is too long: 1075596 tokens > 1048576 maximum`）。拿实际值除我们
// 发出的估算，就得到这次真实偏差——比任何启发式都准。解析失败时退化为「按超出比例
// 收紧」，仍然收敛，只是要多一轮。

// overflowAdjustment 是一次超窗后对该会话的预算修正。
type overflowAdjustment struct {
	// inflation provider 实测 / 我们的估算（≥1）。0 = 未能解析，按「至少超了一点」处理。
	inflation float64
	// maxTokens provider 自己报的上下文上限（0 = 未知）。比配置值小的时候以它为准——
	// 配置可能写的是模型的理论窗口，而端点实际允许的更小。
	maxTokens int
}

// overflowParse 从 provider 的错误文本里取出（实际用量, 上限）。
//
// 各家措辞不同，这里只认三种能确定语义的形态；解析不出就返回 0，由调用方退化处理：
//   - "prompt is too long: 1075596 tokens > 1048576 maximum"
//   - "maximum context length is 8192 tokens ... resulted in 10000 tokens"
//   - "context length is 128000 tokens"（只有上限）
func overflowParse(msg string) (actual, max int) {
	lower := strings.ToLower(msg)
	if m := overflowPairRe.FindStringSubmatch(lower); m != nil {
		return atoiThousands(m[1]), atoiThousands(m[2])
	}
	if m := overflowMaxRe.FindStringSubmatch(lower); m != nil {
		max = atoiThousands(m[1])
	}
	if m := overflowActualRe.FindStringSubmatch(lower); m != nil {
		actual = atoiThousands(m[1])
	}
	return actual, max
}

var (
	// 「A tokens > B」：两数一出现就同时拿到实际值与上限。
	overflowPairRe = regexp.MustCompile(`(\d[\d,]*)\s*tokens?\s*(?:>|greater than|exceeds)\s*(\d[\d,]*)`)
	// 「maximum context length is B tokens」/「context window of B tokens」
	overflowMaxRe = regexp.MustCompile(`(?:maximum (?:context )?(?:length|window)|context (?:length|window))\D{0,24}?(\d[\d,]*)\s*tokens?`)
	// 「your messages resulted in A tokens」
	overflowActualRe = regexp.MustCompile(`resulted in\D{0,8}(\d[\d,]*)\s*tokens?`)
)

func atoiThousands(s string) int {
	n, err := strconv.Atoi(strings.ReplaceAll(s, ",", ""))
	if err != nil {
		return 0
	}
	return n
}

// noteOverflow 记录一次真实超窗，收紧该会话后续（含本次重试）的预算。
//
// sent 是我们为那次请求估算的表面（消息 + 工具目录）。actual/max 来自 provider 的错误
// 文本，解析不出时为 0：此时至少要意识到「我们以为装得下的那份其实装不下」，按
// sent/window 作为偏差下界收紧。
func (m *Manager) noteOverflow(sessionID string, actual, max, sent int) {
	if sessionID == "" {
		return
	}
	adj := overflowAdjustment{maxTokens: max}
	switch {
	case actual > 0 && sent > 0:
		adj.inflation = float64(actual) / float64(sent)
	case sent > 0 && m.contextWindow > 0 && sent > m.contextWindow:
		// 没解析出实际值，但我们自己算出的表面就已经超过窗口——那次根本就不该发。
		// 取「超出比例」作为偏差下界，保证下一轮裁得更狠。
		adj.inflation = float64(sent) / float64(m.contextWindow)
	}
	if adj.inflation < 1 {
		adj.inflation = 1 // 只放大：provider 报得比我们少时不缩小（偏大只会多裁一点）
	}
	if adj.inflation > overflowInflationMax {
		adj.inflation = overflowInflationMax
	}

	m.overflowMu.Lock()
	if m.overflowAdj == nil {
		m.overflowAdj = map[string]overflowAdjustment{}
	}
	// 同一会话可能连续超窗：取更严的那个（偏差取大、上限取小），不要被后一次的
	// 宽松值覆盖掉前一次学到的教训。
	prev, ok := m.overflowAdj[sessionID]
	if ok {
		if prev.inflation > adj.inflation {
			adj.inflation = prev.inflation
		}
		if prev.maxTokens > 0 && (adj.maxTokens == 0 || prev.maxTokens < adj.maxTokens) {
			adj.maxTokens = prev.maxTokens
		}
	}
	m.overflowAdj[sessionID] = adj
	m.overflowMu.Unlock()

	m.logger.Warn("context overflow reported by provider; tightening the request budget and sliding the window",
		"session", sessionID, "provider_actual", actual, "provider_max", max,
		"our_estimate", sent, "inflation", adj.inflation)
}

// clearOverflow 清掉该会话的收紧记录（成功的请求已经证明当前预算可用）。
//
// 必须清：一次性的异常（比如某轮粘贴了巨大内容）不该让这个会话永远按放大系数裁。
// 真正的系统性偏差由 estimateInflation 从成功请求的锚点里继续持有。
func (m *Manager) clearOverflow(sessionID string) {
	if sessionID == "" {
		return
	}
	m.overflowMu.Lock()
	delete(m.overflowAdj, sessionID)
	m.overflowMu.Unlock()
}

// overflowFor 取该会话当前的收紧值（没有则 ok=false）。
func (m *Manager) overflowFor(sessionID string) (overflowAdjustment, bool) {
	m.overflowMu.Lock()
	defer m.overflowMu.Unlock()
	adj, ok := m.overflowAdj[sessionID]
	return adj, ok
}

// effectiveWindow 返回本次请求可用的窗口：配置值，但不超过 provider 亲口报过的上限。
//
// 配置写的是模型理论窗口，端点实际允许的可能更小（代理层截断、部署方限流）。被 4xx
// 教过一次之后就按小的来，不必每次都撞一次墙。
func (m *Manager) effectiveWindow(sessionID string) int {
	win := m.contextWindow
	if adj, ok := m.overflowFor(sessionID); ok && adj.maxTokens > 0 && (win <= 0 || adj.maxTokens < win) {
		return adj.maxTokens
	}
	return win
}
