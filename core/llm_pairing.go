package core

import (
	"strings"

	"dsc/proto"
)

// SanitizeToolPairing 让发给 provider 的消息里 tool_call 与 tool_result 严格配对，
// 返回整理后的消息与「被丢弃的消息条数」。
//
// 为什么需要它：会话历史是 append-only 的 surface 投影，任何在工具执行期间到达的消息
// 都会被插到「发起调用的 assistant 消息」与「它的结果」之间——用户在模型干活时插话、
// 后台任务完成的通知都是这么进来的。投影出来就是：
//
//	assistant(tool_calls=[X]) → user("我插一句") → tool(result X)
//
// 而 OpenAI / Anthropic / 各家兼容协议都要求 tool 结果紧跟着发起它的那条 assistant 消息、
// 且条数相等；中间夹一条 user 就是「tool calls and tool results do not match」
// （实测某 provider 直接 503/11148，并提示「请新建任务」——历史已无法自愈）。
//
// 处理顺序（先索引、再重排）：
//  1. 索引所有 role=tool 的结果（按 call id；重复结果只留首次）；
//  2. 遇到带 tool_calls 的 assistant：只保留「有结果」的调用，然后按调用声明顺序
//     把对应结果紧随其后输出——插在中间的 user/system 消息因此被自然推到结果之后；
//  3. 没有对应调用的 assistant 调用、没有对应调用的结果、重复的结果一律丢弃并计数；
//  4. 调用全无结果且正文为空的 assistant 消息整条丢弃（空 assistant 同样会被拒）。
//
// 只在发送前整理请求副本，不改会话事件日志，因此历史始终可完整恢复。
func SanitizeToolPairing(msgs []*proto.Message) ([]*proto.Message, int) {
	// 0) 早退：历史里既没有 tool 结果、也没有带 tool_calls 的 assistant 时不可能不配对。
	//
	// 这是确定性等价，不是启发式：缺了这两类消息，循环体里每条消息都只可能走 default
	// 分支原样保留，输出必然与输入顺序一致、dropped 必然为 0。此时直接返回原 slice，
	// 省掉下面的索引 map 与结果 slice 的分配（纯对话会话不会有任何工具流量）。
	//
	// 注意两个条件缺一不可：只查 tool 结果会漏掉「有调用、无结果」的历史——那种情况
	// 下面是要丢掉悬空调用的，不能跳过。
	if !hasToolTraffic(msgs) {
		return msgs, 0
	}

	// 1) 索引工具结果
	results := make(map[string]*proto.Message, 8)
	dropped := 0
	for _, m := range msgs {
		if m.GetRole() != "tool" || m.GetToolCallId() == "" {
			continue
		}
		if _, dup := results[m.GetToolCallId()]; dup {
			dropped++ // 同一调用的第二份结果：保留首次
			continue
		}
		results[m.GetToolCallId()] = m
	}

	used := make(map[string]bool, len(results))
	out := make([]*proto.Message, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.GetRole() == "tool":
			continue // 结果由发起它的 assistant 消息带出，不在此处直接输出

		case m.GetRole() == "assistant" && len(m.GetToolCalls()) > 0:
			kept := make([]*proto.ToolCall, 0, len(m.GetToolCalls()))
			for _, tc := range m.GetToolCalls() {
				if tc.GetId() != "" && results[tc.GetId()] != nil {
					kept = append(kept, tc)
					continue
				}
				dropped++ // 调用没有结果（工具被中断/取消）：连同其 id 一起丢弃
			}
			if len(kept) == 0 {
				if strings.TrimSpace(m.GetContent()) == "" {
					continue
				}
				out = append(out, copyMessageWithoutCalls(m))
				continue
			}
			out = append(out, copyMessageWithCalls(m, kept))
			for _, tc := range kept {
				if used[tc.GetId()] {
					continue
				}
				used[tc.GetId()] = true
				out = append(out, results[tc.GetId()])
			}

		default:
			out = append(out, m)
		}
	}
	dropped += len(results) - len(used) // 没有任何 assistant 认领的结果
	return out, dropped
}

// hasToolTraffic 判断这批消息里是否存在任何「工具流量」：tool 结果，或发起调用的 assistant。
// 两者都不存在时 SanitizeToolPairing 必然是恒等变换，可以直接跳过。
func hasToolTraffic(msgs []*proto.Message) bool {
	for _, m := range msgs {
		if m.GetRole() == "tool" {
			return true
		}
		if m.GetRole() == "assistant" && len(m.GetToolCalls()) > 0 {
			return true
		}
	}
	return false
}

// copyMessageWithCalls 复制一条消息并替换其工具调用列表（保留全部字段）。
//
// 不能取值拷贝 *proto.Message：结构体内含 protobuf 内部状态（含 mutex），
// go vet 的 copylocks 会告警，逐字段新建才是安全写法。
func copyMessageWithCalls(m *proto.Message, calls []*proto.ToolCall) *proto.Message {
	return &proto.Message{
		Role:       m.GetRole(),
		Content:    m.GetContent(),
		ToolCallId: m.GetToolCallId(),
		ToolCalls:  calls,
		Images:     append([]string(nil), m.GetImages()...),
	}
}

// copyMessageWithoutCalls 同上，但去掉工具调用（退化为纯文本 assistant 消息）。
func copyMessageWithoutCalls(m *proto.Message) *proto.Message {
	return copyMessageWithCalls(m, nil)
}
