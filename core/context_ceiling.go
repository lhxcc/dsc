package core

import (
	"dsc/proto"
)

// 发出去的消息必须有硬上限。
//
// 背景（一次真实的 400）：1M 窗口的会话，压缩后端把 1.66M 历史渲染成 1.07M 就发出去了，
// provider 直接拒（`prompt is too long: 1075596 tokens > 1048576 maximum`）。整条链上
// **没有任何一层**保证「最终要发的这份 ≤ 窗口」：
//   - agent 侧：有后端时按设计不做请求级裁剪（它看到的是改写**前**的列表，裁了也白裁）；
//   - 后端侧：渲染多少完全由它自己的策略决定（保留区、块覆盖不全、重建后的空档都可能
//     让它超出窗口），它并没有「必须塞进窗口」的义务；
//   - 兜底侧：只有 provider 报错之后的 request-error 重试，而那次压缩未必成功
//     （可压缩范围为空、压缩后仍超、或插件判定不该动），一次救不回来整轮就失败。
//
// 所以在这里补一层**确定性**的最后防线：pre-step 改写完成、请求即将发出之前，量一次
// 真实表面（消息 + 工具目录），超了就从头丢，直到落进窗口上限内。它与压缩后端的职责
// 不冲突——后端负责「少丢、丢得有信息量」，这里只负责「绝不发出一个必然被拒的请求」。
//
// 为什么现在才敢在宿主侧裁：消息身份已经是宿主下发的稳定 id（event-<seq>），裁剪不再
// 让压缩块的引用错位（那正是当初取消裁剪的原因，见 docs/token-accounting.md §7.1）。
const (
	// contextCeilingRatio 发出去的消息最多占窗口的这个比例。
	//
	// 留 10% 余量，是因为我们的启发式估算与 provider 的分词器并不等价（CJK、JSON、
	// 图像引用各有偏差）。实测过一次真实越界：provider 报 1,075,596 而请求正是按
	// 「渲染后 1.07M」发出的——宁可早丢一点，也不要用 400 去发现偏差。
	contextCeilingRatio = 0.90

	// estimateInflationMax 估算偏差系数的上限。只放大、不缩小：估算偏大时多丢历史
	// 是净损失，而偏差系数本身就是从「provider 比我们算得多」这一现象里得来的。
	estimateInflationMax = 1.5
)

// envelopeMatches 报告用量锚点与当前请求是否同一个「信封」（工具目录 + 窗口 + 模型）。
// 三者任一不同，那次用量与本次请求就不可比。
func envelopeMatches(anchor sessionUsageAnchor, curToolsHash, curModel string, window int) bool {
	if anchor.toolsHash == "" || curToolsHash == "" || anchor.toolsHash != curToolsHash {
		return false
	}
	if anchor.contextWindow > 0 && window > 0 && anchor.contextWindow != window {
		return false // 换了窗口：用量不可比
	}
	if anchor.model != "" && curModel != "" && anchor.model != curModel {
		return false // 换了模型：tokenizer/计价都可能变，锚点作废
	}
	return true
}

// estimateInflation 返回「我们的估算相对 provider 实际用量」的放大系数（≥1）。
//
// 锚点记的是同一次请求的两个数：我们算的 sentEstimate 与 provider 报的 promptTokens。
// 两者一除就是这一模型、这类内容的系统性偏差。它正是「算得不准，于是以为装得下」的
// 修正项：判定上限时用放大后的估算，而不是相信我们自己的算数。
//
// 只在该次请求没被改写的情况下有意义——被压缩改写过的那次，promptTokens 描述的是
// 改写后的表面、sentEstimate 也是（recordSessionAnchor 记的正是改写后的 req），所以
// 仍然可比。provider 用量低于我们的估算时不缩小系数（偏大只会白丢历史）。
func (m *Manager) estimateInflation(sessionID, curToolsHash, curModel string, window int) float64 {
	anchor, ok := m.lastSessionAnchor(sessionID)
	if !ok || anchor.promptTokens <= 0 || anchor.sentEstimate <= 0 {
		return 1
	}
	if !envelopeMatches(anchor, curToolsHash, curModel, window) {
		return 1
	}
	inflation := float64(anchor.promptTokens) / float64(anchor.sentEstimate)
	if inflation < 1 {
		return 1
	}
	if inflation > estimateInflationMax {
		return estimateInflationMax
	}
	return inflation
}

// CapMessagesToWindow 从消息列表头部丢弃，直到「保留部分 + 工具目录」落进 limit。
// 返回保留的消息与丢弃条数；装得下时原样返回（dropped=0）。
//
// 规则：
//   - 首条 system 保留：它是部署写的指令，丢了模型就失去了行为约束，而它通常只占
//     很小一部分；
//   - 保留段的起点不能是 tool 消息：那会留下一个没有对应调用的孤儿结果（provider
//     直接 400），遇到就往后顺延到不是 tool 的位置；
//   - 至少保留最后一条：用户这一句或最新的工具结果绝不能丢。
func CapMessagesToWindow(msgs []*proto.Message, toolsTokens, limit int) (kept []*proto.Message, dropped int) {
	if len(msgs) == 0 || limit <= 0 {
		return msgs, 0
	}
	per := make([]int, len(msgs))
	total := toolsTokens
	for i, m := range msgs {
		per[i] = EstimateProtoMessageTokens(m)
		total += per[i]
	}
	if total <= limit {
		return msgs, 0
	}

	head := 0
	if msgs[0].GetRole() == "system" && len(msgs) > 1 {
		head = 1 // 首条 system 永久保留
	}
	// 从尾部累加，找到最靠前的起点 cut，使 msgs[cut:] + tools 仍能装下。
	cut := len(msgs) - 1 // 至少保留最后一条
	acc := per[len(msgs)-1] + toolsTokens
	for i := len(msgs) - 2; i >= head; i-- {
		if acc+per[i] > limit {
			break
		}
		acc += per[i]
		cut = i
	}
	// 起点若落在 tool 结果上（它的调用已被丢掉），顺延到下一个可独立阅读的消息。
	for cut < len(msgs)-1 && msgs[cut].GetRole() == "tool" {
		cut++
	}
	if cut <= head {
		return msgs, 0 // 无处可丢（甚至装不下的就是 system + 最后一条）：原样发
	}

	kept = make([]*proto.Message, 0, len(msgs)-cut+head)
	if head == 1 {
		kept = append(kept, msgs[0])
	}
	kept = append(kept, msgs[cut:]...)
	return kept, cut - head
}

// enforceRequestCeiling 保证「即将发出的请求」不超过窗口上限；返回（可能被裁剪的）
// 消息、裁剪条数与被裁掉的估算量。
//
// inflation 由 estimateInflation 给出（≥1），用于把「我们算的」放大成「provider 大概
// 会算的」再与上限比较。window<=0（窗口未知）时不裁。
func enforceRequestCeiling(msgs []*proto.Message, toolsTokens, window int, inflation float64) (out []*proto.Message, dropped, before, after int) {
	if window <= 0 || len(msgs) == 0 {
		return msgs, 0, 0, 0
	}
	if inflation < 1 {
		inflation = 1
	}
	before = EstimateProtoMessagesTokens(msgs) + toolsTokens
	effective := int(float64(before) * inflation)
	if effective <= window {
		return msgs, 0, before, before
	}
	limit := int(float64(window) * contextCeilingRatio)
	// 裁剪按「真实估算」做，但预算按放大后的口径收紧：把 limit 除以 inflation，
	// 等价于「让 provider 看到的不超过 limit」。
	if inflation > 1 {
		limit = int(float64(limit) / inflation)
	}
	out, dropped = CapMessagesToWindow(msgs, toolsTokens, limit)
	after = EstimateProtoMessagesTokens(out) + toolsTokens
	if dropped == 0 {
		return msgs, 0, before, before
	}
	return out, dropped, before, after
}
