// selection.go：评审范围的确定性筛选与成组。
//
// 筛选规则决定「哪些文件值得发给模型」：二进制/删除/无文本改动、vendored 与
// 生成物、锁文件、测试文件（可选）、超大 diff 一律跳过并注明原因——跳过原因
// 随结果返回，模型与用户能看到评审边界，而不是静默漏审。
package main

import (
	"sort"
	"strconv"
	"strings"
)

// selectOptions 是文件筛选项。
type selectOptions struct {
	IncludeTests     bool
	MaxFiles         int
	MaxFileDiffLines int
}

func defaultSelectOptions() selectOptions {
	return selectOptions{MaxFiles: 40, MaxFileDiffLines: 1500}
}

// skippedFile 是未进入评审的文件及原因。
type skippedFile struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// vendoredSegments 是第三方/缓存目录（不评审）。
var vendoredSegments = map[string]bool{
	"vendor":           true,
	"node_modules":     true,
	"third_party":      true,
	"3rdparty":         true,
	"bower_components": true,
	".git":             true,
	"__pycache__":      true,
	".venv":            true,
	"site-packages":    true,
	".mypy_cache":      true,
	".pytest_cache":    true,
	".gradle":          true,
	".idea":            true,
	".dsc":             true,
	"target":           true,
	"dist":             true,
	".next":            true,
	".nuxt":            true,
	"Pods":             true,
	"DerivedData":      true,
	"testdata":         true,
	"fixtures":         true,
	"golden":           true,
	"__snapshots__":    true,
	".terraform":       true,
}

// lockFiles 是依赖锁文件（机器生成、体积大、无可评审逻辑）。
var lockFiles = map[string]bool{
	"go.sum":            true,
	"package-lock.json": true,
	"pnpm-lock.yaml":    true,
	"yarn.lock":         true,
	"bun.lockb":         true,
	"Cargo.lock":        true,
	"poetry.lock":       true,
	"Pipfile.lock":      true,
	"composer.lock":     true,
	"Gemfile.lock":      true,
	"mix.lock":          true,
	"pubspec.lock":      true,
	"gradle.lockfile":   true,
	"uv.lock":           true,
	"flake.lock":        true,
}

// generatedSuffixes 是代码生成器产物的文件名后缀/片段。
var generatedSuffixes = []string{
	".pb.go", "_grpc.pb.go", "_generated.go", ".gen.go", ".g.go",
	"_string.go", ".min.js", ".min.css", ".bundle.js",
	".generated.ts", ".generated.tsx", ".g.dart", ".designer.cs",
	".pb.cc", ".pb.h", ".pb.swift",
}

// isGeneratedFile 判断是否为生成物（文件名特征）。
func isGeneratedFile(p string) bool {
	base := baseName(p)
	lower := strings.ToLower(base)
	if strings.HasPrefix(lower, "zz_generated") {
		return true
	}
	for _, suf := range generatedSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

// isLockFile 判断是否为依赖锁文件。
func isLockFile(p string) bool {
	return lockFiles[baseName(p)]
}

// isVendoredPath 判断路径是否落在第三方/构建产物目录。
func isVendoredPath(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if vendoredSegments[seg] {
			return true
		}
	}
	return false
}

// isTestFile 判断是否为测试代码（默认不评审，可用 include_tests 打开）。
func isTestFile(p string) bool {
	base := baseName(p)
	lower := strings.ToLower(base)
	switch {
	case strings.HasSuffix(lower, "_test.go"),
		strings.HasSuffix(lower, "_test.py"),
		strings.HasPrefix(lower, "test_") && strings.HasSuffix(lower, ".py"),
		strings.HasSuffix(lower, ".test.js"), strings.HasSuffix(lower, ".test.jsx"),
		strings.HasSuffix(lower, ".test.ts"), strings.HasSuffix(lower, ".test.tsx"),
		strings.HasSuffix(lower, ".spec.js"), strings.HasSuffix(lower, ".spec.jsx"),
		strings.HasSuffix(lower, ".spec.ts"), strings.HasSuffix(lower, ".spec.tsx"),
		strings.HasSuffix(lower, "test.java"), strings.HasSuffix(lower, "tests.java"),
		strings.HasSuffix(lower, "test.kt"), strings.HasSuffix(lower, "tests.kt"),
		strings.HasSuffix(lower, "_test.rs"):
		return true
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "test", "tests", "__tests__", "spec", "e2e":
			return true
		}
	}
	return false
}

// baseName 返回路径最后一段（不引入 filepath，跨平台统一按正斜杆处理）。
func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// selectDiffFiles 按确定性规则筛选 diff 文件，返回入选文件与跳过清单。
func selectDiffFiles(diffs []*fileDiff, o selectOptions) ([]*fileDiff, []skippedFile) {
	kept := make([]*fileDiff, 0, len(diffs))
	skipped := make([]skippedFile, 0)

	for _, f := range diffs {
		path := f.path()
		var reason string
		switch {
		case f.IsBinary:
			reason = "binary file"
		case f.IsDeleted:
			reason = "deleted file"
		case len(f.Hunks) == 0:
			reason = "no textual change"
		case len(f.commentableLines()) == 0:
			reason = "no commentable lines"
		case isVendoredPath(path):
			reason = "vendored or build output path"
		case isGeneratedFile(path):
			reason = "generated file"
		case isLockFile(path):
			reason = "dependency lock file"
		case !o.IncludeTests && isTestFile(path):
			reason = "test file (pass include_tests=true to review)"
		case o.MaxFileDiffLines > 0 && f.hunkLineCount() > o.MaxFileDiffLines:
			reason = "diff too large"
		}
		if reason != "" {
			skipped = append(skipped, skippedFile{Path: path, Reason: reason})
			continue
		}
		kept = append(kept, f)
	}

	sort.SliceStable(kept, func(i, j int) bool { return kept[i].path() < kept[j].path() })
	if o.MaxFiles > 0 && len(kept) > o.MaxFiles {
		for _, f := range kept[o.MaxFiles:] {
			skipped = append(skipped, skippedFile{Path: f.path(), Reason: "beyond max_files"})
		}
		kept = kept[:o.MaxFiles]
	}
	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].Path < skipped[j].Path })
	return kept, skipped
}

// diffBundle 是一次 LLM 评审单元：相关文件成组，避免单次请求过大或过散。
type diffBundle struct {
	Files []*fileDiff
	Lines int
}

// bundleDiffFiles 把已排序的文件按行数/文件数贪心成组。
//
// 单个文件即使超过 maxLines 也独占一组（筛选阶段已限制单文件体积，此处不再
// 拆分，保持 hunk 上下文完整）。
func bundleDiffFiles(files []*fileDiff, maxLines, maxFiles int) []*diffBundle {
	if len(files) == 0 {
		return nil
	}
	if maxLines <= 0 {
		maxLines = 600
	}
	if maxFiles <= 0 {
		maxFiles = 6
	}
	var out []*diffBundle
	cur := &diffBundle{}
	for _, f := range files {
		n := f.hunkLineCount()
		if len(cur.Files) > 0 && (cur.Lines+n > maxLines || len(cur.Files) >= maxFiles) {
			out = append(out, cur)
			cur = &diffBundle{}
		}
		cur.Files = append(cur.Files, f)
		cur.Lines += n
	}
	if len(cur.Files) > 0 {
		out = append(out, cur)
	}
	return out
}

// bundleLabel 生成稳定的评审单元标签（用于错误信息与日志）。
func bundleLabel(files []*fileDiff) string {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.path())
	}
	sort.Strings(paths)
	if len(paths) == 1 {
		return paths[0]
	}
	return paths[0] + " (+" + strconv.Itoa(len(paths)-1) + " more)"
}
