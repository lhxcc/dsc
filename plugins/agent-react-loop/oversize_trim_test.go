package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"dsc/core"
	"dsc/proto"
	"dsc/session"
	"google.golang.org/grpc"
)

// 本文件覆盖「历史总量超出上下文窗口」的兜底：从 1M 云端切回 128K 本地这类场景。
//
// 修复前的真实行为（探针实测）：内联压缩把整段历史交给同一个模型做摘要，压缩请求自身
// 就超窗 → `compact history failed: context length exceeded: 801207 > 131072`，整轮失败，
// 既没压缩也没发主请求。
//
// 修复后：先把派生历史裁进窗口（只动本次请求，不动会话日志），再让压缩处理窗口内的部分。
// 不变量：**发出去的请求绝不超过窗口**；单轮自身超窗则明确报错而非盲发。

// windowEnforcingLLM 像真实 provider 那样在输入超窗时拒绝（Chat 与 ChatStream 都拒），
// 从而证明「最终发出的请求」确实在窗口内，而不只是「我们认为它在窗口内」。
type windowEnforcingLLM struct {
	compactMockLLM

	window int

	mu         sync.Mutex
	chatInputs []int
	streamIn   []int
	streamMsgs []*proto.Message
}

func (m *windowEnforcingLLM) Chat(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (*proto.ChatResponse, error) {
	n := core.EstimateProtoMessagesTokens(in.Messages)
	m.mu.Lock()
	m.chatInputs = append(m.chatInputs, n)
	m.mu.Unlock()
	if n > m.window {
		return nil, fmt.Errorf("context length exceeded: %d > %d", n, m.window)
	}
	return &proto.ChatResponse{Content: "【压缩摘要】此前对话的要点。"}, nil
}

func (m *windowEnforcingLLM) ChatStream(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[proto.ChatStreamResponse], error) {
	n := core.EstimateProtoMessagesTokens(in.Messages)
	m.mu.Lock()
	m.streamIn = append(m.streamIn, n)
	m.streamMsgs = in.Messages
	m.mu.Unlock()
	if n > m.window {
		return nil, fmt.Errorf("context length exceeded: %d > %d", n, m.window)
	}
	return &compactMockStream{}, nil
}

// buildOversizeSession 构造 rounds 轮、每条约 perMsgTokens 估算 token 的历史（含轮次标记，
// 便于断言「会话全量历史仍在」）。总量取 rounds*2*perMsg，远大于测试用的窗口。
func buildOversizeSession(t *testing.T, a *ReactLoopAgent, rounds, perMsgTokens int) {
	t.Helper()
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	body := strings.Repeat("x", perMsgTokens) // ASCII：估算 ≈ 字节 = perMsgTokens
	for i := 0; i < rounds; i++ {
		mark := fmt.Sprintf("#round-%d", i)
		a.sess.Append(session.UserMessage, &session.UserMessageData{Content: mark + body, Source: "user"},
			&session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.AssistantMessage, &session.AssistantMessageData{Content: mark + body},
			&session.SurfaceOp{Op: session.SurfaceAppend})
	}
	markSessionRestored(t, a)
}

// TestOversizeHistoryTrimmedToWindow 历史远超窗口时：不报错、照常请求、且发出去的请求
// 一定在窗口内；会话日志不删除任何记录（切回大窗口模型时全量历史可原样恢复）。
func TestOversizeHistoryTrimmedToWindow(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	buildOversizeSession(t, a, 10, 10000) // 20 条 × 10k ≈ 200K 估算 = 4 倍窗口

	before := core.EstimateProtoMessagesTokens(a.sess.DeriveMessages())
	if before <= a.contextWindow*2 {
		t.Fatalf("夹具不够大：估算 %d，应至少为窗口两倍", before)
	}

	llm := &windowEnforcingLLM{window: a.contextWindow}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	var mu sync.Mutex
	var frames []*core.RunStreamResponse
	res, err := a.runLoop(context.Background(), "继续", nil, func(f *core.RunStreamResponse) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("超窗历史不应让整轮失败（修复前报 compact history failed）: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("结果状态 = %s, 期望 success", res.Status)
	}

	llm.mu.Lock()
	streamIn := append([]int(nil), llm.streamIn...)
	streamMsgs := llm.streamMsgs
	chatInputs := append([]int(nil), llm.chatInputs...)
	llm.mu.Unlock()

	// 1) 主请求发出且不超窗（provider 侧同样校验，超窗会直接报错）
	if len(streamIn) == 0 {
		t.Fatal("主请求未发出")
	}
	if streamIn[0] > a.contextWindow {
		t.Fatalf("主请求 %d tokens 超过窗口 %d（绝不允许超过）", streamIn[0], a.contextWindow)
	}
	for _, n := range chatInputs {
		if n > a.contextWindow {
			t.Fatalf("压缩请求 %d tokens 超过窗口 %d", n, a.contextWindow)
		}
	}

	// 2) 确实裁掉了旧记录（而不是把 200K 硬发出去）
	if got := core.EstimateProtoMessagesTokens(streamMsgs); got >= before/2 {
		t.Fatalf("主请求估算 = %d, 期望远小于全量 %d（应裁掉最旧的部分）", got, before)
	}

	// 3) 会话全量历史仍在（裁剪只作用于本次请求，不写回、不删除）
	after := core.EstimateProtoMessagesTokens(a.sess.DeriveMessages())
	if after < before {
		t.Fatalf("会话历史被裁剪/删除：%d -> %d（裁剪不得写回会话）", before, after)
	}
	full := a.sess.DeriveMessages()
	if len(full) < 3 || !strings.Contains(full[1].Content, "#round-0") {
		t.Fatal("最早一轮的历史应从会话中仍然可派生（切回大窗口时应原样恢复）")
	}

	// 4) 模型被明确告知历史被省略（否则它以为自己看到了全部）
	if len(streamMsgs) == 0 || streamMsgs[0].Role != "system" ||
		!strings.Contains(streamMsgs[0].Content, "已省略") {
		t.Fatalf("system 应说明历史被省略，实际首条 = %+v", streamMsgs[0])
	}

	// 5) 用户可见提示
	mu.Lock()
	notified := false
	for _, f := range frames {
		if strings.Contains(f.Output, "上下文裁剪") {
			notified = true
			break
		}
	}
	mu.Unlock()
	if !notified {
		t.Fatal("未输出上下文裁剪提示帧")
	}
}

// TestOversizeTrimKeepsUserBoundary 裁剪起点必须落在 user 消息上：否则 tool_use 与
// tool_result 会被切断（provider 直接拒），且 Anthropic 要求首条非 system 是 user。
func TestOversizeTrimKeepsUserBoundary(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	body := strings.Repeat("x", 10000)
	for i := 0; i < 10; i++ {
		callID := fmt.Sprintf("call-%d", i)
		a.sess.Append(session.UserMessage, &session.UserMessageData{Content: body, Source: "user"},
			&session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.AssistantMessage, &session.AssistantMessageData{
			ToolCalls: []*proto.ToolCall{{Id: callID, Name: "read", ArgumentsJson: "{}"}},
		}, &session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.ToolResult, &session.ToolResultData{CallID: callID, Content: body},
			&session.SurfaceOp{Op: session.SurfaceAppend})
	}
	markSessionRestored(t, a)

	msgs, dropped := fitMessagesToWindow(a.sess.DeriveMessages(), a.contextWindow)
	if dropped == 0 {
		t.Fatalf("应发生裁剪（总估算 %d, 预算 %d）",
			core.EstimateProtoMessagesTokens(a.sess.DeriveMessages()), int(float64(a.contextWindow)*oversizeTrimRatio))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("首条应为 system，实际 %q", msgs[0].Role)
	}
	if msgs[1].Role != "user" {
		t.Fatalf("裁剪后首条非 system 消息应为 user，实际 %q", msgs[1].Role)
	}
	// 每个 tool 消息都必须有前置的 assistant tool_call（配对完整）
	known := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			known[tc.Id] = true
		}
		if m.Role == "tool" && !known[m.ToolCallId] {
			t.Fatalf("裁剪后出现孤立 tool_result（call_id=%s），配对被切断", m.ToolCallId)
		}
	}
}

// TestSingleTurnOverflowReportsError 单轮自身就超窗（当前输入一次 60k token 到 50k 窗口）：
// 历史怎么丢都救不回来，必须明确报错且不发出请求，而不是盲发给 provider 被拒。
func TestSingleTurnOverflowReportsError(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	a.sess.Append(session.UserMessage, &session.UserMessageData{Content: "先前的短消息", Source: "user"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	markSessionRestored(t, a)

	llm := &windowEnforcingLLM{window: a.contextWindow}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	var mu sync.Mutex
	var errs []string
	_, err := a.runLoop(context.Background(), strings.Repeat("x", 60*1000), nil, func(f *core.RunStreamResponse) {
		if f.Error != "" {
			mu.Lock()
			errs = append(errs, f.Error)
			mu.Unlock()
		}
	})
	if err == nil {
		t.Fatal("单轮超窗应报错")
	}
	if !strings.Contains(err.Error(), "单轮内容超出上下文窗口") {
		t.Fatalf("错误文案应说明单轮超窗，实际 %v", err)
	}

	llm.mu.Lock()
	streamCalls := len(llm.streamIn)
	llm.mu.Unlock()
	if streamCalls != 0 {
		t.Fatalf("单轮超窗不应发出请求，实际发了 %d 次", streamCalls)
	}
	mu.Lock()
	if len(errs) == 0 {
		t.Fatal("应向上层报错误帧")
	}
	mu.Unlock()
}
