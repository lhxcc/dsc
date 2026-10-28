package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dsc/proto"
	"dsc/session"
)

// newSearchFixture 建一个带历史的会话，返回可执行检索的工具。
func newSearchFixture(t *testing.T) (*Manager, *contextSearchTool, context.Context) {
	t.Helper()
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	st, err := m.sessionStore()
	if err != nil {
		t.Fatalf("sessionStore: %v", err)
	}
	sess, err := st.Ensure("sess-search")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	appendMsg := func(typ session.EventType, data any) {
		sess.Append(typ, data, &session.SurfaceOp{Op: session.SurfaceAppend})
	}
	appendMsg(session.UserMessage, &session.UserMessageData{
		Content: "把 build tag 改成 linux-only，只在 Linux 上编译",
		Source:  "user",
	})
	appendMsg(session.AssistantMessage, &session.AssistantMessageData{
		Content: "改好了。顺带提醒：CI 的 runner 是 windows-latest，需要单独排除。",
	})
	appendMsg(session.UserMessage, &session.UserMessageData{
		Content: "那就在 workflow 里加 matrix 排除掉它",
		Source:  "user",
	})
	if err := st.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	return m, &contextSearchTool{m: m}, WithCaller(context.Background(), "sess-search")
}

// TestContextSearchFindsTrimmedHistory 检索要能在整份会话日志里精确定位一句话。
//
// 这正是滑窗模式的缺口：被裁掉的早期记录模型看不见，但它自己（或用户）记得原话。
func TestContextSearchFindsTrimmedHistory(t *testing.T) {
	m, tool, ctx := newSearchFixture(t)
	_ = m

	out, err := tool.Execute(ctx, json.RawMessage(`{"query":"CI windows"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "CI 的 runner 是 windows-latest") {
		t.Fatalf("应命中第二条消息，got:\n%s", out)
	}
	if !strings.Contains(out, "match(es)") {
		t.Fatalf("应带命中计数，got:\n%s", out)
	}
	// AND 语义：第一条只有 build tag、没有 CI/windows，不该被捞出来
	if strings.Contains(out, "build tag") {
		t.Fatalf("多词应按 AND 过滤，不该命中只含部分词的消息，got:\n%s", out)
	}
}

// TestContextSearchNoMatchIsExplicit 无命中要明确说「查过整份日志但没找到」，
// 而不是返回空串——空串会让模型以为工具坏了，转头去翻文件。
func TestContextSearchNoMatchIsExplicit(t *testing.T) {
	_, tool, ctx := newSearchFixture(t)
	out, err := tool.Execute(ctx, json.RawMessage(`{"query":"不存在的关键词xyzzy"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "No match") {
		t.Fatalf("无命中应显式说明，got %q", out)
	}
}

// TestContextSearchMatchesToolArguments 工具调用的参数也要可检索：
// 模型常常只记得「我跑过某条命令」，而那段文本只存在于 tool_calls 的 arguments 里。
func TestContextSearchMatchesToolArguments(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	st, err := m.sessionStore()
	if err != nil {
		t.Fatalf("sessionStore: %v", err)
	}
	sess, err := st.Ensure("sess-args")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sess.Append(session.AssistantMessage, &session.AssistantMessageData{
		Content: "",
		ToolCalls: []*proto.ToolCall{{
			Id:            "call-1",
			Name:          "shell",
			ArgumentsJson: `{"command":"go test ./... -run TestSomethingVerySpecific"}`,
		}},
	}, &session.SurfaceOp{Op: session.SurfaceAppend})
	if err := st.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}

	tool := &contextSearchTool{m: m}
	out, err := tool.Execute(WithCaller(context.Background(), "sess-args"),
		json.RawMessage(`{"query":"TestSomethingVerySpecific"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !strings.Contains(out, "TestSomethingVerySpecific") {
		t.Fatalf("应按工具参数命中，got:\n%s", out)
	}
}

// TestContextSearchEmptyQueryRejected 空查询直接报错，不要退化成「列出全部」。
func TestContextSearchEmptyQueryRejected(t *testing.T) {
	_, tool, ctx := newSearchFixture(t)
	if _, err := tool.Execute(ctx, json.RawMessage(`{"query":"   "}`)); err == nil {
		t.Fatal("空查询应报错")
	}
}

// TestContextSearchNoSession (nil, nil) 语义：没有会话日志时应给出可读提示，不报错。
func TestContextSearchNoSession(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	tool := &contextSearchTool{m: m}
	out, err := tool.Execute(WithCaller(context.Background(), "does-not-exist"), json.RawMessage(`{"query":"x"}`))
	if err != nil {
		t.Fatalf("缺失会话不应报错: %v", err)
	}
	if !strings.Contains(out, "no session log") && !strings.Contains(out, "no recorded messages") {
		t.Fatalf("应给出可读提示，got %q", out)
	}
}
