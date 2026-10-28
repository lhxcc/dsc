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
//	// 每次请求拿到服务端上报的 usage 后，与「该次实际发出的表面」一起锚定
//	// ——消息 **和工具目录**，两者是同一个请求的两半：
//	meter.SampleUsage(dsc.PromptSideTokens(usage),
//		dsc.EstimateProtoMessagesTokens(sent)+dsc.EstimateProtoToolTokens(tools))
//	// 之后问「下一发大概多少」，不要让插件自己再估一遍：
//	projected := meter.Project(dsc.EstimateProtoMessagesTokens(pending)+dsc.EstimateProtoToolTokens(tools))
//	over := meter.Window() > 0 && projected > meter.Window()*95/100
//
// 注意 `SampleUsage` 的第二个参数必须与 `Project` 的入参**同源**（都是将发出的那份
// 表面的估算）：差量是拿两者相减算出来的，不同源就会把「被裁掉/被压缩掉的部分」
// 算成增长。没有基线（只调了 `UpdateUsage`）时 `Project` 直接返回入参估算，
// 不会拿旧实测值糊弄你——见 core.TokenMeter.projectLocked。

import (
	"dsc/core"
	"dsc/proto"
	"dsc/session"
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

// PromptSideTokens 返回 usage 的输入侧用量，**就是 PromptTokens**。
// 上下文占用只算输入侧（输出已随历史进入下一发 prompt，计进来会重复）；
// 且不再叠加 cacheRead/cacheWrite——OpenAI 兼容端点的 PromptTokens 已含缓存部分，
// 相加会把命中缓存的请求算成近两倍（历史 bug，见 commit 1de5494）。
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

// EstimateProtoToolTokens 估算 proto 形态工具目录的 token 数。
//
// 工具目录是**每次请求都携带**的固定成本，必须与消息估算相加后再判定压力/裁剪；
// 只算消息会系统性地低估一截（插件多时可达上万 token），窗口将满仍会判「还宽裕」。
// 与 core.EstimateToolTokens 共用同一单项公式。
func EstimateProtoToolTokens(tools []*proto.Tool) int { return core.EstimateProtoToolTokens(tools) }

// FormatPressure 把压力比格式化为百分数文本（"82.3%"），供日志/提示统一口径。
func FormatPressure(pressure float64) string { return core.FormatPressure(pressure) }

// LastPromptUsage 从会话事件里倒查最近一次成功且带 prompt 用量的 LLM 调用
// （唯一实现在 session.LastPromptUsage）。重启后的首发请求没有服务端锚点，
// 用它把计量器接上「上次真实值」，不要在插件里自己遍历事件。
//
// 用法：拿到会话后 `meter.UpdateUsage(usage)`，之后可用 `meter.MeasuredPromptTokens()`
// 读到这个实测值。注意它**不会**让 `Project` 变成「实测 + 增量」：`UpdateUsage` 没有
// 增量基线（不知道那次请求发的表面有多大），`Project` 会退回调用方给的估算。要精确
// 投影请在拿到真实响应后用 `SampleUsage(用量, 当次发出的表面估算)`。
func LastPromptUsage(sess *session.Session) (int, *Usage, bool) {
	promptTokens, usage, ok := session.LastPromptUsage(sess)
	if !ok {
		return 0, nil, false
	}
	return promptTokens, UsageFromProto(usage), true
}

// UsageAnchor 上一次成功调用的用量锚点（含请求信封指纹，见 session.UsageAnchor）。
type UsageAnchor = session.UsageAnchor

// LastUsageAnchor 倒查最近一次成功且带用量的调用，连同信封指纹返回。
// 需要判断「用量是否与当前请求可比」时用它，不要只看 prompt 数字。
func LastUsageAnchor(sess *session.Session) (UsageAnchor, bool) {
	return session.LastUsageAnchor(sess)
}

// ToolsFingerprint 工具目录指纹（唯一实现在 core.ToolsFingerprint）。
// 写入 llm/attempt 与判定锚点可比性必须用同一个函数。
func ToolsFingerprint(tools []*proto.Tool) string { return core.ToolsFingerprint(tools) }
