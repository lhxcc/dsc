package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"dsc/proto"
	"dsc/session"
)

// 会话历史检索工具（宿主内置，只读）。
//
// 存在的理由：小窗口读超长会话时，滑窗会把早期记录从**本次请求**里裁掉（裁剪只
// 影响这一发，会话日志一字不改）。模型从此看不到那部分内容——它要么凭印象猜，
// 要么重新去读文件/跑命令。这个工具把「整份会话日志」变成可检索的：按关键词找回
// 被滑窗裁掉的原话、命令与输出。
//
// 与记忆插件的分工：记忆是跨会话、面向「值得长期记住的事实」，经过模型摘要；
// 这里检索的是**当前会话的完整原始记录**，不依赖任何摘要，是一次精确回溯。
//
// 为什么由宿主提供、而不是某个插件：数据在宿主（会话日志归宿主所有），且它对
// 「窗口是否装得下历史」这件事有唯一的判断权。
const contextSearchToolName = "context_search"

// contextSearchDefaultLimit / contextSearchMaxLimit 单次返回的命中条数。
// 上限存在是为了别让一次检索把整段历史重新灌回窗口——那就等于没裁剪。
const (
	contextSearchDefaultLimit = 8
	contextSearchMaxLimit     = 20
	contextSearchSnippetRunes = 240
)

type contextSearchTool struct{ m *Manager }

func (t *contextSearchTool) Name() string { return contextSearchToolName }

func (t *contextSearchTool) Description() string {
	return "Search this session's FULL recorded history (every user/assistant message and tool output, including the parts the sliding window has dropped from your context) for a keyword or phrase. " +
		"Use it when you need something that happened earlier but is no longer visible: an earlier decision, a file path, a command you ran, an error message, a value someone gave you. " +
		"Multiple words are ANDed (all must appear in the same message). Returns a short snippet per match plus its position in the log. " +
		"This scans the whole session log, so keep queries specific."
}

func (t *contextSearchTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Keyword or phrase to look for. Multiple space-separated words must all appear in the same message."},
    "limit": {"type": "integer", "description": "Maximum number of matches to return (default 8, max 20)."}
  },
  "required": ["query"]
}`)
}

func (t *contextSearchTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	terms := strings.Fields(strings.ToLower(in.Query))
	if len(terms) == 0 {
		return "", fmt.Errorf("query is empty")
	}
	limit := in.Limit
	if limit <= 0 {
		limit = contextSearchDefaultLimit
	}
	if limit > contextSearchMaxLimit {
		limit = contextSearchMaxLimit
	}

	sess, err := t.m.loadSessionForSearch(CallerFromContext(ctx))
	if err != nil {
		return "", err
	}
	if sess == nil {
		return "(no session log found to search)", nil
	}
	msgs := sess.DeriveMessages()
	if len(msgs) == 0 {
		return "(this session has no recorded messages yet)", nil
	}

	type hit struct {
		idx     int
		role    string
		snippet string
	}
	var hits []hit
	total := 0
	for i, m := range msgs {
		if !matchesAllTerms(searchHaystack(m), terms) {
			continue
		}
		total++
		if len(hits) < limit {
			hits = append(hits, hit{i, m.GetRole(), snippetAround(m.GetContent(), terms, contextSearchSnippetRunes)})
		}
	}
	if total == 0 {
		return fmt.Sprintf("No match for %q in %d recorded messages (searched the whole session log).", in.Query, len(msgs)), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q in %d recorded messages; showing %d. "+
		"These come from the full session log, not from the (possibly trimmed) window:\n",
		total, in.Query, len(msgs), len(hits))
	for _, h := range hits {
		fmt.Fprintf(&b, "\n[#%d %s] %s\n", h.idx, h.role, h.snippet)
	}
	if total > len(hits) {
		fmt.Fprintf(&b, "\n(%d more match(es) not shown — narrow the query or raise limit, max %d.)\n",
			total-len(hits), contextSearchMaxLimit)
	}
	return b.String(), nil
}

// loadSessionForSearch 按调用方会话 id 载入会话日志；不可用时退回默认（项目）会话。
// 为 nil 时返回 (nil, nil)：调用方据此给出「没有可检索的历史」而不是报错。
func (m *Manager) loadSessionForSearch(sessionID string) (*session.Session, error) {
	if m == nil {
		return nil, nil
	}
	st, err := m.sessionStore()
	if err != nil {
		return nil, err
	}
	if sessionID == "" {
		sessionID = m.DefaultSessionID()
	}
	if sessionID == "" {
		return nil, nil
	}
	return st.Load(sessionID)
}

// searchHaystack 收集一条消息里可被检索的文本：正文 + 工具调用的名称与参数。
//
// 参数也要进去：模型常常只记得「我跑过某条命令 / 改过某个文件」，那部分信息在
// tool_calls 的 arguments 里，而不在任何正文里。
func searchHaystack(m *proto.Message) string {
	if m == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(strings.ToLower(m.GetContent()))
	for _, tc := range m.GetToolCalls() {
		b.WriteString("\n")
		b.WriteString(strings.ToLower(tc.GetName()))
		b.WriteString(" ")
		b.WriteString(strings.ToLower(tc.GetArgumentsJson()))
	}
	return b.String()
}

// matchesAllTerms 要求每个词都出现（AND）。多词时用 AND 而不是 OR：
// 检索的目的是「精确定位那一段」，OR 会让常见词把无关记录全捞回来。
func matchesAllTerms(hay string, terms []string) bool {
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// snippetAround 取命中位置附近的一段作为摘要：折叠换行与连续空白成单空格
// （模型与 TUI 都按一行读更省事），两端加省略号表明被截断。
func snippetAround(text string, terms []string, maxRunes int) string {
	if text == "" {
		return "(no text; matched on tool arguments)"
	}
	flat := collapseWhitespace(text)
	lower := strings.ToLower(flat)
	pos := -1
	for _, t := range terms {
		if i := strings.Index(lower, t); i >= 0 && (pos < 0 || i < pos) {
			pos = i
		}
	}
	runes := []rune(flat)
	if pos < 0 || len(runes) <= maxRunes {
		return truncateRunes(flat, maxRunes)
	}
	// pos 是字节下标，换算成 rune 下标再取窗口
	runePos := len([]rune(flat[:pos]))
	start := runePos - maxRunes/3
	if start < 0 {
		start = 0
	}
	end := start + maxRunes
	if end > len(runes) {
		end = len(runes)
		start = end - maxRunes
	}
	out := string(runes[start:end])
	if start > 0 {
		out = "..." + out
	}
	if end < len(runes) {
		out += "..."
	}
	return out
}

// collapseWhitespace 把换行/Tab/连续空白压成单个空格（保留词语边界）。
func collapseWhitespace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r == ' ' {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
