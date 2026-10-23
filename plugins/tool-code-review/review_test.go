package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"dsc/proto"
)

// fakeStream 是 chatStream 的测试替身：按序吐出预设帧，用尽返回 io.EOF。
type fakeStream struct {
	frames []*proto.ChatStreamResponse
	i      int
}

func (s *fakeStream) Recv() (*proto.ChatStreamResponse, error) {
	if s.i >= len(s.frames) {
		return nil, io.EOF
	}
	f := s.frames[s.i]
	s.i++
	return f, nil
}

// fakeLLM 是 chatClient 的测试替身：可按序返回预设响应（用尽后返回 "[]"），
// 也可用 fn 按请求内容动态应答。
type fakeLLM struct {
	mu       sync.Mutex
	replies  []string
	finish   string
	err      error
	fn       func(msgs []*proto.Message) (chatStream, error)
	calls    int
	lastMsgs []*proto.Message
}

func (f *fakeLLM) ChatStream(_ context.Context, msgs []*proto.Message, _ int32) (chatStream, error) {
	f.mu.Lock()
	f.calls++
	f.lastMsgs = msgs
	fn := f.fn
	err := f.err
	content := "[]"
	finish := f.finish
	if len(f.replies) > 0 {
		content = f.replies[0]
		f.replies = f.replies[1:]
	}
	f.mu.Unlock()

	if fn != nil {
		return fn(msgs)
	}
	if err != nil {
		return nil, err
	}
	return &fakeStream{frames: []*proto.ChatStreamResponse{{Content: content, FinishReason: finish}}}, nil
}

func TestParseCommentsFormats(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int
	}{
		{"plain array", `[{"path":"a.go","start_line":2,"severity":"high","content":"boom"}]`, 1},
		{"fenced array", "```json\n[{\"path\":\"a.go\",\"start_line\":2,\"content\":\"boom\"}]\n```", 1},
		{"wrapper object", `{"comments":[{"path":"a.go","start_line":2,"content":"boom"}]}`, 1},
		{"single object", `{"path":"a.go","start_line":2,"content":"boom"}`, 1},
		{"string line and camelCase", `[{"path":"a.go","startLine":"7","endLine":"8","content":"boom","suggestionCode":"fix"}]`, 1},
		{"prose around array", `发现问题如下：[{"path":"a.go","start_line":2,"content":"boom"}] 以上。`, 1},
	}
	for _, c := range cases {
		got, err := parseComments(c.raw)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if len(got) != c.want {
			t.Errorf("%s: got %d comments, want %d", c.name, len(got), c.want)
		}
	}

	if _, err := parseComments("no json here"); err == nil {
		t.Error("expected error for non-JSON response")
	}

	got, err := parseComments(`[{"path":"a.go","start_line":2,"content":"boom","suggestion_code":"x"}]`)
	if err != nil || got[0].SuggestionCode != "x" || got[0].StartLine != 2 {
		t.Fatalf("fields = %+v, err = %v", got, err)
	}
}

func TestNormalizeCommentsSnapAndFilter(t *testing.T) {
	u := &reviewUnit{Files: []unitFile{{
		Path: "dir/a.go", DiffMode: true,
		Added:   map[int]bool{2: true, 3: true},
		MaxLine: 40,
	}}}
	opts := reviewOptions{SeverityMin: "high"}
	raw := []comment{
		{Path: "a.go", StartLine: 2, Severity: "high", Category: "bug", Content: "first"},         // basename 匹配
		{Path: "dir/a.go", StartLine: 4, Severity: "high", Category: "Bug", Content: "snap back"}, // 4→3 吸附
		{Path: "other.go", StartLine: 2, Severity: "high", Content: "wrong file"},                 // 丢弃
		{Path: "a.go", StartLine: 20, Severity: "high", Content: "far away"},                      // 丢弃
		{Path: "a.go", StartLine: 2, Severity: "high", Content: ""},                               // 丢弃
		{Path: "a.go", StartLine: 2, Severity: "low", Content: "low thing"},                       // 被级别过滤
		{Path: "a.go", StartLine: 2, Severity: "high", Category: "bug", Content: "first"},         // 去重
		{Path: "dir/a.go", StartLine: 3, Severity: "HIGH", Category: "wat", Content: "normalize"}, // 枚举归一化
	}
	kept, dropped := normalizeComments(raw, u, opts)
	if dropped != 3 {
		t.Errorf("dropped = %d, want 3", dropped)
	}
	if len(kept) != 3 {
		t.Fatalf("kept = %+v, want 3", kept)
	}
	var snapped, normalized *comment
	for i := range kept {
		switch kept[i].Content {
		case "snap back":
			snapped = &kept[i]
		case "normalize":
			normalized = &kept[i]
		}
	}
	if snapped == nil || snapped.Path != "dir/a.go" || snapped.StartLine != 3 || snapped.EndLine != 3 || snapped.Severity != "high" {
		t.Errorf("snapped comment = %+v", snapped)
	}
	if normalized == nil || normalized.Category != "other" || normalized.Severity != "high" {
		t.Errorf("normalized comment = %+v", normalized)
	}
}

func TestNormalizeCommentsScanRange(t *testing.T) {
	u := &reviewUnit{Files: []unitFile{{Path: "x.py", MinLine: 10, MaxLine: 20}}}
	raw := []comment{
		{Path: "x.py", StartLine: 15, Content: "ok", Severity: "medium"},
		{Path: "x.py", StartLine: 5, Content: "below range", Severity: "medium"},
		{Path: "x.py", StartLine: 21, Content: "above range", Severity: "medium"},
	}
	kept, dropped := normalizeComments(raw, u, reviewOptions{})
	if len(kept) != 1 || kept[0].StartLine != 15 {
		t.Fatalf("kept = %+v", kept)
	}
	if dropped != 2 {
		t.Fatalf("dropped = %d, want 2", dropped)
	}
}

func TestRunReviewIsolatesUnitFailureAndSorts(t *testing.T) {
	// 按请求内容路由：c.go 的单元失败，b.go 的单元成功——并发下也不依赖调用顺序。
	llm := &fakeLLM{fn: func(msgs []*proto.Message) (chatStream, error) {
		user := ""
		if len(msgs) > 0 {
			user = msgs[len(msgs)-1].Content
		}
		if strings.Contains(user, "c.go") {
			return nil, errors.New("connection refused")
		}
		return &fakeStream{frames: []*proto.ChatStreamResponse{{
			Content:      `[{"path":"b.go","start_line":1,"severity":"high","content":"b"}]`,
			FinishReason: "stop",
		}}}, nil
	}}
	units := []*reviewUnit{
		{Label: "u1", User: "scan b.go", Files: []unitFile{{Path: "b.go", MinLine: 1, MaxLine: 10}}},
		{Label: "u2", User: "scan c.go", Files: []unitFile{{Path: "c.go", MinLine: 1, MaxLine: 10}}},
	}
	opts := reviewOptions{Parallelism: 2, Timeout: 5 * time.Second, MaxTokens: 512, MaxFindings: 10}
	out := runReview(context.Background(), llm, units, opts)
	if len(out.Comments) != 1 || out.Comments[0].Path != "b.go" {
		t.Fatalf("comments = %+v", out.Comments)
	}
	if len(out.Errors) != 1 {
		t.Fatalf("errors = %v, want 1 (单个单元失败不影响其他)", out.Errors)
	}
	if llm.calls != 2 {
		t.Fatalf("calls = %d, want 2", llm.calls)
	}
}

func TestRunReviewTruncationAndLLMError(t *testing.T) {
	llm := &fakeLLM{replies: []string{
		`[{"path":"a.go","start_line":1,"content":"1"},{"path":"a.go","start_line":2,"content":"2"}]`,
	}}
	units := []*reviewUnit{{Label: "u", Files: []unitFile{{Path: "a.go", MinLine: 1, MaxLine: 9}}}}
	out := runReview(context.Background(), llm, units, reviewOptions{Parallelism: 1, Timeout: time.Second, MaxTokens: 128, MaxFindings: 1})
	if len(out.Comments) != 1 || !out.Truncated {
		t.Fatalf("out = %+v", out)
	}

	failing := &fakeLLM{err: errors.New("connection refused")}
	out = runReview(context.Background(), failing, units, reviewOptions{Parallelism: 1, Timeout: time.Second, MaxTokens: 128})
	if len(out.Errors) != 1 {
		t.Fatalf("errors = %v", out.Errors)
	}
}

func TestReviewUnitCallTruncatedResponse(t *testing.T) {
	llm := &fakeLLM{finish: "length", replies: []string{`[{"path":"a.go"`}}
	u := &reviewUnit{Label: "u", Files: []unitFile{{Path: "a.go", MinLine: 1, MaxLine: 9}}}
	_, _, err := reviewUnitCall(context.Background(), llm, u, reviewOptions{Timeout: time.Second, MaxTokens: 128})
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !strings.Contains(err.Error(), "truncated") {
		t.Fatalf("err = %v", err)
	}
}

func TestSortCommentsDeterministic(t *testing.T) {
	cs := []comment{
		{Path: "b.go", StartLine: 1, Content: "z"},
		{Path: "a.go", StartLine: 5, Content: "y"},
		{Path: "a.go", StartLine: 1, Content: "x"},
	}
	sortComments(cs)
	got := []string{cs[0].Path + cs[0].Content, cs[1].Path + cs[1].Content, cs[2].Path + cs[2].Content}
	want := []string{"a.gox", "a.goy", "b.goz"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}
