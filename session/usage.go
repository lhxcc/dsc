package session

import "dsc/proto"

// UsageAnchor 是「上一次成功调用」留下的用量锚点：provider 实测的 prompt 大小、
// 产生它时的请求信封指纹（工具目录 + 窗口），以及当时对同一请求的启发式估算。
//
// 为什么要带指纹：用量只对**同一个请求信封**有意义。换了模型/窗口、或工具目录变了，
// 上一次的 prompt 大小就不能直接当本次压力的底数（对齐 DSH tokenMeter.measure：
// 「provider usage 只在 request header 相同时复用」）。
type UsageAnchor struct {
	PromptTokens  int
	Usage         *proto.Usage
	SentEstimate  int    // 产生该用量时对同一请求（消息+工具）的启发式估算
	ToolsHash     string // 工具目录指纹；空 = 旧记录/未知 → 视为不可比
	ContextWindow int
}

// Comparable 报告该锚点能否用于指定信封（工具目录 + 上下文窗口）下的压力计算。
// 指纹为空（旧记录）时一律返回 false：宁可退回估算，也不拿不可比的用量当底数。
func (a UsageAnchor) Comparable(toolsHash string, contextWindow int) bool {
	if a.ToolsHash == "" || toolsHash == "" || a.ToolsHash != toolsHash {
		return false
	}
	// 窗口 0 表示未知（旧记录或未配置），此时不参与比较。
	if a.ContextWindow > 0 && contextWindow > 0 && a.ContextWindow != contextWindow {
		return false
	}
	return true
}

// LastUsageAnchor 从会话事件里倒查最近一次**成功且带 prompt 用量**的 LLM 调用，
// 连同它的请求信封指纹一起返回（这是全仓唯一实现）。
//
// 判定条件与 llm/attempt 事件语义一致：
//   - Error 为空：只认成功结算（失败调用没有可用的 prompt 规模）；
//   - Usage 非 nil 且 PromptTokens > 0：provider 未报用量时不算锚点。
//
// 宿主（core）与 agent 插件（经 sdk）都必须调用这里，不得各自再写一份倒查循环。
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
		return UsageAnchor{
			PromptTokens:  int(d.Usage.PromptTokens),
			Usage:         d.Usage,
			SentEstimate:  d.SentEstimate,
			ToolsHash:     d.ToolsHash,
			ContextWindow: d.ContextWindow,
		}, true
	}
	return UsageAnchor{}, false
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
