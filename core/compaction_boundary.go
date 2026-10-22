package core

import (
	"fmt"
	"sort"
	"strings"

	"dsc/proto"
)

// 工具对边界约束（压缩/投影共用）：
//
//	I1 工具消息必有归属：tool 消息的 tool_call_id 非空，且此前存在 assistant
//	   消息的 tool_calls 含同一 id；
//	I2 工具对不跨切点：压缩保留区起点不得是 tool 消息（否则请求历史以孤立
//	   tool 消息开头，OpenAI 兼容端点直接拒绝）；
//	I3 每个 tool_call 有结果：assistant 的每个 tool_call 都必须有对应 tool 消息。
//
// 这些约束只作用于「发往模型的投影」，事件日志仍 append-only 全量保留。

// SnapRetainStart 把压缩保留区起点向后（向更早）吸附，直到不切开工具对。
//
// retainIdx 为初算的保留区起点（[retainIdx, len) 逐字保留），base 为可压缩
// 下界（system 与既有压缩记录的上界）。返回 (newIdx, ok)：
//   - ok=false 表示无安全切点（吸附会越过 base、或历史本身无法归属），调用方
//     应放弃本次压缩，而不是产出非法历史；
//   - newIdx 落点若为 assistant(tool_calls)，则整对消息留在保留区，宁可略微
//     超出保留预算，也不切开协议配对。
//
// 同一函数同时服务两个调用形态：压力压缩算出的 retainIdx、紧急压缩算出的
// 尾部起点 end（两者语义都是「保留区第一个下标」）。
func SnapRetainStart(msgs []*proto.Message, retainIdx, base int) (int, bool) {
	if base < 0 {
		base = 0
	}
	if retainIdx <= base || retainIdx > len(msgs) {
		return 0, false
	}
	if retainIdx == len(msgs) {
		return retainIdx, true // 尾部为空：全部可压缩
	}
	m := msgs[retainIdx]
	if m == nil || m.GetRole() != "tool" {
		return retainIdx, true
	}
	id := m.GetToolCallId()
	if id == "" {
		// 无法归属的 tool 消息：历史本身不合法，不猜测切点。
		return 0, false
	}
	// 向前找最近的 assistant（允许中间夹着连续 tool 结果与 advisory notice），
	// 只有它确实拥有本次调用时才吸附——否则同样视为历史不合法。
	for i := retainIdx - 1; i > base; i-- {
		if msgs[i] == nil || msgs[i].GetRole() != "assistant" {
			continue
		}
		if hasToolCallID(msgs[i], id) {
			return i, true
		}
		return 0, false
	}
	return 0, false
}

// ValidateToolPairing 按 I1–I3 校验一条即将发往模型的消息列表。
// 返回 nil 表示配对完整；否则返回首个违约点（含下标与 id，便于排障）。
func ValidateToolPairing(msgs []*proto.Message) error {
	pending := map[string]struct{}{}  // 已声明、尚无结果的 tool_call id
	answered := map[string]struct{}{} // 已有结果的 tool_call id
	for i, m := range msgs {
		if m == nil {
			continue
		}
		switch m.GetRole() {
		case "assistant":
			for _, tc := range m.GetToolCalls() {
				id := tc.GetId()
				if id == "" {
					return fmt.Errorf("assistant message at index %d has a tool_call without id (name=%q)", i, tc.GetName())
				}
				if _, dup := answered[id]; dup {
					return fmt.Errorf("assistant message at index %d redeclares answered tool_call id %q", i, id)
				}
				pending[id] = struct{}{}
			}
		case "tool":
			id := m.GetToolCallId()
			if id == "" {
				return fmt.Errorf("tool message at index %d is missing tool_call_id", i)
			}
			if _, ok := pending[id]; !ok {
				return fmt.Errorf("orphan tool message at index %d: tool_call_id %q has no preceding assistant tool_call", i, id)
			}
			delete(pending, id)
			answered[id] = struct{}{}
		}
	}
	if len(pending) > 0 {
		ids := make([]string, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		sort.Strings(ids) // 错误信息确定性（便于测试与日志比对）
		return fmt.Errorf("assistant tool_call(s) without tool result: %s", strings.Join(ids, ", "))
	}
	return nil
}

// RepairToolPairing 在 ValidateToolPairing 失败时做最小修复，返回修复后的列表
// 与被修复项数（0 表示未改动，调用方可直接沿用原切片）：
//   - 丢弃无归属或 id 为空的 tool 消息（复制为 I1）；
//   - 剥离 assistant 上没有结果的 tool_call；若剥离后正文与工具调用皆空，
//     该 assistant 消息一并丢弃（空 assistant 无信息且部分端点拒绝）。
//
// 输入切片与其中的消息不被修改（需要改写 assistant 时重建副本），事件日志
// 因此不受影响。函数幂等：对已合法列表调用返回 n=0。
func RepairToolPairing(msgs []*proto.Message) ([]*proto.Message, int) {
	declared := map[string]struct{}{}
	keepTool := make([]bool, len(msgs))
	answered := map[string]struct{}{}
	for i, m := range msgs {
		if m == nil {
			continue
		}
		switch m.GetRole() {
		case "assistant":
			for _, tc := range m.GetToolCalls() {
				if id := tc.GetId(); id != "" {
					declared[id] = struct{}{}
				}
			}
		case "tool":
			id := m.GetToolCallId()
			if id == "" {
				continue
			}
			if _, ok := declared[id]; !ok {
				continue
			}
			keepTool[i] = true
			answered[id] = struct{}{}
		}
	}

	repaired := 0
	changed := false
	out := make([]*proto.Message, 0, len(msgs))
	for i, m := range msgs {
		if m == nil {
			continue
		}
		switch m.GetRole() {
		case "tool":
			if keepTool[i] {
				out = append(out, m)
				continue
			}
			repaired++ // 无归属或 id 为空的 tool 消息
			changed = true
		case "assistant":
			calls := m.GetToolCalls()
			if len(calls) == 0 {
				out = append(out, m)
				continue
			}
			filtered := make([]*proto.ToolCall, 0, len(calls))
			for _, tc := range calls {
				id := tc.GetId()
				if _, ok := answered[id]; ok && id != "" {
					filtered = append(filtered, tc)
					continue
				}
				repaired++ // 无结果的 tool_call（含 id 为空）
				changed = true
			}
			if len(filtered) == len(calls) {
				out = append(out, m) // 未改动：沿用原消息
				continue
			}
			if len(filtered) == 0 && strings.TrimSpace(m.GetContent()) == "" {
				continue // 空 assistant 无信息，丢弃
			}
			cp := &proto.Message{
				Role:       m.GetRole(),
				Content:    m.GetContent(),
				ToolCallId: m.GetToolCallId(),
				Images:     m.GetImages(),
			}
			cp.ToolCalls = filtered
			out = append(out, cp)
		default:
			out = append(out, m)
		}
	}
	if !changed {
		return msgs, 0
	}
	return out, repaired
}

// hasToolCallID 报告 assistant 消息的 tool_calls 是否包含指定 id。
func hasToolCallID(m *proto.Message, id string) bool {
	for _, tc := range m.GetToolCalls() {
		if tc.GetId() == id {
			return true
		}
	}
	return false
}
