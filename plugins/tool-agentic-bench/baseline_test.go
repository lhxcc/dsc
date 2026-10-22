package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dsc-plugin-tool-agentic-bench/internal/benchreport"
)

// TestBuildReportWithBaselineEmitsDiff 基线模式：report.json 内嵌 baseline/diff，
// 并落盘 report.diff.json；回归用例被正确识别。
func TestBuildReportWithBaselineEmitsDiff(t *testing.T) {
	root := t.TempDir()

	// 基线：全部用例 pass。
	base := benchreport.Report{
		SchemaVersion: benchreport.SchemaVersion,
		Harness:       "dsc-tool-agentic-bench",
		Score:         "100.00",
		Passed:        len(benchCases),
		Total:         len(benchCases),
	}
	for _, c := range benchCases {
		base.Cases = append(base.Cases, benchreport.ReportCase{
			ID: c.ID, Title: c.Title, Status: "pass", Weight: c.Weight, Earned: c.Weight,
		})
	}
	basePath := filepath.Join(root, "baseline.json")
	body, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	if err := os.WriteFile(basePath, body, 0o600); err != nil {
		t.Fatalf("write baseline: %v", err)
	}
	t.Setenv("DSC_BENCH_BASELINE", basePath)

	state.startedAt = time.Now()
	state.root = root
	results := make(map[string]CaseResult, len(benchCases))
	for i, c := range benchCases {
		status := "pass"
		if i == 0 {
			status = "fail" // 制造一个回归
		}
		results[c.ID] = CaseResult{Status: status}
	}

	_, summary, _ := buildReport(root, results, 1234)
	var rep benchreport.Report
	if err := json.Unmarshal([]byte(summary), &rep); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if rep.BaselineError != "" {
		t.Fatalf("baseline_error = %q", rep.BaselineError)
	}
	if rep.Baseline == nil || rep.Diff == nil {
		t.Fatalf("expected baseline+diff, got %+v", rep)
	}
	if len(rep.Diff.Regressions) != 1 || rep.Diff.Regressions[0] != benchCases[0].ID {
		t.Fatalf("regressions = %v, want [%s]", rep.Diff.Regressions, benchCases[0].ID)
	}
	if _, err := os.Stat(filepath.Join(root, benchOutDirName, "report.diff.json")); err != nil {
		t.Fatalf("report.diff.json not written: %v", err)
	}
}

// TestBuildReportWithoutBaselineKeepsSchema 未设置基线时仍产出 schema_version，
// 且不出现 baseline/diff 字段（行为与旧版一致）。
func TestBuildReportWithoutBaselineKeepsSchema(t *testing.T) {
	t.Setenv("DSC_BENCH_BASELINE", "")
	root := t.TempDir()
	state.startedAt = time.Now()
	state.root = root

	_, summary, _ := buildReport(root, map[string]CaseResult{
		benchCases[0].ID: {Status: "pass"},
	}, 10)
	var raw map[string]any
	if err := json.Unmarshal([]byte(summary), &raw); err != nil {
		t.Fatalf("parse report: %v", err)
	}
	if raw["schema_version"] != float64(benchreport.SchemaVersion) {
		t.Fatalf("schema_version = %v", raw["schema_version"])
	}
	if _, ok := raw["diff"]; ok {
		t.Fatal("diff must be absent without baseline")
	}
	if _, ok := raw["baseline"]; ok {
		t.Fatal("baseline must be absent without baseline")
	}
}
