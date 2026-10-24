package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// 事件日志的 JSONL 持久化与恢复。
//
// 每行一个事件（JSON 编码），seq 连续，可直接按行回放重建 Session
// （含 surface 投影与 replaceGeneration）。持久化后端的存储编码是自身的
// 实现细节，只要 load 返回与追加时一致的事件即可——JSONL 是这种编码。
//
// 不做版本头与迁移：本项目高速迭代、无向后兼容（AGENTS.md §4），日志格式
// 即"当前版本"行为，旧版本文件直接当垃圾丢弃，由用户重启会话。

// eventJSON 事件的磁盘表示：data 为按 type 判别的内联对象。
type eventJSON struct {
	Seq     int             `json:"seq"`
	Time    int64           `json:"time"`
	Type    EventType       `json:"type"`
	Data    json.RawMessage `json:"data"`
	Surface *SurfaceOp      `json:"surface,omitempty"`
}

// marshalEvent 将事件编码为一行 JSON（不含换行符）。
func marshalEvent(ev *Event) ([]byte, error) {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return nil, fmt.Errorf("session: marshal event %q data: %w", ev.Type, err)
	}
	return json.Marshal(eventJSON{
		Seq:     ev.Seq,
		Time:    ev.Time,
		Type:    ev.Type,
		Data:    data,
		Surface: ev.Surface,
	})
}

// unmarshalData 按事件类型重建 payload（判别联合的反序列化）。
// 未知类型返回错误——与 DSH 一致：读到不认识的必需事件必须拒绝重建，
// 而非静默丢弃（未知事件可能改变日志的其余解释）。
func unmarshalData(typ EventType, raw json.RawMessage) (any, error) {
	decode := func(out any) error { return json.Unmarshal(raw, out) }
	switch typ {
	case TurnStart:
		var d TurnData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case TurnEnd:
		var d TurnData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case StepStart:
		var d StepData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case StepEnd:
		var d StepData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case UserMessage:
		var d UserMessageData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case AssistantChunk:
		var d AssistantChunkData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case AssistantMessage:
		var d AssistantMessageData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case ToolCallEvent:
		var d ToolCallData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case ToolResult:
		var d ToolResultData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case CompactionSummary:
		var d CompactionSummaryData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case PlanMode:
		var d PlanModeData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case GoalChange:
		var d GoalChangeData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case TodoWrite:
		var d TodoWriteData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case HistoryLimit:
		var d HistoryLimitData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case ApprovalAsked:
		var d ApprovalAskedData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case ApprovalDecided:
		var d ApprovalDecidedData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case ApprovalPolicy:
		var d ApprovalPolicyData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case LLMAttempt:
		var d LLMAttemptData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	case SystemMessage:
		var d SystemMessageData
		if err := decode(&d); err != nil {
			return nil, err
		}
		return &d, nil
	default:
		// 未知类型：DSH fail-closed 守则——拒绝重建而非静默丢弃。日志里出现
		// 本构建不认识的事件，通常由更新的运行时所写；由调用方（Load）补上
		// 事件序列号与原始文件路径后一并上报。
		return nil, fmt.Errorf("unknown event type %q (likely written by a newer DSC runtime)", typ)
	}
}

// Save 将事件日志写入 JSONL 文件。
//
// 事件日志 append-only，故优先走增量快路径：仅当目标文件仍是上次落盘的结果
// （路径相同、大小等于 savedSize、末尾是换行）时，以 O_APPEND 追加
// events[savedCount:]；无新事件则为 no-op。任一校验不符（路径不同、文件缺失
// 或被外部改动、大小/尾字节不符、savedCount 超出日志长度）→ 回退全量原子
// 重写（先写临时文件再重命名，避免崩溃留下半截文件），成功后重置游标。
func (s *Session) Save(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.appendableLocked(path) {
		if s.savedCount == len(s.events) {
			return nil
		}
		return s.appendLocked(path)
	}
	return s.rewriteLocked(path)
}

// appendableLocked 判断能否安全增量追加：仅当 path 仍是上次成功落盘的文件
// （未被外部替换/截断/追加）且日志只增不减时成立。需已持有 s.mu。
func (s *Session) appendableLocked(path string) bool {
	if s.savedPath != path || s.savedCount > len(s.events) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.Size() != s.savedSize {
		return false
	}
	if fi.Size() == 0 {
		return true // 空文件不存在半行，可直接追加
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], fi.Size()-1); err != nil {
		return false
	}
	return last[0] == '\n'
}

// appendLocked 将 events[savedCount:] 追加到 path 并 fsync。需已持有 s.mu。
// 写入失败时不动游标——文件可能已部分增长，下次 Save 会因大小不符回退全量。
func (s *Session) appendLocked(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("session: open append %s: %w", path, err)
	}
	w := bufio.NewWriter(f)
	var size int64
	for _, ev := range s.events[s.savedCount:] {
		b, err := marshalEvent(ev)
		if err != nil {
			_ = f.Close()
			return err
		}
		line := append(b, '\n')
		if _, err := w.Write(line); err != nil {
			_ = f.Close()
			return fmt.Errorf("session: append %s: %w", path, err)
		}
		size += int64(len(line))
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("session: flush %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("session: sync %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("session: close %s: %w", path, err)
	}
	s.savedCount = len(s.events)
	s.savedSize += size
	return nil
}

// rewriteLocked 全量原子重写事件日志（临时文件 + rename），成功后重置游标。
// 需已持有 s.mu。
func (s *Session) rewriteLocked(path string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("session: create %s: %w", tmp, err)
	}
	w := bufio.NewWriter(f)
	var size int64
	for _, ev := range s.events {
		b, err := marshalEvent(ev)
		if err != nil {
			_ = f.Close()
			return err
		}
		if _, err := w.Write(append(b, '\n')); err != nil {
			_ = f.Close()
			return fmt.Errorf("session: write %s: %w", tmp, err)
		}
		size += int64(len(b)) + 1
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("session: flush %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("session: close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("session: rename %s -> %s: %w", tmp, path, err)
	}
	s.savedPath = path
	s.savedCount = len(s.events)
	s.savedSize = size
	return nil
}

// Load 从 JSONL 文件恢复会话；文件不存在返回 (nil, nil)。
// 校验 seq 严格连续（seq == 下标），否则拒绝重建。
// 损坏行直接报错而非静默跳过——fail-closed 守则（对齐 DSH）：日志里出现
// 本构建无法重建的事件，应让上层决定如何处理（备份/报警），绝不让坏事件
// 静默丢失。
func Load(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("session: read %s: %w", path, err)
	}
	s := New()
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // 容忍大行（chunk/工具结果）
	line := 0
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			continue
		}
		var ej eventJSON
		if err := json.Unmarshal([]byte(text), &ej); err != nil {
			return nil, fmt.Errorf("session: line %d: %w", line, err)
		}
		if ej.Seq != len(s.events) {
			return nil, fmt.Errorf("session: %s: event seq %d, want %d (non-contiguous log)", path, ej.Seq, len(s.events))
		}
		data, err := unmarshalData(ej.Type, ej.Data)
		if err != nil {
			return nil, fmt.Errorf("session: %s: event seq %d: %w", path, ej.Seq, err)
		}
		ev := &Event{Seq: ej.Seq, Time: ej.Time, Type: ej.Type, Data: data, Surface: ej.Surface}
		if ev.Surface != nil {
			if !surfaceEventTypes[ev.Type] {
				return nil, fmt.Errorf("session: %s: event seq %d: non-surface event %q carries surface op", path, ev.Seq, ev.Type)
			}
			s.applySurfaceLocked(ev)
		}
		s.events = append(s.events, ev)
		line++
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("session: scan %s: %w", path, err)
	}
	// 记录落点游标：恢复出的会话与磁盘文件严格一致，后续 Save 可直接增量追加。
	s.savedPath = path
	s.savedCount = len(s.events)
	s.savedSize = int64(len(data))
	// 标记「历史到此为止」：从文件读进来的是历史，之后 append 的是本次运行。
	// 历史注入上限据此划界（见 DeriveMessagesLimited）。
	s.restoredThrough = len(s.events)
	return s, nil
}
