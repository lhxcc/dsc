// tool-code-review：把「确定性工程 + 单次专注评审」的代码评审能力做成模型可调用工具。
//
// 设计参考 alibaba/open-code-review：评审质量不能只靠一段提示词——选哪些文件、
// 每个文件适用哪些规则、评论能否落到改动行，必须由确定性代码保证；LLM 只负责
// 在已圈定的上下文里判断缺陷。
//
// 三个只读工具：
//   - code_review_diff  采集 git diff → 解析 hunk/新增行 → 过滤 vendored/生成物/
//     锁文件/测试（可选）/超大 diff → 按行数成组 → 匹配语言规则 → 并发调用宿主
//     聚合 LLM → 评论行号吸附到真实新增行 → 汇总。
//   - code_review_scan  整文件分块扫描（无 diff 可用时审计陌生代码）。
//   - code_review_rules 查看某个路径命中的规则文档（透明可查）。
//
// 评审本身在插件进程内完成，不修改工作区；宿主只需提供一个 LLM 插件（经
// interconnect 聚合下发），插件不需要自己的模型配置。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"dsc-sdk"
)

// errNoLLM 表示宿主未下发聚合 LLM：评审无法进行（明确报错，不静默返回空结果）。
var errNoLLM = errors.New("code review requires an LLM plugin: host interconnect did not provide one")

// workspaceRoot 在运行时取宿主注入的统一工作空间根；单测可替换以便在临时仓库里
// 验证 git diff 采集与文件扫描。
var workspaceRoot = func() string { return dsc.WorkspaceRoot() }

// reviewer 持有宿主聚合 LLM 客户端（SetInterconnect 后注入）。
type reviewer struct {
	mu  sync.Mutex
	llm chatClient
}

func (r *reviewer) setLLM(c chatClient) {
	r.mu.Lock()
	r.llm = c
	r.mu.Unlock()
}

func (r *reviewer) client() chatClient {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.llm
}

// diffReviewArgs 是 code_review_diff 的入参。
type diffReviewArgs struct {
	Base         string   `json:"base"`
	Staged       bool     `json:"staged"`
	Paths        []string `json:"paths"`
	IncludeTests bool     `json:"include_tests"`
	MaxFiles     int      `json:"max_files"`
	SeverityMin  string   `json:"severity_min"`
}

// scanReviewArgs 是 code_review_scan 的入参。
type scanReviewArgs struct {
	Paths        []string `json:"paths"`
	IncludeTests bool     `json:"include_tests"`
	MaxFiles     int      `json:"max_files"`
	MaxLines     int      `json:"max_lines"`
	SeverityMin  string   `json:"severity_min"`
}

// rulesArgs 是 code_review_rules 的入参。
type rulesArgs struct {
	Paths []string `json:"paths"`
}

// ruleMatch 是一个路径命中的规则文档。
type ruleMatch struct {
	Path   string `json:"path"`
	Rule   string `json:"rule"`
	Source string `json:"source"`
}

// rulesResult 是 code_review_rules 的结果。
type rulesResult struct {
	OK    bool        `json:"ok"`
	Rules []ruleMatch `json:"rules"`
}

// reviewResult 是评审结果（结构化返回给模型，同时供 TUI 视图使用）。
type reviewResult struct {
	OK            bool           `json:"ok"`
	Mode          string         `json:"mode"`
	Base          string         `json:"base,omitempty"`
	FilesReviewed int            `json:"files_reviewed"`
	FilesSkipped  []skippedFile  `json:"files_skipped,omitempty"`
	Comments      []comment      `json:"comments"`
	Counts        map[string]int `json:"counts,omitempty"`
	Dropped       int            `json:"dropped_invalid,omitempty"`
	Truncated     bool           `json:"truncated,omitempty"`
	Errors        []string       `json:"errors,omitempty"`
	Note          string         `json:"note,omitempty"`
}

// reviewDiff 执行 diff 评审。
func reviewDiff(ctx context.Context, llm chatClient, args diffReviewArgs) (*reviewResult, error) {
	if llm == nil {
		return nil, errNoLLM
	}
	severity, err := parseSeverityArg(args.SeverityMin)
	if err != nil {
		return nil, err
	}

	sel := defaultSelectOptions()
	if args.MaxFiles > 0 {
		sel.MaxFiles = args.MaxFiles
	}
	text, err := collectDiff(ctx, diffOptions{Base: args.Base, Staged: args.Staged, Paths: args.Paths})
	if err != nil {
		return nil, err
	}
	files, skipped := selectDiffFiles(parseUnifiedDiff(text), sel)

	res := &reviewResult{OK: true, Mode: "diff", Base: args.Base, FilesReviewed: len(files), FilesSkipped: skipped}
	if len(files) == 0 {
		res.Note = "no reviewable changes"
		return res, nil
	}

	resolver := newRuleResolver()
	docs := resolver.forPaths(sortedPaths(files))
	bundles := bundleDiffFiles(files,
		envInt("DSC_CODE_REVIEW_BUNDLE_LINES", 600, 100, 4000),
		envInt("DSC_CODE_REVIEW_BUNDLE_FILES", 6, 1, 20))

	units := make([]*reviewUnit, 0, len(bundles))
	for _, b := range bundles {
		system, user := buildDiffPrompt(b.Files, docs)
		unitFiles := make([]unitFile, 0, len(b.Files))
		for _, f := range b.Files {
			unitFiles = append(unitFiles, unitFile{
				Path:     f.path(),
				DiffMode: true,
				Added:    f.commentableLines(),
				MaxLine:  f.maxNewLine(),
			})
		}
		units = append(units, &reviewUnit{Label: bundleLabel(b.Files), System: system, User: user, Files: unitFiles})
	}

	res.applyOutcome(runReview(ctx, llm, units, reviewOptionsWithSeverity(severity)))
	return res, nil
}

// reviewScan 执行整文件扫描。
func reviewScan(ctx context.Context, llm chatClient, args scanReviewArgs) (*reviewResult, error) {
	if llm == nil {
		return nil, errNoLLM
	}
	severity, err := parseSeverityArg(args.SeverityMin)
	if err != nil {
		return nil, err
	}

	sel := defaultScanOptions()
	if args.MaxFiles > 0 {
		sel.MaxFiles = args.MaxFiles
	}
	if args.MaxLines > 0 {
		sel.MaxLines = args.MaxLines
	}
	files, skipped, err := enumerateScanFiles(args.Paths, sel)
	if err != nil {
		return nil, err
	}

	res := &reviewResult{OK: true, Mode: "scan", FilesReviewed: len(files), FilesSkipped: skipped}
	if len(files) == 0 {
		res.Note = "no scannable files"
		return res, nil
	}

	resolver := newRuleResolver()
	var units []*reviewUnit
	for _, f := range files {
		docs := resolver.forPaths([]string{f.Path})
		for _, ch := range chunkScanFile(f, scanChunkLines) {
			system, user := buildScanPrompt(f.Path, ch.Lines, ch.Start, ch.End, docs)
			units = append(units, &reviewUnit{
				Label:  fmt.Sprintf("%s:%d-%d", ch.Path, ch.Start, ch.End),
				System: system,
				User:   user,
				Files: []unitFile{{
					Path:     ch.Path,
					DiffMode: false,
					MinLine:  ch.Start,
					MaxLine:  ch.End,
				}},
			})
		}
	}

	res.applyOutcome(runReview(ctx, llm, units, reviewOptionsWithSeverity(severity)))
	return res, nil
}

// reviewRules 返回路径命中的规则文档（只读、不调用 LLM）。
func reviewRules(args rulesArgs) *rulesResult {
	resolver := newRuleResolver()
	paths := append([]string(nil), args.Paths...)
	sort.Strings(paths)
	out := &rulesResult{OK: true, Rules: make([]ruleMatch, 0, len(paths))}
	seen := make(map[string]bool)
	for _, p := range paths {
		if seen[p] {
			continue
		}
		seen[p] = true
		doc := resolver.load(ruleNameForPath(p))
		out.Rules = append(out.Rules, ruleMatch{Path: p, Rule: doc.Name, Source: doc.Source})
	}
	return out
}

// applyOutcome 把评审产出写入结果。
func (r *reviewResult) applyOutcome(o reviewOutcome) {
	r.Comments = o.Comments
	r.Dropped = o.Dropped
	r.Truncated = o.Truncated
	r.Errors = o.Errors
	r.Counts = countBySeverity(o.Comments)
	if r.Comments == nil {
		r.Comments = []comment{}
	}
}

func countBySeverity(cs []comment) map[string]int {
	counts := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0}
	for _, c := range cs {
		counts[c.Severity]++
	}
	return counts
}

// parseSeverityArg 校验 severity_min；空值表示不过滤。
func parseSeverityArg(s string) (string, error) {
	sev := normalizeSeverity(s)
	if s != "" && sev == "" {
		return "", fmt.Errorf("invalid severity_min %q: use one of low/medium/high/critical", s)
	}
	return sev, nil
}

func reviewOptionsWithSeverity(severity string) reviewOptions {
	opts := defaultReviewOptions()
	opts.SeverityMin = severity
	return opts
}

// ---- 工具处理器 ----

func handleDiffReview(rv *reviewer) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var p diffReviewArgs
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		res, err := reviewDiff(ctx, rv.client(), p)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(res)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

func handleScanReview(rv *reviewer) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		var p scanReviewArgs
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		res, err := reviewScan(ctx, rv.client(), p)
		if err != nil {
			return "", err
		}
		b, err := json.Marshal(res)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
}

func main() {
	rv := &reviewer{}

	diffSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"base": {"type": "string", "description": "Optional git ref to compare the working tree against (git diff <base>). Empty means review all local changes (git diff HEAD, staged + unstaged)."},
			"staged": {"type": "boolean", "description": "Review only the index (git diff --cached). With base set, compares the index to that base."},
			"paths": {"type": "array", "items": {"type": "string"}, "description": "Optional pathspecs to narrow the review (files or directories, relative to the workspace root)."},
			"include_tests": {"type": "boolean", "description": "Include test files (default false: *_test.go, *.test.ts, tests/ ... are skipped)."},
			"max_files": {"type": "integer", "description": "Maximum files to review after filtering (default 40)."},
			"severity_min": {"type": "string", "enum": ["low", "medium", "high", "critical"], "description": "Drop findings below this severity (default: keep all)."}
		}
	}`)

	scanSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"paths": {"type": "array", "items": {"type": "string"}, "description": "Files or directories to scan (relative to the workspace root). Required."},
			"include_tests": {"type": "boolean", "description": "Include test files (default false)."},
			"max_files": {"type": "integer", "description": "Maximum files to scan (default 20)."},
			"max_lines": {"type": "integer", "description": "Skip files with more than this many lines (default 2000)."},
			"severity_min": {"type": "string", "enum": ["low", "medium", "high", "critical"], "description": "Drop findings below this severity (default: keep all)."}
		},
		"required": ["paths"]
	}`)

	rulesSchema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"paths": {"type": "array", "items": {"type": "string"}, "description": "Paths whose matched review rule documents should be reported."}
		},
		"required": ["paths"]
	}`)

	sdk := dsc.New(dsc.Config{
		Name:    "code-review",
		Version: "1.0.0",
		Type:    dsc.TypeTool,
		Provides: map[string]string{
			"code-review": "true",
		},
	})

	sdk.SetInterconnect(func(ctx context.Context, ic *dsc.Interconnect) error {
		if llm := ic.LLM(); llm != nil {
			rv.setLLM(llmAdapter{c: llm})
		}
		return nil
	})

	sdk.Tool(dsc.Tool{
		Name: "code_review_diff",
		Description: "Review local git changes and return structured findings. Runs `git diff` in the workspace root, " +
			"filters vendored/generated/lock/test files, matches language-specific review rules, and reviews the " +
			"remaining diffs with the host LLM in parallel. Every finding is pinned to a real added line. " +
			"Input: optional base ref, staged flag, pathspecs, include_tests, max_files, severity_min. " +
			"Use it before committing or when asked to review a branch/working tree; do NOT use it to modify code " +
			"(it is read-only) and do not use it when the workspace is not a git repository. " +
			"On error (no LLM plugin, not a git repo, no changes) the error text explains the remedy; a review with " +
			"no findings returns ok=true with an empty comments array.",
		Schema:       diffSchema,
		Handler:      handleDiffReview(rv),
		Capabilities: []string{"read-only"},
		Context: "代码评审工具（code_review_diff / code_review_scan）在插件内独立调用一次 LLM 完成评审，" +
			"返回带文件与行号的结构化评论，不修改工作区。默认不评审 vendored/生成物/锁文件与测试文件。",
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return reviewView(result)
		},
	})

	sdk.Tool(dsc.Tool{
		Name: "code_review_scan",
		Description: "Scan whole files (no diff needed) and return structured findings. Reads the given files/directories, " +
			"chunks long files, matches language-specific review rules, and reviews each chunk with the host LLM. " +
			"Use it to audit unfamiliar code or files without meaningful git history; use code_review_diff instead " +
			"when reviewing changes. Read-only; on error the message explains the remedy.",
		Schema:       scanSchema,
		Handler:      handleScanReview(rv),
		Capabilities: []string{"read-only"},
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return reviewView(result)
		},
	})

	sdk.Tool(dsc.Tool{
		Name: "code_review_rules",
		Description: "Report which review rule document applies to each path (built-in by extension, overridable per " +
			"workspace via .dsc/review-rules/<rule>.md or DSC_CODE_REVIEW_RULES_DIR). Read-only and LLM-free; " +
			"use it to explain why a file is reviewed under a given rule set.",
		Schema: rulesSchema,
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var p rulesArgs
			if err := json.Unmarshal(args, &p); err != nil {
				return "", fmt.Errorf("invalid arguments: %w", err)
			}
			b, err := json.Marshal(reviewRules(p))
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
		Capabilities: []string{"read-only"},
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return rulesView(result)
		},
	})

	sdk.Serve()
}
