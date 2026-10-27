// Package cron 提供 cron 定时任务：任务定义、JSON 持久化与 robfig/cron 调度。
//
// 触发时由宿主注入的执行器（Manager.RunSubagent）执行，走宿主侧 LLM + 工具
// 流水线，不占用主 agent 的交互会话。
package cron

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"

	"dsc/jobs"
)

// Job 一条定时任务定义。
type Job struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Cron    string `json:"cron"` // 标准 5 段 cron 表达式（分 时 日 月 周）
	Prompt  string `json:"prompt"`
	Enabled bool   `json:"enabled"`
	// Notify 该任务每次运行**落定时是否即时通知模型**（默认 false = 静默，靠 cron_list 取）。
	//
	// 不定死一侧的理由：cron 承担的是长期、循环的程序性工作，各任务重要性差别很大。
	// 一律通知会让分钟级任务每分钟唤醒会话（每次唤醒都是一轮 LLM 调用，纯烧钱）；
	// 一律不通知则真正需要跟进的产出（日报、巡检结论）没人知道。故由任务自己声明，
	// 模型（cron_add 的 notify 参数）与人（/cron add --notify、/cron notify）都能设。
	Notify        bool   `json:"notify"`
	MaxIterations int    `json:"max_iterations,omitempty"`
	CreatedAt     int64  `json:"created_at"`
	LastRunAt     int64  `json:"last_run_at,omitempty"`
	LastStatus    string `json:"last_status,omitempty"` // "success" | "error" | "running"
	LastOutput    string `json:"last_output,omitempty"`
	LastError     string `json:"last_error,omitempty"`
}

// Store 任务定义的 JSON 持久化（单文件 cron.json，原子写：临时文件 + 重命名）。
type Store struct {
	mu   sync.Mutex
	path string
	jobs map[string]*Job
}

// NewStore 打开（必要时创建）cron.json 存储。返回 nil 表示目录不可用？不——
// 出错即返回错误，任务持久化是功能前提。
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("cron: create dir: %w", err)
	}
	s := &Store{
		path: posixJoin(dir, "cron.json"),
		jobs: make(map[string]*Job),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次使用，无历史任务
		}
		return fmt.Errorf("cron: read %s: %w", s.path, err)
	}
	var list []*Job
	if err := json.Unmarshal(data, &list); err != nil {
		return fmt.Errorf("cron: parse %s: %w", s.path, err)
	}
	for _, j := range list {
		if j.ID != "" {
			s.jobs[j.ID] = j
		}
	}
	return nil
}

// List 返回所有任务的**副本**（按创建时间排序，稳定输出）。
//
// 必须是副本：调度器落定时会在后台改动运行状态（LastStatus/LastOutput），
// 而 List 的调用方（admin /cron/list、cron_list 工具、TUI /crons）会并发读取。
// 返回内部指针等于把无锁可变状态交给外部——那正是数据竞态的来源。
func (s *Store) List() []*Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	ordered := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		c := *j
		ordered = append(ordered, &c)
	}
	sort.Slice(ordered, func(i, k int) bool { return ordered[i].CreatedAt < ordered[k].CreatedAt })
	return ordered
}

// Get 返回指定任务（副本）。
func (s *Store) Get(id string) (*Job, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return nil, false
	}
	c := *j
	return &c, true
}

// Save 保存任务（新增或更新），存**副本**。
//
// 存副本的理由同 List：调用方（调度器 fire 的 goroutine、工具、admin）持有
// 该指针后仍会继续读写它，别名入表会让「锁内写入」变成谎话。
func (s *Store) Save(j *Job) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := *j
	s.jobs[c.ID] = &c
	return s.writeLocked()
}

// Update 在锁内对**存储里的当前记录**应用 apply，任务不存在时返回 false 且不写盘。
// 典型用途是更新运行状态（LastRunAt/LastStatus/LastOutput/LastError），也用于改
// Notify 这类单个开关。
//
// 为什么不能在落定时直接 Save 开始前取到的副本：那份副本可能已经过期。
//   - 运行期间任务被删除 → Save 会把它写回 cron.json（删了又活）；
//   - 运行期间被停用 → Save 用旧副本覆盖 Enabled，磁盘上又变回启用（且调度条目
//     已移除，表现为「停用了但重启后又开始跑」）。
//
// 只改 apply 里碰到的字段，其余一律沿用存储里的当前值，两个问题一起消失。
func (s *Store) Update(id string, apply func(*Job)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok {
		return false, nil
	}
	apply(j)
	return true, s.writeLocked()
}

// Remove 删除任务；不存在返回 false。
func (s *Store) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.jobs[id]; !ok {
		return false
	}
	delete(s.jobs, id)
	if err := s.writeLocked(); err != nil {
		return false
	}
	return true
}

// writeLocked 原子写整个任务列表（需已持有 s.mu）。
func (s *Store) writeLocked() error {
	list := make([]*Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j)
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("cron: marshal: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return fmt.Errorf("cron: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("cron: rename %s -> %s: %w", tmp, s.path, err)
	}
	return nil
}

// newID 生成任务 id：<cron>-<7 位小写十六进制短哈希>。
// 标识必须跨进程、跨会话唯一——旧的毫秒时间戳虽然不重号，但长得没法在通知和
// 斜杆命令里一眼对上；短哈希与后台任务、workflow 运行同一套形态。
func newID() string {
	return jobs.ShortID("cron")
}
