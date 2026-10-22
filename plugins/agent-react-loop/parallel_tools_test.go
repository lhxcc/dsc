package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"dsc/proto"
	"dsc/session"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc"
)

// parallelProbeTool 记录并发峰值与调用次数，并按 delay 模拟工具耗时。
type parallelProbeTool struct {
	proto.ToolServiceClient
	delay        time.Duration
	panicOn      string
	inFlight     atomic.Int32
	maxInFlight  atomic.Int32
	calls        atomic.Int32
	mu           sync.Mutex
	seenTools    []string
	sawSessionID string
}

func (p *parallelProbeTool) ExecuteTool(ctx context.Context, req *proto.ExecuteToolRequest, _ ...grpc.CallOption) (*proto.ExecuteToolResponse, error) {
	cur := p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	for {
		old := p.maxInFlight.Load()
		if cur <= old || p.maxInFlight.CompareAndSwap(old, cur) {
			break
		}
	}
	p.calls.Add(1)
	p.mu.Lock()
	p.seenTools = append(p.seenTools, req.ToolName)
	p.sawSessionID = req.SessionId
	p.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(p.delay):
	}
	if p.panicOn != "" && req.ToolName == p.panicOn {
		panic("boom")
	}
	return &proto.ExecuteToolResponse{Content: "ok:" + req.ToolName}, nil
}

func newParallelTestAgent(sess *session.Session) *ReactLoopAgent {
	return &ReactLoopAgent{
		logger:           hclog.NewNullLogger(),
		sess:             sess,
		historyInjection: -1,
		approvalPolicy:   "ask",
	}
}

func readOnlyToolProto(name string) *proto.Tool {
	return &proto.Tool{Name: name, Capabilities: []string{"read-only"}}
}

func call(id, name string) *proto.ToolCall {
	return &proto.ToolCall{Id: id, Name: name, ArgumentsJson: "{}"}
}

// TestReadOnlyBatchRunsInParallel 只读批应并发执行（并发峰值 >1，墙钟显著低于串行）。
func TestReadOnlyBatchRunsInParallel(t *testing.T) {
	sess := session.New()
	a := newParallelTestAgent(sess)
	probe := &parallelProbeTool{delay: 120 * time.Millisecond}
	calls := []*proto.ToolCall{call("c1", "r1"), call("c2", "r2"), call("c3", "r3")}
	tools := []*proto.Tool{readOnlyToolProto("r1"), readOnlyToolProto("r2"), readOnlyToolProto("r3")}

	start := time.Now()
	out := a.prefetchReadOnlyBatch(context.Background(), probe, calls, tools, false)
	elapsed := time.Since(start)

	if out == nil {
		t.Fatal("read-only batch should be parallelized")
	}
	if probe.calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", probe.calls.Load())
	}
	if probe.maxInFlight.Load() < 2 {
		t.Fatalf("max in-flight = %d, want >= 2", probe.maxInFlight.Load())
	}
	if elapsed >= 300*time.Millisecond {
		t.Fatalf("parallel batch took %v, want < 300ms (serial would be 360ms)", elapsed)
	}
	if probe.sawSessionID != sess.ID() {
		t.Fatalf("session id = %q, want %q", probe.sawSessionID, sess.ID())
	}
	for i := range calls {
		if out[i] == nil || out[i].err != nil || out[i].resp == nil {
			t.Fatalf("outcome[%d] = %+v, want successful response", i, out[i])
		}
	}
}

// TestMixedBatchStaysSerial 任一条不满足只读声明即整批回退串行（fail-closed）。
func TestMixedBatchStaysSerial(t *testing.T) {
	a := newParallelTestAgent(session.New())
	probe := &parallelProbeTool{delay: time.Millisecond}
	calls := []*proto.ToolCall{call("c1", "r1"), call("c2", "w1")}
	tools := []*proto.Tool{readOnlyToolProto("r1")} // w1 未声明能力

	if out := a.prefetchReadOnlyBatch(context.Background(), probe, calls, tools, false); out != nil {
		t.Fatalf("mixed batch must not be parallelized, got %d outcomes", len(out))
	}
	if probe.calls.Load() != 0 {
		t.Fatalf("prefetch must not execute when falling back to serial, calls = %d", probe.calls.Load())
	}
}

// TestParallelBatchDisabledByConfig DSC_TOOL_PARALLELISM=1 时等价旧串行行为。
func TestParallelBatchDisabledByConfig(t *testing.T) {
	t.Setenv("DSC_TOOL_PARALLELISM", "1")
	a := newParallelTestAgent(session.New())
	probe := &parallelProbeTool{delay: time.Millisecond}
	calls := []*proto.ToolCall{call("c1", "r1"), call("c2", "r2")}
	tools := []*proto.Tool{readOnlyToolProto("r1"), readOnlyToolProto("r2")}

	if out := a.prefetchReadOnlyBatch(context.Background(), probe, calls, tools, false); out != nil {
		t.Fatalf("parallelism=1 must disable prefetch, got %d outcomes", len(out))
	}
}

// TestParallelBatchPanicDoesNotCrash 单个调用 panic 转为错误结果，不崩溃进程。
func TestParallelBatchPanicDoesNotCrash(t *testing.T) {
	a := newParallelTestAgent(session.New())
	probe := &parallelProbeTool{delay: time.Millisecond, panicOn: "r2"}
	calls := []*proto.ToolCall{call("c1", "r1"), call("c2", "r2")}
	tools := []*proto.Tool{readOnlyToolProto("r1"), readOnlyToolProto("r2")}

	out := a.prefetchReadOnlyBatch(context.Background(), probe, calls, tools, false)
	if out == nil {
		t.Fatal("expected parallel outcomes")
	}
	if out[0] == nil || out[0].err != nil {
		t.Fatalf("outcome[0] = %+v, want success", out[0])
	}
	if out[1] == nil || out[1].err == nil {
		t.Fatalf("outcome[1] = %+v, want panic converted to error", out[1])
	}
}

// TestParallelBatchCancelledFillsAllResults 取消后每个调用仍有结果（错误），
// 保证每个 tool_call 都能落一条 tool/result。
func TestParallelBatchCancelledFillsAllResults(t *testing.T) {
	a := newParallelTestAgent(session.New())
	probe := &parallelProbeTool{delay: 50 * time.Millisecond}
	calls := []*proto.ToolCall{call("c1", "r1"), call("c2", "r2")}
	tools := []*proto.Tool{readOnlyToolProto("r1"), readOnlyToolProto("r2")}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := a.prefetchReadOnlyBatch(ctx, probe, calls, tools, false)
	if out == nil {
		t.Fatal("expected parallel outcomes even when cancelled")
	}
	for i := range calls {
		if out[i] == nil || out[i].err == nil {
			t.Fatalf("outcome[%d] = %+v, want cancellation error", i, out[i])
		}
	}
}
