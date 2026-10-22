package core

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"dsc/proto"

	"google.golang.org/grpc/metadata"
)

// mockLLMProvider 测试用 LLM provider：可配置前 N 次失败。
type mockLLMProvider struct {
	mu        sync.Mutex
	failChat  int  // Chat 剩余失败次数
	failStart int  // ChatStream 建立阶段剩余失败次数
	midErr    bool // 流中途失败（已发帧后）
	chatCalls int
}

func (p *mockLLMProvider) Chat(context.Context, []Message, []Tool, int) (*ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.chatCalls++
	if p.failChat > 0 {
		p.failChat--
		return nil, errors.New("provider unavailable")
	}
	return &ChatResponse{Content: "ok", FinishReason: "stop"}, nil
}

func (p *mockLLMProvider) ChatStream(ctx context.Context, _ []Message, _ []Tool) (<-chan *ChatStreamResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failStart > 0 {
		p.failStart--
		return nil, errors.New("stream start failed")
	}
	ch := make(chan *ChatStreamResponse, 2)
	ch <- &ChatStreamResponse{Content: "hi"}
	if p.midErr {
		ch <- &ChatStreamResponse{Error: "mid stream error"}
	}
	close(ch)
	return ch, nil
}

func (p *mockLLMProvider) Name(context.Context) string    { return "mock-llm" }
func (p *mockLLMProvider) Version(context.Context) string { return "1.0.0" }
func (p *mockLLMProvider) VisionEnabled() bool            { return true }
func (p *mockLLMProvider) HealthCheck(context.Context) error {
	return nil
}

// mockChatStream 实现 gRPC 流式发送端。
type mockChatStream struct {
	sent []*proto.ChatStreamResponse
}

func (s *mockChatStream) Send(r *proto.ChatStreamResponse) error {
	s.sent = append(s.sent, r)
	return nil
}

func (s *mockChatStream) Context() context.Context     { return context.Background() }
func (s *mockChatStream) RecvMsg(any) error            { return nil }
func (s *mockChatStream) SendMsg(any) error            { return nil }
func (s *mockChatStream) SetHeader(metadata.MD) error  { return nil }
func (s *mockChatStream) SendHeader(metadata.MD) error { return nil }
func (s *mockChatStream) SetTrailer(metadata.MD)       {}

func TestLLMChatRetrySucceeds(t *testing.T) {
	p := &mockLLMProvider{failChat: 1}
	b := NewEventBus()
	b.OnWaterfall(EventLLMRequest, LLMRetryListener(3, time.Millisecond))
	srv := &llmProviderServer{provider: p, events: b}

	resp, err := srv.Chat(context.Background(), &proto.ChatRequest{})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("content = %q", resp.Content)
	}
	if p.chatCalls != 2 {
		t.Fatalf("chat calls = %d, want 2 (1 fail + 1 retry)", p.chatCalls)
	}
}

func TestLLMChatRetryExhausted(t *testing.T) {
	p := &mockLLMProvider{failChat: 5}
	b := NewEventBus()
	b.OnWaterfall(EventLLMRequest, LLMRetryListener(2, time.Millisecond))
	srv := &llmProviderServer{provider: p, events: b}

	_, err := srv.Chat(context.Background(), &proto.ChatRequest{})
	if err == nil {
		t.Fatal("expected error after retries exhausted")
	}
	if p.chatCalls != 2 {
		t.Fatalf("chat calls = %d, want 2 (maxAttempts)", p.chatCalls)
	}
}

func TestLLMRequestVeto(t *testing.T) {
	p := &mockLLMProvider{}
	b := NewEventBus()
	b.OnWaterfall(EventLLMRequest, func(ctx EventContext, next func(EventContext) error) error {
		return errors.New("vetoed by policy")
	})
	srv := &llmProviderServer{provider: p, events: b}

	_, err := srv.Chat(context.Background(), &proto.ChatRequest{})
	if err == nil || err.Error() != "vetoed by policy" {
		t.Fatalf("err = %v, want vetoed by policy", err)
	}
	if p.chatCalls != 0 {
		t.Fatalf("provider should not be called on veto, got %d", p.chatCalls)
	}
}

func TestLLMChatStreamRetryOnStartFailure(t *testing.T) {
	p := &mockLLMProvider{failStart: 1}
	b := NewEventBus()
	b.OnWaterfall(EventLLMRequest, LLMRetryListener(3, time.Millisecond))
	srv := &llmProviderServer{provider: p, events: b}
	stream := &mockChatStream{}

	err := srv.ChatStream(&proto.ChatRequest{}, stream)
	if err != nil {
		t.Fatalf("chat stream: %v", err)
	}
	if len(stream.sent) != 1 || stream.sent[0].Content != "hi" {
		t.Fatalf("sent = %+v, want one 'hi' frame", stream.sent)
	}
}

func TestLLMChatStreamNoRetryAfterFrames(t *testing.T) {
	p := &mockLLMProvider{midErr: true}
	b := NewEventBus()
	b.OnWaterfall(EventLLMRequest, LLMRetryListener(3, time.Millisecond))
	srv := &llmProviderServer{provider: p, events: b}
	stream := &mockChatStream{}

	err := srv.ChatStream(&proto.ChatRequest{}, stream)
	if err == nil || err.Error() != "LLM stream error: mid stream error" {
		t.Fatalf("err = %v, want mid stream error", err)
	}
	// 已发 1 帧后失败：不得重试（避免重复输出），sent 只有 1 帧
	if len(stream.sent) != 1 {
		t.Fatalf("sent %d frames, want 1 (no retry after frames)", len(stream.sent))
	}
}

// TestProtoMessagesToPluginPreservesToolFields 字段保真往返：宿主聚合层把
// proto.ChatRequest 交给 LLM 插件时必须逐字段存活，尤其是 tool 消息的
// ToolCallId——OpenAI 兼容端点靠它回带 tool_call_id，缺失即 422
// 「missing field tool_call_id」，导致工具执行后的下一步请求整轮失败。
func TestProtoMessagesToPluginPreservesToolFields(t *testing.T) {
	req := &proto.ChatRequest{
		Messages: []*proto.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "看图", Images: []string{"data:image/png;base64,AA"}},
			{Role: "assistant", ToolCalls: []*proto.ToolCall{
				{Id: "call_a", Name: "shell", ArgumentsJson: `{"command":"ls"}`},
				{Id: "call_b", Name: "read", ArgumentsJson: `{"path":"/tmp/a"}`},
			}},
			{Role: "tool", Content: "结果", ToolCallId: "call_a", Images: []string{"dsc-img://x"}},
			{Role: "tool", Content: "另一个结果", ToolCallId: "call_b"},
		},
	}

	msgs, _ := protoMessagesToPlugin(req)
	if len(msgs) != len(req.Messages) {
		t.Fatalf("messages = %d, want %d", len(msgs), len(req.Messages))
	}
	if msgs[1].Images[0] != "data:image/png;base64,AA" {
		t.Fatalf("user images lost: %+v", msgs[1])
	}
	if len(msgs[2].ToolCalls) != 2 ||
		msgs[2].ToolCalls[0].ID != "call_a" || msgs[2].ToolCalls[1].ID != "call_b" ||
		msgs[2].ToolCalls[0].Name != "shell" ||
		msgs[2].ToolCalls[0].Arguments["command"] != "ls" {
		t.Fatalf("assistant tool calls lost: %+v", msgs[2].ToolCalls)
	}
	if msgs[3].ToolCallID != "call_a" || msgs[3].Images[0] != "dsc-img://x" {
		t.Fatalf("tool message fields lost: %+v", msgs[3])
	}
	if msgs[4].ToolCallID != "call_b" {
		t.Fatalf("tool_call_id lost: %+v", msgs[4])
	}

	// 回程（插件 → proto）同样须保真，避免仅单程通过。
	back := convertToProtoMessages(msgs)
	if back[3].GetToolCallId() != "call_a" || back[4].GetToolCallId() != "call_b" {
		t.Fatalf("round trip lost tool_call_id: %q / %q",
			back[3].GetToolCallId(), back[4].GetToolCallId())
	}
	if len(back[2].GetToolCalls()) != 2 || back[2].GetToolCalls()[1].GetId() != "call_b" {
		t.Fatalf("round trip lost tool calls: %+v", back[2].GetToolCalls())
	}
	if back[3].GetImages()[0] != "dsc-img://x" {
		t.Fatalf("round trip lost images: %+v", back[3].GetImages())
	}
}

// TestLLMRetryListenerStopsOnContextCancel 回归：退避等待必须感知请求 ctx 取消，
// 调用方已放弃时立即返回，不再空等完整退避时长后才重试。
func TestLLMRetryListenerStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	listener := LLMRetryListener(3, time.Second)

	start := time.Now()
	err := listener(EventContext{Context: ctx}, func(EventContext) error {
		cancel() // 首次失败后立即取消请求
		return errors.New("provider failed")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("retry sleep ignored cancellation (elapsed %v)", elapsed)
	}
}
