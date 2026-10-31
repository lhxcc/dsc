package core

import (
	"testing"

	"dsc/proto"
)

// 本文件覆盖「宿主 → LLM 插件」的消息转换保真。
//
// 回归背景：宿主有两条等价的 proto→core.Message 转换（router 用 protoMessagesToPlugin，
// 插件侧 gRPC 入口用 convertLLMChatRequest），其中只有后者抄了 ToolCallId。于是所有
// 走 router 的请求里 tool 消息都没有 tool_call_id —— OpenAI/DeepSeek 直接 422
// `messages[3]: missing field tool_call_id`（现场那份会话 472 条 tool 消息全缺），
// 而 Anthropic 插件按顺序配对，反倒看不出来。

// TestProtoMessagesToPluginKeepsToolCallID tool 消息的 tool_call_id 与 assistant 的
// tool_calls id 都必须原样透传——两者缺一，provider 端的工具调用就无法配对。
func TestProtoMessagesToPluginKeepsToolCallID(t *testing.T) {
	req := &proto.ChatRequest{Messages: []*proto.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do it"},
		{Role: "assistant", ToolCalls: []*proto.ToolCall{
			{Id: "call_1", Name: "read", ArgumentsJson: `{"p":"a"}`},
			{Id: "call_2", Name: "write", ArgumentsJson: `{"p":"b"}`},
		}},
		{Role: "tool", Content: "result-1", ToolCallId: "call_1"},
		{Role: "tool", Content: "result-2", ToolCallId: "call_2"},
	}}

	msgs, _ := protoMessagesToPlugin(req)
	if len(msgs) != 5 {
		t.Fatalf("消息数 = %d, 期望 5", len(msgs))
	}
	if got := msgs[3].ToolCallID; got != "call_1" {
		t.Fatalf("tool 消息 ToolCallID = %q, 期望 call_1（缺了会让 OpenAI 422）", got)
	}
	if got := msgs[4].ToolCallID; got != "call_2" {
		t.Fatalf("tool 消息 ToolCallID = %q, 期望 call_2", got)
	}
	if len(msgs[2].ToolCalls) != 2 ||
		msgs[2].ToolCalls[0].ID != "call_1" || msgs[2].ToolCalls[1].ID != "call_2" {
		t.Fatalf("assistant tool_calls 丢失或错序: %+v", msgs[2].ToolCalls)
	}
}

// TestConvertLLMChatRequestDelegates 两条转换必须同源（曾经各写一份导致漂移）。
func TestConvertLLMChatRequestDelegates(t *testing.T) {
	req := &proto.ChatRequest{Messages: []*proto.Message{
		{Role: "tool", Content: "r", ToolCallId: "call_x"},
	}}
	a, _ := convertLLMChatRequest(req)
	b, _ := protoMessagesToPlugin(req)
	if a[0].ToolCallID != "call_x" || b[0].ToolCallID != "call_x" {
		t.Fatalf("tool_call_id 丢失: convert=%q plugin=%q", a[0].ToolCallID, b[0].ToolCallID)
	}
}

// TestUnparseableArgsSurviveTheRoundTrip 回归：一份参数不合法的调用，其原文必须能走完
// "proto → 插件 → proto" 的整个往返。
//
// 现场（2080 Ti）：模型漏写一个 </parameter>，引擎的解析器把后文折进第一个参数值，参数
// 文本断在字符串中间。修这一处之前，每一层都用 `json.Unmarshal` 吞掉错误、得到空 map，
// 再 `json.Marshal` 又写成 "null"——模型回看历史时只看到自己「调过 exec_command、参数
// null」，无从知道错在哪，也就改不掉。原文只在拿到它的那一刻存在，任何一层重新序列化都
// 等于销毁它。
func TestUnparseableArgsSurviveTheRoundTrip(t *testing.T) {
	// 现场那份参数：缺 </parameter>，于是后面整个 description 被折进 command 的值。
	const broken = `{"command":"cd F:/X && sed -i '968d' a.xml <parameter=description> Delete line 968 </tool_call>`

	// 1) proto → 插件：原文进入 RawArguments，解析错误一并带出。
	args, raw, err := DecodeToolArguments("exec_command", broken)
	if err == nil {
		t.Fatal("the specimen must not parse; the test is meaningless otherwise")
	}
	if args != nil {
		t.Fatalf("args = %v, 期望 nil（解析失败不得伪造一份参数）", args)
	}
	if raw != broken {
		t.Fatalf("raw 原文丢失，得到 %q", raw)
	}

	tc := ToolCall{ID: "call_1", Name: "exec_command", Arguments: args, RawArguments: raw}

	// 2) 回写给模型的那一份：必须是原文逐字节，不是 "null"。
	if got := tc.ArgumentsJSON(); got != broken {
		t.Fatalf("ArgumentsJSON() = %q, 期望原文（重新序列化会把原文写成 \"null\"）", got)
	}
	back := convertToolCallsToProto([]ToolCall{tc})
	if len(back) != 1 || back[0].ArgumentsJson != broken {
		t.Fatalf("proto 往返丢了原文: %+v", back)
	}

	// 3) 再转回来（历史回放）仍然完整——模型要靠它发现漏了闭合标签。
	again := toolCallFromProto(back[0])
	if again.RawArguments != broken {
		t.Fatalf("历史回放丢了原文: %q", again.RawArguments)
	}
}

// TestWellFormedCallKeepsNoRaw 合法调用不得被塞进 RawArguments。
//
// 这条不是洁癖：历史里助手的工具调用每次请求都会重新序列化一遍，若合法调用也开始携带
// 原文，前缀缓存里的字节取决于「原文是否恰好等于我们这次的 marshal 结果」，一处不相等
// 就是整段前缀作废。
func TestWellFormedCallKeepsNoRaw(t *testing.T) {
	const good = `{"p": "a"}`
	args, raw, err := DecodeToolArguments("read", good)
	if err != nil {
		t.Fatalf("合法参数不该报错: %v", err)
	}
	if raw != "" {
		t.Fatalf("raw = %q, 期望空（合法调用只走 Arguments）", raw)
	}
	// 序列化结果与历史实现一致（map 键排序后的紧凑 JSON）。
	if got := (&ToolCall{Arguments: args}).ArgumentsJSON(); got != `{"p":"a"}` {
		t.Fatalf("ArgumentsJSON() = %q, 期望紧凑 JSON", got)
	}

	// 空参数（工具不收参数）是合法调用，不是解析失败。
	if _, raw, err := DecodeToolArguments("noargs", ""); err != nil || raw != "" {
		t.Fatalf("空参数应视为合法: raw=%q err=%v", raw, err)
	}
	// nil 的 Arguments 也必须序列化成 {} 而不是 null——provider 端按对象解析。
	if got := (ToolCall{}).ArgumentsJSON(); got != "{}" {
		t.Fatalf("空 ToolCall 的 ArgumentsJSON() = %q, 期望 {}", got)
	}
}
