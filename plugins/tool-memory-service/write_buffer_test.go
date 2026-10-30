package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

// 本文件覆盖「记忆写入不得阻塞 agent/pre-step」的异步落盘改造。
//
// 背景：被动记录推理发生在 pre-step——每次 LLM 请求发出之前。原地做 SQLite 写入等于给
// 每个请求加固定延迟（WAL 化前实测 8.3ms/条、之后 0.8ms），而这条路径不需要任何返回值
// 给模型，完全可以推迟。现在 pre-step 只入队，后台协程按批（单事务）落盘。

// openTestDB 准备一个空的记忆目录并返回统计辅助（句柄按项目键取）。
func openTestDB(t *testing.T) {
	t.Helper()
	setupMemoryDir(t)
}

// testProject 是写入类测试固定的归属键（flush 分组依据）。
const testProject = "proj"

// countMemories 汇总所有项目库里的条数（分库之后「一共有几条」要跨库数）。
func countMemories(t *testing.T) int64 {
	t.Helper()
	keys, err := projectDBKeys()
	if err != nil {
		t.Fatalf("list dbs: %v", err)
	}
	var n int64
	for _, key := range keys {
		var c int64
		if err := dbFor(t, key).Model(&Memory{}).Count(&c).Error; err != nil {
			t.Fatalf("count: %v", err)
		}
		n += c
	}
	return n
}

// TestWALEnabled WAL + synchronous=NORMAL 是「同步写也不贵」的前提（实测 8.3ms → 0.8ms），
// 掉回默认回滚日志模式会让后续异步落盘的每批提交都变慢，故钉住。
func TestWALEnabled(t *testing.T) {
	openTestDB(t)
	var mode string
	if err := dbFor(t, testProject).Raw("PRAGMA journal_mode").Scan(&mode).Error; err != nil {
		t.Fatalf("读 journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, 期望 wal（否则每条写入都要 fsync）", mode)
	}
}

// TestReasoningEnqueueDoesNotWrite pre-step 路径（recordReasoning）不得同步落盘：
// 入队后数据库里仍然没有这条记录，只有 flush 之后才出现。
func TestReasoningEnqueueDoesNotWrite(t *testing.T) {
	openTestDB(t)

	recordReasoning("这是一段只在队列里的推理内容")
	if got := countMemories(t); got != 0 {
		t.Fatalf("pre-step 路径不应同步写库，实际已有 %d 条", got)
	}

	flushPendingMemories()
	if got := countMemories(t); got != 1 {
		t.Fatalf("flush 后应有 1 条，实际 %d 条", got)
	}
	var mem Memory
	if err := dbFor(t, currentProjectKey()).Where("source = ?", "reasoning").First(&mem).Error; err != nil {
		t.Fatalf("应落盘 source=reasoning 的记录: %v", err)
	}
	if mem.Content != "这是一段只在队列里的推理内容" {
		t.Fatalf("内容不符: %q", mem.Content)
	}
}

// TestFlushBatchesAndDedups 一批里重复的内容只落一条（去重查询在同一事务内可见），
// 且 flush 之后队列清空（重复调用不重复写）。
func TestFlushBatchesAndDedups(t *testing.T) {
	openTestDB(t)

	enqueueMemory("同一条结论", "reasoning", testProject)
	enqueueMemory("同一条结论", "reasoning", testProject)
	enqueueMemory("另一条结论", "reasoning", testProject)
	flushPendingMemories()

	if got := countMemories(t); got != 2 {
		t.Fatalf("3 条入队（其中 1 条重复）应落 2 条，实际 %d 条", got)
	}
	flushPendingMemories() // 幂等：队列已空
	if got := countMemories(t); got != 2 {
		t.Fatalf("重复 flush 不应重复写入，实际 %d 条", got)
	}
}

// TestEnqueueFlushesAtBatchThreshold 攒够一批立即落盘，不必等定时器。
func TestEnqueueFlushesAtBatchThreshold(t *testing.T) {
	openTestDB(t)

	for i := 0; i < memoryFlushBatch; i++ {
		enqueueMemory(fmt.Sprintf("批量条目 %d", i), "reasoning", testProject)
	}
	if got := countMemories(t); got != int64(memoryFlushBatch) {
		t.Fatalf("达到批量阈值应立即落盘 %d 条，实际 %d 条", memoryFlushBatch, got)
	}
}

// TestReadToolFlushesPending 读工具入口先落盘：模型「刚写就搜」不会读出空。
func TestReadToolFlushesPending(t *testing.T) {
	openTestDB(t)

	enqueueMemory("写入后立刻检索的标记词 zzzmarker", "reasoning", testProject)
	if got := countMemories(t); got != 0 {
		t.Fatalf("前置条件：此时应仍在队列里，实际已落 %d 条", got)
	}

	args, _ := json.Marshal(map[string]string{"query": "zzzmarker", "scope": "all"})
	out, err := handleSearch(t.Context(), args)
	if err != nil {
		t.Fatalf("memory_search: %v", err)
	}
	if !containsFold(out, "zzzmarker") {
		t.Fatalf("读工具应先落盘待写队列，否则刚写的内容搜不到: %s", out)
	}
}

// resetPendingMemories 清空待写队列（测试隔离；生产不调用）。
func resetPendingMemories() {
	pendingWrites.Lock()
	pendingWrites.items = nil
	pendingWrites.Unlock()
	seenReasoning.Lock()
	seenReasoning.m = map[string]bool{}
	seenReasoning.Unlock()
}

func containsFold(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if equalFoldASCII(s[i:i+len(sub)], sub) {
				return true
			}
		}
		return false
	})()
}

func equalFoldASCII(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
