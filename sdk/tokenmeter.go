package dsc

// 上下文计量（SDK 转发层）。
//
// 为什么要有这一层：上下文「已用多少」只有一个正确来源——core.TokenMeter 的投影值
// （实测锚点 + 启发式增量，对齐 DSH tokenMeter 的 pressureTokens / surfaceTokens /
// projectedTokens）。任何需要判断「会不会超窗」「该不该压缩」的插件（压缩后端、
// image-offload 之类改写历史的驻留）都必须读这同一个数；否则各插件各写一份估算，
// 口径一漂就互相打架：DSC 曾经宿主一套实测（无人调用）、agent 一套裸估算，纯 ASCII
// 被高估 4~5 倍，1M 窗口里服务端实测 332K 的会话被判每轮超窗。
//
// 插件请用 dsc.* 而不是直接 import dsc/core：
//   - 依赖面更小（插件的 go.mod 只跟 sdk 走，core 的内部重构不外溢）
//   - 口径天然一致（同一份实现，不是「复制一份看起来一样的公式」）
//
// 用法：
//
//	meter := dsc.NewTokenMeter(window)
//	// 每次请求拿到服务端上报的 usage 后，与「该次实际发出的消息估算」一起锚定：
//	meter.SampleUsage(dsc.PromptSideTokens(usage), dsc.EstimateProtoMessagesTokens(sent))
//	// 之后问「下一发大概多少」，不要让插件自己再估一遍：
//	projected := meter.Project(dsc.EstimateProtoMessagesTokens(pending))
//	over := meter.Window() > 0 && projected > meter.Window()*95/100

import (
	"dsc/core"
	"dsc/proto"
)

// TokenMeter 上下文占用计量器（唯一实现，见 core.TokenMeter）。
// 类型别名而非包装类型：插件侧与宿主侧是同一套方法与语义，不会因为包装而产生漂移。
type TokenMeter = core.TokenMeter

// TokenMeasurement 一次计量的结果（实测值 + 投影值）。
type TokenMeasurement = core.TokenMeasurement

// Usage 一次 LLM 响应的 token 用量（provider 上报）。
type Usage = core.Usage

// NewTokenMeter 创建上下文计量器；window 为上下文窗口大小（token 数，0 表示未知）。
func NewTokenMeter(window int) *TokenMeter { return core.NewTokenMeter(window) }

// PromptSideTokens 返回 usage 的输入侧用量（prompt + cacheRead + cacheWrite）。
// 上下文占用只算输入侧：输出已随历史进入下一发 prompt，计进来会重复。
func PromptSideTokens(usage *Usage) int { return core.PromptSideTokens(usage) }

// EstimateTextTokens 估算一段文本的 token 数（宽字符 1 token/字，其余 4 字节/token）。
func EstimateTextTokens(s string) int { return core.EstimateTextTokens(s) }

// EstimateMessageTokens 估算单条消息（文本 + 角色/结构开销 + 工具调用 + 图像引用）。
func EstimateMessageTokens(msg *core.Message) int { return core.EstimateMessageTokens(msg) }

// EstimateMessagesTokens 估算消息列表总 token 数。
func EstimateMessagesTokens(msgs []*core.Message) int { return core.EstimateMessagesTokens(msgs) }

// EstimateProtoMessageTokens 估算单条 proto 消息（插件在 pre-step 里拿到的就是 proto 形态）。
func EstimateProtoMessageTokens(m *proto.Message) int { return core.EstimateProtoMessageTokens(m) }

// EstimateProtoMessagesTokens 估算 proto 消息列表总 token 数。
func EstimateProtoMessagesTokens(msgs []*proto.Message) int {
	return core.EstimateProtoMessagesTokens(msgs)
}

// FormatPressure 把压力比格式化为百分数文本（"82.3%"），供日志/提示统一口径。
func FormatPressure(pressure float64) string { return core.FormatPressure(pressure) }
