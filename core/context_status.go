package core

// 上下文管理状态的宿主协议（对齐 agent/status 的做法：**事件名与载荷由宿主定义**，
// 插件按协议发布，前端按协议展示）。
//
// 为什么必须由宿主定义而不是插件自定义：宿主/frontend 只该依赖「能力」，不该依赖
// 某个插件的私有事件名与私有字段。曾经的做法是 TUI 里写死插件的 `acp/status` 事件
// 名和它的 kv 结构——换一个压缩插件，前端就得跟着改代码，这与「插件是外部能力、
// 只能经能力机制接入」的约定相反。
//
// 现在的分工：
//   - 核心：定义事件名 + 载荷结构（本文件）
//   - 压缩后端插件：按结构自报（它自己决定报哪些指标）
//   - 前端（TUI/未来的 Web）：按结构渲染，不解释任何插件语义

// EventContextStatus 上下文管理后端发布的运行状态。发布方是**当前生效的压缩后端**
// （无论它是哪个插件）；宿主只转发，不校验、不改写。
const EventContextStatus EventName = "context/status"

// ContextStatusEvent 一次上下文管理的状态快照。
//
// 只放「任何后端都能填、任何前端都能懂」的量：占多少、压过多少、还有什么可压。
// 后端的专有指标放 Metrics 里自述（标签由后端给），核心不认识它们的含义——
// 这样新增一个压缩插件不需要改核心，也不需要改前端。
type ContextStatusEvent struct {
	// Tokens 当前上下文占用（token）；Limit 上下文窗口上限（0 = 未知）。
	Tokens int `json:"tokens"`
	Limit  int `json:"limit,omitempty"`
	// BlocksActive / BlocksTotal 活跃块数与总块数（不用块的实现留 0）。
	BlocksActive int `json:"blocksActive,omitempty"`
	BlocksTotal  int `json:"blocksTotal,omitempty"`
	// BlocksNote 后端对块的自行分组说明（例如分层计数），前端原样附带显示。
	BlocksNote string `json:"blocksNote,omitempty"`
	// TokensReclaimed 累计回收的 token；Compressions 达成它的压缩次数。
	TokensReclaimed int `json:"tokensReclaimed,omitempty"`
	Compressions    int `json:"compressions,omitempty"`
	// CompressibleRanges 当前还能压的段数（0 = 无/未知）。
	CompressibleRanges int `json:"compressibleRanges,omitempty"`
	// Metrics 后端专有指标，逐条自述（键与含义都由后端决定）。
	Metrics []ContextMetric `json:"metrics,omitempty"`
}

// ContextMetric 后端自述的一条指标。
//
// Value 是计数（次数/条数），Tokens 是与之相关的 token 量（省下的、存下的），
// Note 是自由说明（如「导出 1 · 落空 0」）。前端只按这三列排版，不猜语义。
type ContextMetric struct {
	Label  string `json:"label"`
	Value  int    `json:"value,omitempty"`
	Tokens int    `json:"tokens,omitempty"`
	Note   string `json:"note,omitempty"`
}
