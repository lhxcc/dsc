// Package adapter 把 DSC 的 proto.Message 与 acp CoreMessage 互转。
// 同时负责 state 持久化（JSON 文件存储）。
package adapter

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
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
		// 工具调用与图像引用保真透传（改写往返不丢结构化字段）——
		// ArgumentsJson 必须一起带：丢了以后模型看到自己的历史全是「调过某工具、
		// 参数未知」，既无法判断自己做过什么，也无法复用上一次的命令。
		for _, tc := range pm.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, bcacp.CoreToolCall{
				ID:        tc.Id,
				Name:      tc.Name,
				Arguments: tc.ArgumentsJson,
			})
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
			Id:      cm.ID,
			Role:    string(cm.Role),
			Content: cm.Text,
		}
		if cm.ToolCallID != "" {
			pm.ToolCallId = cm.ToolCallID
		}
		// 工具调用与图像引用保真还原（缺失即破坏 provider 端协议）
		for _, tc := range cm.ToolCalls {
			pm.ToolCalls = append(pm.ToolCalls, &proto.ToolCall{
				Id:            tc.ID,
				Name:          tc.Name,
				ArgumentsJson: tc.Arguments,
			})
		}
		if len(cm.Images) > 0 {
			pm.Images = append([]string(nil), cm.Images...)
		}
		out = append(out, pm)
	}
	return out
}

// defaultIDProvider 取消息身份：宿主下发的稳定 id（proto.Message.id，派生时按
// surface 事件的 seq 填）。
//
// 没有 id 就返回空串——**不得**退回列表下标。下标是列表位置，请求级裁剪、
// history_injection 只带最近 N 条、fork 都会让它整体平移，于是同一个引用指到
// 另一条消息，压缩块遮错内容（实测渲染从 765K 塌到 1.9K，整段对话只剩两条）。
// 那种「退回旧方案」的分支只会让这个错误在没人注意的地方复发：宁可不压（空 id
// 的消息不参与压缩），也不要压错。
//
// 内容 hash 也不行：重复内容（两次「继续」、两次相同编译输出）会撞成同一个 id。
// 事件的 seq 天然唯一，且与派生范围无关。
func defaultIDProvider(_ int, msg *proto.Message) string {
	if msg == nil {
		return ""
	}
	return msg.Id
}

// PluginName 是本插件名（与 sdk.Config.Name 同源）。
const PluginName = "dsc-billion-context"

// StateFileSuffix 是压缩状态文件的命名：<会话 id>.<插件名>.json。
//
// 用插件名而不是某个跨模块共享的固定后缀（如 ".acp.json"）：插件是**可分离**的，
// 宿主核心不该知道某个插件的落盘细节——删掉本插件后，core 里不该残留任何只为它
// 服务的常量或清理代码。文件名自带插件名，同目录多个插件互不冲突、人工也看得懂。
const StateFileSuffix = "." + PluginName + ".json"

// StateStore 负责 acp state 的持久化（JSON 文件）。
// 每个 session 一个 state 文件，路径为 dir/<sessionID>.<插件名>.json，目录由调用方
// 传 dsc.SessionDir()（与会话日志同目录，靠文件名区分）。
type StateStore struct {
	mu    sync.Mutex
	dir   string
	cache map[string]*bcacp.CompressionState // sessionID → state
}

// NewStateStore 创建状态存储。dir 为存储目录（ExecDir/sessions，与宿主会话同目录）。
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
	path, err := s.path(sessionID)
	if err != nil {
		return nil, err
	}
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
	} else if st.Schema != bcacp.StateSchema {
		// 格式版本不符：不解读、不迁移，整个丢弃。压缩记录由会话事件日志重建
		// （compress 调用自带范围与摘要，重放即可复原），旧格式不保留兼容分支。
		fmt.Fprintf(os.Stderr, "[billion-context] state schema %d != %d: discarded, will rebuild from the session log\n",
			st.Schema, bcacp.StateSchema)
		st = *bcacp.CreateInitialState()
	} else {
		// map 写成 null（手工编辑、半截写入）时补初始化：写回 nil map 会 panic。
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
	if err := os.MkdirAll(s.dir, 0755); err != nil {
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
	dst, err := s.path(sessionID)
	if err != nil {
		return err
	}
	// os.Rename 在 Windows 上等价于 MoveFileEx(MOVEFILE_REPLACE_EXISTING)，可覆盖目标
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("commit acp state: %w", err)
	}
	committed = true
	return nil
}

// path 返回 session state 文件路径。
// 会话 id 只做校验、不做字符替换：替换会把两个不同会话折成同一个文件名（例如
// 空格与下划线），使 state 串到别的会话上。合法性判断复用 SDK 的唯一实现。
func (s *StateStore) path(sessionID string) (string, error) {
	if !dsc.ValidSessionID(sessionID) {
		return "", fmt.Errorf("invalid session id %q", sessionID)
	}
	return dsc.PJoin(s.dir, sessionID+StateFileSuffix), nil
}

// PruneOrphans 清理孤儿状态文件：会话已被删除（<id>.jsonl 不存在）时，其 state
// 没有归属，留着只会在 sessions/ 里越积越多。
//
// 之所以由插件自己清理而不是宿主删除：宿主不该知道插件的落盘命名（插件可分离）。
// 幂等、best-effort：任何错误都跳过，返回删除数量。
func PruneOrphans(sessionsDir string) int {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, StateFileSuffix) {
			continue
		}
		id := strings.TrimSuffix(name, StateFileSuffix)
		if !dsc.ValidSessionID(id) {
			continue
		}
		if _, err := os.Stat(dsc.PJoin(sessionsDir, id+dsc.SessionFileSuffix)); err == nil {
			continue // 会话仍在
		}
		if err := os.Remove(dsc.PJoin(sessionsDir, name)); err == nil {
			removed++
		}
	}
	return removed
}
