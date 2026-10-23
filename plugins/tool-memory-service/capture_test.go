package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAllTodosCompleted 只有「清单非空且全部 completed」才算收尾：
// 空清单、仍有 pending/in_progress 都不触发写记忆请求。
func TestAllTodosCompleted(t *testing.T) {
	yes := []string{
		`{"todos":[{"content":"a","status":"completed"}]}`,
		`{"todos":[{"status":"completed"},{"status":"completed"}]}`,
	}
	for _, s := range yes {
		if !allTodosCompleted([]byte(s)) {
			t.Errorf("应判定为全部完成: %s", s)
		}
	}
	no := []string{
		`{"todos":[]}`,
		`{"todos":[{"status":"completed"},{"status":"pending"}]}`,
		`{"todos":[{"status":"completed"},{"status":"in_progress"}]}`,
		`not json`,
		`{}`,
	}
	for _, s := range no {
		if allTodosCompleted([]byte(s)) {
			t.Errorf("不应判定为全部完成: %s", s)
		}
	}
}

// TestGoalCompleted 只有 Snapshot.Phase == complete 才触发（pause/blocked 不算）。
func TestGoalCompleted(t *testing.T) {
	if !goalCompleted([]byte(`{"Snapshot":{"Phase":"complete"},"Deleted":false}`)) {
		t.Error("complete 应触发")
	}
	for _, s := range []string{
		`{"Snapshot":{"Phase":"active"}}`,
		`{"Snapshot":{"Phase":"blocked"}}`,
		`{"Deleted":true}`,
		`{}`,
		`oops`,
	} {
		if goalCompleted([]byte(s)) {
			t.Errorf("不应触发: %s", s)
		}
	}
}

// TestSessionTailReadsOnlyNewLines 日志跟随只取新增完整行：同一行不得重复处理
// （否则同一段推理会被反复入库），半截行留到下次。
func TestSessionTailReadsOnlyNewLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.jsonl")
	if err := os.WriteFile(p, []byte("line1\nline2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var tl sessionTail
	got := tl.readNew(p)
	if len(got) != 2 || string(got[0]) != "line1" || string(got[1]) != "line2" {
		t.Fatalf("首次应读到两行，got %q", got)
	}
	if again := tl.readNew(p); len(again) != 0 {
		t.Fatalf("无新增时不应返回数据，got %q", again)
	}
	// 追加半行：不应被当作完整事件，追加换行后才交付
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("line3")
	_ = f.Close()
	if got := tl.readNew(p); len(got) != 0 {
		t.Fatalf("半截行不应交付，got %q", got)
	}
	f, _ = os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	_, _ = f.WriteString("\n")
	_ = f.Close()
	if got := tl.readNew(p); len(got) != 1 || string(got[0]) != "line3" {
		t.Fatalf("补完换行后应交付 line3，got %q", got)
	}
	// 文件被截断（新会话复写）→ 从头读
	if err := os.WriteFile(p, []byte("fresh\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := tl.readNew(p); len(got) != 1 || string(got[0]) != "fresh" {
		t.Fatalf("截断后应从头读，got %q", got)
	}
}

// TestTakeTaskRecordNoticeOnce 同一任务只插播一次（待办完成与目标完成常先后发生）：
// 通知是「取走即清空」的一次性插播，不随每步重复。
func TestTakeTaskRecordNoticeOnce(t *testing.T) {
	clearTaskRecord()
	if got := takeTaskRecordNotice(); got != "" {
		t.Fatalf("初始不应有插播，got %q", got)
	}
	requestTaskRecord("待办清单已全部完成")
	first := takeTaskRecordNotice()
	if first == "" {
		t.Fatal("应产生插播")
	}
	if again := takeTaskRecordNotice(); again != "" {
		t.Fatalf("插播取走后不应重复，got %q", again)
	}
	// 同一任务再次登记：已被取走，不应复活（任务已完成且已提醒过）
	requestTaskRecord("目标已标记完成")
	second := takeTaskRecordNotice()
	if second == "" {
		t.Fatal("新登记应可再次插播")
	}
	// 文案只提「记忆能力」，不点名任何插件/工具
	for _, text := range []string{first, second} {
		for _, banned := range []string{"memory_add", "memory-service", "tool-memory"} {
			if strings.Contains(text, banned) {
				t.Fatalf("插播不应点名具体实现 %q: %s", banned, text)
			}
		}
	}
	clearTaskRecord()
	if got := takeTaskRecordNotice(); got != "" {
		t.Fatal("清除后不应再有插播")
	}
}

// TestProcessSessionEventRoutes 事件分派：推理入库、待办/目标完成登记、用户输入清空。
func TestProcessSessionEventRoutes(t *testing.T) {
	clearTaskRecord()
	processSessionEvent([]byte(`{"type":"todo/write","data":{"todos":[{"status":"completed"}]}}`))
	if takeTaskRecordNotice() == "" {
		t.Fatal("待办全部完成应登记插播")
	}
	processSessionEvent([]byte(`{"type":"user/message","data":{"Content":"next"}}`))
	if got := takeTaskRecordNotice(); got != "" {
		t.Fatal("用户新输入应清掉上一任务的插播")
	}
	processSessionEvent([]byte(`{"type":"goal/change","data":{"Snapshot":{"Phase":"complete"}}}`))
	// 用户输入事件已清空，此处应重新登记
	if takeTaskRecordNotice() == "" {
		t.Fatal("目标完成应登记插播")
	}
	clearTaskRecord()
}

// TestTaskNoticePayloadShape 插播载荷形状：{"append":[{"role":"system","content":…}]}。
// 宿主按这个约定把它追加到本轮消息末尾（不改 system prompt）。
func TestTaskNoticePayloadShape(t *testing.T) {
	var payload struct {
		Append []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"append"`
	}
	if err := json.Unmarshal([]byte(taskNoticePayload("task done")), &payload); err != nil {
		t.Fatalf("载荷应可解析: %v", err)
	}
	if len(payload.Append) != 1 || payload.Append[0].Role != "system" || payload.Append[0].Content != "task done" {
		t.Fatalf("载荷不符: %+v", payload)
	}
}
