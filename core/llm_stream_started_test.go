package core

import (
	"context"
	"strings"
	"testing"

	"dsc/proto"
)

// fakeStream 只实现 chatStreamWithProvider 用到的 Send/Context。
// 嵌入接口：其余方法一旦被调用就是 nil panic，正好暴露「测试走到了不该走的路径」。
type fakeStream struct {
	proto.LLMService_ChatStreamServer
	sent int
}

func (f *fakeStream) Send(*proto.ChatStreamResponse) error { f.sent++; return nil }
func (f *fakeStream) Context() context.Context             { return context.Background() }

// fakeStreamProvider 按给定顺序吐帧（可含 Error 帧）。
type fakeStreamProvider struct{ items []*ChatStreamResponse }

func (p *fakeStreamProvider) Chat(context.Context, []Message, []Tool, int) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (p *fakeStreamProvider) ChatStream(context.Context, []Message, []Tool) (<-chan *ChatStreamResponse, error) {
	ch := make(chan *ChatStreamResponse, len(p.items))
	for _, it := range p.items {
		ch <- it
	}
	close(ch)
	return ch, nil
}
func (p *fakeStreamProvider) Name(context.Context) string       { return "fake" }
func (p *fakeStreamProvider) Version(context.Context) string    { return "1" }
func (p *fakeStreamProvider) HealthCheck(context.Context) error { return nil }
func (p *fakeStreamProvider) VisionEnabled() bool               { return false }

// TestStreamStartedIgnoresErrorOnlyFrame 一个还没吐字就报错的帧，不得被当成
// 「流已开始」——否则上层会跳过重试与超窗收紧，把原始 400 直接抛给用户。
//
// 现场：provider 把 HTTP 400 也做成一个带 Error 的帧
// （`LLM stream error: … prompt is too long: 1268641 tokens > 1048576 maximum`），
// 旧实现在处理 Error 之前无条件置 StreamStarted=true，于是「4xx → 收紧预算 → 滑窗
// → 重试」整条自愈路径被跳过，一轮就这么失败。
func TestStreamStartedIgnoresErrorOnlyFrame(t *testing.T) {
	p := &fakeStreamProvider{items: []*ChatStreamResponse{
		{Error: "error, status code: 400, message: {\"msg\":\"prompt is too long: 1268641 tokens > 1048576 maximum\"}"},
	}}
	call := &LLMCall{}
	err := chatStreamWithProvider(p, &proto.ChatRequest{}, &fakeStream{}, call)
	if err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("应返回原始错误，got %v", err)
	}
	if call.StreamStarted {
		t.Fatal("没有发出任何内容的错误帧不得标记 StreamStarted（会跳过重试与收紧）")
	}
}

// TestStreamStartedAfterContent 一旦真的发出过内容，之后失败就必须保留
// StreamStarted（重试会重复输出，不能重试）。
func TestStreamStartedAfterContent(t *testing.T) {
	p := &fakeStreamProvider{items: []*ChatStreamResponse{
		{Content: "先说一句"},
		{Error: "connection reset by peer"},
	}}
	fs := &fakeStream{}
	call := &LLMCall{}
	if err := chatStreamWithProvider(p, &proto.ChatRequest{}, fs, call); err == nil {
		t.Fatal("应返回错误")
	}
	if !call.StreamStarted {
		t.Fatal("已经发出过内容，必须标记 StreamStarted")
	}
	if fs.sent != 1 {
		t.Fatalf("错误帧不应转发给客户端，sent=%d", fs.sent)
	}
}
