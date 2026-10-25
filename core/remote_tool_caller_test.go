package core

import (
	"context"
	"encoding/json"
	"testing"

	"dsc/proto"

	"google.golang.org/grpc"
)

// callerCapturingTool 记录宿主实际下发给插件的请求（用于验证身份透传）。
type callerCapturingTool struct {
	proto.ToolServiceClient
	got *proto.ExecuteToolRequest
}

func (c *callerCapturingTool) ExecuteTool(_ context.Context, req *proto.ExecuteToolRequest, _ ...grpc.CallOption) (*proto.ExecuteToolResponse, error) {
	c.got = req
	return &proto.ExecuteToolResponse{Content: "ok"}, nil
}

// TestRemoteToolForwardsCallerIdentity 插件必须知道「这次调用属于哪个会话」：
// 否则它做按会话隔离的状态（持久 shell 的 cwd）时只剩一个全局单例，多个对话
// 会共用同一个 shell。此前这两个字段在宿主→插件这一跳被静默丢弃。
func TestRemoteToolForwardsCallerIdentity(t *testing.T) {
	capture := &callerCapturingTool{}
	tool := &RemoteTool{name: "shell", client: capture}

	ctx := WithCaller(context.Background(), "conv-42")
	ctx = WithToolCallID(ctx, "call-42")
	if _, _, _, err := tool.ExecuteWithViewAndImages(ctx, []byte(`{}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if capture.got.GetSessionId() != "conv-42" {
		t.Fatalf("会话标识应透传到插件，got %q", capture.got.GetSessionId())
	}
	if capture.got.GetToolCallId() != "call-42" {
		t.Fatalf("调用 id 应透传到插件，got %q", capture.got.GetToolCallId())
	}

	// 无调用方上下文时留空（插件侧据此退化为自己的默认值，而不是拿到垃圾值）
	empty := &callerCapturingTool{}
	tool2 := &RemoteTool{name: "shell", client: empty}
	if _, _, _, err := tool2.ExecuteWithViewAndImages(context.Background(), []byte(`{}`)); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if empty.got.GetSessionId() != "" || empty.got.GetToolCallId() != "" {
		t.Fatalf("无上下文时应留空，got session=%q call=%q", empty.got.GetSessionId(), empty.got.GetToolCallId())
	}
}

// probeTool 是最小可注册工具，执行时回调传入的闭包。
type probeTool func(ctx context.Context) (string, error)

func (p probeTool) Name() string        { return "probe" }
func (p probeTool) Description() string { return "probe" }
func (p probeTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (p probeTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) { return p(ctx) }

// TestToolGRPCServerInjectsCallIDs ExecuteTool 入口把会话与调用 id 放进 ctx
// （下游 RemoteTool 才能转发）。
func TestToolGRPCServerInjectsCallIDs(t *testing.T) {
	var gotSession, gotCall string
	m := NewManager(&ManagerConfig{})
	m.GetToolRegistry().Register(probeTool(func(ctx context.Context) (string, error) {
		gotSession, gotCall = CallerFromContext(ctx), ToolCallIDFromContext(ctx)
		return "ok", nil
	}))
	srv := NewToolGRPCServer(m)
	if _, err := srv.ExecuteTool(context.Background(), &proto.ExecuteToolRequest{
		ToolName: "probe", ArgumentsJson: "{}", SessionId: "s-1", ToolCallId: "c-1",
	}); err != nil {
		t.Fatalf("ExecuteTool: %v", err)
	}
	if gotSession != "s-1" || gotCall != "c-1" {
		t.Fatalf("ctx 应带会话与调用 id，got %q / %q", gotSession, gotCall)
	}
}
