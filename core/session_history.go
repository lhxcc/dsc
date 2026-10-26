package core

import (
	"strings"

	"dsc/proto"
	"dsc/session"
)

// HistoryShowsDefault history_shows 为 0（未设置）时，TUI 默认回显的历史消息条数。
// 它是**渲染上限**，不是注入上限——模型仍按 history_injection 的语义拿恢复历史。
const HistoryShowsDefault = 20

// resolveHistoryShowsCount 把 history_shows 配置解析为「最多显示多少条」。
//
// injectCount 是 history_injection 的有效值（负值 = 不注入恢复历史）。显示层受
// 注入层门控：注入关闭时没有可显示的恢复历史，显示也一并关闭。其余沿用 DSC 三值
// 语义：负值 = 不显示、0 = 未设置（默认 HistoryShowsDefault）、N>0 = 显示 N 条。
//
// 返回值只是「请求条数」；最终还会被实际可显示的消息数截断（见 LoadDisplayHistory），
// 因此显示量不会超过注入量。
func resolveHistoryShowsCount(shows, injectCount int) int {
	if injectCount < 0 {
		return 0
	}
	switch {
	case shows < 0:
		return 0
	case shows == 0:
		return HistoryShowsDefault
	default:
		return shows
	}
}

// LoadDisplayHistory 加载当前工作区项目会话，按历史注入设置派生消息，并截取最近
// resolveHistoryShowsCount(historyShows, historyInjection) 条可显示消息（仅
// user/assistant，跳过 system/tool），供 TUI 启动时在正文区回显——避免用户重启后
// 面对空白界面、想不起先前的对话。
//
// 与 agent 视角严格对齐（这三点保证「显示的就是模型也看得到的那部分恢复历史」）：
//   - 会话键与 agent 相同：session.SessionKeyForProject(WorkspaceRoot)；
//   - 注入条数以会话内 history/limit 事件优先（/settings history 持久化），无记录
//     时回退 historyInjection 配置值——agent 每次 Run 也是这样折叠的；
//   - 派生走同一个 session.DeriveMessagesLimited，因此「注入关闭 = 不显示」与
//     「显示不超过实际注入量」天然成立，不需要在此另做上限换算。
//
// 返回 nil 表示无需回显（无会话、注入关闭、显示关闭、或没有可显示消息）。
func (m *Manager) LoadDisplayHistory(historyInjection, historyShows int) []*AgentDebugMessage {
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
	count := resolveHistoryShowsCount(historyShows, inject)
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
