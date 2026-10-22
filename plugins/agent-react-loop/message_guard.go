package main

import (
	"dsc/core"
	"dsc/proto"
	"dsc/session"
)

// deriveMessages 从会话 surface 派生请求历史，并守住工具对完整性。
//
// 孤立 tool 消息（没有前置 assistant tool_call 归属）会让 OpenAI 兼容端点
// 直接拒绝整个请求（422 missing field / orphan tool message），而这可能来自
// 压缩切点、历史注入或旧会话恢复。校验失败时做最小修复（丢弃孤立 tool 消息、
// 剥离无结果的 tool_call）并留一条 Warn：修复只作用于本次请求副本，事件日志
// 仍是 append-only 全量事实源。
func (a *ReactLoopAgent) deriveMessages(sess *session.Session) []*proto.Message {
	msgs := sess.DeriveMessagesLimited(a.historyInjection)
	if err := core.ValidateToolPairing(msgs); err != nil {
		fixed, n := core.RepairToolPairing(msgs)
		a.logger.Warn("repaired tool pairing in request history",
			"session", sess.ID(), "repaired", n, "reason", err.Error())
		return fixed
	}
	return msgs
}
