package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsc/proto"
)

// buildSample 构造一个包含全部事件类型、且结束于完整轮次的会话。
func buildSample(t *testing.T) *Session {
	t.Helper()
	return buildSampleOn(t, New())
}

// buildSampleOn 在给定会话上构造完整示例事件序列（供需要预绑定 Store id 的用例复用）。
func buildSampleOn(t *testing.T, s *Session) *Session {
	t.Helper()
	appendSystemPrompt(t, s, "sys")
	s.Append(TurnStart, &TurnData{Turn: 1}, nil)
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "u1", Source: "user"})
	s.Append(StepStart, &StepData{Turn: 1, Step: 1}, nil)
	s.Append(AssistantChunk, &AssistantChunkData{Turn: 1, Step: 1, Content: "hi "}, nil)
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{
		Turn: 1, Step: 1, Content: "hi",
		ToolCalls: []*proto.ToolCall{{Id: "t1", Name: "tool-a", ArgumentsJson: `{"x":1}`}},
		Usage:     &proto.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	})
	s.Append(ToolCallEvent, &ToolCallData{Turn: 1, Step: 1, CallID: "t1", Name: "tool-a", Arguments: `{"x":1}`}, nil)
	appendSurface(t, s, ToolResult, &ToolResultData{Turn: 1, Step: 1, CallID: "t1", Content: "r1"})
	s.Append(StepEnd, &StepData{Turn: 1, Step: 1}, nil)
	appendSurface(t, s, UserMessage, &UserMessageData{Content: "u2", Source: "user"})
	s.Append(StepStart, &StepData{Turn: 1, Step: 2}, nil)
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Turn: 1, Step: 2, Content: "a2"})
	s.Append(StepEnd, &StepData{Turn: 1, Step: 2}, nil)
	s.Append(TurnEnd, &TurnData{Turn: 1, Reason: "completed"}, nil)
	return s
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	orig := buildSample(t)
	if err := orig.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	restored, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if restored == nil {
		t.Fatal("load returned nil session")
	}
	if restored.Len() != orig.Len() {
		t.Fatalf("restored len = %d, want %d", restored.Len(), orig.Len())
	}
	// 事件逐一比对（seq/time/type/data/surface）
	oe, re := orig.Events(), restored.Events()
	for i := range oe {
		if re[i].Seq != oe[i].Seq || re[i].Type != oe[i].Type {
			t.Fatalf("event %d mismatch: %+v vs %+v", i, oe[i], re[i])
		}
		if (re[i].Surface == nil) != (oe[i].Surface == nil) {
			t.Fatalf("event %d surface presence mismatch", i)
		}
		if re[i].Surface != nil && (*re[i].Surface != *oe[i].Surface) {
			t.Fatalf("event %d surface = %+v, want %+v", i, re[i].Surface, oe[i].Surface)
		}
	}
	// 派生历史一致（含工具调用）
	om, rm := orig.DeriveMessages(), restored.DeriveMessages()
	if len(om) != len(rm) {
		t.Fatalf("derived %d vs %d messages", len(om), len(rm))
	}
	for i := range om {
		if om[i].Role != rm[i].Role || om[i].Content != rm[i].Content {
			t.Fatalf("derived msg %d mismatch: %+v vs %+v", i, om[i], rm[i])
		}
		if len(om[i].ToolCalls) != len(rm[i].ToolCalls) {
			t.Fatalf("derived msg %d tool calls mismatch", i)
		}
	}
}

func TestLoadMissingFile(t *testing.T) {
	s, err := Load(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if s != nil {
		t.Fatalf("expected nil session for missing file, got %+v", s)
	}
}

func TestLoadRejectsNonContiguousSeq(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	// 手工构造断号日志
	if err := writeLines(path, `{"seq":0,"time":1,"type":"user/message","data":{"content":"u","source":"user"},"surface":{"op":"append"}}`, `{"seq":2,"time":2,"type":"user/message","data":{"content":"u2","source":"user"},"surface":{"op":"append"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for non-contiguous seq log")
	}
}

func TestLoadRejectsUnknownType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.jsonl")
	// 首条为合法事件（seq 0），第二条在 seq 1 处放置未知类型事件，
	// 以便验证诊断携带事件名、序列号与原始文件路径（而非先触发非连续校验）。
	seq := 1
	err := writeLines(path,
		`{"seq":0,"time":1,"type":"user/message","data":{"content":"u","source":"user"},"surface":{"op":"append"}}`,
		fmt.Sprintf(`{"seq":%d,"time":1,"type":"future/event","data":{},"surface":null}`, seq))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Load(path)
	if err == nil {
		t.Fatal("expected error for unknown event type")
	}
	msg := err.Error()
	if !strings.Contains(msg, "future/event") {
		t.Fatalf("diagnostic should name the unknown event type, got: %s", msg)
	}
	if !strings.Contains(msg, fmt.Sprintf("seq %d", seq)) {
		t.Fatalf("diagnostic should include the event sequence, got: %s", msg)
	}
	if !strings.Contains(msg, path) {
		t.Fatalf("diagnostic should include the raw artifact path, got: %s", msg)
	}
}

// TestLoadRestoresTodoWrite 回归：todo/write 事件可持久化并恢复。
// 曾因 unmarshalData 漏注册 TodoWrite 类型，导致任何含 todo/write 的会话
// 在恢复/导出时报 unknown event type "todo/write"，进程无法开始对话。
func TestLoadRestoresTodoWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	orig := New()
	orig.Append(TurnStart, &TurnData{Turn: 1}, nil)
	appendSurface(t, orig, UserMessage, &UserMessageData{Content: "u", Source: "user"})
	orig.Append(StepStart, &StepData{Turn: 1, Step: 1}, nil)
	orig.Append(TodoWrite, &TodoWriteData{Todos: []TodoItem{
		{Content: "任务 A", Status: TodoInProgress},
		{Content: "任务 B", Status: TodoPending},
	}}, nil)
	appendSurface(t, orig, AssistantMessage, &AssistantMessageData{Turn: 1, Step: 1, Content: "ok"})
	orig.Append(StepEnd, &StepData{Turn: 1, Step: 1}, nil)
	orig.Append(TurnEnd, &TurnData{Turn: 1, Reason: "completed"}, nil)
	if err := orig.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	restored, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if restored == nil {
		t.Fatal("load returned nil session")
	}
	if restored.Len() != orig.Len() {
		t.Fatalf("restored len = %d, want %d", restored.Len(), orig.Len())
	}
	// todo 投影一致（FoldTodos 从恢复事件重建）
	ot, rt := FoldTodos(orig.Events()), FoldTodos(restored.Events())
	if len(ot) != len(rt) {
		t.Fatalf("folded todos %d vs %d", len(ot), len(rt))
	}
	for i := range ot {
		if ot[i] != rt[i] {
			t.Fatalf("todo %d mismatch: %+v vs %+v", i, ot[i], rt[i])
		}
	}
	// 派生历史一致（todo 为 log-only，不进历史）
	om, rm := orig.DeriveMessages(), restored.DeriveMessages()
	if len(om) != len(rm) {
		t.Fatalf("derived %d vs %d messages", len(om), len(rm))
	}
}

// TestLoadRestoresHistoryLimit 回归：history/limit 事件可持久化并恢复。
// 曾因 unmarshalData 漏注册 HistoryLimit 类型，导致任何含 history/limit 的会话
// 第二次启动恢复时（加载历史注入设置）报 unknown event type "history/limit"。
func TestLoadRestoresHistoryLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	orig := New()
	orig.Append(TurnStart, &TurnData{Turn: 1}, nil)
	appendSurface(t, orig, UserMessage, &UserMessageData{Content: "u", Source: "user"})
	orig.Append(HistoryLimit, &HistoryLimitData{Count: 4}, nil)
	appendSurface(t, orig, AssistantMessage, &AssistantMessageData{Turn: 1, Step: 1, Content: "ok"})
	orig.Append(TurnEnd, &TurnData{Turn: 1, Reason: "completed"}, nil)
	if err := orig.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	restored, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if restored == nil {
		t.Fatal("load returned nil session")
	}
	if restored.Len() != orig.Len() {
		t.Fatalf("restored len = %d, want %d", restored.Len(), orig.Len())
	}
	// history/limit 折叠一致（从恢复事件重建，最后一条生效）
	ol, of := FoldHistoryLimit(orig.Events())
	rl, rf := FoldHistoryLimit(restored.Events())
	if of != rf || ol != rl {
		t.Fatalf("folded history limit %d/%v vs %d/%v", ol, of, rl, rf)
	}
	// 派生历史一致（history/limit 为 log-only，不进历史）
	om, rm := orig.DeriveMessages(), restored.DeriveMessages()
	if len(om) != len(rm) {
		t.Fatalf("derived %d vs %d messages", len(om), len(rm))
	}
}

func TestForkCopiesPrefix(t *testing.T) {
	orig := buildSample(t)
	// boundary = 最后一个事件（完整轮次前缀）→ 可 fork
	child, err := orig.Fork(orig.Len() - 1)
	if err != nil {
		t.Fatalf("fork: %v", err)
	}
	if child.Len() != orig.Len() {
		t.Fatalf("child len = %d, want %d", child.Len(), orig.Len())
	}
	// 子会话独立追加不影响父会话
	appendSurface(t, child, UserMessage, &UserMessageData{Content: "child-msg", Source: "user"})
	if child.Len() != orig.Len()+1 || orig.Len() != buildSample(t).Len() {
		t.Fatalf("child/orig lens = %d/%d after append, want %d/%d",
			child.Len(), orig.Len(), buildSample(t).Len()+1, buildSample(t).Len())
	}
}

func TestForkRejectsOpenTurn(t *testing.T) {
	orig := buildSample(t)
	// boundary = 8（user u2，轮次仍开放）→ 拒绝
	if _, err := orig.Fork(8); err == nil {
		t.Fatal("expected error for fork ending inside an open turn")
	}
}

func TestForkRejectsOutOfRange(t *testing.T) {
	orig := buildSample(t)
	if _, err := orig.Fork(orig.Len()); err == nil {
		t.Fatal("expected error for out-of-range boundary")
	}
}

// TestStoreForkAssignsIDAndPersists 回归：Store.Fork 必须给子会话分配新 id 并
// 立即落盘，使 fork 结果可直接 Load/继续使用（Session.Fork 自身不携带 id）。
func TestStoreForkAssignsIDAndPersists(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	parent, err := store.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	buildSampleOn(t, parent)
	if err := store.Save(parent); err != nil {
		t.Fatalf("save parent: %v", err)
	}

	child, err := store.Fork(parent.id, parent.Len()-1)
	if err != nil {
		t.Fatalf("Store.Fork: %v", err)
	}
	if child.id == "" || child.id == parent.id {
		t.Fatalf("child id = %q, want a fresh non-empty id", child.id)
	}
	if child.Len() != parent.Len() {
		t.Fatalf("child len = %d, want %d", child.Len(), parent.Len())
	}

	loaded, err := store.Load(child.id)
	if err != nil {
		t.Fatalf("Load child: %v", err)
	}
	if loaded == nil || loaded.Len() != child.Len() {
		t.Fatalf("forked session not persisted: %+v", loaded)
	}
	infos, err := store.List()
	if err != nil || len(infos) != 2 {
		t.Fatalf("List = %v, %v; want 2 sessions", infos, err)
	}
}

func TestLastTurn(t *testing.T) {
	orig := buildSample(t)
	if got := orig.LastTurn(); got != 1 {
		t.Fatalf("LastTurn = %d, want 1", got)
	}
	empty := New()
	if got := empty.LastTurn(); got != 0 {
		t.Fatalf("empty LastTurn = %d, want 0", got)
	}
}

func writeLines(path string, lines ...string) error {
	return os.WriteFile(path, []byte(joinLines(lines...)), 0644)
}

func joinLines(lines ...string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out + "\n"
}

// requireContiguousSeq 断言恢复出的日志 seq 严格连续（seq == 下标）。
func requireContiguousSeq(t *testing.T, s *Session) {
	t.Helper()
	for i, ev := range s.Events() {
		if ev.Seq != i {
			t.Fatalf("event %d has seq %d, want %d", i, ev.Seq, i)
		}
	}
}

// TestSaveAppendsOnlyNewEvents 快路径：首次保存后追加事件再保存，只在新尾部
// 追加行（原文件未被替换，inode 不变），文件只增长且 Load 得到全部连续事件。
func TestSaveAppendsOnlyNewEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	s := buildSample(t)
	if err := s.Save(path); err != nil {
		t.Fatalf("first save: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	appendSurface(t, s, UserMessage, &UserMessageData{Content: "u3", Source: "user"})
	appendSurface(t, s, AssistantMessage, &AssistantMessageData{Turn: 2, Step: 1, Content: "a3"})
	if err := s.Save(path); err != nil {
		t.Fatalf("append save: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("expected in-place append, but file was rewritten")
	}
	if afterInfo.Size() <= beforeInfo.Size() {
		t.Fatalf("file size %d did not grow from %d", afterInfo.Size(), beforeInfo.Size())
	}
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("incremental append altered existing content")
	}

	restored, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if restored.Len() != s.Len() {
		t.Fatalf("restored len = %d, want %d", restored.Len(), s.Len())
	}
	requireContiguousSeq(t, restored)
	tail := restored.Events()[restored.Len()-1]
	if d, ok := tail.Data.(*AssistantMessageData); !ok || d.Content != "a3" {
		t.Fatalf("last restored event = %+v, want assistant message a3", tail)
	}
}

// TestSaveNoopWhenUnchanged 无新事件时重复保存为 no-op：文件不被重写、大小不变。
func TestSaveNoopWhenUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	s := buildSample(t)
	if err := s.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	first, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := s.Save(path); err != nil {
		t.Fatalf("second save: %v", err)
	}
	second, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after second save: %v", err)
	}
	if second.Size() != first.Size() {
		t.Fatalf("size changed on no-op save: %d -> %d", first.Size(), second.Size())
	}
	if !os.SameFile(first, second) {
		t.Fatal("no-op save rewrote the file")
	}
	recontent, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(recontent) != string(content) {
		t.Fatal("no-op save changed file content")
	}
}

// TestSaveFallsBackOnExternalModification 外部改动文件（这里是截短为合法前缀）
// 后保存：游标校验失败，自动回退全量原子重写，Load 得到当前完整日志。
func TestSaveFallsBackOnExternalModification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	s := buildSample(t)
	if err := s.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// 外部把文件截成只剩首行（仍是合法 JSONL，但已不是本会话的落盘结果）
	firstLine := strings.SplitN(string(content), "\n", 2)[0] + "\n"
	if err := os.WriteFile(path, []byte(firstLine), 0644); err != nil {
		t.Fatalf("external modify: %v", err)
	}

	appendSurface(t, s, UserMessage, &UserMessageData{Content: "after-external", Source: "user"})
	if err := s.Save(path); err != nil {
		t.Fatalf("save after external modification: %v", err)
	}

	restored, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if restored.Len() != s.Len() {
		t.Fatalf("restored len = %d, want %d (full rewrite expected)", restored.Len(), s.Len())
	}
	requireContiguousSeq(t, restored)
}

// TestSaveRecoversFromHalfLine 模拟崩溃留下的半行：把文件截断到非换行位置后
// 保存，尾部校验失败 → 回退全量重写，Load 成功且事件完整。
func TestSaveRecoversFromHalfLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	s := buildSample(t)
	if err := s.Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Truncate(path, fi.Size()-3); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if err := s.Save(path); err != nil {
		t.Fatalf("save after partial line: %v", err)
	}

	restored, err := Load(path)
	if err != nil {
		t.Fatalf("load after recovery: %v", err)
	}
	if restored.Len() != s.Len() {
		t.Fatalf("restored len = %d, want %d", restored.Len(), s.Len())
	}
	requireContiguousSeq(t, restored)
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final: %v", err)
	}
	if !strings.HasSuffix(string(final), "\n") {
		t.Fatal("rewritten log should end with a newline")
	}
}

// TestStoreSaveRepeatedly 经 Store 连续多轮保存：每轮增量追加后 Load 均可完整
// 恢复（seq 连续、事件数一致）。
func TestStoreSaveRepeatedly(t *testing.T) {
	st, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	sess, err := st.Create()
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for i := 0; i < 5; i++ {
		appendSurface(t, sess, UserMessage, &UserMessageData{Content: fmt.Sprintf("u%d", i), Source: "user"})
		appendSurface(t, sess, AssistantMessage, &AssistantMessageData{Turn: i + 1, Step: 1, Content: fmt.Sprintf("a%d", i)})
		if err := st.Save(sess); err != nil {
			t.Fatalf("save round %d: %v", i, err)
		}
		loaded, err := st.Load(sess.id)
		if err != nil {
			t.Fatalf("load round %d: %v", i, err)
		}
		if loaded == nil || loaded.Len() != sess.Len() {
			t.Fatalf("round %d: loaded len = %v, want %d", i, loaded, sess.Len())
		}
		requireContiguousSeq(t, loaded)
	}
}
