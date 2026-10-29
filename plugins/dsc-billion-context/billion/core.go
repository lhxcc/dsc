package billion

import (
	"fmt"
	"strings"
)

// Decompress 恢复一个块的内容（对齐 acp-kernel decompress）。
// 默认恢复一层（T2→T1 摘要）；full=true 恢复到原始消息。
//
// MVP 实现：仅返回块的 summary 文本（T1 块无嵌套，无法恢复原始消息）。
// T2/T3 嵌套恢复留待后续实现。
func Decompress(blockID string, state *CompressionState, full bool) (string, error) {
	block := state.BlockByID(blockID)
	if block == nil {
		return "", fmt.Errorf("block %s not found", blockID)
	}
	if full {
		// MVP 限制：T1 块的原始消息已被压缩，无法恢复。
		// 后续可经 session 事件日志回放重建（event-sourced）。
		return block.Summary, fmt.Errorf("full decompress not implemented for T1 blocks (summary only)")
	}
	return block.Summary, nil
}

// SearchResults 搜索结果。
type SearchResult struct {
	BlockID  string `json:"blockId"`
	Topic    string `json:"topic"`
	Summary  string `json:"summary"`
	Score    int    `json:"score"` // 命中次数
	StartRef string `json:"startRef"`
	EndRef   string `json:"endRef"`
}

// Search 在所有 active 块的 summary 中搜索关键词（升级：hybrid BM25+fuzzy
// 打分，对齐 acp-kernel 默认检索算法；接口与 active-only 语义保持不变，
// 全量文档集版本见 SearchBlocks + BlockDocs/MessageDocs）。
func Search(query string, state *CompressionState, limit int) []SearchResult {
	docs := BlockDocs(state)
	scored := SearchBlocks(docs, query, SearchOptions{Limit: limit})
	out := make([]SearchResult, 0, len(scored))
	for _, s := range scored {
		out = append(out, SearchResult{
			BlockID:  s.BlockID,
			Topic:    s.Title,
			Summary:  s.Preview,
			Score:    int(s.Score * 100),
			StartRef: s.Ref,
			EndRef:   s.Ref,
		})
	}
	// 回填整块 startRef/endRef（bN 自身即范围）
	for k := range out {
		if b := state.BlockByID(out[k].BlockID); b != nil {
			out[k].Summary = b.Summary
			out[k].Topic = b.Topic
		}
	}
	return out
}

// StatusReport 上下文状态报告（对齐 acp-kernel StatusReport）。
type StatusReport struct {
	ContextUsage       float64             `json:"contextUsage"` // 0..1
	TokenCount         int                 `json:"tokenCount"`
	ModelContextLimit  int                 `json:"modelContextLimit"`
	ActiveBlocks       int                 `json:"activeBlocks"`
	TotalBlocks        int                 `json:"totalBlocks"`
	TokensCompressed   int                 `json:"tokensCompressed"`
	CompressibleRanges []CompressibleRange `json:"compressibleRanges"`
	Breakdown          map[string]int      `json:"breakdown"`
}

// BuildStatus 构建状态报告（对齐 acp-kernel buildStatusReport）。
func BuildStatus(messages []CoreMessage, state *CompressionState, config Config, tokenCount int) StatusReport {
	usage := 0.0
	if config.ModelContextLimit > 0 {
		usage = float64(tokenCount) / float64(config.ModelContextLimit)
	}
	activeCount := 0
	for _, b := range state.Blocks {
		if b.Active {
			activeCount++
		}
	}
	t1, t2, t3 := 0, 0, 0
	for _, b := range state.Blocks {
		if !b.Active {
			continue
		}
		switch b.Tier {
		case Tier1:
			t1++
		case Tier2:
			t2++
		case Tier3:
			t3++
		}
	}
	st := state.Stats
	return StatusReport{
		ContextUsage:       usage,
		TokenCount:         tokenCount,
		ModelContextLimit:  config.ModelContextLimit,
		ActiveBlocks:       activeCount,
		TotalBlocks:        len(state.Blocks),
		TokensCompressed:   st.TokensCompressed,
		CompressibleRanges: ComputeCompressibleRanges(messages, state, config),
		Breakdown: map[string]int{
			"tokensCompressed":  st.TokensCompressed,
			"compressionCount":  st.CompressionCount,
			"activeBlocks":      activeCount,
			"totalBlocks":       len(state.Blocks),
			"tier1":             t1,
			"tier2":             t2,
			"tier3":             t3,
			"absorbedTokens":    st.AbsorbedTokens,
			"crushCount":        st.CrushCount,
			"crushTokensSaved":  st.CrushTokensSaved,
			"ccrStored":         st.CCRStored,
			"ccrStoredTokens":   st.CCRStoredTokens,
			"ccrRetrieved":      st.CCRRetrieved,
			"ccrExported":       st.CCRExported,
			"retrievalMisses":   st.RetrievalMisses,
			"tokensInContext":   tokenCount,
			"modelContextLimit": config.ModelContextLimit,
		},
	}
}

// FormatStatus 把状态报告格式化为模型可读文本。
func FormatStatus(report StatusReport) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("billion context status:\n"))
	sb.WriteString(fmt.Sprintf("  Usage: %d / %d tokens (%.0f%%)\n",
		report.TokenCount, report.ModelContextLimit, report.ContextUsage*100))
	sb.WriteString(fmt.Sprintf("  Blocks: %d active / %d total\n", report.ActiveBlocks, report.TotalBlocks))
	sb.WriteString(fmt.Sprintf("  Compressed: %d tokens reclaimed across %d compressions\n",
		report.TokensCompressed, report.Breakdown["compressionCount"]))
	// 节省明细：压缩/确定性压缩/无损卸载三条通路各自省了多少
	bd := report.Breakdown
	if bd["crushCount"] > 0 || bd["ccrStored"] > 0 || bd["absorbedTokens"] > 0 {
		sb.WriteString(fmt.Sprintf("  Tiers: T1=%d T2=%d T3=%d active\n", bd["tier1"], bd["tier2"], bd["tier3"]))
		if bd["crushTokensSaved"] > 0 {
			sb.WriteString(fmt.Sprintf("  Crush: %d deterministic compactions, %d tokens saved\n",
				bd["crushCount"], bd["crushTokensSaved"]))
		}
		if bd["ccrStored"] > 0 {
			sb.WriteString(fmt.Sprintf("  Stored: %d payloads (%d tokens) retrievable via billion_retrieve\n",
				bd["ccrStored"], bd["ccrStoredTokens"]))
			sb.WriteString(fmt.Sprintf("  Retrievals: %d (%d exported to files)\n",
				bd["ccrRetrieved"], bd["ccrExported"]))
		}
		if bd["absorbedTokens"] > 0 {
			sb.WriteString(fmt.Sprintf("  Absorbed: %d tokens distilled\n", bd["absorbedTokens"]))
		}
	}
	if len(report.CompressibleRanges) > 0 {
		sb.WriteString(fmt.Sprintf("\nCompressible ranges (%d):\n", len(report.CompressibleRanges)))
		for i, r := range report.CompressibleRanges {
			sb.WriteString(fmt.Sprintf("  %d. %s..%s (%d messages, ~%d tokens)\n",
				i+1, r.StartRef, r.EndRef, r.Count, r.Tokens))
		}
	} else {
		sb.WriteString("\nNo compressible ranges (context is lean or all old content already compressed).\n")
	}
	return sb.String()
}

// ProcessTurnResult processTurn 的返回值（对齐 acp-kernel ProcessTurnResult）。
type ProcessTurnResult struct {
	Messages []CoreMessage     `json:"messages"`
	State    *CompressionState `json:"state"`
	Nudge    *NudgeDecision    `json:"nudge,omitempty"`
	// CCROffloads 本步需要宿主落盘的内容（kernel 不做 IO，只产出副作用）。
	CCROffloads []CCROffload `json:"-"`
}

// ProcessTurn 每轮跑 pipeline（对齐 acp-kernel processTurn）。
//
// 节点顺序（对齐 DESIGN §4 的 canonical order，已接的部分）：
//
//	assign-refs → advance-survival → sync-blocks → prune(render)
//	→ hide-compress-calls → absorb(hide+prompt) → decide-nudge → emergency-truncate
//
// 仍未实现：filter（可插拔过滤框架）、output-steering（冗长度分级）、
// packs（提示面/工具面定制）。
//
// 关于 merge-blocks（old-gen 块批合并）：acp-kernel 的 README/DESIGN 仍写着这个
// 节点与 mergeMarkedBlocks，但**当前源码的 pipeline 里已经没有它**（节点表见
// src/compress.ts 的 buildNodes）。块膨胀在上游是靠「蒸馏」（模型用 bN 引用把
// T1 折成 T2/T3）解决的，不是靠自动批合并——这里也不实现它，而是把蒸馏的触发
// 条件做对（按摘要质量而非块数，见 nudge.go）。
//
// 已对齐（勿重复实现）：assign-refs / 存活升级 / sync-blocks / prune /
// ccr-store（无损回取，复用 dsc-txt:// 附件库）/ crush（确定性压缩）/
// hide-compress-calls / absorb(hide+prompt) /
// decide-nudge（计数+质量双路径）/ emergency-truncate / 孤儿配对清理。
//
// 前缀缓存稳定性：renderTags=false——不把 <billion tokens="N">mNNNNN</billion> 标签注入
// 消息文本。原因：tokens 属性每轮估算会变化（消息文本可能改变），导致前缀缓存
// 失效。模型经 billion_status 工具按需查询 ref 映射即可（compress 调用时模型从
// nudge 提示或 billion_status 获取 mNNNNN refs）。
func ProcessTurn(messages []CoreMessage, state *CompressionState, config Config, tokenCount int) ProcessTurnResult {
	// 1. assign-refs：为新消息分配 mNNNNN
	AssignRefs(messages, state)

	// 2. advance-survival：推进块存活计数（驱动 young→old 升级）
	state.AdvanceSurvival(config.PromotionThreshold)

	// 3. sync-blocks：按当前视图校正块可见性——视图里已不存在的块停用，避免
	// 「幽灵摘要」继续注入（宿主裁剪历史、会话 fork、块被消费后都可能出现）。
	_ = SyncBlocks(messages, state)

	// 4. render-messages：应用 prune（renderTags=false 保持前缀缓存稳定）
	rendered := RenderMessages(messages, state, config, false)

	// 5. hide-compress-calls：折叠已消费的 compress 调用对。不折叠时 summary 会在
	// 「块」与「调用参数」里各存一份，压缩收益被抵消，模型还会反复看到旧压缩调用。
	if hid := HideConsumedCompressCalls(state, rendered); hid.Hidden > 0 {
		rendered = hid.Messages
	}

	// 5.5 ccr-store：大工具结果到达即按内容 hash 卸载，线上只留确定性占位符。
	// 放在 absorb 之前：absorb 看到的是占位符（很短），自然不会再要求蒸馏一份
	// 已经不存在的原文——这也是内核把 ccr-store 排在 absorb 前面的原因。
	var ccrOffloads []CCROffload
	if config.CCR.Enabled {
		if cr, off := ApplyCCR(rendered, state, config); len(off) > 0 || config.CCR.Enabled {
			rendered = cr
			ccrOffloads = off
			for _, o := range off {
				state.Stats.CCRStored++
				state.Stats.CCRStoredTokens += o.Entry.Tokens
			}
		}
	}

	// 6. absorb（DSC 扩展，仅 Absorb.Enabled）：先隐藏已吸收的 tool-call/result 对，
	// 再为达标的大工具结果追加 [billion absorb] 提示。此前两个节点都没有接线，导致
	// absorb 工具只写记录、原文照旧发送——等于不生效。
	if config.Absorb.Enabled {
		rendered = HideAbsorbedMessages(rendered, state)
		// crush：absorb 的第一段——先用纯确定性策略把大结果压下去，压到阈值下
		// 就不必再让模型蒸馏（省一次往返，也就省一次可能塞不进窗口的调用）。
		if cr, cs := ApplyCrush(rendered, state, config, tokenCount); cs.Crushed+cs.Distilled > 0 {
			rendered = cr
			state.Stats.CrushCount += cs.Crushed + cs.Distilled
			state.Stats.CrushTokensSaved += cs.TokensSaved
		}
		if ap := AppendAbsorbPrompts(rendered, state, config, tokenCount); ap.PromptedCount > 0 {
			rendered = ap.Messages
		}
	}

	// 7. decide-nudge：决策是否注入压缩提示（注入由 adapter 追加为尾部 user 消息）
	nudge := DecideNudge(messages, state, config, tokenCount)
	// 记下这次提示发生时的上下文规模：下一次提示必须在此基础上再涨够
	// growthFloor（见 DecideNudge 的增长门控）。
	//
	// 这一步以前是缺的——baseline 只会被置 0、从不被写入真实规模，于是
	// `if BaselineTokens > 0` 的增长门控恒不成立：只要越过使用率阈值，之后每一轮
	// 都会再提示一次「该压缩了」，模型于是每隔几步就压一次，把细节反复蒸干。增长
	// 门控才是「攒够一段再压」的唯一实现，留空等于没有。
	if nudge.ShouldInject {
		state.Nudge.BaselineTokens = tokenCount
	}

	// 8. emergency-truncate：最后安全阀——上下文接近满且模型未主动压缩时，截断
	// 超大工具输出，避免直接撞窗口（DESIGN 的 LAST safety valve）。
	if tr := TruncateLargeToolOutputs(rendered, tokenCount, config, false); tr.TruncatedCount > 0 {
		rendered = tr.Messages
	}

	return ProcessTurnResult{
		Messages:    rendered,
		State:       state,
		Nudge:       &nudge,
		CCROffloads: ccrOffloads,
	}
}
