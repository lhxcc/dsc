package acp

// sync-blocks 节点（对齐 acp-kernel sync.ts）：把块状态与会话消息列表对齐。
//
// 职责（v0.0.75 语义）：
//   - 被蒸馏消费的块（出现在其他块的 DirectBlockIDs 中）置 inactive；
//   - 用户已显式 decompress 的块（Expanded）保持 inactive——防止下一轮把
//     已恢复的原始消息重新折叠（双倍成本 + 丢原文）；
//   - 其余块置 active，但若其覆盖的原始消息与其渲染 summary 均不在当前
//     消息列表中（stillPresent 判定），置 inactive——宿主传裁剪视图时不丢块活性；
//   - 返回本轮被停用的块 ID 列表（供宿主日志/诊断）。

// SyncBlocks 把块 active 状态与消息列表对齐（原地修改 state）。
// 只按**语义**停用：被蒸馏消费、或被用户显式展开。曾经还按「覆盖消息不在本轮视图里」
// 停用，那是把**临时视图**当成事实——history_injection 只带最近 N 条、宿主裁剪、
// fork 都会让块本轮不可见，停用一旦写回状态，用户把配置改回来也复原不了，压缩收益
// 永久丢失（配置是用户输入，随时会改）。缺席的块本轮不注入摘要即可（见
// collectSummaryAnchors），状态保持可用。
func SyncBlocks(messages []CoreMessage, state *CompressionState) []string {
	presentIDs := map[string]bool{}
	for _, m := range messages {
		if m.ID != "" {
			presentIDs[m.ID] = true
		}
	}

	// 被蒸馏消费的块集合（其他块的 DirectBlockIDs 引用即视为被消费）
	consumed := map[string]bool{}
	for i := range state.Blocks {
		for _, id := range state.Blocks[i].DirectBlockIDs {
			consumed[id] = true
		}
	}

	var deactivated []string
	for i := range state.Blocks {
		b := &state.Blocks[i]
		switch {
		case consumed[b.BlockID]:
			b.Active = false
			continue
		case b.Expanded:
			// 用户显式展开：保持 inactive（展开语义见字段注释）
			b.Active = false
			continue
		}
		b.Active = true
	}
	return deactivated
}
