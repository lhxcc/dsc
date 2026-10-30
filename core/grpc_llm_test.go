package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dsc/proto"
	"dsc/proto/metadata"
	grpcmetadata "google.golang.org/grpc/metadata"
)

// fakeVisionProvider 固定上报视觉能力的 LLMProvider（仅用于 GetInfo 能力测试）。
type fakeVisionProvider struct {
	vision bool
}

func (f *fakeVisionProvider) Chat(context.Context, []Message, []Tool, int) (*ChatResponse, error) {
	return nil, nil
}
func (f *fakeVisionProvider) ChatStream(context.Context, []Message, []Tool) (<-chan *ChatStreamResponse, error) {
	return nil, nil
}
func (f *fakeVisionProvider) Name(context.Context) string    { return "fake" }
func (f *fakeVisionProvider) Version(context.Context) string { return "1.0.0" }
func (f *fakeVisionProvider) HealthCheck(context.Context) error {
	return nil
}
func (f *fakeVisionProvider) VisionEnabled() bool { return f.vision }

// TestLLMMetadataCapabilities 校验 llmMetadataServer.GetInfo 把视觉能力经 capabilities
// 上报给宿主（TUI 据此提示「图片未随消息发送：当前模型不支持图像输入」）。
func TestLLMMetadataCapabilities(t *testing.T) {
	on := &llmMetadataServer{impl: &fakeVisionProvider{vision: true}}
	info, err := on.GetInfo(context.Background(), &metadata.Empty{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.Capabilities["supports_images"] != "true" {
		t.Fatalf("vision=true 时应上报 supports_images=true, got %q", info.Capabilities["supports_images"])
	}

	off := &llmMetadataServer{impl: &fakeVisionProvider{vision: false}}
	infoOff, err := off.GetInfo(context.Background(), &metadata.Empty{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if infoOff.Capabilities["supports_images"] != "false" {
		t.Fatalf("vision=false 时应上报 supports_images=false, got %q", infoOff.Capabilities["supports_images"])
	}

	// 所有 LLM 插件默认提供 "llm" 能力，供 agent 经 Requires 声明依赖
	if info.Capabilities[CapabilityLLM] != "true" {
		t.Fatalf("LLM 插件应上报 %s=true, got %q", CapabilityLLM, info.Capabilities[CapabilityLLM])
	}
	if infoOff.Capabilities[CapabilityLLM] != "true" {
		t.Fatalf("LLM 插件（vision=false）也应上报 %s=true, got %q", CapabilityLLM, infoOff.Capabilities[CapabilityLLM])
	}
}

// A call whose arguments do not parse must be REPORTED, not run with an empty argument set.
//
// Reported from a 2080 Ti box.  The model left one </parameter> out, the engine's parser folded the rest of the call
// into the first value, and the assembled arguments ended inside a string.  This file used to unmarshal that into
// map[string]interface{}{} and hand the tool an empty command - a tool acting on a guess.
func TestUnparseableArgsIsReported(t *testing.T) {
	// The shape the service produced for that report.
	broken := `{"command":"cd F:/X && sed -i '968d' a.xml <parameter=description> Delete line 968 </tool_call>`

	var args map[string]interface{}
	err := json.Unmarshal([]byte(broken), &args)
	if err == nil {
		t.Fatal("the specimen must not parse; the test is meaningless otherwise")
	}

	got := unparseableArgs("exec_command", broken, err)
	if got == nil {
		t.Fatal("a call with unparseable arguments must produce an error")
	}
	msg := got.Error()
	for _, want := range []string{"exec_command", "not valid JSON", "</parameter>", "sed -i"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message should carry %q so a reader can tell model from parser; got: %s", want, msg)
		}
	}

	// A well-formed call is unaffected.
	if e := unparseableArgs("x", `{"a":1}`, nil); e == nil {
		t.Log("nil cause still produces a message (never called that way, but it must not panic)")
	}

	// The message is bounded: a huge argument string must not become a huge log line.  The wording around it is
	// fixed, so what is checked is that the ARGUMENTS were clipped, not the total length.
	long := strings.Repeat("a", 5000)
	m := unparseableArgs("t", long, err).Error()
	if strings.Contains(m, strings.Repeat("a", 201)) {
		t.Errorf("the argument dump should be clipped to 200 characters, got %d chars total", len(m))
	}
	if !strings.Contains(m, "...") {
		t.Errorf("a clipped dump should say so with an ellipsis: %s", m)
	}
}

// TestMalformedArgsDoNotEndTheStream 回归：内建 gRPC 服务端（sdk 路径）不得把「参数不合法」
// 变成流错误。
//
// 这条链是 SDK 里每个 provider 插件的实际入口（sdk/dsc.go 用 core.NewLLMServiceServer）。
// 它曾把解析失败做成一个带 Error 的帧然后 return——而 agent 收到 Error 帧就结束整轮。于是
// 一次漏写的闭合标签 = 一个死会话，模型永远不知道自己错在哪，用户只能手动说「继续」。
//
// 现在参数不合法只是一个普通帧：调用照常送出（带原文），由工具把「你写的参数不是合法 JSON，
// 这是原文」当作结果回给模型。
func TestMalformedArgsDoNotEndTheStream(t *testing.T) {
	const broken = `{"command":"cd F:/X && sed -i '968d' a.xml <parameter=description> Delete line 968 </tool_call>`
	p := &scriptedProvider{stream: []*ChatStreamResponse{{
		FinishReason: "tool_calls",
		ToolCalls:    []ToolCall{{ID: "call_1", Name: "shell", Arguments: nil, RawArguments: broken}},
	}}}
	srv := NewLLMServiceServer(p)
	stream := &captureChatStream{ctx: context.Background()}

	if err := srv.ChatStream(&proto.ChatRequest{}, stream); err != nil {
		t.Fatalf("ChatStream 返回错误 %v：畸形参数不得终止流", err)
	}
	if len(stream.sent) != 1 {
		t.Fatalf("发出 %d 帧, 期望 1", len(stream.sent))
	}
	if stream.sent[0].Error != "" {
		t.Fatalf("帧携带 Error %q：会把整轮打断（模型再也看不到这个调用）", stream.sent[0].Error)
	}
	if len(stream.sent[0].ToolCalls) != 1 {
		t.Fatalf("工具调用未送出: %+v", stream.sent[0].ToolCalls)
	}
	if got := stream.sent[0].ToolCalls[0].ArgumentsJson; got != broken {
		t.Fatalf("参数原文未保真送出:\n 得到 %q\n 期望 %q", got, broken)
	}
}

// scriptedProvider 按脚本吐帧的 LLMProvider 替身。
type scriptedProvider struct {
	stream []*ChatStreamResponse
}

func (p *scriptedProvider) Chat(context.Context, []Message, []Tool, int) (*ChatResponse, error) {
	return &ChatResponse{}, nil
}
func (p *scriptedProvider) ChatStream(context.Context, []Message, []Tool) (<-chan *ChatStreamResponse, error) {
	ch := make(chan *ChatStreamResponse, len(p.stream))
	for _, f := range p.stream {
		ch <- f
	}
	close(ch)
	return ch, nil
}
func (p *scriptedProvider) Name(context.Context) string       { return "scripted" }
func (p *scriptedProvider) Version(context.Context) string    { return "0" }
func (p *scriptedProvider) HealthCheck(context.Context) error { return nil }
func (p *scriptedProvider) VisionEnabled() bool               { return false }

// captureChatStream 收集服务端发出的帧。
type captureChatStream struct {
	ctx  context.Context
	sent []*proto.ChatStreamResponse
}

func (s *captureChatStream) Send(r *proto.ChatStreamResponse) error {
	s.sent = append(s.sent, r)
	return nil
}
func (s *captureChatStream) Context() context.Context         { return s.ctx }
func (s *captureChatStream) SetHeader(grpcmetadata.MD) error  { return nil }
func (s *captureChatStream) SendHeader(grpcmetadata.MD) error { return nil }
func (s *captureChatStream) SetTrailer(grpcmetadata.MD)       {}
func (s *captureChatStream) SendMsg(any) error                { return nil }
func (s *captureChatStream) RecvMsg(any) error                { return nil }
