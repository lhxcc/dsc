// scan.go：整文件扫描（无 diff 可用时审计陌生代码）。
//
// 与 diff 评审共用同一套规则匹配与评论校验，区别只在于上下文来源：这里是文件
// 片段（按行分块），那里是 hunk。分块让单次请求有界，也让超长文件不至于被整份
// 塞进一次调用。
package main

import (
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"

	dsc "dsc-sdk"
)

const (
	// maxScanFileBytes 限制单个文件的读取体积：超出多半是数据文件而非源码。
	maxScanFileBytes = 512 << 10
	// scanChunkLines 是单次扫描请求覆盖的最大行数。
	scanChunkLines = 400
)

// scanFile 是一个待扫描文件（已按行拆分）。
type scanFile struct {
	Path  string
	Lines []string
}

// scanOptions 是整文件扫描的筛选与规模上限。
type scanOptions struct {
	IncludeTests bool
	MaxFiles     int
	MaxLines     int
}

func defaultScanOptions() scanOptions {
	return scanOptions{MaxFiles: 20, MaxLines: 2000}
}

// scanChunk 是一次扫描请求的行区间（Start/End 均为 1-based）。
type scanChunk struct {
	Path  string
	Start int
	End   int
	Lines []string
}

// scanCandidate 是候选文件的工作区相对路径与真实路径。
type scanCandidate struct {
	rel string
	abs string
}

// enumerateScanFiles 展开路径参数（文件或目录），按确定性规则筛选并读取。
func enumerateScanFiles(paths []string, o scanOptions) ([]scanFile, []skippedFile, error) {
	if len(paths) == 0 {
		return nil, nil, fmt.Errorf("paths is required: pass files or directories to scan")
	}
	root := workspaceRoot()

	var candidates []scanCandidate
	seen := make(map[string]bool)
	add := func(rel, abs string) {
		if seen[rel] {
			return
		}
		seen[rel] = true
		candidates = append(candidates, scanCandidate{rel: rel, abs: abs})
	}

	for _, p := range paths {
		abs := p
		if !isAbsPath(p) {
			abs = dsc.PJoin(root, p)
		}
		abs = dsc.PClean(abs)
		info, err := os.Stat(abs)
		if err != nil {
			return nil, nil, fmt.Errorf("stat %s: %v", p, err)
		}
		if !info.IsDir() {
			add(relativeTo(root, abs), abs)
			continue
		}
		walkErr := dsc.PWalkDir(abs, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // 尽力而为：单个目录不可读不中断整轮扫描
			}
			if d.IsDir() {
				if path != abs && skipScanDir(d.Name()) {
					return fs.SkipDir
				}
				return nil
			}
			add(relativeTo(root, path), path)
			return nil
		})
		if walkErr != nil {
			return nil, nil, fmt.Errorf("walk %s: %v", p, walkErr)
		}
	}

	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].rel < candidates[j].rel })

	var (
		files   []scanFile
		skipped []skippedFile
	)
	for _, c := range candidates {
		var reason string
		switch {
		case isVendoredPath(c.rel):
			reason = "vendored or build output path"
		case isGeneratedFile(c.rel):
			reason = "generated file"
		case isLockFile(c.rel):
			reason = "dependency lock file"
		case !o.IncludeTests && isTestFile(c.rel):
			reason = "test file (pass include_tests=true to review)"
		}
		if reason != "" {
			skipped = append(skipped, skippedFile{Path: c.rel, Reason: reason})
			continue
		}

		f, err := readScanFile(c.rel, c.abs, o.MaxLines)
		if err != nil {
			skipped = append(skipped, skippedFile{Path: c.rel, Reason: err.Error()})
			continue
		}
		if o.MaxFiles > 0 && len(files) >= o.MaxFiles {
			skipped = append(skipped, skippedFile{Path: c.rel, Reason: "beyond max_files"})
			continue
		}
		files = append(files, f)
	}

	sort.SliceStable(skipped, func(i, j int) bool { return skipped[i].Path < skipped[j].Path })
	return files, skipped, nil
}

// relativeTo 返回 abs 相对 root 的正斜杆路径；不在 root 下时原样返回。
func relativeTo(root, abs string) string {
	rel, err := dsc.PRel(root, abs)
	if err != nil || rel == "" {
		return abs
	}
	return rel
}

// isAbsPath 判断路径是否已是绝对路径（POSIX 根 / Windows 盘符）。
func isAbsPath(p string) bool {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return true
	}
	return len(p) >= 2 && p[1] == ':'
}

// skipScanDir 判断目录遍历时是否跳过（第三方/构建产物目录）。
func skipScanDir(name string) bool {
	return vendoredSegments[name]
}

// readScanFile 读取并校验单个文件：拒绝二进制、超大与超行数文件。
func readScanFile(rel, abs string, maxLines int) (scanFile, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return scanFile{}, fmt.Errorf("stat: %v", err)
	}
	if info.Size() > maxScanFileBytes {
		return scanFile{}, fmt.Errorf("file too large (%d bytes)", info.Size())
	}
	data, err := dsc.ReadFile(abs)
	if err != nil {
		return scanFile{}, err
	}
	if looksBinary(data) {
		return scanFile{}, fmt.Errorf("binary file")
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if len(lines) == 0 {
		return scanFile{}, fmt.Errorf("empty file")
	}
	if maxLines > 0 && len(lines) > maxLines {
		return scanFile{}, fmt.Errorf("file has %d lines (max_lines=%d)", len(lines), maxLines)
	}
	return scanFile{Path: rel, Lines: lines}, nil
}

// looksBinary 以 NUL 字节作为二进制判定（对源码足够准确）。
func looksBinary(data []byte) bool {
	limit := len(data)
	if limit > 8192 {
		limit = 8192
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return true
		}
	}
	return false
}

// chunkScanFile 把文件按行分块。
func chunkScanFile(f scanFile, size int) []scanChunk {
	if size <= 0 {
		size = scanChunkLines
	}
	var out []scanChunk
	for start := 0; start < len(f.Lines); start += size {
		end := start + size
		if end > len(f.Lines) {
			end = len(f.Lines)
		}
		out = append(out, scanChunk{
			Path:  f.Path,
			Start: start + 1,
			End:   end,
			Lines: f.Lines[start:end],
		})
	}
	return out
}
