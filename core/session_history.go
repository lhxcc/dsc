package core

import (
	"strings"

	"dsc/proto"
	"dsc/session"
)

// HistoryShowsDefault 恢复历史回显的默认条数：当 history_injection 为 0（未设置，
// 即「不限制注入」）时，TUI 只回显最近这么多条，避免长会话启动即刷屏。它是**渲染
// 上限**，不是注入上限——模型仍按 history_injection 的语义拿到全部恢复历史。
const HistoryShowsDefault = 20

// displayCountForInjection 由 history_injection 派生 TUI 回显的条数——显示与注入
// 共用同一个值，不再有第二个「显示条数」配置项，因此不存在两个旋钮不一致的对齐问题。
//
//   - 负值：不注入恢复历史 → 也不显示；
//   - 0（未设置，不限制注入）：回显默认 HistoryShowsDefault 条（渲染上限，非注入上限）；
//   - N>0：回显最近 N 条（不超过注入量）。
func displayCountForInjection(injectCount int) int {
	if injectCount < 0 {
		return 0
	}
	if injectCount == 0 {
		return HistoryShowsDefault
	}
	return injectCount
}

// LoadDisplayHistory 加载当前工作区项目会话，按历史注入设置派生消息，并截取最近
// displayCountForInjection(historyInjection) 条可显示消息（仅 user/assistant，跳过
// system/tool），供 TUI 启动时在正文区回显——避免用户重启后面对空白界面、想不起
// 先前的对话。
//
// 与 agent 视角严格对齐（这三点保证「显示的就是模型也看得到的那部分恢复历史」）：
//   - 会话键与 agent 相同：session.SessionKeyForProject(WorkspaceRoot)；
//   - 注入条数以会话内 history/limit 事件优先（/settings history 持久化），无记录
//     时回退 historyInjection 配置值——agent 每次 Run 也是这样折叠的；
//   - 派生走同一个 session.DeriveMessagesLimited，因此「注入关闭 = 不显示」与
//     「显示不超过实际注入量」天然成立，不需要在此另做上限换算。
//
// 返回 nil 表示无需回显（无会话、注入关闭、或没有可显示消息）。
func (m *Manager) LoadDisplayHistory(historyInjection int) []*AgentDebugMessage {
	if m == nil {
		return nil
	}
	st, err := m.sessionStore()
	if err != nil {
		return nil
	}
	sess, err := st.Load(session.SessionKeyForProject(WorkspaceRoot))
	if err != nil || sess == nil {
		return nil
	}
	inject := historyInjection
	if limit, ok := session.FoldHistoryLimit(sess.Events()); ok {
		inject = limit // 会话内 /settings history 覆盖配置缺省（与 agent 一致）
	}
	count := displayCountForInjection(inject)
	if count == 0 {
		return nil
	}
	// DeriveMessagesLimited 已按 inject 截好「恢复历史」，此处只做可读性过滤。
	out := displayableMessages(sess.DeriveMessagesLimited(inject))
	if len(out) > count {
		out = out[len(out)-count:]
	}
	return out
}

// displayableMessages 从派生消息里挑出适合给用户看的（user/assistant，且正文非空）。
// system 是内部提示、tool 是过程产物，回显它们只会淹没对话主线。
func displayableMessages(msgs []*proto.Message) []*AgentDebugMessage {
	out := make([]*AgentDebugMessage, 0, len(msgs))
	for _, msg := range msgs {
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		if strings.TrimSpace(msg.Content) == "" {
			continue
		}
		out = append(out, &AgentDebugMessage{Role: msg.Role, Content: msg.Content})
	}
	return out
}
