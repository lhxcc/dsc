package main

import (
	"encoding/json"
	"strconv"
	"strings"
)

// isTruncatedFinishReason 判定 finish_reason 是否为「输出长度截断」。
// 各 LLM 插件把上游词汇原样透传：Anthropic 系为 max_tokens，OpenAI 系为 length，
// Gemini 系为大写 MAX_TOKENS——故比较统一小写化并容忍首尾空白。
// LLM 插件已从源头移除默认 max_tokens 上限（不携带该字段、等模型自然结束），
// 残余截断来自 provider 侧默认输出上限（真机实测：anthropic 兼容口把 6144 词元
// 的长报告拦腰截断）。收尾侧按本判定做两件事：残参工具调用拒执行（防半截
// JSON 下发），以及预算内「从中断处继续」自动续行（截断不弃任务，见 main.go
// continuation drivers）。
func isTruncatedFinishReason(finishReason string) bool {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "max_tokens", "length":
		return true
	}
	return false
}

// truncationCause 说明本次参数不合法是否与「输出被截断」有关。
//
// 分开写是因为两者该给模型的建议不同：截断是它自己的输出限额问题（同一段继续写完），
// 而标签漏写是格式问题（重发时把 </parameter> 补齐）。合成一句话会让模型以为只要重试就
// 行，于是把同一段残句再发一遍。
func truncationCause(truncated bool, finishReason string) string {
	if truncated {
		return "本次输出在 finish_reason=" + finishReason + " 处被截断，参数因此不完整。"
	}
	return "本次输出没有被标记为截断（finish_reason=" + finishReason + "），因此更可能是调用本身的闭合标签缺失。"
}

// argsSyntaxProblem 用少量字节描述一份参数文本为什么不是合法 JSON。
//
// 只说「不合法」对模型没有行动价值：它需要知道自己是断在未闭合的字符串里，还是少了一个
// 右括号。原文已经在会话历史里，这里补的是**位置**与**断点附近的内容**——两者合起来才让
// 「补哪个括号」这个问题变得可以回答。
//
// 样例故意只留断点前 60 个字节：这条消息会作为工具结果永久留在上下文里，把整段原文（可能
// 很长）再复制一遍，等于此后每次重放都为它付费一次。
func argsSyntaxProblem(argumentsJSON string) string {
	if argumentsJSON == "" {
		return "参数为空"
	}
	// json.Valid 只给布尔值，位置由 Unmarshal 给出。这里只取偏移，不直接引用包的英文错误
	// 文本：模型要的是「在第几个字节、那里写的是什么」。
	var v any
	err := json.Unmarshal([]byte(argumentsJSON), &v)
	if err == nil {
		return "参数看起来合法，但调用仍被判为不可执行"
	}
	se, ok := err.(*json.SyntaxError)
	if !ok || se.Offset <= 0 {
		return err.Error()
	}
	end := int(se.Offset) - 1
	if end > len(argumentsJSON) {
		end = len(argumentsJSON)
	}
	near := argumentsJSON[:end]
	const tail = 60
	if len(near) > tail {
		near = "..." + near[len(near)-tail:]
	}
	// 「到输入结束还没闭合」是漏写标签的典型形状（现场就是这种），单独点明比让人自己数
	// 偏移有用。注意不能用 end >= len 判断：JSON 在最后一个字节发现语法错误时（如
	// `{"a":1,}`）偏移同样等于长度。Go 对「读到结尾」用固定文案 "unexpected end of
	// JSON input"，按它区分才准确。
	if strings.Contains(se.Error(), "unexpected end") {
		return "内容在第 " + strconv.Itoa(end) + " 个字节处突然结束（JSON 没有闭合），末尾是 " +
			strconv.Quote(near)
	}
	return "在第 " + strconv.Itoa(end+1) + " 个字节处无法继续解析，该处起的内容是 " +
		strconv.Quote(argumentsJSON[end:min(len(argumentsJSON), end+40)])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
