// Package benchreport 是 agentic-bench 报告的比较引擎：报告 schema、读取与基线 diff。
// 插件（bench_report 工具）与独立 CLI（cmd/benchcmp）共用同一实现，避免两处口径漂移。
package benchreport

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
)

// SchemaVersion 当前报告 schema 版本（新增 baseline/diff 字段后的版本）。
const SchemaVersion = 2

// 用例变化分类。
const (
	KindRegression  = "regression"  // 基线 pass → 当前 fail/skip
	KindImprovement = "improvement" // 基线 fail/skip → 当前 pass
	KindNew         = "new"         // 基线不存在该用例
	KindMissing     = "missing"     // 当前不存在该用例
	KindSame        = "same"        // 状态一致
	KindChanged     = "changed"     // 状态变化但不构成明确的改进/回归（如 fail→skip）
)

// Report 是 report.json 的结构（字段与落盘 JSON 一一对应）。
type Report struct {
	SchemaVersion int          `json:"schema_version"`
	Harness       string       `json:"harness"`
	BenchRoot     string       `json:"bench_root"`
	StartedAt     string       `json:"started_at"`
	DurationMS    int64        `json:"duration_ms"`
	Passed        int          `json:"passed"`
	Failed        int          `json:"failed"`
	Total         int          `json:"total"`
	Score         string       `json:"score"`
	Ratio         string       `json:"ratio"`
	Cases         []ReportCase `json:"cases"`
	Baseline      *BaselineRef `json:"baseline,omitempty"`
	// BaselineError 记录基线加载失败原因（比较不可用时的显式留痕，不静默跳过）。
	BaselineError string `json:"baseline_error,omitempty"`
	Diff          *Diff  `json:"diff,omitempty"`
}

// ReportCase 单用例结果（字段保真：与既有 report.json 完全一致）。
type ReportCase struct {
	ID              string `json:"id"`
	Title           string `json:"title"`
	Status          string `json:"status"` // pass | fail | skip
	Weight          int    `json:"weight"`
	Earned          int    `json:"earned"`
	DurationMS      int64  `json:"duration_ms"`
	Feedback        string `json:"feedback"`
	ArtifactContent string `json:"artifact_content"`
}

// BaselineRef 本次比较所用基线的摘要。
type BaselineRef struct {
	Path       string `json:"path"`
	Score      string `json:"score"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	DurationMS int64  `json:"duration_ms"`
}

// Diff 两次报告的差异（regressions/improvements/cases 一律按 id 升序，确定性）。
type Diff struct {
	ScoreDelta   string     `json:"score_delta"`
	Regressions  []string   `json:"regressions"`
	Improvements []string   `json:"improvements"`
	Unchanged    int        `json:"unchanged"`
	Cases        []DiffCase `json:"cases"`
}

// DiffCase 单个用例的变化。
type DiffCase struct {
	ID         string `json:"id"`
	BaseStatus string `json:"base_status"`
	CurStatus  string `json:"cur_status"`
	DeltaMS    int64  `json:"delta_ms"`
	Kind       string `json:"kind"`
}

// Load 读取一份 report.json。空报告、损坏 JSON、重复用例 id 一律报错——
// 比较静默接受坏基线会让回归检查失去意义。
func Load(path string) (*Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read baseline %s: %w", path, err)
	}
	var r Report
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("parse baseline %s: %w", path, err)
	}
	if len(r.Cases) == 0 {
		return nil, fmt.Errorf("baseline %s has no cases", path)
	}
	seen := make(map[string]struct{}, len(r.Cases))
	for _, c := range r.Cases {
		if c.ID == "" {
			return nil, fmt.Errorf("baseline %s contains a case without id", path)
		}
		if _, dup := seen[c.ID]; dup {
			return nil, fmt.Errorf("baseline %s contains duplicate case id %q", path, c.ID)
		}
		seen[c.ID] = struct{}{}
	}
	return &r, nil
}

// DiffReports 按用例 id 对齐两份报告并分类。结果按 id 升序，便于 diff 与缓存。
func DiffReports(base, cur *Report) *Diff {
	baseByID := make(map[string]ReportCase, len(base.Cases))
	for _, c := range base.Cases {
		baseByID[c.ID] = c
	}
	curByID := make(map[string]ReportCase, len(cur.Cases))
	for _, c := range cur.Cases {
		curByID[c.ID] = c
	}
	ids := make([]string, 0, len(baseByID)+len(curByID))
	for id := range baseByID {
		ids = append(ids, id)
	}
	for id := range curByID {
		if _, ok := baseByID[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	d := &Diff{
		ScoreDelta:   scoreDelta(base.Score, cur.Score),
		Regressions:  []string{},
		Improvements: []string{},
		Cases:        make([]DiffCase, 0, len(ids)),
	}
	for _, id := range ids {
		b, inBase := baseByID[id]
		c, inCur := curByID[id]
		dc := DiffCase{ID: id, BaseStatus: b.Status, CurStatus: c.Status}
		switch {
		case !inBase:
			dc.Kind = KindNew
		case !inCur:
			dc.Kind = KindMissing
		default:
			dc.Kind = classify(b.Status, c.Status)
			dc.DeltaMS = c.DurationMS - b.DurationMS
		}
		switch dc.Kind {
		case KindRegression:
			d.Regressions = append(d.Regressions, id)
		case KindImprovement:
			d.Improvements = append(d.Improvements, id)
		case KindSame:
			d.Unchanged++
		}
		d.Cases = append(d.Cases, dc)
	}
	return d
}

// classify 按状态迁移分类：pass 为最好、fail 次之、skip 最差（未跑完）。
// pass↔fail / pass↔skip 分别记为回归与改进；fail↔skip 只算 changed（无明确优劣）。
func classify(base, cur string) string {
	switch {
	case base == cur:
		return KindSame
	case base == "pass" && (cur == "fail" || cur == "skip"):
		return KindRegression
	case (base == "fail" || base == "skip") && cur == "pass":
		return KindImprovement
	default:
		return KindChanged
	}
}

// scoreDelta 返回带符号的得分差（两位小数）；无法解析按 0 处理。
func scoreDelta(base, cur string) string {
	b, _ := strconv.ParseFloat(base, 64)
	c, _ := strconv.ParseFloat(cur, 64)
	d := c - b
	if d == 0 {
		return "0.00"
	}
	return fmt.Sprintf("%+.2f", d)
}

// HasRegression 报告是否存在明确回归（供 CLI 退出码判定）。
func (d *Diff) HasRegression() bool {
	return d != nil && len(d.Regressions) > 0
}

// HasNewFail 报告是否存在「基线未通过（含新增/缺失/skip）而当前 fail」的用例。
func (d *Diff) HasNewFail() bool {
	if d == nil {
		return false
	}
	for _, c := range d.Cases {
		if c.CurStatus == "fail" && c.BaseStatus != "fail" {
			return true
		}
	}
	return false
}
