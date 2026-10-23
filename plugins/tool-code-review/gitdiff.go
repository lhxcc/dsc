// gitdiff.go：采集 git diff 并解析为结构化文件/行号模型。
//
// 这是「确定性工程」负责的部分——评审范围从哪来、评论能落在哪些行，必须由代码
// 而非提示词决定。解析出的 commentableLines 是后续把模型评论吸附回真实改动行的依据。
package main

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"

	dsc "dsc-sdk"
)

const (
	// maxDiffBytes 限制单次 git diff 输出体积：超大 diff 一次性塞给模型既无意义
	// 又可能撑爆请求；调用方应改用 paths 缩小范围。
	maxDiffBytes = 8 << 20
	// diffContextLines 是 hunk 上下文行数（与 git 默认一致，显式写出便于阅读）。
	diffContextLines = 3
)

// diffLine 是 hunk 内的一行：Kind 为 '+'/'-'/' '，行号按各自文件版本编号。
type diffLine struct {
	Kind  byte
	Text  string
	OldNo int
	NewNo int
}

// diffHunk 是一个 unified diff 区块。
type diffHunk struct {
	OldStart, OldLines int
	NewStart, NewLines int
	Section            string
	Lines              []diffLine
}

// fileDiff 是单个文件的改动。
type fileDiff struct {
	OldPath   string
	NewPath   string
	IsNew     bool
	IsDeleted bool
	IsRenamed bool
	IsBinary  bool
	Hunks     []diffHunk
}

// path 返回评审使用的路径：优先新路径（删除文件没有新路径）。
func (f *fileDiff) path() string {
	if f.NewPath != "" {
		return f.NewPath
	}
	return f.OldPath
}

// commentableLines 返回可落评论的新文件行号集合：
//   - 每个「+」新增/修改行自身；
//   - 每个「-」删除行的锚点——删除后在它位置出现的新文件行（块尾则取该 hunk 的
//     最后一个新行）。删除代码同样值得评审（如删掉了校验），但新版本里已无该行，
//     只能把评论锚定到紧随其后的行，避免这类发现被静默丢弃。
func (f *fileDiff) commentableLines() map[int]bool {
	out := make(map[int]bool)
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.Kind == '+' || l.Kind == '-' {
				if l.NewNo > 0 {
					out[l.NewNo] = true
				}
			}
		}
	}
	return out
}

// maxNewLine 返回新文件在 diff 里出现过的最大行号。
func (f *fileDiff) maxNewLine() int {
	max := 0
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			if l.NewNo > max {
				max = l.NewNo
			}
		}
	}
	return max
}

// hunkLineCount 返回 hunk 行数（含上下文），用于成组与体积上限判断。
func (f *fileDiff) hunkLineCount() int {
	n := 0
	for _, h := range f.Hunks {
		n += len(h.Lines)
	}
	return n
}

// sortedPaths 返回文件的稳定排序键列表（AGENTS.md 规则 6：进模型输入的有序产物
// 一律按名升序，不依赖 map 迭代序）。
func sortedPaths(files []*fileDiff) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.path())
	}
	sort.Strings(out)
	return out
}

var (
	hunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@ ?(.*)$`)
	diffGitRe    = regexp.MustCompile(`^diff --git a/(.+) b/(.+)$`)
	// git 只有在路径含空格/非 ASCII/特殊字符时才加引号并做 C 风格转义。
	diffGitQuotedRe = regexp.MustCompile(`^diff --git ("(?:[^"\\]|\\.)*") ("(?:[^"\\]|\\.)*")$`)
)

// parseUnifiedDiff 把 unified diff 文本解析为按文件分组的结构化模型。
func parseUnifiedDiff(text string) []*fileDiff {
	var (
		files      []*fileDiff
		cur        *fileDiff
		h          *diffHunk
		inHunk     bool
		oldNo      int
		newNo      int
		hunkNewMax int
	)
	flushHunk := func() {
		if cur != nil && h != nil {
			cur.Hunks = append(cur.Hunks, *h)
			h = nil
		}
	}
	flushFile := func() {
		flushHunk()
		if cur != nil {
			files = append(files, cur)
			cur = nil
		}
	}

	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSuffix(raw, "\r")

		if strings.HasPrefix(line, "diff --git ") {
			flushFile()
			cur = &fileDiff{}
			inHunk = false
			switch {
			case diffGitQuotedRe.MatchString(line):
				m := diffGitQuotedRe.FindStringSubmatch(line)
				cur.OldPath = stripDiffPrefix(unquoteGitPath(m[1]))
				cur.NewPath = stripDiffPrefix(unquoteGitPath(m[2]))
			case diffGitRe.MatchString(line):
				m := diffGitRe.FindStringSubmatch(line)
				cur.OldPath = m[1]
				cur.NewPath = m[2]
			}
			continue
		}
		if cur == nil {
			continue
		}

		if strings.HasPrefix(line, "@@ ") {
			flushHunk()
			m := hunkHeaderRe.FindStringSubmatch(line)
			if m == nil {
				inHunk = false
				continue
			}
			h = &diffHunk{
				OldStart: atoiDefault(m[1], 0),
				OldLines: atoiDefault(m[2], 1),
				NewStart: atoiDefault(m[3], 0),
				NewLines: atoiDefault(m[4], 1),
				Section:  strings.TrimSpace(m[5]),
			}
			oldNo, newNo = h.OldStart, h.NewStart
			// 该 hunk 覆盖的最大新行号；纯删除 hunk（NewLines=0）取其前一行作为
			// 删除位置的锚点，避免锚到文件末尾之外。
			hunkNewMax = h.NewStart + h.NewLines - 1
			if hunkNewMax < 1 {
				hunkNewMax = 1
			}
			inHunk = true
			continue
		}

		if inHunk {
			if strings.HasPrefix(line, `\`) {
				continue // "\ No newline at end of file"
			}
			if line == "" {
				continue // 文本末尾的空行
			}
			switch line[0] {
			case '+':
				h.Lines = append(h.Lines, diffLine{Kind: '+', Text: line[1:], NewNo: newNo})
				newNo++
				continue
			case '-':
				// 删除行在新文件中不存在：记录它位置上的新行作为评论锚点。
				anchor := newNo
				if anchor > hunkNewMax {
					anchor = hunkNewMax
				}
				h.Lines = append(h.Lines, diffLine{Kind: '-', Text: line[1:], OldNo: oldNo, NewNo: anchor})
				oldNo++
				continue
			case ' ':
				h.Lines = append(h.Lines, diffLine{Kind: ' ', Text: line[1:], OldNo: oldNo, NewNo: newNo})
				oldNo++
				newNo++
				continue
			}
			// 不是 hunk 内容行：hunk 已结束，退出 hunk 态按扩展头处理。
			inHunk = false
		}

		switch {
		case strings.HasPrefix(line, "new file mode "):
			cur.IsNew = true
		case strings.HasPrefix(line, "deleted file mode "):
			cur.IsDeleted = true
		case strings.HasPrefix(line, "rename from "):
			cur.OldPath = unquoteGitPath(strings.TrimPrefix(line, "rename from "))
			cur.IsRenamed = true
		case strings.HasPrefix(line, "rename to "):
			cur.NewPath = unquoteGitPath(strings.TrimPrefix(line, "rename to "))
			cur.IsRenamed = true
		case strings.HasPrefix(line, "--- "):
			p := stripDiffPrefix(unquoteGitPath(strings.TrimSpace(strings.TrimPrefix(line, "--- "))))
			if p == "/dev/null" || p == "" {
				cur.OldPath = ""
				cur.IsNew = true
			} else {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			p := stripDiffPrefix(unquoteGitPath(strings.TrimSpace(strings.TrimPrefix(line, "+++ "))))
			if p == "/dev/null" || p == "" {
				cur.NewPath = ""
				cur.IsDeleted = true
			} else {
				cur.NewPath = p
			}
		case strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
			cur.IsBinary = true
		}
	}
	flushFile()
	return files
}

// stripDiffPrefix 去掉 git 的 a/ 与 b/ 前缀。
func stripDiffPrefix(p string) string {
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
}

// unquoteGitPath 还原 git 对含特殊字符路径的 C 风格引号转义。
func unquoteGitPath(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return strings.Trim(s, `"`)
	}
	return s
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// diffOptions 是 code_review_diff 的采集参数。
type diffOptions struct {
	Base   string
	Staged bool
	Paths  []string
}

// collectDiff 在统一工作空间根采集 unified diff 文本。
//
// 语义（写进工具描述，避免模型误解）：
//   - base 为空、staged=false：工作区全部改动（git diff HEAD，含暂存与未暂存）；
//     仓库尚无提交时退化为未暂存的 git diff。
//   - base 为空、staged=true：仅暂存区（git diff --cached）。
//   - base 非空：工作区相对该基线（git diff <base>，含未提交改动）；staged 时只看暂存区。
func collectDiff(ctx context.Context, opts diffOptions) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is required for code review but was not found in PATH")
	}
	root := workspaceRoot()
	if root == "" {
		return "", fmt.Errorf("workspace root is unknown")
	}
	if _, err := runGit(ctx, root, "rev-parse", "--is-inside-work-tree"); err != nil {
		return "", fmt.Errorf("not a git repository at %s: %v", root, err)
	}

	args := []string{"diff", "--no-color", "--no-ext-diff", "--find-renames", "-U" + strconv.Itoa(diffContextLines)}
	if opts.Staged {
		args = append(args, "--cached")
	}
	rev := strings.TrimSpace(opts.Base)
	if rev == "" && !opts.Staged {
		if _, err := runGit(ctx, root, "rev-parse", "--verify", "--quiet", "HEAD"); err == nil {
			rev = "HEAD"
		}
	}
	if rev != "" {
		args = append(args, rev)
	}
	if len(opts.Paths) > 0 {
		args = append(args, "--")
		args = append(args, opts.Paths...)
	}

	out, err := runGit(ctx, root, args...)
	if err != nil {
		return "", err
	}
	if len(out) > maxDiffBytes {
		return "", fmt.Errorf("diff is too large (%d bytes, limit %d); narrow the review with paths", len(out), maxDiffBytes)
	}
	return string(out), nil
}

// runGit 在 dir 执行 git 并返回 stdout；失败时带上 stderr 摘要。
func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	full := append([]string{"-c", "core.quotepath=false"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	dsc.ConfigureChildProcessAttrs(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}
