package dsc

import "context"

// 工具调用的上下文信息（本次调用的调用方身份）。
//
// 为什么需要：工具插件常要做「按会话隔离」的状态（例如持久 shell 的 cwd），而
// session_id 只在 ExecuteToolRequest 里——插件拿不到就只能退化成全局单例，多个
// 对话互相污染。SDK 在调用 Handler 之前把它放进 ctx，插件按需读取即可，不必为
// 每个工具再手写一遍"从哪儿拿会话 id"。
//
// 注意语义：这里的是**调用方**（对话/会话）标识，不是插件自己维护的 shell 会话
// 句柄——后者由插件自行命名（例如作为工具参数）。
type CallInfo struct {
	// SessionID 调用方会话标识（宿主 ChatRequest.session_id 透传；可能为空，
	// 例如宿主内部发起的调用）。
	SessionID string
	// ToolCallID 本次工具调用 id（用于把插件侧日志与模型侧调用对上）。
	ToolCallID string
}

type callInfoKey struct{}

// WithCallInfo 把调用信息放入 ctx（宿主/SDK 内部使用；插件侧只读）。
func WithCallInfo(ctx context.Context, info CallInfo) context.Context {
	return context.WithValue(ctx, callInfoKey{}, info)
}

// CallInfoFrom 读取调用信息；不在工具调用上下文中时返回 false。
func CallInfoFrom(ctx context.Context) (CallInfo, bool) {
	if ctx == nil {
		return CallInfo{}, false
	}
	info, ok := ctx.Value(callInfoKey{}).(CallInfo)
	return info, ok
}

// CallSessionID 返回调用方会话标识（不可用时返回空串）。
//
// 插件用它来给「按会话隔离的状态」（持久 shell、缓存等）选键；空串表示宿主没提供
// 身份（内部调用或无会话上下文），此时插件应退化为自己的默认值，而不是 panic。
func CallSessionID(ctx context.Context) string {
	info, _ := CallInfoFrom(ctx)
	return info.SessionID
}
