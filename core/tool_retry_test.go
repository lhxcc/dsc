package core

import (
	"context"
	"encoding/json"
	"errors"
	"syscall"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// flakyTool 前 failures 次返回 err，之后成功；记录实际尝试次数。
type flakyTool struct {
	name     string
	caps     []string
	err      error
	failures int
	attempts int
}

func (f *flakyTool) Name() string                      { return f.name }
func (f *flakyTool) Description() string               { return "flaky test tool" }
func (f *flakyTool) ParametersSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (f *flakyTool) Capabilities() []string            { return f.caps }
func (f *flakyTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	f.attempts++
	if f.attempts <= f.failures {
		return "", f.err
	}
	return "ok", nil
}

func TestIsTransientToolError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "连接重置", err: syscall.ECONNRESET, want: true},
		{name: "连接拒绝", err: syscall.ECONNREFUSED, want: true},
		{name: "broken pipe", err: syscall.EPIPE, want: true},
		{name: "EOF", err: status.Error(codes.Unavailable, "unavailable"), want: true},
		{name: "取消", err: context.Canceled, want: false},
		{name: "超时", err: context.DeadlineExceeded, want: false},
		{name: "工具超时", err: &ToolTimeoutError{Tool: "t", Ms: 100}, want: false},
		{name: "普通业务错误", err: errors.New("bad args"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTransientToolError(tt.err); got != tt.want {
				t.Fatalf("isTransientToolError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestToolRetryReadOnlyTransientSucceeds 只读工具连接级瞬态失败应重试并最终成功，
// 且 pre/post 流水线阶段各只执行一次（重试只包裹执行体）。
func TestToolRetryReadOnlyTransientSucceeds(t *testing.T) {
	m := newPipelineManager(t)
	tool := &flakyTool{name: "flaky-read", caps: []string{CapabilityReadOnly}, err: syscall.ECONNRESET, failures: 2}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	var pre, post int
	m.events.OnWaterfall(EventToolPreExecute, func(ev EventContext, next func(EventContext) error) error {
		pre++
		return next(ev)
	})
	m.events.OnWaterfall(EventToolPostExecute, func(ev EventContext, next func(EventContext) error) error {
		post++
		return next(ev)
	})

	res, err := m.ExecuteTool(context.Background(), "flaky-read", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if res != "ok" {
		t.Fatalf("result = %q, want ok", res)
	}
	if tool.attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (1 + 2 retries)", tool.attempts)
	}
	if pre != 1 || post != 1 {
		t.Fatalf("pre/post = %d/%d, want 1/1 (retry must stay inside execute body)", pre, post)
	}
}

// TestToolRetrySkippedForWriteTool 写语义调用不重试（避免重复副作用）。
func TestToolRetrySkippedForWriteTool(t *testing.T) {
	m := newPipelineManager(t)
	tool := &flakyTool{name: "flaky-write", caps: []string{CapabilityWritesUnknown}, err: syscall.ECONNRESET, failures: 1}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := m.ExecuteTool(context.Background(), "flaky-write", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error")
	}
	if tool.attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (write semantics must not retry)", tool.attempts)
	}
}

// TestToolRetrySkippedForUndeclaredTool 未声明能力的旧工具不重试（fail-closed）。
func TestToolRetrySkippedForUndeclaredTool(t *testing.T) {
	m := newPipelineManager(t)
	tool := &flakyTool{name: "legacy-tool", err: syscall.ECONNRESET, failures: 1}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := m.ExecuteTool(context.Background(), "legacy-tool", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error")
	}
	if tool.attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (undeclared capability must not retry)", tool.attempts)
	}
}

// TestToolRetryDisabledByEnv DSC_TOOL_RETRY=0 关闭重试。
func TestToolRetryDisabledByEnv(t *testing.T) {
	t.Setenv("DSC_TOOL_RETRY", "0")
	m := newPipelineManager(t)
	tool := &flakyTool{name: "flaky-env", caps: []string{CapabilityReadOnly}, err: syscall.ECONNRESET, failures: 1}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := m.ExecuteTool(context.Background(), "flaky-env", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected error")
	}
	if tool.attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (retry disabled)", tool.attempts)
	}
}

// TestToolRetrySkippedOnTimeout 超时是策略预算，不重试（否则墙钟翻倍）。
func TestToolRetrySkippedOnTimeout(t *testing.T) {
	m := newPipelineManager(t)
	tool := &flakyTool{name: "flaky-timeout", caps: []string{CapabilityReadOnly}, err: context.DeadlineExceeded, failures: 1}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := m.ExecuteTool(context.Background(), "flaky-timeout", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected timeout error")
	}
	if tool.attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (timeout must not retry)", tool.attempts)
	}
}
