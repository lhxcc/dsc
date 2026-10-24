package main

import (
	"testing"

	"dsc-sdk"
	"dsc/proto"
	"dsc/session"

	"github.com/hashicorp/go-hclog"
)

// TestDeriveMessagesRepairsOrphanToolResult 回归：会话里若有孤立 tool 结果
// （压缩切点/旧会话恢复可产生），请求历史必须被最小修复，否则 OpenAI 兼容
// 端点会以 422 拒绝整轮请求。
func TestDeriveMessagesRepairsOrphanToolResult(t *testing.T) {
	sess := session.New()
	sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	sess.Append(session.UserMessage, &session.UserMessageData{Content: "hi"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	// 无前置 assistant tool_call 归属的孤立 tool 结果。
	sess.Append(session.ToolResult, &session.ToolResultData{CallID: "orphan", Content: "r"},
		&session.SurfaceOp{Op: session.SurfaceAppend})

	a := &ReactLoopAgent{logger: hclog.NewNullLogger(), historyInjection: -1}
	msgs := a.deriveMessages(sess)

	if err := dsc.ValidateToolPairing(msgs); err != nil {
		t.Fatalf("derived history still invalid: %v", err)
	}
	for _, m := range msgs {
		if m.GetRole() == "tool" {
			t.Fatalf("orphan tool message survived repair: %+v", m)
		}
	}
}

// TestDeriveMessagesKeepsValidPairing 合法工具对不被修复改动。
func TestDeriveMessagesKeepsValidPairing(t *testing.T) {
	sess := session.New()
	sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	sess.Append(session.UserMessage, &session.UserMessageData{Content: "hi"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	sess.Append(session.AssistantMessage, &session.AssistantMessageData{
		ToolCalls: []*proto.ToolCall{{Id: "call-1", Name: "shell", ArgumentsJson: "{}"}},
	}, &session.SurfaceOp{Op: session.SurfaceAppend})
	sess.Append(session.ToolResult, &session.ToolResultData{CallID: "call-1", Content: "ok"},
		&session.SurfaceOp{Op: session.SurfaceAppend})

	a := &ReactLoopAgent{logger: hclog.NewNullLogger(), historyInjection: -1}
	msgs := a.deriveMessages(sess)
	if err := dsc.ValidateToolPairing(msgs); err != nil {
		t.Fatalf("valid history reported error: %v", err)
	}
	if msgs[len(msgs)-1].GetToolCallId() != "call-1" {
		t.Fatalf("tool_call_id lost during derive: %+v", msgs[len(msgs)-1])
	}
}
