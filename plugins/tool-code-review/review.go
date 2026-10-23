// review.go：评审单元的 LLM 调度、结果解析与行号校验。
//
// 职责划分对齐 open-code-review 的「确定性工程 × agent」：模型只负责在已圈定的
// 上下文里判断缺陷；「评论能否落地」由代码校验——路径必须属于本单元、行号必须
// 命中新增行（±3 行内吸附，否则丢弃）。这样模型的小幅行号漂移不会污染结果。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dsc/core/llmclient"
	"dsc/proto"
)

// chatStream 是流式响应的最小接口（*llmclient.Client 返回的流满足）。
type chatStream interface {
	Recv() (*proto.ChatStreamResponse, error)
}

// chatClient 是宿主聚合 LLM 的最小接口。
//
// 只走流式：推理模型的 unary Chat 可能只产出思考块而正文为空（SDK 已注明），
// 评审必须拿到正文才能解析 JSON。
type chatClient interface {
	ChatStream(ctx context.Context, messages []*proto.Message, maxTokens int32) (chatStream, error)
}

// llmAdapter 收敛 *llmclient.Client 的方法签名（返回最小流接口），
// 使单测可以注入假流而不必构造 gRPC 流。
type llmAdapter struct {
	c *llmclient.Client
}

func (a llmAdapter) ChatStream(ctx context.Context, messages []*proto.Message, maxTokens int32) (chatStream, error) {
	st, err := a.c.ChatStream(ctx, messages, maxTokens)
	if err != nil {
		return nil, err
	}
	return st, nil
}

// comment 是一条可落地的评审意见。
type comment struct {
	Path           string `json:"path"`
	StartLine      int    `json:"start_line"`
	EndLine        int    `json:"end_line"`
	Severity       string `json:"severity"`
	Category       string `json:"category"`
	Content        string `json:"content"`
	SuggestionCode string `json:"suggestion_code,omitempty"`
}

// unitFile 是一个评审单元内允许落评论的文件。
// diff 模式用 Added（新增行集合）；scan 模式用 [MinLine, MaxLine] 区间。
type unitFile struct {
	Path     string
	DiffMode bool
	Added    map[int]bool
	MinLine  int
	MaxLine  int
}

// reviewUnit 是一次独立的 LLM 评审请求。
type reviewUnit struct {
	Label  string
	System string
	User   string
	Files  []unitFile
}

// reviewOptions 控制并发、超时与结果过滤。
type reviewOptions struct {
	Parallelism int
	Timeout     time.Duration
	MaxTokens   int32
	SeverityMin string
	MaxFindings int
}

func defaultReviewOptions() reviewOptions {
	return reviewOptions{
		Parallelism: envInt("DSC_CODE_REVIEW_PARALLELISM", 3, 1, 8),
		Timeout:     envDuration("DSC_CODE_REVIEW_TIMEOUT", 120*time.Second),
		MaxTokens:   int32(envInt("DSC_CODE_REVIEW_MAX_TOKENS", 8192, 512, 32768)),
		MaxFindings: envInt("DSC_CODE_REVIEW_MAX_FINDINGS", 100, 1, 1000),
	}
}

// reviewOutcome 汇总一次评审的全部产出。
type reviewOutcome struct {
	Comments  []comment
	Dropped   int  // 行号/路径/内容不合法的评论数
	Truncated bool // 超过 MaxFindings 被截断
	Errors    []string
}

// runReview 并发执行各评审单元。单个单元失败不影响其余单元——失败原因随结果
// 返回，避免「一个文件把整轮评审拖垮」。
func runReview(ctx context.Context, llm chatClient, units []*reviewUnit, opts reviewOptions) reviewOutcome {
	var out reviewOutcome
	if len(units) == 0 {
		return out
	}
	if opts.Parallelism <= 0 {
		opts.Parallelism = 1
	}

	type result struct {
		comments []comment
		dropped  int
		err      string
	}
	results := make([]result, len(units))
	sem := make(chan struct{}, opts.Parallelism)
	var wg sync.WaitGroup

	for i, u := range units {
		i, u := i, u
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			defer func() {
				// LLM 客户端/序列化异常不得带崩插件进程（工具执行的三层 recover 之一）。
				if r := recover(); r != nil {
					results[i] = result{err: fmt.Sprintf("%s: review panicked: %v", u.Label, r)}
				}
			}()
			cs, dropped, err := reviewUnitCall(ctx, llm, u, opts)
			if err != nil {
				results[i] = result{err: err.Error()}
				return
			}
			results[i] = result{comments: cs, dropped: dropped}
		}()
	}
	wg.Wait()

	for _, r := range results {
		out.Comments = append(out.Comments, r.comments...)
		out.Dropped += r.dropped
		if r.err != "" {
			out.Errors = append(out.Errors, r.err)
		}
	}
	sort.Strings(out.Errors)
	sortComments(out.Comments)
	if opts.MaxFindings > 0 && len(out.Comments) > opts.MaxFindings {
		out.Comments = out.Comments[:opts.MaxFindings]
		out.Truncated = true
	}
	return out
}

// reviewUnitCall 对单个单元发起一次 LLM 调用并校验结果。
func reviewUnitCall(ctx context.Context, llm chatClient, u *reviewUnit, opts reviewOptions) ([]comment, int, error) {
	callCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	content, finish, err := streamComplete(callCtx, llm, []*proto.Message{
		{Role: "system", Content: u.System},
		{Role: "user", Content: u.User},
	}, opts.MaxTokens)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", u.Label, err)
	}
	content = strings.TrimSpace(content)
	switch finish {
	case "length", "max_tokens":
		// 截断的 JSON 一定解析失败；直接说明原因，提示缩小范围或提高输出上限。
		if content == "" {
			return nil, 0, fmt.Errorf("%s: LLM output was truncated (finish_reason=%q) before any content; "+
				"raise DSC_CODE_REVIEW_MAX_TOKENS or narrow the review", u.Label, finish)
		}
		return nil, 0, fmt.Errorf("%s: LLM output was truncated (finish_reason=%q); narrow paths or reduce bundle size", u.Label, finish)
	}
	if content == "" {
		return nil, 0, fmt.Errorf("%s: LLM returned an empty response (finish_reason=%q)", u.Label, finish)
	}

	raw, err := parseComments(content)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", u.Label, err)
	}
	kept, dropped := normalizeComments(raw, u, opts)
	return kept, dropped, nil
}

// streamComplete 消费流式响应并聚合正文；finish_reason 取最后一个非空帧。
func streamComplete(ctx context.Context, llm chatClient, messages []*proto.Message, maxTokens int32) (string, string, error) {
	stream, err := llm.ChatStream(ctx, messages, maxTokens)
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	finish := ""
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return b.String(), finish, nil
		}
		if err != nil {
			return "", "", err
		}
		if msg := frame.GetError(); msg != "" {
			return "", "", errors.New(msg)
		}
		b.WriteString(frame.GetContent())
		if fr := frame.GetFinishReason(); fr != "" {
			finish = fr
		}
	}
}

var severityRanks = map[string]int{"low": 0, "medium": 1, "high": 2, "critical": 3}

var validCategories = map[string]bool{
	"bug": true, "security": true, "performance": true, "maintainability": true,
	"test": true, "documentation": true, "style": true, "other": true,
}

// normalizeSeverity 归一化外部传入的级别；无法识别时返回空（表示不过滤）。
func normalizeSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := severityRanks[s]; ok {
		return s
	}
	return ""
}

// normalizeComments 校验并修正模型输出：路径归属、行号吸附、枚举归一化、去重。
// 返回保留的评论与被丢弃（路径/行号/内容不合法）的数量。
func normalizeComments(raw []comment, u *reviewUnit, opts reviewOptions) ([]comment, int) {
	kept := make([]comment, 0, len(raw))
	seen := make(map[string]bool)
	dropped := 0

	for _, c := range raw {
		path, ok := u.canonicalPath(c.Path)
		if !ok {
			dropped++
			continue
		}
		line, ok := u.placeLine(path, c.StartLine)
		if !ok {
			dropped++
			continue
		}
		end := c.EndLine
		if end < line {
			end = line
		}
		if max := u.maxLine(path); max > 0 && end > max {
			end = max
		}

		sev := strings.ToLower(strings.TrimSpace(c.Severity))
		if _, ok := severityRanks[sev]; !ok {
			sev = "medium"
		}
		if opts.SeverityMin != "" && severityRanks[sev] < severityRanks[opts.SeverityMin] {
			continue
		}

		cat := strings.ToLower(strings.TrimSpace(c.Category))
		if !validCategories[cat] {
			cat = "other"
		}
		content := strings.TrimSpace(c.Content)
		if content == "" {
			dropped++
			continue
		}
		key := path + "|" + strconv.Itoa(line) + "|" + content
		if seen[key] {
			continue
		}
		seen[key] = true

		kept = append(kept, comment{
			Path:           path,
			StartLine:      line,
			EndLine:        end,
			Severity:       sev,
			Category:       cat,
			Content:        content,
			SuggestionCode: strings.TrimSpace(c.SuggestionCode),
		})
	}
	return kept, dropped
}

// canonicalPath 把模型写的路径归一到本单元的真实文件：
// 允许 "./x"、反斜杆、绝对路径后缀与唯一 basename。
func (u *reviewUnit) canonicalPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, `\\`, "/")
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return "", false
	}

	var match string
	hits := 0
	for _, f := range u.Files {
		switch {
		case f.Path == p:
			return f.Path, true
		case strings.HasSuffix(f.Path, "/"+p):
			match, hits = f.Path, hits+1
		case baseName(f.Path) == p:
			match, hits = f.Path, hits+1
		}
	}
	if hits == 1 {
		return match, true
	}
	return "", false
}

// placeLine 把评论行号吸附到真实可评论行：diff 模式只接受新增行（±3 行内吸附），
// scan 模式接受文件区间内任意行。
func (u *reviewUnit) placeLine(path string, line int) (int, bool) {
	f, ok := u.file(path)
	if !ok || line <= 0 {
		return 0, false
	}
	if !f.DiffMode {
		if line < f.MinLine || line > f.MaxLine {
			return 0, false
		}
		return line, true
	}
	if f.Added[line] {
		return line, true
	}
	for d := 1; d <= 3; d++ {
		if f.Added[line+d] {
			return line + d, true
		}
		if line-d > 0 && f.Added[line-d] {
			return line - d, true
		}
	}
	return 0, false
}

func (u *reviewUnit) file(path string) (unitFile, bool) {
	for _, f := range u.Files {
		if f.Path == path {
			return f, true
		}
	}
	return unitFile{}, false
}

func (u *reviewUnit) maxLine(path string) int {
	if f, ok := u.file(path); ok {
		return f.MaxLine
	}
	return 0
}

// parseComments 容错解析模型输出：允许 ```json 围栏、{"comments": [...]} 包裹、
// 行号写成字符串，以及数组前后的解释文字。
func parseComments(raw string) ([]comment, error) {
	text := stripCodeFence(raw)

	var list []map[string]any
	if arr, ok := sliceBetween(text, '[', ']'); ok {
		if err := json.Unmarshal([]byte(arr), &list); err != nil {
			return nil, fmt.Errorf("parse review comments: %w", err)
		}
	} else if obj, ok := sliceBetween(text, '{', '}'); ok {
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal([]byte(obj), &wrapper); err != nil {
			return nil, fmt.Errorf("parse review comments: %w", err)
		}
		found := false
		for _, key := range []string{"comments", "findings", "issues"} {
			if b, ok := wrapper[key]; ok {
				if err := json.Unmarshal(b, &list); err != nil {
					return nil, fmt.Errorf("parse review comments (%s): %w", key, err)
				}
				found = true
				break
			}
		}
		if !found {
			// 单条评论对象也接受
			var one map[string]any
			if err := json.Unmarshal([]byte(obj), &one); err == nil {
				list = []map[string]any{one}
			}
		}
	} else {
		return nil, fmt.Errorf("LLM response contains no JSON array of comments")
	}

	out := make([]comment, 0, len(list))
	for _, m := range list {
		c := comment{
			Path:           asString(m["path"]),
			StartLine:      asInt(firstPresent(m, "start_line", "startLine", "line")),
			EndLine:        asInt(firstPresent(m, "end_line", "endLine")),
			Severity:       asString(m["severity"]),
			Category:       asString(m["category"]),
			Content:        asString(firstPresent(m, "content", "message", "body")),
			SuggestionCode: asString(firstPresent(m, "suggestion_code", "suggestionCode", "suggestion")),
		}
		out = append(out, c)
	}
	return out, nil
}

func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// sliceBetween 返回文本中从第一个 open 到最后一个 close 的子串。
func sliceBetween(s string, open, close byte) (string, bool) {
	i := strings.IndexByte(s, open)
	if i < 0 {
		return "", false
	}
	j := strings.LastIndexByte(s, close)
	if j <= i {
		return "", false
	}
	return s[i : j+1], true
}

func firstPresent(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			return v
		}
	}
	return nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

func asInt(v any) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	case json.Number:
		n, _ := t.Int64()
		return int(n)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(t))
		return n
	}
	return 0
}

func sortComments(cs []comment) {
	sort.SliceStable(cs, func(i, j int) bool {
		a, b := cs[i], cs[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		return a.Content < b.Content
	})
}

func envInt(name string, def, min, max int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}

func envDuration(name string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
