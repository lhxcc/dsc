// Package adapter 把 DSC 的 proto.Message 与 acp CoreMessage 互转。
// 同时负责 state 持久化（JSON 文件存储）。
package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	bcacp "dsc-billion-context/acp"
	dsc "dsc-sdk"
	"dsc/proto"
)

// ToCoreMessages 把 DSC proto.Message 列表转为 acp CoreMessage 列表。
// ID 由调用方提供（通常为消息在 session 中的 seq 或 hash）。
//
// 转换规则：
//   - role 直接映射（user/assistant/system/tool）
//   - contentType 由 role + 是否有 ToolCalls 推断
//   - text 取 Content 字段
//   - toolName / toolCallId 取自 ToolCallId / 第一个 ToolCall.Name
func ToCoreMessages(protoMsgs []*proto.Message, idProvider func(idx int, msg *proto.Message) string) []bcacp.CoreMessage {
	if idProvider == nil {
		idProvider = defaultIDProvider
	}
	out := make([]bcacp.CoreMessage, 0, len(protoMsgs))
	for i, pm := range protoMsgs {
		cm := bcacp.CoreMessage{
			ID:   idProvider(i, pm),
			Role: bcacp.MessageRole(pm.Role),
			Text: pm.Content,
		}
		// 推断 contentType
		switch pm.Role {
		case "tool":
			cm.ContentType = bcacp.ContentTypeToolResult
			cm.ToolCallID = pm.ToolCallId
		case "assistant":
			if len(pm.ToolCalls) > 0 {
				cm.ContentType = bcacp.ContentTypeToolCall
				cm.ToolName = pm.ToolCalls[0].Name
				cm.ToolCallID = pm.ToolCalls[0].Id
			} else {
				cm.ContentType = bcacp.ContentTypeText
			}
		case "system":
			cm.ContentType = bcacp.ContentTypeText
		default: // user
			cm.ContentType = bcacp.ContentTypeText
		}
		// 工具调用与图像引用保真透传（改写往返不丢结构化字段）
		for _, tc := range pm.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, bcacp.CoreToolCall{ID: tc.Id, Name: tc.Name})
		}
		if len(pm.Images) > 0 {
			cm.Images = append([]string(nil), pm.Images...)
		}
		out = append(out, cm)
	}
	return out
}

// FromCoreMessages 把 acp CoreMessage 列表转回 DSC proto.Message。
// 用于把改写后的消息列表发回给 LLM。
func FromCoreMessages(coreMsgs []bcacp.CoreMessage) []*proto.Message {
	out := make([]*proto.Message, 0, len(coreMsgs))
	for _, cm := range coreMsgs {
		pm := &proto.Message{
			Role:    string(cm.Role),
			Content: cm.Text,
		}
		if cm.ToolCallID != "" {
			pm.ToolCallId = cm.ToolCallID
		}
		// 工具调用与图像引用保真还原（缺失即破坏 provider 端协议）
		for _, tc := range cm.ToolCalls {
			pm.ToolCalls = append(pm.ToolCalls, &proto.ToolCall{Id: tc.ID, Name: tc.Name})
		}
		if len(cm.Images) > 0 {
			pm.Images = append([]string(nil), cm.Images...)
		}
		out = append(out, pm)
	}
	return out
}

// defaultIDProvider 默认 ID 提供者：**以消息在原始历史中的位置作为身份**。
//
// 为什么不是内容 hash：两条内容相同的消息（重复的"继续"、两次相同 compile 输出、
// 相同工具结果）会得到同一个 ID，于是 MessageRefs.ByRaw 互相覆盖、CoveredMessageIDs
// 判定错位——压缩会命中错误的消息。acp-kernel 里 ID 由宿主会话日志提供（天然唯一），
// DSC 的 proto.Message 没有 id/seq 字段，位置就是最接近的等价物。
//
// 位置为什么稳定：pre-step 每轮收到的是 agent 的 append-only 原始历史（插件的改写
// 只影响发给 LLM 的视图，不回写 agent 历史），因此同一消息在跨轮的位置不变；
// 压缩/prune 造成的"视图平移"发生在渲染之后，不参与下一轮的 ID 分配。
//
// 注意：状态里若残留旧版（内容 hash）ID 的块，升级后首轮因 ID 失配会被 sync-blocks
// 停用，等于一次性重新建立压缩记录；这是从错误 ID 迁移出来的一次性代价。
func defaultIDProvider(idx int, _ *proto.Message) string {
	return fmt.Sprintf("msg-%d", idx)
}

// StateStore 负责 acp state 的持久化（JSON 文件）。
// 每个 session 一个 state 文件，路径为 dir/<sessionID>.json。
type StateStore struct {
	mu    sync.Mutex
	dir   string
	cache map[string]*bcacp.CompressionState // sessionID → state
}

// NewStateStore 创建状态存储。dir 为存储目录（通常 ExecDir/billion-context）。
func NewStateStore(dir string) *StateStore {
	return &StateStore{
		dir:   dir,
		cache: map[string]*bcacp.CompressionState{},
	}
}

// Load 加载（或创建）指定 session 的 state。
func (s *StateStore) Load(sessionID string) (*bcacp.CompressionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.cache[sessionID]; ok {
		return st, nil
	}
	path := s.path(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			st := bcacp.CreateInitialState()
			s.cache[sessionID] = st
			return st, nil
		}
		return nil, fmt.Errorf("load acp state: %w", err)
	}
	var st bcacp.CompressionState
	if err := json.Unmarshal(data, &st); err != nil {
		// 损坏的 state 文件：重建（避免阻塞会话）
		st = *bcacp.CreateInitialState()
	} else {
		// 确保map初始化（旧版本可能缺少字段）
		if st.MessageRefs.ByRaw == nil {
			st.MessageRefs.ByRaw = map[string]string{}
		}
		if st.MessageRefs.ByRef == nil {
			st.MessageRefs.ByRef = map[string]string{}
		}
		if st.Nudge.Anchors == nil {
			st.Nudge.Anchors = map[string]any{}
		}
	}
	s.cache[sessionID] = &st
	return &st, nil
}

// Save 持久化指定 session 的 state。
//
// 原子写：先写同目录临时文件并 fsync，再 rename 覆盖目标。直接 WriteFile 在进程崩溃
// 或断电时可能留下半截 JSON——下次 Load 解析失败虽然会重建，但压缩记录全丢，
// 上下文随即重新膨胀（压缩是"只增不减"的收益，丢一次等于全部白做）。
func (s *StateStore) Save(sessionID string, state *bcacp.CompressionState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache[sessionID] = state
	if err := os.MkdirAll(s.dir, 0700); err != nil {
		return fmt.Errorf("create acp state dir: %w", err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal acp state: %w", err)
	}

	tmp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp acp state: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp acp state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temp acp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp acp state: %w", err)
	}
	// os.Rename 在 Windows 上等价于 MoveFileEx(MOVEFILE_REPLACE_EXISTING)，可覆盖目标
	if err := os.Rename(tmpName, s.path(sessionID)); err != nil {
		return fmt.Errorf("commit acp state: %w", err)
	}
	committed = true
	return nil
}

// path 返回 session state 文件路径。
func (s *StateStore) path(sessionID string) string {
	// 防止路径遍历：sessionID 仅允许字母数字与 -_
	safe := sanitizeSessionID(sessionID)
	return dsc.PJoin(s.dir, safe+".json")
}

// sanitizeSessionID 把 sessionID 限制为安全字符（防止路径遍历）。
func sanitizeSessionID(id string) string {
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '-' || c == '_' {
			out = append(out, c)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "default"
	}
	return string(out)
}
