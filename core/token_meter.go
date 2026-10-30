package core

import (
	"fmt"
	"strconv"
	"sync"

	"dsc/proto"
)

// Token 计量服务（对齐 DSH/Cordis 的 ctx.tokenMeter 服务）。
//
// DSH 的 TokenMeter 是一个 Service（TokenMeter extends Service），提供：
//   - measure(session, requestHeader) → TokenMeasurement（压力感知 + 表面 token 计量）
//   - 经 sessionProjections 注册 tokenUsage / contextPressure / contextBreakdown 投影
//   - 重放感知：基于最新成功调用的 provider usage 作为锚点，增量计算
//
// DSC 的适配：DSC 的 agent 是独立 gRPC 插件进程，session 是宿主侧的 event-sourced 日志。
// 宿主侧的 TokenMeter 提供同步的 token 估算与用量追踪，agent 经 admin API 或
// 直接经宿主调用获取计量结果。计量策略对齐 DSH：
//   - 优先使用 provider 上报的 Usage（精确）
//   - provider 不可用或低估（提示缓存命中时 input_tokens 仅含增量）时回退字节级启发式
//   - 提示缓存感知：input_tokens + cache_read_input_tokens 才是完整请求 token
//   - 上下文压力 = 已用 token / 上下文窗口

// TokenMeasurement 一次 token 计量的结果（对齐 DSH TokenMeasurement）。
type TokenMeasurement struct {
	// TotalTokens 当前上下文总 token 数（含 system prompt、历史、工具定义）。
	TotalTokens int `json:"total_tokens"`
	// PromptTokens 最近一次 LLM 请求的 prompt token 数（provider 上报值）。
	PromptTokens int `json:"prompt_tokens"`
	// CompletionTokens 最近一次 LLM 响应的 completion token 数。
	CompletionTokens int `json:"completion_tokens"`
	// CacheReadTokens 提示缓存命中的读取 token 数。
	CacheReadTokens int `json:"cache_read_tokens"`
	// CacheCreationTokens 提示缓存写入的 token 数。
	CacheCreationTokens int `json:"cache_creation_tokens"`
	// ContextWindow 上下文窗口大小（token 数）。
	ContextWindow int `json:"context_window"`
	// Pressure 上下文压力比（0.0 ~ 1.0+），= TotalTokens / ContextWindow。
	Pressure float64 `json:"pressure"`
	// ProjectedTokens 下一次请求的预计占用（实测锚点 + 自锚点以来的启发式增量）。
	// 决策（裁剪/压缩）与提示文案一律用这个值，与 TUI 显示的实测值同源。
	ProjectedTokens int `json:"projected_tokens"`
	// Source 计量来源："usage"（provider 上报）或 "estimate"（启发式估算）。
	Source string `json:"source"`
}

// TokenMeter token 计量服务（对齐 DSH TokenMeter Service）。
// 线程安全；agent 的 ReAct 循环每轮调用 UpdateUsage 更新锚点，
// 调用 Measure 获取当前压力感知。
type TokenMeter struct {
	mu            sync.RWMutex
	lastUsage     *Usage // 最近一次 provider 上报的用量（精确锚点）
	contextWindow int    // 上下文窗口大小
	totalTokens   int    // 当前估算的总 token（用于无 usage 时回退）

	// 锚点与增量基线（对齐 DSH tokenMeter 的 pressureTokens / sampledSurfaceTokens）：
	//   pressureTokens  服务端上报的 prompt 侧用量（PromptTokens；OpenAI 兼容端点已含
	//                   缓存命中，再叠加 cacheRead/cacheWrite 会重复计数——见 PromptSideTokens）
	//   sampledEstimate 取锚点那一刻「所发请求」的启发式估算（消息 + 工具目录）
	// 两者必须来自同一个请求，否则增量算错。projected = pressure + (当前估算 − sampled)。
	pressureTokens  int
	sampledEstimate int
	hasSample       bool
}

// NewTokenMeter 创建 token 计量服务。contextWindow 为上下文窗口大小（token 数）。
func NewTokenMeter(contextWindow int) *TokenMeter {
	return &TokenMeter{
		contextWindow: contextWindow,
	}
}

// UpdateUsage 更新最近一次 provider 上报的用量（线程安全）。
// agent 的 ReAct 循环在每次 LLM 响应后调用此方法。
// 提示缓存感知：若 cache_read > 0，实际请求 token = prompt_tokens + cache_read_tokens
// （provider 在缓存命中时只上报增量部分，需加回 cache_read 才是完整请求大小）。
func (tm *TokenMeter) UpdateUsage(usage *Usage) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if usage == nil {
		return
	}
	tm.lastUsage = usage
	fullPrompt := PromptSideTokens(usage)
	tm.totalTokens = fullPrompt + int(usage.CompletionTokens)
	// 只有用量、不掌握「当次所发消息的估算」时也建立锚点，但不设增量基线：
	// 投影会退化为实测值（保守下界），而不是把整段估算加到实测上（那会重复计）。
	// 不设增量基线：我们不知道这次请求发出去时消息的估算是多少，无法换算增量。
	// 此后 Project 会**退回调用方给的估算**（见 projectLocked），而不是把这个
	// 实测值当投影——实测值只描述那一次请求，钉住它就是「已用永远不动」。
	tm.pressureTokens = fullPrompt
	tm.sampledEstimate = 0
	tm.hasSample = true
}

// SampleUsage 记录一次「实测锚点 + 增量基线」：promptSideTokens 是服务端为某个请求
// 上报的 prompt 侧用量，sentEstimate 是**同一个请求**发出的消息列表的启发式估算。
// 之后用 Project 回答「下一发大概多少」——启发式只承担增量，绝对误差在锚点处被实测抵消。
func (tm *TokenMeter) SampleUsage(promptSideTokens, sentEstimate int) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.pressureTokens = promptSideTokens
	tm.sampledEstimate = sentEstimate
	tm.hasSample = true
	tm.totalTokens = promptSideTokens
}

// Reset 清空锚点（会话切换、压缩改写历史后调用）：此后 Project 退回纯估算，
// 直到下一次服务端上报重新锚定。
func (tm *TokenMeter) Reset() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.pressureTokens = 0
	tm.sampledEstimate = 0
	tm.hasSample = false
	tm.totalTokens = 0
	tm.lastUsage = nil
}

// Project 返回「即将发出的请求」的上下文占用估计：
//   - 有锚点：实测 pressure + 自锚点以来的启发式增量
//   - 无锚点（首次请求 / 重启恢复 / 刚压缩完）：退回 currentEstimate
//
// currentEstimate 为当前待发消息列表的启发式估算（core.EstimateProtoMessagesTokens）。
func (tm *TokenMeter) Project(currentEstimate int) int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.projectLocked(currentEstimate)
}

// Window 返回当前上下文窗口大小。
func (tm *TokenMeter) Window() int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.contextWindow
}

// PromptSideTokens 返回 usage 的 prompt 侧用量——上下文占用只算输入侧；输出已随历史
// 进入下一发的 prompt，计进来会重复。
//
// **口径契约（改动前先读这段）**：Usage.PromptTokens 必须是**完整 prompt**（含命中缓存
// 的全部输入），CacheRead/CacheCreation 只是其中「由缓存承担 / 写入缓存」的部分，用于
// 显示命中率，**不得再加进总数**。各家上报语义本就不同（Anthropic 的 input_tokens 不含
// 缓存、OpenAI 兼容端点的 prompt_tokens 含缓存），统一在 provider 的 usage 映射里归一
// ——归一化只做一次，消费侧（本函数、压缩、裁剪、TUI）都按同一个数走。
//
// 曾经的 bug：这里写成 prompt + cacheRead + cacheCreation，而 OpenAI 兼容端点本就
// prompt = miss + cached，于是变成 P + cached + (P-cached) = 2P。缓存命中 99.95% 的
// 会话（服务端实测 498K）被算成 ~1.05M，误报「单轮内容超出上下文窗口」。
func PromptSideTokens(usage *Usage) int {
	if usage == nil {
		return 0
	}
	return int(usage.PromptTokens)
}

// UpdateContextWindow 更新上下文窗口大小（模型探测后可能变化）。
func (tm *TokenMeter) UpdateContextWindow(window int) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.contextWindow = window
}

// Measure 返回当前 token 计量结果（线程安全）。
// messages 为当前请求的消息列表（供无 usage 时字节级启发式估算回退）。
func (tm *TokenMeter) Measure(messages []*Message) TokenMeasurement {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	m := TokenMeasurement{
		ContextWindow: tm.contextWindow,
	}

	est := EstimateMessagesTokens(messages)
	if tm.lastUsage != nil {
		// 精确路径：provider 上报了 usage
		m.PromptTokens = int(tm.lastUsage.PromptTokens)
		m.CompletionTokens = int(tm.lastUsage.CompletionTokens)
		m.CacheReadTokens = int(tm.lastUsage.CacheReadInputTokens)
		m.CacheCreationTokens = int(tm.lastUsage.CacheCreationInputTokens)
		// PromptTokens 已含缓存部分（见 PromptSideTokens 的契约说明），不再相加
		m.TotalTokens = m.PromptTokens + m.CompletionTokens
		m.Source = "usage"
		m.ProjectedTokens = tm.projectLocked(est)
	} else {
		// 回退路径：字节级启发式估算（对齐 DSH estimateMessage）
		m.TotalTokens = est
		m.ProjectedTokens = est
		m.Source = "estimate"
	}

	if tm.contextWindow > 0 {
		m.Pressure = float64(m.TotalTokens) / float64(tm.contextWindow)
	}

	return m
}

// ShouldCompact 判断「下一次请求」的预计占用是否超过阈值，需要触发压缩。
// currentEstimate 为待发消息列表的启发式估算，thresholdRatio 为触发比例（如 0.80）。
func (tm *TokenMeter) ShouldCompact(currentEstimate int, thresholdRatio float64) bool {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if tm.contextWindow <= 0 {
		return false
	}
	projected := tm.projectLocked(currentEstimate)
	return projected > 0 && float64(projected)/float64(tm.contextWindow) >= thresholdRatio
}

// projectLocked 是 Project 的无锁实现（须已持有读锁）。
func (tm *TokenMeter) projectLocked(currentEstimate int) int {
	if currentEstimate < 0 {
		currentEstimate = 0
	}
	if !tm.hasSample {
		return currentEstimate
	}
	// 锚点自身已超出窗口：说明窗口配置小于 provider 实际接受的规模（探测值偏小，
	// 或 usage 口径含缓存/思考 token）。继续以它为底数，会把**之后每一次**请求都判成
	// 超窗——历史被裁到只剩最近几条也照样报错，而 provider 明明一直在接收。
	// 这种锚点不再描述「现在的请求」，退回纯估算（与无锚点时同样的下界语义）。
	if tm.contextWindow > 0 && tm.pressureTokens > tm.contextWindow {
		return currentEstimate
	}
	// 没有增量基线（只有用量、不知道那一次请求的表面）：实测值**不能**替换当前估算。
	//
	// 它描述的是「那一次请求」，而 currentEstimate 描述的是「即将发出的这一发」；两者
	// 之间没有任何可换算的关系，拿实测当投影就等于把一个旧数字钉死在界面上。
	//
	// 曾经这里返回实测值当「保守下界」。在小窗口读超长会话（滑动窗口）的常态下那是
	// 灾难：每步都在裁剪 → 裁剪后不重新锚定 → sampledEstimate 永远是 0 → Project 恒等
	// 于启动时倒查到的那个旧用量。实测现场：会话已涨到 730K（provider 实测），界面
	// 「已用」从首帧起一直是 33K / 3.29% 不动，而速率、命中率照常变（它们读的是另一
	// 个数）。无基线就退回估算——它会随历史增长，且与裁剪判定同口径。
	if tm.sampledEstimate <= 0 {
		return currentEstimate
	}
	if p := tm.pressureTokens + (currentEstimate - tm.sampledEstimate); p > 0 {
		return p
	}
	return 0
}

// MeasuredPromptTokens 返回最近一次采样到的 provider prompt 侧用量（0 = 尚无采样）。
//
// 只用于**展示**的兜底（宿主侧计量实例在没有上报值、也没有改写表面时读它）：
// 那是一个真实发生过的请求规模，不需要再拿估算去换算。注意不能拿 Project(0) 代替
// ——那算的是 «实测 − 该次请求的估算»，也就是启发式的**误差**，不是规模。
func (tm *TokenMeter) MeasuredPromptTokens() int {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	if !tm.hasSample {
		return 0
	}
	return tm.pressureTokens
}

// EstimateMessagesTokens 估算消息列表的总 token 数（对齐 DSH estimateMessage）。
// 字节级启发式：取「字节数/4」与「rune 数」的较大者——英文按 /4（约 4 字符 1 token），
// CJK（UTF-8 每字 3 字节）字节/4 会低估，故回调为 rune 数（每字按 1 token）。
func EstimateMessagesTokens(messages []*Message) int {
	total := 0
	for _, msg := range messages {
		total += EstimateMessageTokens(msg)
	}
	return total
}

// imageStructuralTokens 单个图像引用的启发式成本（对齐 DSH tokenMeter 的
// estimateStructuralBlock：BLOCK_OVERHEAD + 引用 JSON / 4）。
//
// 图像的特殊性——「当时有效，过期无效」：DSC 消息面只承载 dsc-img:// 引用
// （字节计量在附件/Provider 解析层，引用本身无字节可计）。真实图像 token 按
// provider 路由计价（尺寸相关、上限由 provider 决定，如 DeepSeek 单图 1024），
// 经最近一次请求的 provider usage 锚点进入压力（当时有效）；过期图像由
// image-offload 驻留按 count 预算最旧退役为占位文本，不再逐请求重放（过期无效）。
// 故启发式只计结构引用成本——若按固定上限（如 384）永久计入历史图像，工具截图
// 随轮次线性累积的图像密集会话会被持续高估，误判超阈值而提前压缩文本历史。
func imageStructuralTokens(ref string) int {
	if ref == "" {
		return 0
	}
	return 4 + (len(ref)+3)/4 // BLOCK_OVERHEAD + ceil(引用字符 / 4)
}

// EstimateMessageTokens 估算单条消息的 token 数（对齐 DSH estimateMessage）。
// 文本估算 + 每条消息的固定结构开销（角色、tool_call_id 等）；
// 工具调用额外计入名称与参数；图像按结构引用计价（见 imageStructuralTokens）。
func EstimateMessageTokens(msg *Message) int {
	if msg == nil {
		return 0
	}
	toks := EstimateTextTokens(msg.Content)
	if msg.Role == "tool" {
		toks += 6 // tool 角色 + tool_call_id 开销
	} else {
		toks += 4 // role + delimiter 开销
	}
	for _, img := range msg.Images {
		toks += imageStructuralTokens(img)
	}
	for _, tc := range msg.ToolCalls {
		// 工具调用开销：name + arguments（合并估算避免 name 重复计入）
		argsJSON := fmt.Sprintf("%v", tc.Arguments)
		toks += EstimateTextTokens(tc.Name+argsJSON) + 8
	}
	return toks
}

// 每千字符的 token 数（per-mille）。这组系数不是猜的：用真实会话内容切段，逐段
// 发给模型端点读 `usage.prompt_tokens`，再对「各类字符数 → token 数」做最小二乘得到
// （36 段训练 / 12 段留出，留出集 pred/actual 中位 1.008、p10–p90 = 0.90–1.10）。
//
// 旧公式是「宽字符 1 token/字、其余 4 字节/token」，两个假设都被实测否证：
//   - 窄字符并不等价：标点 0.73/字、数字 0.62/字、字母 0.23/字、空白 0.14/字。
//     工具输出、代码、JSON 全是前两类，按 0.25 估会低估 1.5–3 倍；
//   - CJK 也不是 1 token/字，实测 0.87（合成重复文本才会掉到 0.62——BPE 会合并）。
//
// 这两处误差方向相反，在混合内容里互相抵消，所以旧公式「看起来还能用」；但只要会话
// 偏向一侧（纯中文，或纯工具输出）就会系统性偏掉，而偏差系数只能修正整体比例，
// 修不了「按内容构成变化」的部分——那正是这份分类系数的意义。
const (
	tokPerMilleLetter = 226  // ASCII 字母 / 拉丁词
	tokPerMilleSpace  = 139  // 空白（空格、换行、制表）
	tokPerMilleDigit  = 624  // ASCII 数字
	tokPerMillePunct  = 734  // ASCII 标点与符号：{}[](),:;" + - * / = 等
	tokPerMilleWide   = 873  // CJK 表意、假名、谚文、全角符号
	tokPerMilleEmoji  = 1800 // emoji（多为多 token）
	tokPerMilleSymbol = 1000 // 其余非 ASCII 符号与字母（破折号、箭头、引号、带音标字母）
)

// EstimateTextTokens 估算一段文本的 token 数：**按字符类别加权累加**（千分整数，
// 最后一次性四舍五入，避免逐字取整把短文本抬到 1 token/字）。
//
// 分类见 tokPerMille* 常量。非空文本至少返回 1——空串返回 0，调用方据此区分
// 「没有内容」与「内容很短」。
func EstimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	acc := 0
	for _, r := range s {
		acc += tokenWeightPerMille(r)
	}
	if acc <= 0 {
		return 1
	}
	if t := (acc + 999) / 1000; t > 0 {
		return t
	}
	return 1
}

// tokenWeightPerMille 返回单个字符的千分权重（见 tokPerMille* 常量）。
func tokenWeightPerMille(r rune) int {
	switch {
	case r < 0x80:
		switch {
		case r >= '0' && r <= '9':
			return tokPerMilleDigit
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			return tokPerMilleSpace
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			return tokPerMilleLetter
		}
		return tokPerMillePunct
	case isWideRune(r):
		if r >= 0x1F300 && r <= 0x1FAFF {
			return tokPerMilleEmoji
		}
		return tokPerMilleWide
	}
	return tokPerMilleSymbol
}

// isWideRune 报告是否为 CJK 一族（一字接近一个 token）的宽字符：CJK 部首、假名、
// 统一表意、谚文、兼容表意、全角字符、emoji、扩展区。判宽的代价（按 0.87/字）远小于
// 把 CJK 当 ASCII 按字节折算（会低估到 0.75/字以下）。
func isWideRune(r rune) bool {
	switch {
	case r < 0x1100:
		return false // ASCII / 拉丁 / 希腊 / 西里尔等
	case r <= 0x115F:
		return true // 韩文字母
	case r == 0x2329 || r == 0x232A:
		return true
	case r >= 0x2E80 && r <= 0xA4CF:
		return true // CJK 部首、假名、CJK 统一表意、彝文
	case r >= 0xAC00 && r <= 0xD7A3:
		return true // 韩文音节
	case r >= 0xF900 && r <= 0xFAFF:
		return true // CJK 兼容表意
	case r >= 0xFE30 && r <= 0xFE6F:
		return true // CJK 兼容形式
	case r >= 0xFF00 && r <= 0xFF60:
		return true // 全角字符
	case r >= 0xFFE0 && r <= 0xFFE6:
		return true // 全角符号
	case r >= 0x1F300 && r <= 0x1FAFF:
		return true // emoji
	case r >= 0x20000 && r <= 0x3FFFD:
		return true // CJK 扩展 B 及以后
	}
	return false
}

// EstimateProtoMessageTokens 估算单条 proto 消息的 token 数（proto 版
// EstimateMessageTokens）：复用 EstimateTextTokens（CJK 感知）+ 每条消息的
// 结构开销（角色/tool_call_id）+ 图像结构引用 + 工具调用名称与参数。供宿主
// pre-step、压缩插件等经 proto.ChatRequest.Messages 判定压力的路径共用同一
// 公用启发式，避免各处自造粗略字节/4 估算（不含工具定义、CJK 低估）造成
// 压力判定漂移——真实用量已超阈值而压缩不触发。
func EstimateProtoMessageTokens(m *proto.Message) int {
	if m == nil {
		return 0
	}
	toks := EstimateTextTokens(m.GetContent())
	if m.GetRole() == "tool" {
		toks += 6 // tool 角色 + tool_call_id 开销
	} else {
		toks += 4 // role + delimiter 开销
	}
	for _, img := range m.GetImages() {
		toks += imageStructuralTokens(img)
	}
	for _, tc := range m.GetToolCalls() {
		toks += EstimateTextTokens(tc.GetName()+tc.GetArgumentsJson()) + 8
	}
	return toks
}

// EstimateProtoMessagesTokens 估算 proto 消息列表的总 token 数（proto 版
// EstimateMessagesTokens，含 system 前缀消息）。
func EstimateProtoMessagesTokens(msgs []*proto.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateProtoMessageTokens(m)
	}
	return total
}

// EstimateToolTokens 估算工具定义的 token 数（system prompt 中的工具目录）。
// 每个工具按 name + description + parameters JSON 的字符估算 + 固定结构开销。
func EstimateToolTokens(tools []Tool) int {
	total := 0
	for _, t := range tools {
		total += estimateOneToolTokens(t.Name, t.Description, t.ParametersJSON)
	}
	return total
}

// EstimateProtoToolTokens 估算 proto 形态的工具目录 token 数（宿主 pre-step 与 agent
// 请求构造拿到的就是 proto.Tool）。
//
// 为什么必须单独算：工具目录是**每一次请求都携带**的固定成本，且不随会话增长消失。
// 此前所有压力估算都只算消息（EstimateProtoMessagesTokens），工具目录从未计入——
// 插件多的配置下这块可达数千到上万 token，等于压力永远被低估一截：窗口快满时仍判
// 「还宽裕」，该压缩不压。DSH 的 tokenMeter.measure 把 estimateToolsTokens(header)
// 计入 baseline，此处对齐同一口径。
//
// 与 EstimateToolTokens 共用同一个单项公式（estimateOneToolTokens），两种入参形态不会
// 算出两个数。
func EstimateProtoToolTokens(tools []*proto.Tool) int {
	total := 0
	for _, t := range tools {
		total += estimateOneToolTokens(t.GetName(), t.GetDescription(), t.GetParametersJson())
	}
	return total
}

// estimateOneToolTokens 单个工具定义的 token 估算（唯一公式）。
//
// **刻意不用 EstimateTextTokens**：那组系数是为「异质内容」拟合的，而工具定义是
// 高度重复的同构 JSON——provider 紧凑序列化后，`"type":"string"`、`"description":`
// 这类片段被 BPE 反复合并。实测边际成本只有 0.257 token/字符（schema 250 字符 → 75
// token；2274 字符 → 595 token），用内容系数（0.6–0.7/字符）会高估 2 倍以上，而工具
// 目录是每次请求都发的固定成本，几百个工具下差出来就是几万 token。
//
// 因此这里按「字节/4 + 小固定开销」，与实测同量级。
func estimateOneToolTokens(name, description, parametersJSON string) int {
	return toolDefinitionOverheadTokens + (len(name)+len(description)+len(parametersJSON)+3)/4
}

// toolDefinitionOverheadTokens 单个工具定义的固定结构开销（外层对象、字段名、
// 分隔符等），与具体 schema 无关的常量部分。实测截距约 11 token。
const toolDefinitionOverheadTokens = 11

// FormatPressure 格式化压力比为人读字符串（如 "78%"）。
func FormatPressure(pressure float64) string {
	pct := int(pressure * 100)
	return strconv.Itoa(pct) + "%"
}
