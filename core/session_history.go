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

// HistoryReplay 启动回显所需的完整可显示历史与初始渲染条数。
//
// Messages 是**全量**可显示历史（旧→新，不受 history_injection 上限约束），供
// TUI 向上翻页时逐页回填；Initial 是启动时先渲染的尾部条数（受 history_shows
// 与注入量共同约束）。这样初始屏幕上看到的是「模型也看得到的那部分」，而向上
// 滚动可以继续读到更早的完整对话。
type HistoryReplay struct {
	Messages []*AgentDebugMessage `json:"messages"`
	Initial  int                  `json:"initial"`
}

// LoadHistoryReplay 加载当前工作区项目会话，构造启动回显数据：
//   - Messages 取全量可显示消息（user/assistant，不含 system/tool）；
//   - Initial 取 resolveHistoryShowsCount(historyShows, historyInjection)，并再以
//     「注入视角下的可显示条数」封顶，保证初始显示不超过实际注入量。
//
// 与 agent 视角严格对齐：
//   - 会话键与 agent 相同：session.SessionKeyForProject(WorkspaceRoot)；
//   - 注入条数以会话内 history/limit 事件优先（/settings history 持久化），无记录
//     时回退 historyInjection 配置值——agent 每次 Run 也是这样折叠的。
//
// 返回 nil 表示无需回显（无会话、注入关闭、显示关闭、或没有可显示消息）。
func (m *Manager) LoadHistoryReplay(historyInjection, historyShows int) *HistoryReplay {
	if m == nil {
		return nil
	}
	st, err := m.sessionStore()
	if err != nil {
		return nil
	}
	key := session.SessionKeyForProject(WorkspaceRoot)
	sess, err := st.Load(key)
	if err != nil || sess == nil {
		return nil
	}
	// 重启恢复：用会话日志里上次成功请求的 prompt 用量重建宿主侧读数，供 pre-step
	// 的 LastUsageTokens 使用。否则首发判定退回全量派生估算（把压缩后端遮蔽、模型
	// 看不到的历史也算进去），压缩压力虚高、可能误触发压缩/超窗。
	m.seedSessionPromptFromLog(key, sess)
	inject := historyInjection
	if limit, ok := session.FoldHistoryLimit(sess.Events()); ok {
		inject = limit // 会话内 /settings history 覆盖配置缺省（与 agent 一致）
	}
	count := resolveHistoryShowsCount(historyShows, inject)
	if count == 0 {
		return nil
	}
	// 初始显示量封顶在「注入视角下的可显示条数」：DeriveMessagesLimited(inject)
	// 只截掉头部，其尾部与全量历史的尾部一致，故取尾部即与注入历史对齐。
	injected := displayableMessages(sess.DeriveMessagesLimited(inject))
	initial := count
	if initial > len(injected) {
		initial = len(injected)
	}
	if initial == 0 {
		return nil
	}
	all := displayableMessages(sess.DeriveMessages())
	if len(all) == 0 {
		return nil
	}
	return &HistoryReplay{Messages: all, Initial: initial}
}

// LoadDisplayHistory 返回启动时初始渲染的那段切片（LoadHistoryReplay 的尾部
// Initial 条）。保留此便捷方法供只关心首屏的调用方与测试使用。
func (m *Manager) LoadDisplayHistory(historyInjection, historyShows int) []*AgentDebugMessage {
	r := m.LoadHistoryReplay(historyInjection, historyShows)
	if r == nil {
		return nil
	}
	return r.Messages[len(r.Messages)-r.Initial:]
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

// seedSessionPromptFromLog 从会话日志恢复上次成功请求的 prompt 用量，写入宿主侧
// per-session 读数（pre-step 事件的 LastUsageTokens 即来源于此）。重启后的首发请求
// 没有服务端 usage 可依，若不恢复，压缩插件会把全量派生历史当作已用（其中大部分
// 已被压缩后端遮蔽、模型根本看不到），压缩压力虚高。
//
// 倒查逻辑本身在 session.LastPromptUsage（唯一实现，agent 插件经 sdk 调用同一份），
// 这里只负责把结果写进宿主的 per-session 读数表。
func (m *Manager) seedSessionPromptFromLog(sessionID string, sess *session.Session) {
	if m == nil {
		return
	}
	// recordSessionUsage 沿用 proto.Usage 的 int32 口径（与 ChatStream 结算处同一入参类型）。
	if promptTokens, _, ok := session.LastPromptUsage(sess); ok {
		m.recordSessionUsage(sessionID, int32(promptTokens))
	}
}
