package session

import "dsc/proto"

// UsageAnchor 是「上一次成功调用」留下的用量锚点：provider 实测的 prompt 大小，
// 以及产生它时的请求信封（工具目录指纹 + 模型 + 窗口）和当时的启发式估算。
//
// 为什么必须带信封：用量只对**同一个请求信封**有意义。换了模型/窗口、或工具目录
// 变了，上一次的 prompt 大小就不能直接当本次压力的底数（对齐 DSH
// tokenMeter.measure：「provider usage 只在 request header 相同时复用」）。
type UsageAnchor struct {
	PromptTokens  int
	Usage         *proto.Usage
	SentEstimate  int    // 产生该用量时对同一请求（消息+工具）的启发式估算
	ToolsHash     string // 工具目录指纹；空 = 无 header（旧日志）→ 视为不可比
	Model         string // 产生该用量时的模型名；空 = 未知
	ContextWindow int    // 产生该用量时的上下文窗口（0 = 未知）
}

// Comparable 报告该锚点能否用于指定信封（工具目录 + 模型 + 窗口）下的压力计算。
// 缺少指纹（旧日志/无 header）时一律返回 false：宁可退回估算，也不拿不可比的
// 用量当底数。
func (a UsageAnchor) Comparable(toolsHash, model string, contextWindow int) bool {
	if a.ToolsHash == "" || toolsHash == "" || a.ToolsHash != toolsHash {
		return false
	}
	// 模型名两侧都已知且不同 → 换了模型，tokenizer 与计价都可能变，锚点作废。
	if a.Model != "" && model != "" && a.Model != model {
		return false
	}
	// 窗口 0 表示未知（旧记录或未配置），此时不参与比较。
	if a.ContextWindow > 0 && contextWindow > 0 && a.ContextWindow != contextWindow {
		return false
	}
	return true
}

// LastUsageAnchor 从会话事件里倒查最近一次**成功且带 prompt 用量**的 LLM 调用，
// 并向前回找 Turn/Step 相同的 request/header 作为它的信封（这是全仓唯一实现）。
//
// 判定条件：
//   - Error 为空：只认成功结算（失败调用没有可用的 prompt 规模）；
//   - Usage 非 nil 且 PromptTokens > 0：provider 未报用量时不算锚点；
//   - header 缺失（本改动之前写下的日志）→ ToolsHash 为空 → Comparable 恒 false。
//
// 宿主（core）与 agent 插件（经 sdk）都必须调用这里，不得各自再写倒查循环。
func LastUsageAnchor(sess *Session) (UsageAnchor, bool) {
	if sess == nil {
		return UsageAnchor{}, false
	}
	evs := sess.Events()
	for i := len(evs) - 1; i >= 0; i-- {
		d, ok := evs[i].Data.(*LLMAttemptData)
		if !ok || d.Error != "" || d.Usage == nil || d.Usage.PromptTokens <= 0 {
			continue
		}
		a := UsageAnchor{PromptTokens: int(d.Usage.PromptTokens), Usage: d.Usage}
		if h, ok := headerBefore(evs, i, d.Turn, d.Step); ok {
			a.ToolsHash = h.ToolsHash
			a.Model = h.Model
			a.ContextWindow = h.ContextWindow
			a.SentEstimate = h.SentEstimate
		}
		return a, true
	}
	return UsageAnchor{}, false
}

// headerBefore 在 events[:before] 里向前找**最近的**同 Turn/Step 的 request/header。
// 同一步可能因失败重试写多条 header，取最近一条才是这次尝试用的信封。
func headerBefore(evs []*Event, before, turn, step int) (*RequestHeaderData, bool) {
	for j := before - 1; j >= 0; j-- {
		h, ok := evs[j].Data.(*RequestHeaderData)
		if !ok {
			continue
		}
		if h.Turn == turn && h.Step == step {
			return h, true
		}
	}
	return nil, false
}

// LastPromptUsage 是 LastUsageAnchor 的简化形态（只要 prompt 大小与 usage）。
// 需要判断信封可比性（压力计算）时必须用 LastUsageAnchor，不要用这个。
func LastPromptUsage(sess *Session) (int, *proto.Usage, bool) {
	a, ok := LastUsageAnchor(sess)
	if !ok {
		return 0, nil, false
	}
	return a.PromptTokens, a.Usage, true
}
