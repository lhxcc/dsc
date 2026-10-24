package dsc

// 工具对边界（SDK 转发层）。
//
// 压缩与投影都要保证「发往模型的历史」满足工具配对不变量（I1–I3）：tool 消息必有
// 归属、工具对不跨切点、每个 tool_call 都有结果。插件（agent 内联压缩、压缩后端、
// 消息守卫）都靠这三个函数判断与修复；转发到 core 的唯一实现，避免各插件各写一份
// 「看起来一样」的边界判断，口径一漂就产出非法历史。

import (
	"dsc/core"
	"dsc/proto"
)

// SnapRetainStart 把压缩保留区起点向后（向更早）吸附，直到不切开工具对。
// 返回 ok=false 表示无安全切点，调用方应放弃本次压缩。
func SnapRetainStart(msgs []*proto.Message, retainIdx, base int) (int, bool) {
	return core.SnapRetainStart(msgs, retainIdx, base)
}

// ValidateToolPairing 按工具配对不变量校验消息列表；nil 表示配对完整。
func ValidateToolPairing(msgs []*proto.Message) error { return core.ValidateToolPairing(msgs) }

// RepairToolPairing 在配对不完整时做最小修复，返回修复后的列表与被修复项数。
func RepairToolPairing(msgs []*proto.Message) ([]*proto.Message, int) {
	return core.RepairToolPairing(msgs)
}
