package dsc

// 插件面向的协议类型与工具函数转发层。
//
// 分层约定：**sdk 导入 core，插件只导入 sdk**。
//   - core 是宿主内核（事件总线、会话、计量、协议实现），会持续重构；
//   - sdk 是插件的稳定面（类型别名 + 薄转发），插件据此编译，不随内核重构而碎；
//   - 同一份口径只实现一次（如 token 计量见 tokenmeter.go），插件引 sdk 即天然一致。
//
// 类型一律用别名（type X = core.X）而非包装类型：插件侧与宿主侧是同一套方法与
// 语义，包装会多一层转换并让两边的行为有机会漂移。
//
// 命名冲突说明：dsc.Tool 是「插件定义的工具」（Name/Description/Schema/Handler，
// 见 tool.go），而 core.Tool 是「发给模型的工具 schema」（Name/Description/
// ParametersJSON）。两者是不同概念，故后者在 SDK 里叫 LLMTool。

import (
	"context"
	"time"

	"dsc/core"
	"dsc/proto"
)

// ---- agent 协议 ----

type (
	// RunStreamResponse agent 每步向宿主/TUI 推送的流式帧。
	RunStreamResponse = core.RunStreamResponse
	// AgentResult 一次 Run/RunStream 的最终结果。
	AgentResult = core.AgentResult
	// AgentDebugMessage / AgentGoalDebugInfo / AgentDebugSnapshot 调试快照。
	AgentDebugMessage  = core.AgentDebugMessage
	AgentGoalDebugInfo = core.AgentGoalDebugInfo
	AgentDebugSnapshot = core.AgentDebugSnapshot
)

// ---- agent 事件（agent/pre-step、agent/request-error、agent/status…） ----

type (
	// AgentStatusEvent agent/status 载荷（running/idle）。
	AgentStatusEvent = core.AgentStatusEvent
	// AgentPreStepEvent agent/pre-step 载荷：本步消息列表（MessagesJSON）与工具目录，
	// 压缩/注入类插件在此改写 msgs。
	AgentPreStepEvent = core.AgentPreStepEvent
	// UserQuestionAskedEvent userquestion/asked 载荷：问题数量与首个标题。
	UserQuestionAskedEvent = core.UserQuestionAskedEvent

	// AgentRequestErrorEvent agent/request-error 载荷：请求失败码（如
	// context_window_exceeded），插件可返回 retry 让宿主重走 pre-step。
	AgentRequestErrorEvent = core.AgentRequestErrorEvent
)

// Handshake 宿主与插件的握手配置（MagicCookie 校验）。插件侧以 dsc.Handshake 构造
// go-plugin 服务端，不要自行拼一份——cookie 对不上会导致插件被宿主强杀。
var Handshake = core.Handshake

// AgentStatusValue 回合状态取值类型；AgentStatusIdle 表示回合完成、agent 回到空闲。
type AgentStatusValue = core.AgentStatusValue

const AgentStatusIdle = core.AgentStatusIdle

// 运行模式名（对应 config/presets/<name>.yaml，PluginInfo/env DSC_MODE 里可见）。
const (
	ModeMinimal  = core.ModeMinimal
	ModeStandard = core.ModeStandard
	ModeCreation = core.ModeCreation
	ModePTC      = core.ModePTC
)

const (
	EventAgentPreStep      = core.EventAgentPreStep
	EventAgentRequestError = core.EventAgentRequestError
	EventAgentStatus       = core.EventAgentStatus
	EventAgentError        = core.EventAgentError
	EventApprovalAsked     = core.EventApprovalAsked
	// EventUserQuestionAsked 宿主即将向用户提问（ask_user_question / 计划评审）。
	// 通知类插件订阅它提醒用户「需要你处理」——agent 此时阻塞等待回答。
	EventUserQuestionAsked = core.EventUserQuestionAsked
	EventApprovalDecided   = core.EventApprovalDecided
	EventApprovalPolicy    = core.EventApprovalPolicy
)

// ---- LLM 协议 ----

type (
	// Message 一条对话消息（role/content/tool_call_id/tool_calls/images）。
	Message = core.Message
	// ToolCall assistant 发起的工具调用。
	ToolCall = core.ToolCall
	// LLMTool 发给模型的工具 schema（对应 core.Tool；与 dsc.Tool 不同，见文件头说明）。
	LLMTool = core.Tool
	// ChatResponse 非流式响应。
	ChatResponse = core.ChatResponse
	// ChatStreamResponse 流式响应的一个分片（含 finish_reason 与 usage）。
	ChatStreamResponse = core.ChatStreamResponse
)

// ---- 工具视图（TUI 卡片渲染） ----

// ToolView 工具结果的结构化视图 spec（其余视图类型见 view.go）。
type ToolView = core.ToolView

// ---- 插件服务端实现（go-plugin 入口） ----

type (
	// Agent 宿主侧的 agent 抽象（Run/RunStream/会话管理），插件实现它即可被宿主调用。
	Agent = core.Agent
	// AgentGRPCPlugin 把 agent 实现包装为宿主可加载的 gRPC 插件服务端。
	AgentGRPCPlugin = core.AgentGRPCPlugin
	// LLMGRPCPlugin 把 LLM provider 实现包装为宿主可加载的 gRPC 插件服务端。
	LLMGRPCPlugin = core.LLMGRPCPlugin
)

// UsageFromProto 把 proto 用量转成内部用法结构（provider 插件回传 usage 时用）。
func UsageFromProto(u *proto.Usage) *Usage { return core.UsageFromProto(u) }

// ---- 路径与文本 ----

// SlashErr 把错误文本里的路径归一为正斜杆（见 AGENTS.md §10）。
func SlashErr(err error) error { return core.SlashErr(err) }

// SlashErrText 同 SlashErr，作用于字符串。
func SlashErrText(s string) string { return core.SlashErrText(s) }

// CanonicalPath 返回规范化后的绝对路径（正斜杆）。
func CanonicalPath(p string) (string, error) { return core.CanonicalPath(p) }

// TextRefPrefix 是文本引用的协议前缀（dsc-txt://）。
const TextRefPrefix = core.TextRefPrefix

// ResolveTextRef 解析文本引用（dsc-txt://<id>）为内容。
func ResolveTextRef(ref string) (string, error) { return core.ResolveTextRef(ref) }

// ---- 图像与附件 ----

// DefaultProjectionMaxSide 图像投影的默认最长边（像素）。
const DefaultProjectionMaxSide = core.DefaultProjectionMaxSide

// ProjectImageRef 按最长边限制投影图像引用。
func ProjectImageRef(ref string, maxSide int) (string, error) {
	return core.ProjectImageRef(ref, maxSide)
}

// SaveImageAttachment 保存图像附件并返回引用。
func SaveImageAttachment(data []byte) (string, error) { return core.SaveImageAttachment(data) }

// SaveTextAttachment 保存文本附件并返回引用。
func SaveTextAttachment(data []byte) (string, error) { return core.SaveTextAttachment(data) }

// ---- 进程与空闲看护 ----

// WithIdleDeadline 派生出「空闲超时即取消」的上下文。
func WithIdleDeadline(ctx context.Context, idle time.Duration) (context.Context, context.CancelFunc) {
	return core.WithIdleDeadline(ctx, idle)
}

// TouchActivity 标记上下文有新活动（重置空闲计时）。
func TouchActivity(ctx context.Context) { core.TouchActivity(ctx) }

// IdleDeadlineExceeded 报告上下文是否因空闲超时被取消。
func IdleDeadlineExceeded(ctx context.Context) bool { return core.IdleDeadlineExceeded(ctx) }

// ---- LLM 能力探测 ----

// ModelSupportsImages 探测某模型端点是否支持图像输入。
func ModelSupportsImages(baseURL, model string) bool { return core.ModelSupportsImages(baseURL, model) }

// ClassifyLLMErrorCode 把 LLM 错误归类为稳定错误码（供 request-error 事件与重试决策）。
func ClassifyLLMErrorCode(err error) string { return core.ClassifyLLMErrorCode(err) }
