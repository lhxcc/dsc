package core

import (
	"context"
	"fmt"
	"testing"

	"dsc/proto"
)

func msgUser(content string) *proto.Message {
	return &proto.Message{Role: "user", Content: content}
}

// msgAssistantWithCalls 构造一条发起工具调用的 assistant 消息。
func msgAssistantWithCalls(content string, ids ...string) *proto.Message {
	m := &proto.Message{Role: "assistant", Content: content, Images: []string{"dsc-img://keep"}}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, &proto.ToolCall{Id: id, Name: "shell", ArgumentsJson: `{"command":"ls"}`})
	}
	return m
}

func msgToolResult(id, content string) *proto.Message {
	return &proto.Message{Role: "tool", Content: content, ToolCallId: id}
}

// BenchmarkSanitizeToolPairing 量化早退的收益：无工具流量的会话只剩一遍轻量扫描，
// 不建索引 map 也不复制结果 slice。
func BenchmarkSanitizeToolPairingClean(b *testing.B)     { benchmarkSanitize(b, false) }
func BenchmarkSanitizeToolPairingWithTools(b *testing.B) { benchmarkSanitize(b, true) }

func benchmarkSanitize(b *testing.B, withTools bool) {
	msgs := []*proto.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 500; i++ {
		msgs = append(msgs, msgUser("这是一条普通对话消息"))
		if withTools {
			id := fmt.Sprintf("call_%d", i)
			msgs = append(msgs, msgAssistantWithCalls("", id), msgToolResult(id, "output"))
			continue
		}
		msgs = append(msgs, &proto.Message{Role: "assistant", Content: "回复"})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, dropped := SanitizeToolPairing(msgs); dropped != 0 {
			b.Fatalf("dropped = %d, want 0", dropped)
		}
	}
}

// TestSanitizeToolPairingSkipsCleanHistory 没有任何工具流量时早退：
// 返回的是输入本身（同一底层数组，不做索引也不复制），dropped 为 0。
func TestSanitizeToolPairingSkipsCleanHistory(t *testing.T) {
	in := []*proto.Message{
		{Role: "system", Content: "sys"},
		msgUser("讲讲 Go 的调度器"),
		{Role: "assistant", Content: "好"},
		msgUser("再细一点"),
	}
	out, dropped := SanitizeToolPairing(in)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	if len(out) != len(in) {
		t.Fatalf("len = %d, want %d", len(out), len(in))
	}
	for i := range in {
		if out[i] != in[i] {
			t.Fatalf("#%d 不是同一条消息：整理函数不该在无工具流量时重建消息", i)
		}
	}
}

// TestSanitizeToolPairingEarlyExitNeedsBothChecks 早退的两个条件缺一不可：
// 「有调用、无结果」的历史没有 tool 消息，但悬空调用仍必须被清掉——不能因为
// 只查 tool 结果就跳过整理，否则 provider 照样报 calls 与 results 不匹配。
func TestSanitizeToolPairingEarlyExitNeedsBothChecks(t *testing.T) {
	in := []*proto.Message{
		msgUser("跑一下"),
		msgAssistantWithCalls("", "call_X"), // 工具被中断，没有结果
	}
	out, dropped := SanitizeToolPairing(in)
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
	if len(out) != 1 || out[0].GetRole() != "user" {
		t.Fatalf("悬空调用没被清掉：%+v", out)
	}
}

// TestSanitizeToolPairingKeepsIntactHistory 正常配对的历史必须原样保留：
// 既不重排、也不丢字段（这条是整理函数不能「好心办坏事」的底线）。
func TestSanitizeToolPairingKeepsIntactHistory(t *testing.T) {
	in := []*proto.Message{
		{Role: "system", Content: "sys"},
		msgUser("hi"),
		msgAssistantWithCalls("", "call_1"),
		msgToolResult("call_1", "out"),
	}
	out, dropped := SanitizeToolPairing(in)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	want := []string{"system", "user", "assistant", "tool"}
	if len(out) != len(want) {
		t.Fatalf("len = %d, want %d", len(out), len(want))
	}
	for i, role := range want {
		if out[i].GetRole() != role {
			t.Fatalf("#%d role = %q, want %q", i, out[i].GetRole(), role)
		}
	}
	// 字段保真：assistant 的调用与图像引用都还在
	if len(out[2].GetToolCalls()) != 1 || out[2].GetToolCalls()[0].GetId() != "call_1" {
		t.Fatalf("assistant 的调用丢失：%+v", out[2].GetToolCalls())
	}
	if len(out[2].GetImages()) != 1 {
		t.Fatalf("图像引用丢失：%v", out[2].GetImages())
	}
}

// TestSanitizeToolPairingMovesInterruptedInjection 用户在工具执行期间插话时，
// 插进来的消息必须让位：工具结果要紧跟发起它的 assistant 消息。
//
// 真实案例（会话日志 seq 340134–340137）：assistant(job_output) → 后台任务通知
// （user）→ tool(result)。provider 收到这种顺序直接 503「tool calls and tool
// results do not match」，并提示历史已损坏、只能新建任务。
func TestSanitizeToolPairingMovesInterruptedInjection(t *testing.T) {
	in := []*proto.Message{
		msgUser("跑一下"),
		msgAssistantWithCalls("", "call_X"),
		msgUser("background job shell-1 finished"), // 插话落在调用与结果之间
		msgToolResult("call_X", "job output"),
	}
	out, dropped := SanitizeToolPairing(in)
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0（插话与结果都在，只是顺序要修）", dropped)
	}
	var order []string
	for _, m := range out {
		order = append(order, m.GetRole())
	}
	// user → assistant → tool → user：结果被拉近，插话顺延到结果之后
	want := []string{"user", "assistant", "tool", "user"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
	// 相邻性：tool 结果紧跟 assistant，且 id 对得上
	if out[2].GetToolCallId() != out[1].GetToolCalls()[0].GetId() {
		t.Fatalf("结果与调用对不上：%q vs %q", out[2].GetToolCallId(), out[1].GetToolCalls()[0].GetId())
	}
}

// TestSanitizeToolPairingDropsUnmatched 调用没有结果（工具被中断/取消）时，
// 连调用一起丢；没有对应调用的结果也丢。宁可少一条，也不能让条数对不上。
func TestSanitizeToolPairingDropsUnmatched(t *testing.T) {
	// assistant 发了两个调用，只有 A 有结果
	out, dropped := SanitizeToolPairing([]*proto.Message{
		msgAssistantWithCalls("", "call_A", "call_B"),
		msgToolResult("call_A", "ok"),
		msgToolResult("call_orphan", "没人认领"),
	})
	if dropped != 2 { // call_B 的调用 + 孤儿结果
		t.Fatalf("dropped = %d, want 2", dropped)
	}
	if len(out) != 2 || out[0].GetRole() != "assistant" || out[1].GetRole() != "tool" {
		t.Fatalf("out = %v", out)
	}
	if n := len(out[0].GetToolCalls()); n != 1 || out[0].GetToolCalls()[0].GetId() != "call_A" {
		t.Fatalf("应只保留有结果的调用，实际 %+v", out[0].GetToolCalls())
	}
}

// TestSanitizeToolPairingDropsEmptyAssistant 调用全无结果且没有正文的 assistant
// 消息整条丢弃（空 assistant 同样会被 provider 拒）；有正文的退化成文本保留。
func TestSanitizeToolPairingDropsEmptyAssistant(t *testing.T) {
	out, dropped := SanitizeToolPairing([]*proto.Message{
		msgAssistantWithCalls("", "call_gone"),
	})
	if len(out) != 0 {
		t.Fatalf("空 assistant 应整条丢弃，实际 %v", out)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}

	out, dropped = SanitizeToolPairing([]*proto.Message{
		msgAssistantWithCalls("我先说明一下", "call_gone"),
	})
	if len(out) != 1 || out[0].GetContent() != "我先说明一下" || len(out[0].GetToolCalls()) != 0 {
		t.Fatalf("有正文的应退化为文本消息：%+v", out)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
}

// TestSanitizeToolPairingDedupsResult 同一调用的重复结果只留首次。
func TestSanitizeToolPairingDedupsResult(t *testing.T) {
	out, dropped := SanitizeToolPairing([]*proto.Message{
		msgAssistantWithCalls("", "call_A"),
		msgToolResult("call_A", "first"),
		msgToolResult("call_A", "second"),
	})
	if len(out) != 2 || out[1].GetContent() != "first" {
		t.Fatalf("应保留首次结果：%+v", out)
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d, want 1", dropped)
	}
}

// TestAggregateChatSendsPairedMessages 端到端：请求经聚合层交给 provider 时，
// tool 结果必须紧跟发起它的 assistant 消息——插话造成的错位在这一跳就被修掉。
func TestAggregateChatSendsPairedMessages(t *testing.T) {
	m := newRouterManager()
	p := &mockLLMProvider{}
	m.llms["primary"] = p
	m.llmOrder = []string{"primary"}
	m.agentLLMName = "primary"

	srv := &llmAggregateServer{m: m}
	req := &proto.ChatRequest{Messages: []*proto.Message{
		msgUser("跑一下"),
		msgAssistantWithCalls("", "call_X"),
		msgUser("background job shell-1 finished"), // 插话夹在中间
		msgToolResult("call_X", "job output"),
	}}
	if _, err := srv.Chat(context.Background(), req); err != nil {
		t.Fatalf("chat: %v", err)
	}

	got := p.lastMessages
	var roles []string
	for _, m := range got {
		roles = append(roles, m.Role)
	}
	want := []string{"user", "assistant", "tool", "user"}
	if len(roles) != len(want) {
		t.Fatalf("provider 收到 %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("provider 收到 %v, want %v", roles, want)
		}
	}
	// assistant 的调用数与紧随其后的结果 id 必须一致
	if len(got[1].ToolCalls) != 1 || got[2].ToolCallID != got[1].ToolCalls[0].ID {
		t.Fatalf("调用与结果不配对：calls=%+v result=%q", got[1].ToolCalls, got[2].ToolCallID)
	}
}
