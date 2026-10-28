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
// TUI 向上翻页时逐页回填；Initial 是启动时先渲染的尾部**条目数**（受 history_shows
// 与注入量共同约束）。这样初始屏幕上看到的是「模型也看得到的那部分」，而向上
// 滚动可以继续读到更早的完整对话。
type HistoryReplay struct {
	Messages []*AgentDebugMessage `json:"messages"`
	Initial  int                  `json:"initial"`
}

// LoadHistoryReplay 加载当前工作区项目会话，构造启动回显数据：
//   - Messages 取全量回放条目：user/assistant 文本 + 工具调用/结果（不含 system）；
//   - Initial 取「最近 N 条对话消息」所覆盖的条目数（N 由 resolveHistoryShowsCount
//     决定，并以「注入视角下的对话条数」封顶）——工具卡不占名额，但会随所属消息
//     一起进入初始切片。
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
	// 初始显示量封顶在「注入视角下可显示的**对话条数**」：DeriveMessagesLimited(inject)
	// 只截掉头部，其尾部与全量历史的尾部一致，故取尾部即与注入历史对齐。
	// 注意按对话消息计数，不把工具卡算进名额（否则 history_shows=20 只够三四个来回）。
	injected := displayableMessages(sess.DeriveMessagesLimited(inject))
	shows := count
	if conv := countConversationMessages(injected); shows > conv {
		shows = conv
	}
	if shows == 0 {
		return nil
	}
	all := displayableMessages(sess.DeriveMessages())
	if len(all) == 0 {
		return nil
	}
	// Initial 是**条目数**（TUI 按条目切片）：最近 shows 条对话消息 + 它们的工具卡。
	initial := tailEntryCountForMessages(all, shows)
	if initial <= 0 {
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

// displayableMessages 把派生消息展开成**回放条目**：文本气泡 + 工具过程。
//
// system 是内部提示，不回显；但 tool 过程必须回显——否则重启后只剩「结论」，
// 看不到模型改代码、跑命令的过程（用户实测正是这个缺口）。展开规则：
//   - user/assistant 正文非空 → 一条文本气泡（正文为空但有工具调用的助手消息不产生气泡）；
//   - assistant 的每个 ToolCalls → 一条 DebugKindToolCall（名称 + 参数 JSON）；
//   - role=="tool" → 一条 DebugKindToolResult（结果文本，失败按 exit_code 判定）。
//
// 条目顺序即真实发生顺序：助手文字 → 它的若干调用 → 对应结果 → 下一条消息。
func displayableMessages(msgs []*proto.Message) []*AgentDebugMessage {
	out := make([]*AgentDebugMessage, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.GetRole() {
		case "user", "assistant":
			if strings.TrimSpace(msg.GetContent()) != "" {
				out = append(out, &AgentDebugMessage{Role: msg.GetRole(), Content: msg.GetContent()})
			}
			for _, tc := range msg.GetToolCalls() {
				out = append(out, &AgentDebugMessage{
					Role:     "tool", // 语义角色仍是 tool（Kind 再细分调用/结果）
					Kind:     DebugKindToolCall,
					ToolName: tc.GetName(),
					ToolArgs: tc.GetArgumentsJson(),
				})
			}
		case "tool":
			out = append(out, &AgentDebugMessage{
				Role:    "tool",
				Kind:    DebugKindToolResult,
				Content: msg.GetContent(),
				IsError: toolResultIsError(msg.GetContent()),
			})
		}
	}
	return out
}

// isConversationEntry 报告是否为「一条对话消息」（文本气泡）。
// 工具卡是某条消息的附属过程，不占 history_shows 的名额——否则 20 条里只能塞下
// 三四个来回，回显量骤减。
func isConversationEntry(m *AgentDebugMessage) bool {
	return m != nil && m.Kind == "" && (m.Role == "user" || m.Role == "assistant")
}

// countConversationMessages 数回放条目里的对话消息条数（忽略工具卡）。
func countConversationMessages(entries []*AgentDebugMessage) int {
	n := 0
	for _, e := range entries {
		if isConversationEntry(e) {
			n++
		}
	}
	return n
}

// tailEntryCountForMessages 从尾部数 n 条对话消息，返回它们（含各自工具卡）占用的
// 条目数，用于「初始只显示最近 n 条消息」。不足 n 条时返回全部条目数。
func tailEntryCountForMessages(entries []*AgentDebugMessage, n int) int {
	if n <= 0 {
		return 0
	}
	seen := 0
	for i := len(entries) - 1; i >= 0; i-- {
		if !isConversationEntry(entries[i]) {
			continue
		}
		seen++
		if seen == n {
			return len(entries) - i
		}
	}
	return len(entries)
}

// toolResultIsError 判定工具结果是否失败：工具统一在结果尾部附 [exit_code: N]
// （见各工具的结果格式化），N != 0 即失败。没有该标记的一律按成功处理。
func toolResultIsError(content string) bool {
	const marker = "[exit_code: "
	i := strings.LastIndex(content, marker)
	if i < 0 {
		return false
	}
	rest := content[i+len(marker):]
	end := strings.IndexByte(rest, ']')
	if end < 0 {
		return false
	}
	code := strings.TrimSpace(rest[:end])
	return code != "" && code != "0"
}

// seedSessionPromptFromLog 从会话日志恢复上次成功请求的用量锚点（含请求信封指纹），
// 写入宿主侧 per-session 锚点表（pre-step 的投影压力即来源于此）。重启后的首发请求
// 没有服务端 usage 可依，若不恢复，压缩插件会把全量派生历史当作已用（其中大部分
// 已被压缩后端遮蔽、模型根本看不到），压缩压力虚高。
//
// 注意：旧记录没有信封指纹（ToolsHash 为空）→ 锚点会被判为不可比，首发的压力退回
// 纯估算。这是刻意的保守取舍：拿不可比的用量当底数会把压力算小。
//
// 倒查逻辑本身在 session.LastUsageAnchor（唯一实现，agent 插件经 sdk 调用同一份）。
func (m *Manager) seedSessionPromptFromLog(sessionID string, sess *session.Session) {
	if m == nil {
		return
	}
	a, ok := session.LastUsageAnchor(sess)
	if !ok {
		return
	}
	m.recordSessionAnchor(sessionID, sessionUsageAnchor{
		promptTokens:  a.PromptTokens,
		sentEstimate:  a.SentEstimate,
		toolsHash:     a.ToolsHash,
		model:         a.Model,
		contextWindow: a.ContextWindow,
	})
	// 同时预置展示用的「已用」：恢复有历史的会话时，用户发出第一个请求之前
	// 标题栏不该显示 0——上下文里已经躺着这段历史了。取上一次请求的 prompt 侧实测
	// （没有则退回当时的表面估算），第一帧真实用量到达后自然被替换。
	seed := a.PromptTokens
	if seed <= 0 {
		seed = a.SentEstimate
	}
	m.SeedContextUsage(sessionID, seed)
}
