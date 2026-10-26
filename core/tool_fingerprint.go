package core

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"dsc/proto"
)

// ToolsFingerprint 计算工具目录的稳定指纹（用于判断「用量锚点与当前请求是否同一个
// 信封」）。对齐 DSH tokenMeter 的 EpochHeader 比较：信封不同（工具增删/描述变化）
// 时，上一次的 provider 用量不能再当作本次压力的底数。
//
// 规范化：按名称排序后，逐项写入 name + description + parameters_json，用不可出现的
// 分隔符拼接（避免「a|b」与「a」+「|b」相撞），取 sha256 前 16 个 hex 字符。
// 只用 16 字符：这是「是否同一信封」的判定，不是安全摘要，碰撞概率足够低且日志可读。
//
// 与 agent 侧写入 llm/attempt 时用的是同一个函数（经 sdk 转发），两侧不会算出两个值。
func ToolsFingerprint(tools []*proto.Tool) string {
	if len(tools) == 0 {
		return "" // 无工具：视为「无信封信息」，由调用方按不可比处理更保守
	}
	sorted := make([]*proto.Tool, len(tools))
	copy(sorted, tools)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].GetName() < sorted[j].GetName() })

	h := sha256.New()
	for _, t := range sorted {
		// \x00 不会出现在工具名/描述/JSON 里，用它做字段与条目分隔。
		h.Write([]byte(t.GetName()))
		h.Write([]byte{0})
		h.Write([]byte(t.GetDescription()))
		h.Write([]byte{0})
		h.Write([]byte(t.GetParametersJson()))
		h.Write([]byte{0, 1})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}
