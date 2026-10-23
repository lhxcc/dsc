package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	dsc "dsc-sdk"
)

// 记忆采集策略（对齐「只留有用信息」的目标）：
//
//  1. 被动记录模型的**推理内容**（正式回答前那段思考，TUI 以暗色呈现）——它才是
//     过程中真正有复用价值的东西；工具结果、用户消息等噪声一律不入库。
//  2. 任务收尾时**主动请求模型写一次记忆**：待办清单全部完成，或目标被标记完成。
//     两者属同一任务时只请求一次。
//
// 数据来源是宿主规范会话日志（sessions/<id>.jsonl，事件溯源）：其中 assistant/message
// 带 Reasoning 字段，todo/write 与 goal/change 带完成状态。插件按文件增量跟随，
// 不重复解析历史。
//
// 请求文案只描述「记忆能力」，不点名任何插件或工具名——谁提供记忆能力谁承接。

// seenReasoning 已入库推理的内容哈希（进程内去重：日志补写/重读会再次看到同一段）。
var seenReasoning = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// taskRecord 任务收尾的「请写记忆」请求状态。
var taskRecord struct {
	sync.Mutex
	reason string // 非空 = 待请求（值用于向模型说明触发原因）
}

// sessionTail 增量跟随当前会话事件日志，只返回上次之后的新完整行。
type sessionTail struct {
	mu     sync.Mutex
	path   string
	offset int64
}

var tail sessionTail

// readNew 返回 path 中自上次读取以来的新增完整行（不完整的尾行留待下次）。
// 路径变化或文件被截断（新会话/重建）时从头读。
func (t *sessionTail) readNew(path string) [][]byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.path != path {
		t.path, t.offset = path, 0
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	if fi.Size() < t.offset {
		t.offset = 0
	}
	if fi.Size() == t.offset {
		return nil
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b != '\n' {
			continue
		}
		if i > start {
			lines = append(lines, data[start:i])
		}
		start = i + 1
	}
	t.offset += int64(start) // 只推进到最后一个完整行的末尾
	return lines
}

// capturePreStep 处理 agent/pre-step：跟随会话日志，被动记录推理，并在任务收尾
// 时登记一次「请模型写记忆」。
func capturePreStep(dataJSON string) {
	var ev struct {
		Session   string `json:"session"`
		UserInput bool   `json:"user_input"`
	}
	if err := json.Unmarshal([]byte(dataJSON), &ev); err != nil {
		return
	}
	// 新一轮用户输入 = 新任务开始：清掉上一轮未兑现的请求
	if ev.UserInput {
		clearTaskRecord()
	}
	if ev.Session == "" {
		return
	}
	path := dsc.PJoin(dsc.SessionDir(), ev.Session+dsc.SessionFileSuffix)
	for _, line := range tail.readNew(path) {
		processSessionEvent(line)
	}
}

// processSessionEvent 解析单条会话事件并分派到对应处理。
func processSessionEvent(line []byte) {
	var e struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return
	}
	switch e.Type {
	case "assistant/message":
		var d struct {
			Reasoning string `json:"Reasoning"`
		}
		if json.Unmarshal(e.Data, &d) == nil {
			recordReasoning(d.Reasoning)
		}
	case "todo/write":
		if allTodosCompleted(e.Data) {
			requestTaskRecord("待办清单已全部完成")
		}
	case "goal/change":
		if goalCompleted(e.Data) {
			requestTaskRecord("目标已标记完成")
		}
	case "user/message":
		// 用户新输入：上一任务的收尾请求作废（模型没写就算了，不跨任务重复催）
		clearTaskRecord()
	}
}

// allTodosCompleted 判断 todo/write 是否表示「清单非空且全部完成」。
func allTodosCompleted(data json.RawMessage) bool {
	var d struct {
		Todos []struct {
			Status string `json:"status"`
		} `json:"todos"`
	}
	if json.Unmarshal(data, &d) != nil || len(d.Todos) == 0 {
		return false
	}
	for _, t := range d.Todos {
		if t.Status != "completed" {
			return false
		}
	}
	return true
}

// goalCompleted 判断 goal/change 是否把目标推进到 complete 阶段。
func goalCompleted(data json.RawMessage) bool {
	var d struct {
		Snapshot *struct {
			Phase string `json:"Phase"`
		} `json:"Snapshot"`
	}
	if json.Unmarshal(data, &d) != nil || d.Snapshot == nil {
		return false
	}
	return d.Snapshot.Phase == "complete"
}

// recordReasoning 把模型推理完整写入记忆库。
// 只做内容去重（同一段推理可能因日志重读被再次看到），不截断、不按长度丢弃——
// 推理的价值正在细节里，砍半条比不记更糟。
func recordReasoning(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	sum := sha256.Sum256([]byte(text))
	key := hex.EncodeToString(sum[:8])
	seenReasoning.Lock()
	if seenReasoning.m[key] {
		seenReasoning.Unlock()
		return
	}
	seenReasoning.m[key] = true
	seenReasoning.Unlock()

	// 推理按当前项目归属：换项目后不该在另一个项目的记忆里翻到这些细节
	if _, _, err := addMemory(text, "reasoning", currentProjectKey()); err != nil {
		fmt.Fprintf(os.Stderr, "memory-service: record reasoning failed: %v\n", err)
	}
}

// requestTaskRecord 登记一次「请模型写记忆」的请求；同一任务只登记一次
// （待办完成与目标完成常先后发生，属同一任务）。
func requestTaskRecord(reason string) {
	taskRecord.Lock()
	defer taskRecord.Unlock()
	if taskRecord.reason != "" {
		return
	}
	taskRecord.reason = reason
}

// clearTaskRecord 作废当前请求（已写入记忆 / 任务切换）。
func clearTaskRecord() {
	taskRecord.Lock()
	taskRecord.reason = ""
	taskRecord.Unlock()
}

// taskNoticePayload 构造 pre-step 的插播载荷：一条 role=system 的消息，由宿主
// 追加到本轮消息末尾（见 core 的 parsePreStepAppends）。
func taskNoticePayload(notice string) string {
	b, err := json.Marshal(map[string]any{
		"append": []map[string]string{{"role": "system", "content": notice}},
	})
	if err != nil {
		return ""
	}
	return string(b)
}

// takeTaskRecordNotice 取走待发的一次性插播文案（取走即清空）。
//
// 为什么走消息而不是 system prompt：系统提示是缓存前缀，改它会让整段前缀失效；
// 插播只追加在消息末尾，代价小得多，也符合「临时提醒」的性质。取走即清空，
// 避免每步重复插播把历史塞满。
func takeTaskRecordNotice() string {
	taskRecord.Lock()
	reason := taskRecord.reason
	taskRecord.reason = ""
	taskRecord.Unlock()
	if reason == "" {
		return ""
	}
	return "【记忆】" + reason + "，本次任务已收尾。请使用当前可用的记忆能力保存一次值得长期留存的内容：" +
		"关键事实、结论、以及可复用的经验教训（不要复述过程、不要粘贴大段代码）。写完后无需再写第二次。"
}
