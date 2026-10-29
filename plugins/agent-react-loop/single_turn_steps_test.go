package main

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	dsc "dsc-sdk"
	"dsc/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// 本文件回归「单轮模式（-input / -headless）只发一次 LLM 请求」的缺陷：
//
// 单轮模式曾把迭代上限硬设成 1，本意是「跑完一轮自然退出」，实际含义却是「只请求一次
// 模型」。模型一调用工具，这一轮就在工具结果之后结束，再没有第二次请求去用结果作答：
// `dsc -input` 永远只打印工具卡片、没有答案，MCP 那种 connect→list→load→call 的多步
// 流程根本无法在自动化里验证。
//
// 约束是「单轮 = 不自动续行」，不是「不许第二步」。

// toolCallOnceLLM 第一次请求返回一次工具调用，第二次返回最终答案。
type toolCallOnceLLM struct {
	proto.LLMServiceClient

	mu    sync.Mutex
	calls int
}

func (m *toolCallOnceLLM) ChatStream(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[proto.ChatStreamResponse], error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	return &toolCallOnceStream{call: call}, nil
}

func (m *toolCallOnceLLM) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

type toolCallOnceStream struct{ call, recv int }

func (s *toolCallOnceStream) Recv() (*proto.ChatStreamResponse, error) {
	s.recv++
	if s.recv > 1 {
		return nil, io.EOF
	}
	if s.call == 1 {
		return &proto.ChatStreamResponse{
			FinishReason: "tool_calls",
			ToolCalls: []*proto.ToolCall{{
				Id:            "call-1",
				Name:          "shell",
				ArgumentsJson: `{"command":"echo hi"}`,
			}},
		}, nil
	}
	// 第二步：用工具结果作答（真实模型在这一步给出最终回答）
	return &proto.ChatStreamResponse{Content: "结果是 hi", FinishReason: "stop"}, nil
}

func (s *toolCallOnceStream) Header() (metadata.MD, error) { return nil, nil }
func (s *toolCallOnceStream) Trailer() metadata.MD         { return nil }
func (s *toolCallOnceStream) CloseSend() error             { return nil }
func (s *toolCallOnceStream) Context() context.Context     { return context.Background() }
func (s *toolCallOnceStream) SendMsg(m any) error          { return nil }
func (s *toolCallOnceStream) RecvMsg(m any) error          { return nil }

// countingToolClient 记录 ExecuteTool 次数并返回固定结果。
type countingToolClient struct {
	proto.ToolServiceClient

	mu       sync.Mutex
	executes int
}

func (m *countingToolClient) ListTools(ctx context.Context, in *proto.ListToolsRequest, opts ...grpc.CallOption) (*proto.ListToolsResponse, error) {
	return &proto.ListToolsResponse{}, nil
}

func (m *countingToolClient) ListContext(ctx context.Context, in *proto.ListContextRequest, opts ...grpc.CallOption) (*proto.ListContextResponse, error) {
	return &proto.ListContextResponse{}, nil
}

func (m *countingToolClient) ExecuteTool(ctx context.Context, in *proto.ExecuteToolRequest, opts ...grpc.CallOption) (*proto.ExecuteToolResponse, error) {
	m.mu.Lock()
	m.executes++
	m.mu.Unlock()
	return &proto.ExecuteToolResponse{Content: "hi"}, nil
}

func (m *countingToolClient) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.executes
}

// TestSingleTurnContinuesAfterToolCall 单轮模式下工具调用之后必须继续下一步并给出答案。
func TestSingleTurnContinuesAfterToolCall(t *testing.T) {
	a := newTestAgent(t)
	a.singleTurn = true
	a.llmServiceID = 1
	a.toolServiceID = 1
	llm := &toolCallOnceLLM{}
	tools := &countingToolClient{}
	a.llmClient = llm
	a.toolClient = tools

	type outcome struct {
		res *agentResultShim
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := a.runLoop(context.Background(), "跑一下 echo", nil, func(*dsc.RunStreamResponse) {})
		if res == nil {
			done <- outcome{nil, err}
			return
		}
		done <- outcome{&agentResultShim{Output: res.Output, Status: res.Status}, err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("runLoop err: %v", got.err)
		}
		if got.res.Status != "success" {
			t.Fatalf("状态 = %q, 期望 success（单轮模式不应在工具执行后中断）", got.res.Status)
		}
		if !strings.Contains(got.res.Output, "结果是 hi") {
			t.Fatalf("输出 = %q, 期望含模型最终回答（工具结果之后必须再请求一次模型）", got.res.Output)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runLoop 超时：单轮模式下工具调用之后没有继续下一步")
	}

	if got := tools.count(); got != 1 {
		t.Fatalf("ExecuteTool 次数 = %d, 期望 1", got)
	}
	if got := llm.count(); got != 2 {
		t.Fatalf("ChatStream 次数 = %d, 期望 2（工具调用 → 用结果作答）", got)
	}
}

// agentResultShim 只取断言需要的两个字段，避免测试依赖 dsc.AgentResult 的全部字段。
type agentResultShim struct {
	Output string
	Status string
}
