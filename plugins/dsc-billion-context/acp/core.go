package acp

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
	return StatusReport{
		ContextUsage:       usage,
		TokenCount:         tokenCount,
		ModelContextLimit:  config.ModelContextLimit,
		ActiveBlocks:       activeCount,
		TotalBlocks:        len(state.Blocks),
		TokensCompressed:   state.Stats.TokensCompressed,
		CompressibleRanges: ComputeCompressibleRanges(messages, state, config),
		Breakdown: map[string]int{
			"tokensCompressed": state.Stats.TokensCompressed,
			"compressionCount": state.Stats.CompressionCount,
			"activeBlocks":     activeCount,
			"totalBlocks":      len(state.Blocks),
		},
	}
}

// FormatStatus 把状态报告格式化为模型可读文本。
func FormatStatus(report StatusReport) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("ACP Context Status:\n"))
	sb.WriteString(fmt.Sprintf("  Usage: %d / %d tokens (%.0f%%)\n",
		report.TokenCount, report.ModelContextLimit, report.ContextUsage*100))
	sb.WriteString(fmt.Sprintf("  Blocks: %d active / %d total\n", report.ActiveBlocks, report.TotalBlocks))
	sb.WriteString(fmt.Sprintf("  Compressed: %d tokens reclaimed across %d compressions\n",
		report.TokensCompressed, report.Breakdown["compressionCount"]))
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
}

// ProcessTurn 每轮跑 pipeline（对齐 acp-kernel processTurn）。
//
// 节点顺序（对齐 DESIGN §4 的 canonical order，已接的部分）：
//
//	assign-refs → advance-survival → sync-blocks → prune(render)
//	→ hide-compress-calls → absorb(hide+prompt) → decide-nudge → emergency-truncate
//
// 仍未实现：merge-blocks（old-gen 块批合并；acp 包内只有范围合并 mergeBatch，
// 不是块合并）、filter/ccr-store（acp_retrieve 内容仓库）。缺失 merge 的后果是
// 块数量随压缩次数线性增长，每个块每轮都渲染 summary——压缩收益会被自身开销
// 逐步吃掉，属后续待补项。
//
// 前缀缓存稳定性：renderTags=false——不把 <acp tokens="N">mNNNNN</acp> 标签注入
// 消息文本。原因：tokens 属性每轮估算会变化（消息文本可能改变），导致前缀缓存
// 失效。模型经 acp_status 工具按需查询 ref 映射即可（compress 调用时模型从
// nudge 提示或 acp_status 获取 mNNNNN refs）。
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

	// 6. absorb（DSC 扩展，仅 Absorb.Enabled）：先隐藏已吸收的 tool-call/result 对，
	// 再为达标的大工具结果追加 [ACP absorb] 提示。此前两个节点都没有接线，导致
	// absorb 工具只写记录、原文照旧发送——等于不生效。
	if config.Absorb.Enabled {
		rendered = HideAbsorbedMessages(rendered, state)
		if ap := AppendAbsorbPrompts(rendered, state, config, tokenCount); ap.PromptedCount > 0 {
			rendered = ap.Messages
		}
	}

	// 7. decide-nudge：决策是否注入压缩提示（注入由 adapter 追加为尾部 user 消息）
	nudge := DecideNudge(messages, state, config, tokenCount)

	// 8. emergency-truncate：最后安全阀——上下文接近满且模型未主动压缩时，截断
	// 超大工具输出，避免直接撞窗口（DESIGN 的 LAST safety valve）。
	if tr := TruncateLargeToolOutputs(rendered, tokenCount, config, false); tr.TruncatedCount > 0 {
		rendered = tr.Messages
	}

	return ProcessTurnResult{
		Messages: rendered,
		State:    state,
		Nudge:    &nudge,
	}
}
