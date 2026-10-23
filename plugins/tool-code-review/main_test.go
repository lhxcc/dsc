package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"

	dsc "dsc-sdk"
)

func TestReviewDiffWithFakeLLM(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newGitRepo(t)
	writeFileT(t, dir, "a.go", "package main\n\nfunc main() {}\n")
	gitCmdT(t, dir, "add", ".")
	gitCmdT(t, dir, "commit", "-q", "-m", "init")
	writeFileT(t, dir, "a.go", "package main\n\nfunc main() { println(\"hi\") }\n")
	setWorkspaceRootT(t, dir)

	// 先取真实新增行号，模拟模型命中的位置。
	text, err := collectDiff(context.Background(), diffOptions{})
	if err != nil {
		t.Fatalf("collectDiff: %v", err)
	}
	files := parseUnifiedDiff(text)
	added := files[0].commentableLines()
	var line int
	for n := range added {
		line = n
	}
	reply, _ := json.Marshal([]map[string]any{{
		"path": "a.go", "start_line": line, "end_line": line,
		"severity": "high", "category": "bug", "content": "println 遗留调试输出",
	}})
	llm := &fakeLLM{replies: []string{string(reply)}}

	res, err := reviewDiff(context.Background(), llm, diffReviewArgs{})
	if err != nil {
		t.Fatalf("reviewDiff: %v", err)
	}
	if !res.OK || res.Mode != "diff" || res.FilesReviewed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Comments) != 1 || res.Comments[0].Path != "a.go" || res.Comments[0].StartLine != line {
		t.Fatalf("comments = %+v", res.Comments)
	}
	if res.Counts["high"] != 1 {
		t.Fatalf("counts = %v", res.Counts)
	}
	if llm.calls != 1 {
		t.Fatalf("llm calls = %d, want 1", llm.calls)
	}
}

func TestReviewDiffRequiresLLM(t *testing.T) {
	if _, err := reviewDiff(context.Background(), nil, diffReviewArgs{}); err != errNoLLM {
		t.Fatalf("err = %v, want errNoLLM", err)
	}
	if _, err := reviewScan(context.Background(), nil, scanReviewArgs{Paths: []string{}}); err != errNoLLM {
		t.Fatalf("err = %v, want errNoLLM", err)
	}
}

func TestReviewDiffNoChanges(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newGitRepo(t)
	writeFileT(t, dir, "a.go", "package main\n")
	gitCmdT(t, dir, "add", ".")
	gitCmdT(t, dir, "commit", "-q", "-m", "init")
	setWorkspaceRootT(t, dir)

	llm := &fakeLLM{}
	res, err := reviewDiff(context.Background(), llm, diffReviewArgs{})
	if err != nil {
		t.Fatalf("reviewDiff: %v", err)
	}
	if res.FilesReviewed != 0 || res.Note != "no reviewable changes" {
		t.Fatalf("result = %+v", res)
	}
	if llm.calls != 0 {
		t.Fatalf("llm should not be called without changes, calls = %d", llm.calls)
	}
}

func TestReviewDiffSkipsVendoredAndTests(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := newGitRepo(t)
	writeFileT(t, dir, "go.mod", "module x\n")
	gitCmdT(t, dir, "add", ".")
	gitCmdT(t, dir, "commit", "-q", "-m", "init")

	writeFileT(t, dir, "a.go", "package x\nvar A = 1\n")
	if err := os.MkdirAll(dsc.PJoin(dir, "vendor/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, dir, "vendor/lib/v.go", "package lib\n")
	writeFileT(t, dir, "a_test.go", "package x\n")
	// git diff 不显示未跟踪文件：以 intent-to-add 纳入差异。
	gitCmdT(t, dir, "add", "-N", "a.go", "a_test.go", "vendor/lib/v.go")
	setWorkspaceRootT(t, dir)

	llm := &fakeLLM{}
	res, err := reviewDiff(context.Background(), llm, diffReviewArgs{})
	if err != nil {
		t.Fatalf("reviewDiff: %v", err)
	}
	if res.FilesReviewed != 1 {
		t.Fatalf("files reviewed = %d, want 1 (只评审 a.go): %+v", res.FilesReviewed, res.FilesSkipped)
	}
	skipped := map[string]string{}
	for _, s := range res.FilesSkipped {
		skipped[s.Path] = s.Reason
	}
	if skipped["vendor/lib/v.go"] == "" || skipped["a_test.go"] == "" {
		t.Fatalf("skipped = %+v", skipped)
	}
}

func TestReviewScanWithFakeLLM(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, dir, "main.go", "package main\n\nfunc main() {}\n")
	setWorkspaceRootT(t, dir)

	llm := &fakeLLM{replies: []string{
		`[{"path":"main.go","start_line":3,"severity":"low","category":"maintainability","content":"空 main"}]`,
	}}
	res, err := reviewScan(context.Background(), llm, scanReviewArgs{Paths: []string{dsc.PJoin(dir, "main.go")}})
	if err != nil {
		t.Fatalf("reviewScan: %v", err)
	}
	if res.Mode != "scan" || res.FilesReviewed != 1 {
		t.Fatalf("result = %+v", res)
	}
	if len(res.Comments) != 1 || res.Comments[0].Path != "main.go" || res.Comments[0].StartLine != 3 {
		t.Fatalf("comments = %+v", res.Comments)
	}
	if llm.calls != 1 {
		t.Fatalf("calls = %d, want 1", llm.calls)
	}
}

func TestReviewScanSkipsBinaryAndInvalidArgs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dsc.PJoin(dir, "bin.dat"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	setWorkspaceRootT(t, dir)

	if _, err := reviewScan(context.Background(), &fakeLLM{}, scanReviewArgs{}); err == nil {
		t.Fatal("expected error for empty paths")
	}

	res, err := reviewScan(context.Background(), &fakeLLM{}, scanReviewArgs{Paths: []string{dir}})
	if err != nil {
		t.Fatalf("reviewScan: %v", err)
	}
	if res.FilesReviewed != 0 || len(res.FilesSkipped) != 1 || res.FilesSkipped[0].Reason != "binary file" {
		t.Fatalf("result = %+v", res)
	}
}

func TestReviewRulesArgSortsAndDedupes(t *testing.T) {
	res := reviewRules(rulesArgs{Paths: []string{"b.py", "a.go", "a.go"}})
	if !res.OK || len(res.Rules) != 2 {
		t.Fatalf("rules = %+v", res)
	}
	if res.Rules[0].Path != "a.go" || res.Rules[0].Rule != "go" {
		t.Fatalf("first = %+v", res.Rules[0])
	}
	if res.Rules[0].Source != "builtin" {
		t.Fatalf("source = %q", res.Rules[0].Source)
	}
}

func TestParseSeverityArg(t *testing.T) {
	if _, err := parseSeverityArg("urgent"); err == nil {
		t.Fatal("expected error for unknown severity")
	}
	if sev, err := parseSeverityArg("HIGH"); err != nil || sev != "high" {
		t.Fatalf("sev = %q, err = %v", sev, err)
	}
	if sev, err := parseSeverityArg(""); err != nil || sev != "" {
		t.Fatalf("empty severity = %q, err = %v", sev, err)
	}
}

func TestHandlersRejectInvalidJSON(t *testing.T) {
	rv := &reviewer{}
	if _, err := handleDiffReview(rv)(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Fatal("diff handler should reject invalid JSON")
	}
	if _, err := handleScanReview(rv)(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Fatal("scan handler should reject invalid JSON")
	}
	// 无 LLM 时给出明确错误，而不是空结果
	if _, err := handleDiffReview(rv)(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("diff handler should fail without LLM")
	}
}
