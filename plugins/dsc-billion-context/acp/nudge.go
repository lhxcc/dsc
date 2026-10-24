package acp

import (
	"fmt"
	"sort"
	"strings"
)

// NudgeDecision nudge 决策结果（对齐 acp-kernel NudgeDecision）。
type NudgeDecision struct {
	ShouldInject       bool                `json:"shouldInject"`
	Reason             string              `json:"reason"`
	CompressibleRanges []CompressibleRange `json:"compressibleRanges"`
	ContextUsage       float64             `json:"contextUsage"` // 0..1
	Breakdown          map[string]int      `json:"breakdown"`
	// Tier 触发的蒸馏层级（对齐 acp-kernel NudgeDecision.tier）：
	//   nil  — 不触发蒸馏（仅标准 T1 nudge 或无 nudge）
	//   2    — T1 块数达 Tier2Trigger，提示蒸馏为 T2
	//   3    — T2 块数达 Tier3Trigger，提示凝结为 T3
	Tier *CompressionTier `json:"tier,omitempty"`
	// RecommendedRanges 经 MergeRangesToThreshold 合并后的推荐范围
	//（每批各自越过 MinCompressRangeChars——推荐永不低于 apply 侧最小门，#309）
	RecommendedRanges []CompressibleRange `json:"recommendedRanges,omitempty"`
	// TierTargetBlocks 待蒸馏的目标块列表（对齐 acp-kernel tierTargetBlocks）。
	// 模型在 compress 调用中用这些块的 bN 作为 startId/endId。
	TierTargetBlocks []CompressionBlock `json:"tierTargetBlocks,omitempty"`
}

// CompressibleRange 一个可压缩范围（供模型在 compress 调用中引用）。
type CompressibleRange struct {
	StartRef string `json:"startRef"`
	EndRef   string `json:"endRef"`
	Count    int    `json:"count"`
	Tokens   int    `json:"tokens"`
	Chars    int    `json:"chars"` // 真实字符量（minCompressRange 记账单位，对齐 #309）
}

// blocksSummaryTokens 汇总一组块的可回收摘要质量（tokens）。
//
// 用「摘要本身的大小」而不是 CompressedTokens（原文被压掉的量）：蒸馏一层是把
// N 条摘要换成 1 条，能省下的就是这些摘要的体积——原文那部分早已在上一轮省掉了，
// 拿它当判据会立刻超阈值、把蒸馏变成无条件动作。
func blocksSummaryTokens(blocks []CompressionBlock) int {
	total := 0
	for _, b := range blocks {
		total += estimateTokensForText(b.Summary)
	}
	return total
}

// NudgeGrowthTokens 返回本轮「算得上增长」的 token 量（对齐 acp-kernel
// resolveAdaptiveGrowth）：与窗口成比例并夹在 [GrowthFloor, GrowthCap] 内。
//
// 为什么不能写死：128K 窗口的 1000 token 增长值得提示，1M 窗口的 1000 token
// 只是噪声；反过来，把 1M 的 50K 要求套到 128K 上则几乎永不触发。
func NudgeGrowthTokens(config Config) int {
	limit := config.ModelContextLimit
	floor := config.GrowthFloor
	cap := config.GrowthCap
	if floor <= 0 {
		floor = 1000
	}
	if cap <= 0 {
		cap = floor
	}
	if limit <= 0 {
		return floor
	}
	v := int(float64(limit) * config.GrowthRatio)
	if v < floor {
		v = floor
	}
	if v > cap {
		v = cap
	}
	return v
}

// GrowthFloorTokens 返回「自上次 nudge 之后至少涨多少才再提示」的门槛
// （对齐 acp-kernel growthFloor = max(minGrowthFloor, minGrowthRatio × growth)）。
func GrowthFloorTokens(config Config) int {
	floor := int(config.MinGrowthRatio * float64(NudgeGrowthTokens(config)))
	if floor < config.MinGrowthFloor {
		floor = config.MinGrowthFloor
	}
	if floor < 1 {
		floor = 1
	}
	return floor
}

// TierMassThreshold 返回「这一层值得蒸馏」的可回收摘要质量门槛
// （对齐 acp-kernel tier2GrowthMultiplier × nudgeGrowthTokens）。
func TierMassThreshold(config Config) int {
	m := config.TierGrowthMultiplier
	if m <= 0 {
		m = 1.5
	}
	th := int(m * float64(NudgeGrowthTokens(config)))
	if th < 1 {
		th = 1
	}
	return th
}

// DecideNudge 决策是否注入 nudge 提示（对齐 acp-kernel decideNudge）。
//
// 触发条件：
//  1. 上下文使用率 ≥ NudgeThresholdPct（如 45%）
//  2. 自上次 nudge 后有增长（防反馈循环）
//  3. 存在可压缩范围（被覆盖消息外、保留区外）
//
// 增长门控（growth-gating）：
//   - baselineTokens 在压缩成功后被重置为 0（见 ApplyCompression）
//   - 下次 nudge 要求 currentTokens > baselineTokens + growthFloor
//   - 这防止"压缩后立即又触发 nudge"的反馈循环
func DecideNudge(messages []CoreMessage, state *CompressionState, config Config, tokenCount int) NudgeDecision {
	decision := NudgeDecision{Breakdown: map[string]int{}}

	// 计算上下文使用率
	usage := 0.0
	if config.ModelContextLimit > 0 {
		usage = float64(tokenCount) / float64(config.ModelContextLimit)
	}
	decision.ContextUsage = usage
	decision.Breakdown["usage"] = tokenCount
	decision.Breakdown["modelContextLimit"] = config.ModelContextLimit

	// 阈值门控
	thresholdTokens := int(float64(config.ModelContextLimit) * config.NudgeThresholdPct)
	decision.Breakdown["thresholdTokens"] = thresholdTokens
	if tokenCount < thresholdTokens {
		decision.Reason = fmt.Sprintf("below threshold (%d < %d)", tokenCount, thresholdTokens)
		return decision
	}

	// 增长门控：自上次 nudge/baseline 后必须有增长（门槛随窗口缩放）
	growthFloor := GrowthFloorTokens(config)
	if state.Nudge.BaselineTokens > 0 {
		if tokenCount-state.Nudge.BaselineTokens < growthFloor {
			decision.Reason = fmt.Sprintf("insufficient growth since baseline (%d - %d < %d)",
				tokenCount, state.Nudge.BaselineTokens, growthFloor)
			return decision
		}
	}
	decision.Breakdown["growthFloor"] = growthFloor
	decision.Breakdown["baselineTokens"] = state.Nudge.BaselineTokens

	// 计算可压缩范围，并按最小字符量门合并（#309：推荐永不低于 apply 侧最小门）
	ranges := ComputeCompressibleRanges(messages, state, config)
	decision.CompressibleRanges = ranges
	decision.Breakdown["compressibleRanges"] = len(ranges)
	recommended := MergeRangesToThreshold(ranges, config.MinCompressRangeChars)
	decision.RecommendedRanges = recommended
	decision.Breakdown["recommendedRanges"] = len(recommended)

	// 检查 T2/T3 蒸馏触发条件（对齐 acp-kernel tier distillation）
	// T2：active T1 块数达 Tier2Trigger
	// T3：active T2 块数达 Tier3Trigger
	activeBlocks := state.ActiveBlocks()
	t1Count, t2Count := 0, 0
	var t1Blocks, t2Blocks []CompressionBlock
	for _, b := range activeBlocks {
		if b.Tier == Tier1 {
			t1Count++
			t1Blocks = append(t1Blocks, b)
		} else if b.Tier == Tier2 {
			t2Count++
			t2Blocks = append(t2Blocks, b)
		}
	}
	decision.Breakdown["t1Count"] = t1Count
	decision.Breakdown["t2Count"] = t2Count

	// 两条触发路径取「或」（对齐 acp-kernel）：
	//   a) 计数：显式配置了较小的 Tier2/3Trigger（默认 1000/2000 = 关）；
	//   b) 质量：目标块的可回收摘要 tokens 达到 TierMassThreshold——
	//      这才是需求信号。只数块数会在上下文还宽裕时把细节蒸干。
	massThreshold := TierMassThreshold(config)
	t1Mass := blocksSummaryTokens(t1Blocks)
	t2Mass := blocksSummaryTokens(t2Blocks)
	decision.Breakdown["t1Mass"] = t1Mass
	decision.Breakdown["t2Mass"] = t2Mass
	decision.Breakdown["tierMassThreshold"] = massThreshold
	t3CountReady := config.Tiers.Tier3Trigger > 0 && t2Count >= config.Tiers.Tier3Trigger
	t2CountReady := config.Tiers.Tier2Trigger > 0 && t1Count >= config.Tiers.Tier2Trigger
	// T3 需要「T2 块本身的质量」达标；T2 需要「T1 块的质量」达标。
	// 高层级还要压过低层级（T3 的待压质量必须大于 T2），否则没必要往上蒸。
	t3MassReady := t2Mass >= massThreshold && t2Mass > t1Mass
	t2MassReady := t1Mass >= massThreshold

	// T3 蒸馏优先（更高层级，更经济）
	if t3CountReady || t3MassReady {
		tier := Tier3
		decision.Tier = &tier
		decision.TierTargetBlocks = t2Blocks
		decision.ShouldInject = true
		// 不暴露使用率百分比（对齐 ACP 设计：模型不应知道总上下文大小，
		// 避免模型"认为还很健康就懒得压缩"）
		decision.Reason = fmt.Sprintf("T3 condense: %d tier-2 blocks (%d tokens of summaries, threshold %d)",
			t2Count, t2Mass, massThreshold)
		return decision
	}

	// T2 蒸馏：T1 块的质量达标，或显式配置的计数达标
	if t2CountReady || t2MassReady {
		tier := Tier2
		decision.Tier = &tier
		decision.TierTargetBlocks = t1Blocks
		decision.ShouldInject = true
		decision.Reason = fmt.Sprintf("T2 distill: %d tier-1 blocks (%d tokens of summaries, threshold %d)",
			t1Count, t1Mass, massThreshold)
		return decision
	}

	// 全部条件满足：注入 nudge——标准 T1 路径要求存在越过最小门的推荐范围
	//（#309：低于门的范围推荐出去只会被 apply 侧拒绝）；层级蒸馏路径不受
	// 此门约束（蒸馏操作对象是块，与范围大小无关）
	if len(recommended) == 0 {
		decision.Reason = "no compressible ranges above the minimum size gate"
		return decision
	}
	// 全部条件满足：注入 nudge
	// 不暴露使用率百分比——只告诉模型有可压缩内容（对齐 ACP 设计）
	decision.ShouldInject = true
	decision.Reason = fmt.Sprintf("%d compressible ranges detected", len(recommended))
	return decision
}

// ComputeCompressibleRanges 计算可压缩范围（对齐 acp-kernel buildCompressibleRanges）。
// 跳过：已被覆盖的消息、保留区内的尾部消息、受保护工具的 tool-call/result。
func ComputeCompressibleRanges(messages []CoreMessage, state *CompressionState, config Config) []CompressibleRange {
	covered := state.CoveredMessageIDs()
	protected := map[string]bool{}
	for _, t := range config.ProtectedTools {
		protected[t] = true
	}

	preserveFromIdx := len(messages) - config.PreserveRecent
	if preserveFromIdx < 0 {
		preserveFromIdx = 0
	}

	// 收集可压缩消息的索引（连续段合并为 range）
	var ranges []CompressibleRange
	var curStart, curEnd, curTokens, curChars int
	inRange := false

	for i, msg := range messages {
		// 跳过保留区
		if i >= preserveFromIdx {
			break
		}
		// 跳过已覆盖
		if msg.ID != "" && covered[msg.ID] {
			if inRange {
				ranges = append(ranges, finalizeRange(messages, curStart, curEnd, curTokens, curChars, state))
				inRange = false
			}
			continue
		}
		// 跳过受保护工具
		if msg.ToolName != "" && protected[msg.ToolName] {
			if inRange {
				ranges = append(ranges, finalizeRange(messages, curStart, curEnd, curTokens, curChars, state))
				inRange = false
			}
			continue
		}
		// 跳过 system 消息（v3 system prompt surface node 0 + billion-context
		// 已渲染的 summary 消息均为 system role）——对齐 DSH "node 0 is never
		// inside a compaction range" 与 billion-context 自身的 summary 不可再压缩语义。
		// 此前仅跳过 isRenderedSummary（ID 形如 acp_summary_bN），漏过了 v3 新增的
		// system prompt（surface node 0 派生、ID 非 summary 前缀），导致 nudge 可能
		// 把 system prompt 纳入可压缩范围、模型调用 compress 时丢失基础指令。
		if msg.Role == RoleSystem {
			if inRange {
				ranges = append(ranges, finalizeRange(messages, curStart, curEnd, curTokens, curChars, state))
				inRange = false
			}
			continue
		}
		// 进入或扩展 range
		if !inRange {
			curStart = i
			curTokens = 0
			inRange = true
		}
		curEnd = i
		curTokens += estimateTokensForText(msg.Text)
		curChars += runeLen(msg.Text)
	}
	if inRange {
		ranges = append(ranges, finalizeRange(messages, curStart, curEnd, curTokens, curChars, state))
	}

	// 过滤零 token 段；过小段的合并交给 MergeRangesToThreshold（#309
	// 字符记账 + 尾部并入前批），此处不再按 token 硬切
	filtered := ranges[:0]
	for _, r := range ranges {
		if r.Tokens > 0 {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// finalizeRange 把 [startIdx, endIdx] 范围转为 CompressibleRange。
func finalizeRange(messages []CoreMessage, startIdx, endIdx, tokens, chars int, state *CompressionState) CompressibleRange {
	startRef := ""
	endRef := ""
	count := 0
	for i := startIdx; i <= endIdx; i++ {
		if messages[i].ID == "" {
			continue
		}
		ref := RefForRaw(messages[i].ID, state)
		if ref == "" {
			continue
		}
		if startRef == "" {
			startRef = ref
		}
		endRef = ref
		count++
	}
	return CompressibleRange{
		StartRef: startRef,
		EndRef:   endRef,
		Count:    count,
		Tokens:   tokens,
		Chars:    chars,
	}
}

// FormatNudgeText 把 nudge 决策渲染为模型可见的提示文本。
// 对齐 acp-kernel renderNudgeText。
func FormatNudgeText(d NudgeDecision) string {
	if !d.ShouldInject {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("[ACP Nudge: %s]\n", d.Reason))

	// 蒸馏提示（T2/T3）：列出待蒸馏的目标块
	if d.Tier != nil && len(d.TierTargetBlocks) > 0 {
		tier := *d.Tier
		if tier == Tier2 {
			sb.WriteString(fmt.Sprintf("Your tier-1 compression summaries have accumulated (%d blocks). ",
				len(d.TierTargetBlocks)))
			sb.WriteString("Distill them into a single denser tier-2 summary. ")
			sb.WriteString("Use block IDs as boundaries (startId and endId as bN).\n")
		} else if tier == Tier3 {
			sb.WriteString(fmt.Sprintf("Your tier-2 compression summaries have accumulated (%d blocks). ",
				len(d.TierTargetBlocks)))
			sb.WriteString("Condense them further into a tier-3 ultra-condensed summary. ")
			sb.WriteString("Use block IDs as boundaries (startId and endId as bN).\n")
		}
		// 列出目标块
		sb.WriteString("Target blocks to distill:\n")
		for i, b := range d.TierTargetBlocks {
			topic := b.Topic
			if topic == "" {
				topic = "untitled"
			}
			sb.WriteString(fmt.Sprintf("  %d. %s (tier-%d, %s, ~%d tokens)\n",
				i+1, b.BlockID, b.Tier, topic, b.CompressedTokens))
		}
		sb.WriteString("\nExample: compress({content:[{startId:\"b0\", endId:\"b4\", summary:\"...\", topic:\"...\"}]})\n")
		return sb.String()
	}

	// 标准 T1 压缩提示（展示合并后的推荐范围——每批各自越过最小门）
	if len(d.RecommendedRanges) > 0 {
		sb.WriteString("Compressible ranges (call `compress` to reclaim context):\n")
		// 按 tokens 降序，让模型优先压缩最大的范围
		ranges := make([]CompressibleRange, len(d.RecommendedRanges))
		copy(ranges, d.RecommendedRanges)
		sort.Slice(ranges, func(i, j int) bool { return ranges[i].Tokens > ranges[j].Tokens })
		for i, r := range ranges {
			sb.WriteString(fmt.Sprintf("  %d. %s..%s (%d messages, ~%d tokens)\n",
				i+1, r.StartRef, r.EndRef, r.Count, r.Tokens))
		}
		sb.WriteString("\nUse the `compress` tool with content=[{startId, endId, summary, topic?}] to compress a range.")
	}
	return sb.String()
}
