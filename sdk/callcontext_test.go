package dsc

import (
	"context"
	"encoding/json"
	"testing"

	"dsc/proto"
)

// TestCallInfoContext 读写与缺省行为（空 ctx、非工具上下文）。
func TestCallInfoContext(t *testing.T) {
	if got := CallSessionID(context.Background()); got != "" {
		t.Fatalf("非工具上下文应返回空串，got %q", got)
	}
	if _, ok := CallInfoFrom(nil); ok {
		t.Fatal("nil ctx 不应 panic，且应返回 false")
	}
	ctx := WithCallInfo(context.Background(), CallInfo{SessionID: "s1", ToolCallID: "c1"})
	info, ok := CallInfoFrom(ctx)
	if !ok || info.SessionID != "s1" || info.ToolCallID != "c1" {
		t.Fatalf("CallInfoFrom = %+v ok=%v", info, ok)
	}
	if got := CallSessionID(ctx); got != "s1" {
		t.Fatalf("CallSessionID = %q", got)
	}
}

// TestExecuteToolInjectsCallInfo 宿主请求里的 session_id 必须到得了 Handler——
// 否则插件做「按会话隔离」的状态时拿不到身份，只能退化成全局单例。
func TestExecuteToolInjectsCallInfo(t *testing.T) {
	var gotSession, gotCall string
	instance := New(Config{Name: "p", Type: TypeTool})
	instance.Tool(Tool{
		Name: "probe",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			gotSession = CallSessionID(ctx)
			info, _ := CallInfoFrom(ctx)
			gotCall = info.ToolCallID
			return "ok", nil
		},
	})
	srv := &toolServiceServer{sdk: instance}

	resp, err := srv.ExecuteTool(context.Background(), &proto.ExecuteToolRequest{
		ToolName: "probe", ArgumentsJson: "{}", SessionId: "conv-9", ToolCallId: "call-9",
	})
	if err != nil || resp.GetError() != "" {
		t.Fatalf("ExecuteTool: %v / %s", err, resp.GetError())
	}
	if gotSession != "conv-9" || gotCall != "call-9" {
		t.Fatalf("Handler 应拿到调用信息，got session=%q call=%q", gotSession, gotCall)
	}
}
