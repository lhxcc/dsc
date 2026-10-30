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

// newLongMessageFixture 建一个含「一条超长 assistant 消息」的会话：用来验证
// 「命中给片段、按 id 取回给全文」这一对行为。
func newLongMessageFixture(t *testing.T) (*contextSearchTool, context.Context, string) {
	t.Helper()
	return newMessageFixture(t, strings.Repeat("构建日志：磁盘已满，写入失败。", 200))
}

// newMessageFixture 建一个只含一条 assistant 消息（正文为 content）的会话。
func newMessageFixture(t *testing.T, content string) (*contextSearchTool, context.Context, string) {
	t.Helper()
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	st, err := m.sessionStore()
	if err != nil {
		t.Fatalf("sessionStore: %v", err)
	}
	sess, err := st.Ensure("sess-fetch")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	sess.Append(session.AssistantMessage, &session.AssistantMessageData{
		Content: content,
		ToolCalls: []*proto.ToolCall{
			{Id: "call-1", Name: "shell", ArgumentsJson: `{"command":"df -h"}`},
		},
	}, &session.SurfaceOp{Op: session.SurfaceAppend})
	if err := st.Save(sess); err != nil {
		t.Fatalf("save: %v", err)
	}
	return &contextSearchTool{m: m}, WithCaller(context.Background(), "sess-fetch"), content
}

// extractMessageID 从检索结果里取出第一条命中的消息 id（形如 event-3）。
func extractMessageID(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "event-")
	if i < 0 {
		t.Fatalf("检索结果应带消息 id，got:/n%s", out)
	}
	j := i
	for j < len(out) && (out[j] == 'e' || out[j] == 'v' || out[j] == 'n' || out[j] == 't' || out[j] == '-' || (out[j] >= '0' && out[j] <= '9')) {
		j++
	}
	return out[i:j]
}

// TestContextSearchFetchFullMessage 检索只给片段，按 id 取回要给**整条**消息。
//
// 单位是「一条派生消息」：取回的正是刚才那条 assistant（含它的工具调用），
// 既不是整轮，也不是同一步里的其它工具输出。
func TestContextSearchFetchFullMessage(t *testing.T) {
	tool, ctx, long := newLongMessageFixture(t)

	out, err := tool.Execute(ctx, json.RawMessage(`{"query":"磁盘已满"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if strings.Contains(out, long) {
		t.Fatal("检索结果应是片段，不应整段返回")
	}
	id := extractMessageID(t, out)
	if !strings.Contains(out, "message_id") {
		t.Fatalf("检索结果应提示可按 id 取回全文，got:/n%s", out)
	}

	full, err := tool.Execute(ctx, json.RawMessage(`{"message_id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("取回: %v", err)
	}
	if !strings.Contains(full, long) {
		t.Fatalf("应按 id 返回完整正文（%d 字符），got 长度 %d:/n%s", len([]rune(long)), len([]rune(full)), full)
	}
	if strings.Contains(full, "truncated") {
		t.Fatalf("未超上限时不应截断:\n%s", full)
	}
	if !strings.Contains(full, "[tool calls in this message]") || !strings.Contains(full, "df -h") {
		t.Fatalf("同一条消息里的工具调用应一并给出:\n%s", full)
	}
	if !strings.Contains(full, "assistant") || !strings.Contains(full, id) {
		t.Fatalf("回执应标明 id 与角色:\n%s", full)
	}
}

// TestContextSearchFetchPaginates 超长消息分段取：offset 续读能接上，且明确告知剩余。
func TestContextSearchFetchPaginates(t *testing.T) {
	tool, ctx, long := newLongMessageFixture(t)
	id := extractMessageID(t, mustSearch(t, tool, ctx, "磁盘已满"))

	first, err := tool.Execute(ctx, json.RawMessage(`{"message_id":"`+id+`","max_chars":100}`))
	if err != nil {
		t.Fatalf("取回: %v", err)
	}
	if !strings.Contains(first, "truncated at 100 of") || !strings.Contains(first, "offset=100") {
		t.Fatalf("第一段应给出续读提示:\n%s", first)
	}
	if strings.Contains(first, "[tool calls in this message]") == false {
		t.Fatalf("第一段应带工具调用（续读时不重复）:\n%s", first)
	}

	second, err := tool.Execute(ctx, json.RawMessage(`{"message_id":"`+id+`","max_chars":100,"offset":100}`))
	if err != nil {
		t.Fatalf("续读: %v", err)
	}
	runes := []rune(long)
	if !strings.Contains(second, string(runes[100:200])) {
		t.Fatalf("第二段应接着第 100 个字符往下，got:/n%s", second)
	}
	if strings.Contains(second, "[tool calls in this message]") {
		t.Fatalf("续读段不应重复工具调用:\n%s", second)
	}
}

// TestContextSearchFetchUnknownID 不存在的 id 要报错，不能静默返回空串。
func TestContextSearchFetchUnknownID(t *testing.T) {
	tool, ctx, _ := newLongMessageFixture(t)
	if _, err := tool.Execute(ctx, json.RawMessage(`{"message_id":"event-999999"}`)); err == nil {
		t.Fatal("未知 id 应报错")
	}
	if _, err := tool.Execute(ctx, json.RawMessage(`{}`)); err == nil {
		t.Fatal("既无 query 又无 message_id 应报错")
	}
}

// mustSearch 执行一次检索并返回结果文本（测试辅助）。
func mustSearch(t *testing.T, tool *contextSearchTool, ctx context.Context, query string) string {
	t.Helper()
	out, err := tool.Execute(ctx, json.RawMessage(`{"query":"`+query+`"}`))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return out
}

// TestContextSearchFetchUsesRequestHeadroom 取回的上限来自**上下文余量**，不是常量：
// 同样一条 1.2 万字符的消息，大窗口应整条读完，小窗口才分段。
//
// 这条测试防的就是「写死 8000 字符」：窗口 200K、表面才十几 K 时，一条 1.2 万字符的
// 消息完全放得下，截断只会逼模型再调一次（甚至换别的工具绕路），纯属浪费。
func TestContextSearchFetchUsesRequestHeadroom(t *testing.T) {
	content := strings.Repeat("磁盘已满写入失败", 1500) // 12000 字符，远超旧的 8000 上限
	tool, ctx, long := newMessageFixture(t, content)
	id := extractMessageID(t, mustSearch(t, tool, ctx, "磁盘已满"))

	tool.m.SetContextWindow(200000)
	full, err := tool.Execute(ctx, json.RawMessage(`{"message_id":"`+id+`"}`))
	if err != nil {
		t.Fatalf("取回: %v", err)
	}
	if !strings.Contains(full, long) {
		t.Fatalf("大窗口下应整条返回（%d 字符），got 长度 %d", len([]rune(long)), len([]rune(full)))
	}
	if strings.Contains(full, "truncated") {
		t.Fatalf("放得下时不应截断:\n%.400s", full)
	}

	// 同一个窗口收到 20K：余量不够，必须分段并说清是余量不够（不是模型要得少）
	tool2, ctx2, _ := newMessageFixture(t, content)
	id2 := extractMessageID(t, mustSearch(t, tool2, ctx2, "磁盘已满"))
	tool2.m.SetContextWindow(20000)
	part, err := tool2.Execute(ctx2, json.RawMessage(`{"message_id":"`+id2+`"}`))
	if err != nil {
		t.Fatalf("取回: %v", err)
	}
	if !strings.Contains(part, "truncated at") || !strings.Contains(part, "remaining context headroom") {
		t.Fatalf("小窗口应因余量不足而分段:\n%.400s", part)
	}
	if !strings.Contains(part, "tokens of request headroom left") {
		t.Fatalf("回执应带上算出来的余量，便于判断:\n%.400s", part)
	}
	if strings.Contains(part, long) {
		t.Fatal("小窗口下不应整条返回")
	}
}
