package acp

// CCR：内容缓存回取（对齐 acp-kernel src/ccr.ts + content-store.ts）。
//
// 问题：absorb / crush 都是有损的——一旦被摘要成一段话，原文就没了。模型想不起
// 某个精确的报错行或完整 JSON 时只能重跑工具。
//
// CCR 的做法：工具结果到达时就按内容 hash 存一次，线上只留一个**确定性的占位符**
// （带种类、大小、命令预览、ref），模型用 acp_retrieve 把原文取回来。
//
// ## 与 DSC 现有机制的关系（这是本实现与内核最大的不同）
//
// 内核自带一套 content-store（JSON 落盘）。DSC 已经有**内容寻址的附件库**：
// `dsc-txt://<sha>` / `dsc-img://<sha>` 存在 AttachmentDir，provider 在请求时解析
// 它们——这本身就是一个现成的 content store。所以这里：
//
//   - **文本**：字节交给 dsc.SaveTextAttachment，拿回 dsc-txt://<sha>（sha 即内容
//     hash，天然去重），不复制存储层；
//   - **图像**：工具结果本就以 dsc-img://<sha> 形式随消息透传（provider 已能解析），
//     CCR 只把这些 ref 记进条目，取回时原样再挂回工具结果，不搬字节；
//   - **其它二进制**：导出成工作区文件，回取时给指针，由模型自己的文件读取工具翻页。
//
// 纯逻辑（本文件）不碰 IO：它只决定「要不要卸载」「占位符长什么样」「回取怎么切」。
// 落盘与持久化由插件侧（main.go / adapter）执行——与内核"kernel performs no I/O
// as host effects"的约定一致。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// CCRConfig 内容缓存回取配置（默认关闭）。
type CCRConfig struct {
	Enabled bool `json:"enabled"`
	// MinToolTokens 工具结果达到该 token 数才卸载（默认 2000，比 absorb 的门更高：
	// 小结果留在线上更省事，来回取一次也是成本）。
	MinToolTokens int `json:"minToolTokens"`
	// InlineLimitTokens 回取时单次内联上限（默认 4000）：超过就导出成文件给指针，
	// 避免"取回"把上下文又撑爆——这是 CCR 的边界，不是越取越好。
	InlineLimitTokens int `json:"inlineLimitTokens"`
	// ToolName 模型可见的回取工具名（默认 acp_retrieve）。
	ToolName string `json:"toolName"`
	// MaxEntries 每会话最多保留条目数（默认 200，超出后最旧的先淘汰）。
	MaxEntries int `json:"maxEntries"`
}

// CCRKind 条目种类。
type CCRKind string

const (
	// CCRKindText 文本（工具输出、日志、代码）。
	CCRKindText CCRKind = "text"
	// CCRKindImage 图像：字节不搬，只记 ref（provider 负责解析）。
	CCRKindImage CCRKind = "image"
)

// CCREntry 一条已卸载内容。字段顺序即序列化顺序，便于人工比对。
type CCREntry struct {
	Ref string `json:"ref"` // 短 ref（c00001），模型在 acp_retrieve 里引用它
	// Shasum 内容的 SHA-256（十六进制）——内容寻址，相同字节只存一份。
	Shasum string  `json:"sha"`
	Kind   CCRKind `json:"kind"`
	// Tool 产出该结果的工具名（占位符里展示，帮助模型判断相关性）。
	Tool string `json:"tool,omitempty"`
	// Preview 占位符里的单行预览（命令/首行），已截断到 PreviewMaxRunes。
	Preview string `json:"preview,omitempty"`
	// Tokens / Bytes 原文规模（占位符展示用；解码后不再重新计算）。
	Tokens int `json:"tokens"`
	Bytes  int `json:"bytes"`
	// Images 图像条目的 ref 列表（text 条目为空）。
	Images []string `json:"images,omitempty"`
	// Exported 已导出到工作区文件时的相对路径（回取超限时产生）。
	Exported string `json:"exported,omitempty"`
}

// PreviewMaxRunes 占位符预览的最大字符数。
const PreviewMaxRunes = 80

// ccrPlaceholderOpen/Close 占位符标记。两者成对出现，用于识别"这条已经是占位符"。
const (
	ccrPlaceholderOpen  = "📦 [acp-stored "
	ccrPlaceholderClose = "\n   → acp_retrieve("
)

// CCRRefFor 生成短 ref。编号来自 state.NextCCRID，单调递增、永不复用
// （复用会让「取回的其实是别的内容」这种幻觉无从分辨）。
func CCRRefFor(n int) string {
	return fmt.Sprintf("c%05d", n)
}

// CCRKindOf 判断一条工具结果该按什么种类卸载：
//   - 带图像 ref 且正文很短 → image（正文没什么可取的，值钱的是图）
//   - 其余 → text
func CCRKindOf(msg CoreMessage) CCRKind {
	if len(msg.Images) > 0 {
		return CCRKindImage
	}
	return CCRKindText
}

// ShouldOffload 判断一条消息是否该由 CCR 卸载。
//
// 门控（顺序即优先级，任何一步不满足都不动）：
//  1. 开关打开
//  2. 是工具结果、且不是 ACP 自己的工具（compress/absorb/acp_retrieve 的结果
//     本来就不该被再卸载一层）
//  3. 不是已经卸载过的占位符（幂等）
//  4. 规模达 MinToolTokens
func ShouldOffload(msg CoreMessage, config Config) bool {
	cc := config.CCR
	if !cc.Enabled {
		return false
	}
	if msg.ContentType != ContentTypeToolResult {
		return false
	}
	if isACPManagedTool(msg.ToolName, config.Absorb) {
		return false
	}
	if IsCCRPlaceholder(msg.Text) {
		return false
	}
	minTokens := cc.MinToolTokens
	if minTokens <= 0 {
		minTokens = 2000
	}
	if estimateTokensForText(msg.Text) < minTokens {
		return false
	}
	return true
}

// IsCCRPlaceholder 报告文本是否已是 CCR 占位符。
func IsCCRPlaceholder(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, strings.TrimSpace(ccrPlaceholderOpen)) &&
		strings.Contains(t, strings.TrimSpace(ccrPlaceholderClose))
}

// BuildCCRPlaceholder 生成占位符。
//
// **必须是确定性的**：同样的条目字段必然产出同样的字节。到达时替换一次，此后
// 每轮看到的都是这串字节，前缀缓存才不会失效（随机数、时间戳、行号都会打破它）。
func BuildCCRPlaceholder(entry CCREntry, toolName string) string {
	if toolName == "" {
		toolName = "acp_retrieve"
	}
	var b strings.Builder
	b.WriteString(ccrPlaceholderOpen)
	b.WriteString(entry.Ref)
	b.WriteString(" · ")
	if entry.Kind == CCRKindImage {
		b.WriteString(fmt.Sprintf("%d 张图", len(entry.Images)))
	} else {
		b.WriteString(entry.Kind.String())
	}
	if entry.Tool != "" {
		b.WriteString(" · ")
		b.WriteString(entry.Tool)
	}
	b.WriteString(fmt.Sprintf(" · %d tok", entry.Tokens))
	b.WriteString("]")
	if p := strings.TrimSpace(entry.Preview); p != "" {
		b.WriteString(" `")
		b.WriteString(truncateRunes(p, PreviewMaxRunes))
		b.WriteString("`")
	}
	b.WriteString(ccrPlaceholderClose)
	b.WriteString(toolName)
	b.WriteString("\"")
	b.WriteString(entry.Ref)
	b.WriteString("\")")
	if entry.Kind == CCRKindImage {
		b.WriteString(" 把图像重新挂回结果")
	} else {
		b.WriteString(" 取回完整内容")
	}
	return b.String()
}

// String 返回可读的种类名。
func (k CCRKind) String() string {
	switch k {
	case CCRKindImage:
		return "image"
	default:
		return "text"
	}
}

// BuildCCRPreview 从工具结果正文里取一行预览（第一条非空行的前若干字符）。
// 只取正文、不加时间戳——保持确定性。
func BuildCCRPreview(text string, toolArgs string) string {
	// 优先用命令参数：比输出第一行更能说明"这是什么"
	if args := strings.TrimSpace(toolArgs); args != "" {
		return truncateRunes(args, PreviewMaxRunes)
	}
	for _, line := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return truncateRunes(t, PreviewMaxRunes)
		}
	}
	return ""
}

// sha256Hex 内容 hash（十六进制小写）。与附件库的 sha 命名一致。
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// truncateRunes 按字符（而非字节）截断，避免把多字节字符切坏。
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// CCRRetrieval 一次回取的形态。
type CCRRetrieval struct {
	// Text 内联正文（Text 条目且未超限时填写）
	Text string
	// Images 重新挂回工具结果的图像 ref
	Images []string
	// ExportPath 需要导出为文件时的相对路径（插件据此写盘并给指针）
	ExportPath string
	// Inline 是否走内联
	Inline bool
}

// PlanRetrieval 决定一次回取怎么交付（纯函数，不写盘）。
//
// 规则：
//   - image 条目 → 直接把 ref 挂回（provider 解析字节，不占文本预算）
//   - text 且 tokens ≤ InlineLimit → 内联
//   - text 超限 → 导出（ExportPath 由调用方按工作区拼），返回指针文案
//
// 为什么要有上限：回取不设上限就等于把刚刚省下的上下文又塞回去，而且可能一次
// 塞爆窗口——CCR 的价值是"需要时才取、只取够用的一段"。
func PlanRetrieval(entry CCREntry, config Config, fullText string) CCRRetrieval {
	if entry.Kind == CCRKindImage {
		r := CCRRetrieval{Images: entry.Images}
		// 图像条目可能同时带一段说明文字：能内联就一并给出，
		// 免得模型为了看这句话又要多取一次。
		if limit := config.CCR.InlineLimitTokens; fullText != "" &&
			(limit <= 0 || estimateTokensForText(fullText) <= limit) {
			r.Text = fullText
			r.Inline = true
		}
		return r
	}
	limit := config.CCR.InlineLimitTokens
	if limit <= 0 {
		limit = 4000
	}
	if estimateTokensForText(fullText) <= limit {
		return CCRRetrieval{Text: fullText, Inline: true}
	}
	return CCRRetrieval{ExportPath: entry.Shasum + ".txt"}
}

// CCRPointerText 生成"超限已导出"的模型可见文案。
func CCRPointerText(entry CCREntry, exportPath string, inlineTokens int) string {
	return fmt.Sprintf(
		"[acp-retrieved %s 已导出到工作区文件: %s（%d tok，超过单次内联上限 %d）]\n"+
			"请用你的文件读取工具分页读取该文件（offset/limit），避免一次性塞满上下文。",
		entry.Ref, exportPath, entry.Tokens, inlineTokens)
}

// TrimCCREntries 把条目数裁到上限：**按创建顺序淘汰最旧的**，但保留即将被引用
// 的那条（retainRef）。ref 永不复用，所以淘汰不会造成"取到别的内容"。
func TrimCCREntries(entries []CCREntry, max int, retainRef string) []CCREntry {
	if max <= 0 {
		max = 200
	}
	if len(entries) <= max {
		return entries
	}
	drop := len(entries) - max
	out := make([]CCREntry, 0, max)
	for _, e := range entries {
		if drop > 0 && e.Ref != retainRef {
			drop--
			continue
		}
		out = append(out, e)
	}
	return out
}

// FindCCREntry 按 ref 查条目（大小写不敏感：模型偶尔会写出 C00001）。
func FindCCREntry(entries []CCREntry, ref string) (CCREntry, bool) {
	want := strings.ToLower(strings.TrimSpace(ref))
	for _, e := range entries {
		if strings.ToLower(e.Ref) == want {
			return e, true
		}
	}
	return CCREntry{}, false
}

// CCREffects 由 ProcessTurn 产出、交由插件侧执行的副作用。
//
// 内核的约定是"kernel 不做 IO，只产出 host effects"；DSC 照此办理：
// acp 只算出「这条消息该卸载、ref 是 c00007、字节是这些」，真正写附件库、
// 写状态文件的是 main.go。
type CCREffects struct {
	// Offloads 需要落盘并替换为占位符的消息
	Offloads []CCROffload
}

// CCROffload 一条待卸载内容。
type CCROffload struct {
	MessageID string
	Entry     CCREntry
	// Body 文本条目的原始字节（image 条目为空：字节已在附件库里）
	Body string
}

// ApplyCCR 执行「到达即卸载」：为每条够大的工具结果建索引、把正文换成确定性
// 占位符，并返回需要宿主落盘的副作用。
//
// 幂等：已是占位符的消息不再处理（ShouldOffload 已挡）。相同内容重复到达时，
// SHA 相同 → 复用同一条目（内容寻址去重），但仍会分配新的 ref 吗？——不会：
// 复用已有条目的 ref，避免同一份内容在索引里堆多条。
func ApplyCCR(messages []CoreMessage, state *CompressionState, config Config) ([]CoreMessage, []CCROffload) {
	if !config.CCR.Enabled || len(messages) == 0 {
		return messages, nil
	}
	bySHA := make(map[string]CCREntry, len(state.CCR))
	for _, e := range state.CCR {
		bySHA[e.Shasum] = e
	}
	out := make([]CoreMessage, len(messages))
	offloads := make([]CCROffload, 0, 4)
	for i, msg := range messages {
		out[i] = msg
		if !ShouldOffload(msg, config) {
			continue
		}
		sha := sha256Hex(msg.Text)
		entry, exists := bySHA[sha]
		if !exists {
			state.NextCCRID++
			entry = CCREntry{
				Ref:     CCRRefFor(state.NextCCRID),
				Shasum:  sha,
				Kind:    CCRKindOf(msg),
				Tool:    msg.ToolName,
				Preview: BuildCCRPreview(msg.Text, ""),
				Tokens:  estimateTokensForText(msg.Text),
				Bytes:   len(msg.Text),
				Images:  append([]string(nil), msg.Images...),
			}
			state.CCR = append(state.CCR, entry)
			bySHA[sha] = entry
			// 图像条目的正文同样要存：占位符会把正文替换掉，不存就等于丢弃
			// （截图工具往往既有图又有一段说明文字）。
			offloads = append(offloads, CCROffload{
				MessageID: msg.ID,
				Entry:     entry,
				Body:      msg.Text,
			})
		}
		out[i].Text = BuildCCRPlaceholder(entry, config.CCR.ToolName)
	}
	if len(state.CCR) > 0 {
		state.CCR = TrimCCREntries(state.CCR, config.CCR.MaxEntries, "")
	}
	return out, offloads
}

// CCRToolSchema 回取工具的 JSON Schema（模型侧唯一入口）。
func CCRToolSchema() string {
	return `{
  "type": "object",
  "properties": {
    "ref": {"type": "string", "description": "占位符里的 ref（如 c00003）"}
  },
  "required": ["ref"]
}`
}

// CCRRetrieveNotFound 幻觉 ref 的回复：一次工具调用即可纠正，不是错误。
func CCRRetrieveNotFound(ref string) string {
	return "没有这个回取条目：" + ref + "。只能取占位符里出现过的 ref（形如 c00003）；" +
		"如果你需要的内容已被压缩摘要覆盖，用 search_context 找块，或 decompress 还原。"
}
