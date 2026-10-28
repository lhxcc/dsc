package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	dsc "dsc-sdk"
)

// 状态文件：<session>.dsc-system.json（与会话存档同目录，用插件名后缀区分）。
//
// 这里存的是**摘要缓存 + 审计记录**，与会话日志没有关系：
//   - 不改写、不遮蔽真实的会话历史（那需要往 session 日志里写 surface 事件——本插件的
//     pre-step 改写只作用于本次请求，会话日志一字不改）；
//   - 因此丢了这个文件、或它被改坏，后果只是「下次重新摘要一次」，模型看到的历史不变；
//   - 滑窗的不可逆性讨论也不受影响：滑窗丢的只是本次请求的视图，会话日志与这份缓存都不碰。
//
// 为什么还要落盘：不落盘的话，进程重启后进程内记录清空，第一次请求要**重新调用一次
// LLM** 把同一段前缀摘要一遍（云端就是白花的钱与几十秒延迟）。落盘后指纹仍吻合即复用。
//
// 目录由 main 装配时注入（测试因此不会写进用户的 sessions/）；nil store = 纯内存模式。
type compactionStore struct {
	dir string

	// warnOnce 让写失败只报一次：压缩本身是尽力而为，失败只影响「重启要不要重压」，
	// 不该每步刷日志，也不该让请求失败。
	warnOnce sync.Once
}

// compactionStateSchema 是状态文件的格式版本。AGENTS.md §4：不写逐字段迁移——
// 版本不符即整体丢弃（等同「从未压缩过」），而不是把语义已经变了的数据读进来。
const compactionStateSchema = 1

// compactionStateFile 是磁盘格式。Session 字段用于防串档：文件名经过字符替换，
// 理论上存在碰撞（不同 id 映射到同一文件名），内容里的 id 一比对就能发现。
type compactionStateFile struct {
	Schema  int                `json:"schema"`
	Session string             `json:"session"`
	Model   string             `json:"model,omitempty"`
	Written int64              `json:"written_ms,omitempty"`
	Records []compactionRecord `json:"records"`
}

// newCompactionStore 构建状态存储；dir 为空返回 nil（纯内存）。
func newCompactionStore(dir string) *compactionStore {
	if dir == "" {
		return nil
	}
	return &compactionStore{dir: dir}
}

// path 把会话 id 映射成状态文件路径。只接受 SDK 认可的会话 id（拒绝路径分隔符与 ".."），
// 其余一律不落盘——宁可重启后重压一次，也不要在 sessions/ 之外造文件。
func (c *compactionStore) path(sessID string) (string, bool) {
	if c == nil || c.dir == "" || !dsc.ValidSessionID(sessID) {
		return "", false
	}
	return filepath.Join(c.dir, sessID+".dsc-system.json"), true
}

// Load 读回该会话的记录。文件不存在 / 损坏 / schema 不符 / 串档 → nil（等同未压缩过）。
//
// 读取失败一律 fail-soft：压缩的可用性不该依赖一个缓存文件。
func (c *compactionStore) Load(sessID string) []compactionRecord {
	p, ok := c.path(sessID)
	if !ok {
		return nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil // 首次运行就是这条路径，不算错误
	}
	var f compactionStateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		c.warn("compaction state unreadable, ignoring", p, err)
		return nil
	}
	if f.Schema != compactionStateSchema {
		c.warn("compaction state schema mismatch, discarding", p, fmt.Errorf("schema=%d want=%d", f.Schema, compactionStateSchema))
		return nil
	}
	if f.Session != sessID {
		c.warn("compaction state belongs to another session, discarding", p, fmt.Errorf("file_session=%q", f.Session))
		return nil
	}
	out := make([]compactionRecord, 0, len(f.Records))
	for _, r := range f.Records {
		// 结构性字段缺失的记录直接跳过：它无法被 applyRecords 正确使用，
		// 留着只会让 validRecords 误判整串有效。
		if r.UpTo <= 0 || r.HeadHash == "" || r.Summary == "" {
			continue
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Save 原子写回（临时文件 + rename）：进程被杀也不会留下半个 JSON 让下次读到垃圾。
// 写失败只报一次，不影响本次请求。
func (c *compactionStore) Save(sessID string, records []compactionRecord) {
	p, ok := c.path(sessID)
	if !ok {
		return
	}
	if len(records) == 0 {
		// 记录被判定作废（历史被改写/回滚）：把文件一并删掉，别让下次启动又读回一份
		// 早已失配的记录去重算一遍指纹。
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			c.warn("compaction state cleanup failed", p, err)
		}
		return
	}
	f := compactionStateFile{
		Schema:  compactionStateSchema,
		Session: sessID,
		Model:   dsc.EnvStr("DSC_MODEL", ""),
		Written: time.Now().UnixMilli(),
		Records: records,
	}
	raw, err := json.Marshal(&f)
	if err != nil {
		c.warn("compaction state marshal failed", p, err)
		return
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		c.warn("compaction state dir unavailable", c.dir, err)
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		c.warn("compaction state write failed", p, err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		_ = os.Remove(tmp)
		c.warn("compaction state rename failed", p, err)
	}
}

// warn 每次进程只报一次（重复的写失败没有增量信息）。
func (c *compactionStore) warn(msg, path string, err error) {
	c.warnOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "dsc-system: compaction-state: %s path=%s err=%v\n", msg, path, err)
	})
}
