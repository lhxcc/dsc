package main

import (
	"context"
	dsc "dsc-sdk"
	"io"
	"strings"
	"sync"
	"testing"

	"dsc/proto"
	"dsc/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// 本文件回归测试「重启恢复大历史会话 + 无上次请求 usage」的压缩对齐（DSH compaction-basic）：
// 修复前，压缩触发依赖 lastPromptTokens > 0（来自上一次请求的服务端 usage），重启后该值为 0，
// 第一发请求会把整段历史直接发给模型——正是「第二次启动加载历史会话数据、似乎几大都加载」的根源。
// 修复后，压缩前移到请求发送前，无 usage 时退回字符启发式估算触发压缩，且保留近期尾部逐字。

// compactMockLLM 记录 Chat（压缩摘要调用）与 ChatStream（压缩后的主请求）的调用与入参。
type compactMockLLM struct {
	proto.LLMServiceClient

	mu          sync.Mutex
	chatCalls   int
	streamCalls int
	streamMsgs  []*proto.Message
	done        chan struct{} // ChatStream 首次收到请求时关闭
	usagePrompt int32         // 若 >0，主请求流携带该 prompt_tokens（模拟服务端低估 usage）
}

func (m *compactMockLLM) Chat(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (*proto.ChatResponse, error) {
	m.mu.Lock()
	m.chatCalls++
	m.mu.Unlock()
	return &proto.ChatResponse{Content: "【压缩摘要】此前对话的要点记录。"}, nil
}

func (m *compactMockLLM) ChatStream(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[proto.ChatStreamResponse], error) {
	m.mu.Lock()
	m.streamCalls++
	m.streamMsgs = in.Messages
	m.mu.Unlock()
	select {
	case <-m.done:
	default:
		close(m.done)
	}
	var usage *proto.Usage
	if m.usagePrompt > 0 {
		usage = &proto.Usage{PromptTokens: m.usagePrompt}
	}
	return &compactMockStream{usage: usage}, nil
}

type compactMockStream struct {
	sent  bool
	usage *proto.Usage
}

func (s *compactMockStream) Recv() (*proto.ChatStreamResponse, error) {
	if !s.sent {
		s.sent = true
		return &proto.ChatStreamResponse{Content: "继续执行", FinishReason: "stop", Usage: s.usage}, nil
	}
	return nil, io.EOF
}
func (s *compactMockStream) Header() (metadata.MD, error) { return nil, nil }
func (s *compactMockStream) Trailer() metadata.MD         { return nil }
func (s *compactMockStream) CloseSend() error             { return nil }
func (s *compactMockStream) Context() context.Context     { return context.Background() }
func (s *compactMockStream) SendMsg(m any) error          { return nil }
func (s *compactMockStream) RecvMsg(m any) error          { return nil }

// TestPreDispatchCompactionOnRestore 验证重启恢复场景：
// lastPromptTokens 为 0（无上次 usage）时，仍基于字符估算在首请求发送前触发压缩，
// 折叠最旧前缀为摘要，并保留近期尾部（含刚追加的当前用户消息）逐字。
func TestPreDispatchCompactionOnRestore(t *testing.T) {
	a := newTestAgent(t)
	a.llmServiceID = 1
	a.toolServiceID = 1
	a.contextWindow = 50000 // 模拟宿主注入 DSC_CONTEXT_WINDOW
	a.lastPromptTokens = 0  // 模拟重启：无上次请求的 usage

	// 预置历史：2 轮（4 条）约 84% 窗口（42k/50k）——**在窗口之内**，故走压缩而非裁剪。
	// 超过窗口的情形由 TestOversizeHistoryTrimmedToWindow 覆盖（那里压缩请求自身就会
	// 超限，只能先裁）。
	// v3 不变量：surface node 0 必须是 system/message（runLoop 每步经
	// commitSystemPrompt 提交到事件日志）。测试预置 system 节点模拟生产启动。
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	big := strings.Repeat("历史对话内容撑大上下文占用。", 750) // 14 字 ×750 = 10.5k 字 → 估算 10.5k token/条
	for i := 0; i < 2; i++ {
		a.sess.Append(session.UserMessage, &session.UserMessageData{Content: big, Source: "user"},
			&session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.AssistantMessage, &session.AssistantMessageData{Content: big},
			&session.SurfaceOp{Op: session.SurfaceAppend})
	}
	// 预置内容属于「重启恢复」的部分：标记恢复边界（压缩路径与注入上限都按它判定）
	markSessionRestored(t, a)
	if got := dsc.EstimateProtoMessagesTokens(a.sess.DeriveMessages()); got < a.contextWindow*8/10 {
		t.Fatalf("预置历史估算 = %d token, 需 >= %d 才能触发压缩", got, a.contextWindow*8/10)
	}

	llm := &compactMockLLM{done: make(chan struct{})}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	var mu sync.Mutex
	var frames []*dsc.RunStreamResponse
	emit := func(f *dsc.RunStreamResponse) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}

	res, err := a.runLoop(context.Background(), "继续", nil, emit)
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("结果状态 = %s, 期望 success", res.Status)
	}

	// 1) 首请求发送前必须触发过压缩 Chat 调用（修复前 chatCalls == 0）
	llm.mu.Lock()
	chatCalls := llm.chatCalls
	streamMsgs := llm.streamMsgs
	llm.mu.Unlock()
	if chatCalls < 1 {
		t.Fatalf("压缩 Chat 调用次数 = %d, 期望 >= 1（重启后首请求前应先压缩历史）", chatCalls)
	}

	// 2) 主请求收到的历史已被压缩：远小于全量（4 条历史 + 1 条当前 = 5 条）
	if len(streamMsgs) >= 5 {
		t.Fatalf("主请求消息数 = %d, 期望 < 5（压缩后应显著减少，而非把全量历史发给模型）", len(streamMsgs))
	}

	// 3) 摘要节点在列（压缩落地）
	hasSummary := false
	for _, m := range streamMsgs {
		if m.Role == "user" && strings.Contains(m.Content, "压缩摘要") {
			hasSummary = true
			break
		}
	}
	if !hasSummary {
		t.Fatalf("主请求历史中缺少压缩摘要节点")
	}

	// 4) 尾部保留：最后一条仍是当前用户消息，逐字未被摘要替换
	if last := streamMsgs[len(streamMsgs)-1]; last.Role != "user" || last.Content != "继续" {
		t.Fatalf("主请求最后一条 = role=%s content=%q, 期望逐字保留当前用户消息", last.Role, last.Content)
	}

	// 5) 前端不得出现压缩提示：压缩是宿主自己的调度动作，用户不需要为它处理任何事。
	//    观察到它请用 -log（agent 侧 logger.Info("compacting history (inline)")）。
	mu.Lock()
	defer mu.Unlock()
	for _, f := range frames {
		if strings.Contains(f.Output, "上下文压缩") {
			t.Fatalf("前端不应输出压缩提示帧，实际：%q", f.Output)
		}
	}
}

// TestCompactionNotTriggeredOnAccurateUsage 回归「本地估算虚高导致假压缩」：
// 服务端已上报精确已用容量（llm-anthropic 等插件层已把提示缓存命中一并计入，
// 上报值即真实上下文）且未达 80% 阈值时，不得被字符启发式估算（中文每字 1 token、
// 英文按 /4）顶到阈值而触发压缩——否则会出现「TUI 显示 30% 而压缩提示 81%」的
// 数值不同步与假压缩，无谓地把早期对话折叠成摘要。
// 估算兜底（服务端未上报、lastPromptTokens==0）由 TestPreDispatchCompactionOnRestore 覆盖。
func TestCompactionNotTriggeredOnAccurateUsage(t *testing.T) {
	a := newTestAgent(t)
	a.llmServiceID = 1
	a.toolServiceID = 1
	a.contextWindow = 50000
	a.lastPromptTokens = 12000 // 服务端上报精确值：24% 窗口，远低于 80% 阈值

	// 预置足够大的历史：字符估算超过 80% 窗口（40k），验证「估算虚高不再触发压缩」
	big := strings.Repeat("历史对话内容撑大上下文占用。", 1000) // 14 字 ×1000 = 14k 字 → 估算约 14k token/条
	for i := 0; i < 6; i++ {
		a.sess.Append(session.UserMessage, &session.UserMessageData{Content: big, Source: "user"},
			&session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.AssistantMessage, &session.AssistantMessageData{Content: big},
			&session.SurfaceOp{Op: session.SurfaceAppend})
	}
	// 预置内容属于「重启恢复」的部分：标记恢复边界（压缩路径与注入上限都按它判定）
	markSessionRestored(t, a)
	if got := dsc.EstimateProtoMessagesTokens(a.sess.DeriveMessages()); got < a.contextWindow*8/10 {
		t.Fatalf("预置历史估算 = %d token, 需 >= %d 才能验证「估算虚高不触发压缩」", got, a.contextWindow*8/10)
	}

	// 主请求流每帧回报精确 prompt_tokens=12000（真实上下文，低于阈值）
	llm := &compactMockLLM{done: make(chan struct{}), usagePrompt: 12000}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	var mu sync.Mutex
	var frames []*dsc.RunStreamResponse
	emit := func(f *dsc.RunStreamResponse) {
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
	}

	res, err := a.runLoop(context.Background(), "继续", nil, emit)
	if err != nil {
		t.Fatalf("runLoop: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("结果状态 = %s, 期望 success", res.Status)
	}

	llm.mu.Lock()
	chatCalls := llm.chatCalls
	llm.mu.Unlock()
	if chatCalls != 0 {
		t.Fatalf("压缩 Chat 调用次数 = %d, 期望 0（服务端已上报精确用量且未达阈值，不应假压缩）", chatCalls)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, f := range frames {
		if strings.Contains(f.Output, "上下文压缩") {
			t.Fatalf("前端不应输出压缩提示帧，实际：%q", f.Output)
		}
	}
}
