package main

import (
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
)

// repairToolPairing 修复 Anthropic 消息序列的 tool_use / tool_result 配对不变量。
//
// Anthropic 协议（真实端点与 DeepSeek /anthropic 都一样严格）要求：
//  1. assistant 消息里每个 tool_use，都必须在其**紧接的下一条** user 消息中有对应
//     tool_result（同一条消息里聚合该 assistant 的全部 result）；
//  2. tool_result 的 tool_use_id 必须能对应到更早的 tool_use，且不得重复。
//
// DSC 的历史是 OpenAI 风格的扁平消息：并行工具调用会产生多条相邻的 tool 消息，
// 逐一转换就是多条 user 消息，正好违反第 1 条的「必须紧接、必须聚在同一条」；工具
// 被中断/取消/审批拒绝时还可能没有 result。两者都会得到 400：
// "tool_use ids were found without tool_result blocks immediately after"。
//
// 本函数做四件事：合并并行的 tool_result → 剔除孤儿/重复 result → 为缺失的补合成
// result（is_error=true，让模型知道该工具没有返回结果）→ 末尾悬空的 tool_use 也补上。
func repairToolPairing(msgs []anthropic.MessageParam) []anthropic.MessageParam {
	merged := mergeParallelToolResults(msgs)

	out := make([]anthropic.MessageParam, 0, len(merged)+2)
	known := make(map[string]struct{})    // 已出现过的 tool_use id
	answered := make(map[string]struct{}) // 已有 result 的 tool_use id

	for i := 0; i < len(merged); i++ {
		m := merged[i]

		if m.Role == anthropic.MessageParamRoleAssistant {
			ids := toolUseIDs(m.Content)
			for _, id := range ids {
				known[id] = struct{}{}
			}
			out = append(out, m)
			// 后面不是 user 消息（或已到末尾）→ 立即补一条合成 result，
			// 否则该 tool_use 永远等不到紧随的 tool_result。
			if len(ids) > 0 && (i+1 >= len(merged) || merged[i+1].Role != anthropic.MessageParamRoleUser) {
				out = append(out, anthropic.NewUserMessage(syntheticToolResults(ids, answered)...))
			}
			continue
		}

		if m.Role != anthropic.MessageParamRoleUser {
			out = append(out, m)
			continue
		}

		// user 消息：先剔除非法的 tool_result（孤儿 / 重复）。
		cleaned := make([]anthropic.ContentBlockParamUnion, 0, len(m.Content))
		for _, b := range m.Content {
			if b.OfToolResult != nil {
				id := b.OfToolResult.ToolUseID
				if _, ok := known[id]; !ok {
					continue // 孤儿：前面没有任何 tool_use
				}
				if _, dup := answered[id]; dup {
					continue // 重复 result
				}
				answered[id] = struct{}{}
			}
			cleaned = append(cleaned, b)
		}

		// 若紧接的上一轮是 assistant(tool_use)，补齐它缺失的 result，且置于消息最前，
		// 保证 result 紧跟在 assistant 之后。
		if n := len(out); n > 0 && out[n-1].Role == anthropic.MessageParamRoleAssistant {
			if missing := unansweredToolUses(out[n-1].Content, answered); len(missing) > 0 {
				cleaned = append(syntheticToolResults(missing, answered), cleaned...)
			}
		}

		if len(cleaned) == 0 {
			continue // 内容全被剔除（只剩孤儿 result）→ 丢弃该消息，避免空消息
		}
		m.Content = cleaned
		out = append(out, m)
	}

	return out
}

// mergeParallelToolResults 把相邻且「只含 tool_result」的 user 消息合并为一条。
// 并行工具调用在扁平历史里是多条 tool 消息，转换后即多条 user 消息；Anthropic
// 要求这些 result 聚合在紧跟 assistant 的同一条消息里。
func mergeParallelToolResults(msgs []anthropic.MessageParam) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, 0, len(msgs))
	for _, m := range msgs {
		if n := len(out); n > 0 &&
			m.Role == anthropic.MessageParamRoleUser &&
			out[n-1].Role == anthropic.MessageParamRoleUser &&
			isToolResultOnly(m.Content) && isToolResultOnly(out[n-1].Content) {
			out[n-1].Content = append(out[n-1].Content, m.Content...)
			continue
		}
		out = append(out, m)
	}
	return out
}

// toolUseIDs 取出一条消息里全部 tool_use 的 id（按出现顺序）。
func toolUseIDs(blocks []anthropic.ContentBlockParamUnion) []string {
	var ids []string
	for _, b := range blocks {
		if b.OfToolUse != nil && b.OfToolUse.ID != "" {
			ids = append(ids, b.OfToolUse.ID)
		}
	}
	return ids
}

// unansweredToolUses 返回消息中尚无 result 的 tool_use id。
func unansweredToolUses(blocks []anthropic.ContentBlockParamUnion, answered map[string]struct{}) []string {
	var ids []string
	for _, id := range toolUseIDs(blocks) {
		if _, ok := answered[id]; !ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// isToolResultOnly 判断消息是否只由 tool_result 块构成（可安全合并）。
func isToolResultOnly(blocks []anthropic.ContentBlockParamUnion) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if b.OfToolResult == nil {
			return false
		}
	}
	return true
}

// syntheticToolResults 为缺失结果的工具调用生成占位 result（is_error=true），
// 并登记为已应答，防止后续重复补齐。文案说明「未执行或结果不可用」，模型据此
// 决定重试或改道，而不是把缺失当成空结果继续推理。
func syntheticToolResults(ids []string, answered map[string]struct{}) []anthropic.ContentBlockParamUnion {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(ids))
	for _, id := range ids {
		if _, ok := answered[id]; ok {
			continue
		}
		answered[id] = struct{}{}
		blocks = append(blocks, anthropic.NewToolResultBlock(id,
			fmt.Sprintf("tool call %s produced no result (interrupted, cancelled, or rejected before execution)", id), true))
	}
	return blocks
}
