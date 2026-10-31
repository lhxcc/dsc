package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"sync"

	"dsc-sdk"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

type AnthropicProvider struct {
	client anthropic.Client
	model  string
	// thinking 是否启用扩展思考（extended thinking）。默认开启（DeepSeek anthropic 接口
	// 支持并返回 thinking 块）；ANTHROPIC_THINKING=0 可关闭。
	thinking       bool
	thinkingBudget int64
	// reasoningEffort 统一推理强度档位（DSC_REASONING_EFFORT 的原文 none/minimal/
	// low/medium/high/max）。用于构造 output_config.effort：DeepSeek 的 /anthropic 口
	// 忽略 thinking.budget_tokens，改以 output_config.effort 控制思考强度；真实 Anthropic
	// 则吃 budget_tokens。两者同时下发互不冲突。
	reasoningEffort string
	// maxTokens 单轮输出上限（插件级默认）。取值来源：显式 DSC_ANTHROPIC_MAX_OUTPUT_TOKENS
	//（旧名 ANTHROPIC_MAX_OUTPUT_TOKENS 回退）> 通用 DSC_MAX_OUTPUT_TOKENS；三态：>0 携带、
	// -1/0/不写=不携带（**宿主不再按 context_window 兜底注入**：这只把请求顶到
	// 「prompt + max_tokens > n_ctx」的边界，云端还会先吃一次 400）。
	// 请求级参数（压缩等场景的窗口净余值）经 resolveMaxTokens 优先于本值。零值由 omitZeroMaxTokens 中间件从
	// 请求体摘除（SDK 无 omitempty，不摘则会上送 "max_tokens":0 被服务端拒绝）；
	// 负值因 `>0 才携带` 的判断天然被忽略。
	maxTokens int64
	// vision 是否启用图像输入：默认按模型能力自动判断，DSC_NO_VISION=1 强制关闭。
	vision bool
	// filesAPI 是否可把超大图自动上传 DeepSeek Files API（base URL 为 deepseek.com 时启用）。
	filesAPI bool
	// fileCache 已上传图片的 data URL → file_id 缓存（同一图多轮复用，避免重复上传）。
	fileMu    sync.Mutex
	fileCache map[string]string
	// apiKey/baseURL 供 Files API 上传与消息请求使用（DeepSeek anthropic 端点）。
	apiKey  string
	baseURL string
}

// maxInlineImageBytes 内联 base64 单图大小上限（对齐 DeepSeek 32 MiB 内联限制，
// 预留余量避免请求体逼近 48 MiB 上限；超出则走 Files API 上传）。包级变量以便
// Files API 阈值测试注入小阈值。
var maxInlineImageBytes = 20 << 20 // 20 MiB

// omitZeroMaxTokens 从请求体 JSON 中摘除值为 0 的 max_tokens 字段。
// 本仓 anthropic SDK 的 MessageNewParams.MaxTokens 无 omitempty（协议标 required），
// 零值会以 "max_tokens":0 上送——轻则参数错误，重则被理解为零输出。而「不人为
// 限制输出」的语义必须是不携带该字段、等模型自然结束（对齐 llm-openai 行为：
// 仅显式 >0 才携带）。故以中间件在 HTTP 层摘除零值，显式配置不受影响。
func omitZeroMaxTokens(req *http.Request, next option.MiddlewareNext) (*http.Response, error) {
	if req.Body == nil {
		return next(req)
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("read request body: %w", err)
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) == nil {
		if raw, ok := payload["max_tokens"]; ok && string(bytes.TrimSpace(raw)) == "0" {
			delete(payload, "max_tokens")
			if rebuilt, merr := json.Marshal(payload); merr == nil {
				body = rebuilt
			}
		}
	}
	newReq := req.Clone(req.Context())
	newReq.Body = io.NopCloser(bytes.NewReader(body))
	newReq.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	newReq.ContentLength = int64(len(body))
	return next(newReq)
}

// buildMessageParams 构建 Anthropic 请求参数（消息、system、工具定义）。
// maxTokens <= 0 表示不携带该字段（omitZeroMaxTokens 摘除零值，等模型自然结束）；
// >0 时才显式携带。第二个返回值标记本请求是否引用了 Files API 的 file_id（须带 anthropic-beta 头）。
func (p *AnthropicProvider) buildMessageParams(messages []dsc.Message, tools []dsc.LLMTool, maxTokens int64) (anthropic.MessageNewParams, bool) {
	// 1. 构建 Anthropic 消息
	var systemMsg string
	userMessages := make([]anthropic.MessageParam, 0, len(messages))
	usesBetaHeader := false

	for _, m := range messages {
		switch m.Role {
		case "system":
			if systemMsg == "" {
				systemMsg = m.Content
				continue
			}
			// 后续 system 消息（插件的一次性插播，如「任务已完成，请写记忆」）：
			// Anthropic 只接受顶层 system，消息列表里没有 system 角色。降级为
			// user 轮并显式标注，保留「这不是用户发言」的语义；并入上一条 user
			// 消息以避免同角色相邻（Anthropic 要求 user/assistant 交替）。
			note := "[system] " + m.Content
			if n := len(userMessages); n > 0 {
				last := &userMessages[n-1]
				if last.Role == anthropic.MessageParamRoleUser {
					last.Content = append(last.Content, anthropic.NewTextBlock(note))
					continue
				}
			}
			userMessages = append(userMessages, anthropic.NewUserMessage(anthropic.NewTextBlock(note)))
		case "user":
			if len(m.Images) > 0 {
				// 多模态分支：文本块 + 文件附件块（图像受 p.vision 门控；
				// dsc-txt 文本引用不受视觉限制，始终注入）
				blocks, usesFile := p.fileContentBlocks(m.Content, m.Images)
				if usesFile {
					usesBetaHeader = true
				}
				userMessages = append(userMessages, anthropic.NewUserMessage(blocks...))
			} else {
				userMessages = append(userMessages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
			}
		case "assistant":
			// Anthropic 要求：若后续跟随 tool_result（tool 消息），本 assistant 消息必须
			// 回带对应的 tool_use 块，否则 API 无法把工具结果关联到之前的调用
			//（REX 同样回显 tool_use）。无文本且无工具调用时跳过，避免空内容消息。
			var blocks []anthropic.ContentBlockParamUnion
			if m.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(m.Content))
			}
			for _, tc := range m.ToolCalls {
				// ArgumentsJSON() 保真回放：解析失败的调用带原文，重新 marshal 会写成 "null"
				input := []byte(tc.ArgumentsJSON())
				blocks = append(blocks, anthropic.NewToolUseBlock(tc.ID, json.RawMessage(input), tc.Name))
			}
			if len(blocks) > 0 {
				userMessages = append(userMessages, anthropic.NewAssistantMessage(blocks...))
			}
		case "tool":
			// Anthropic 要求 tool 结果以 user 消息发送，并附上对应的 tool_use_id
			// 如果 m.ToolCallID 为空，降级为纯文本（兼容旧逻辑）
			if m.ToolCallID != "" && len(m.Images) > 0 {
				// 工具结果携带图像（如 computer-use 截图）：按 Anthropic 规范把图像块
				// 内嵌 tool_result.content（text/image 混合块），与文本结果同帧送达。
				// 图像受 p.vision 门控；超内联上限的大图沿用 Files API 上传路径。
				resultBlocks, usesFile := p.toolResultContentBlocks(m.Content, m.Images)
				if usesFile {
					usesBetaHeader = true
				}
				userMessages = append(userMessages, anthropic.NewUserMessage(
					anthropic.ContentBlockParamUnion{
						OfToolResult: &anthropic.ToolResultBlockParam{
							ToolUseID: m.ToolCallID,
							Content:   resultBlocks,
							IsError:   anthropic.Bool(false),
						},
					},
				))
			} else if m.ToolCallID != "" {
				userMessages = append(userMessages, anthropic.NewUserMessage(
					anthropic.NewToolResultBlock(m.ToolCallID, m.Content, false),
				))
			} else {
				userMessages = append(userMessages, anthropic.NewUserMessage(
					anthropic.NewTextBlock(fmt.Sprintf("Tool result: %s", m.Content)),
				))
			}
		default:
			userMessages = append(userMessages, anthropic.NewUserMessage(anthropic.NewTextBlock(m.Content)))
		}
	}

	// 3. 修复 tool_use / tool_result 配对（并行调用的多条结果合并、缺失结果补齐、
	// 孤儿结果剔除）。不修则 Anthropic 兼容端点会以 400 拒绝：
	// "messages.N: tool_use ids were found without tool_result blocks immediately after"。
	userMessages = repairToolPairing(userMessages)

	// 4. 转换工具定义
	var toolParams []anthropic.ToolUnionParam
	if len(tools) > 0 {
		for _, t := range tools {
			// 直接反序列化 JSON Schema 為 ToolInputSchemaParam，
			// 避免把完整 schema 錯誤地包進 properties 導致服務端解析失敗
			var inputSchema anthropic.ToolInputSchemaParam
			if err := json.Unmarshal([]byte(t.ParametersJSON), &inputSchema); err != nil {
				// 若 schema 解析失败，使用空对象（type 默認為 object）
				inputSchema = anthropic.ToolInputSchemaParam{}
			}
			toolParams = append(toolParams, anthropic.ToolUnionParam{
				OfTool: &anthropic.ToolParam{
					Name:        t.Name,
					InputSchema: inputSchema,
					Description: anthropic.String(t.Description),
				},
			})
		}
	}

	// 5. 构建请求参数
	msgParams := anthropic.MessageNewParams{
		Model:    anthropic.Model(p.model),
		Messages: userMessages,
	}
	// max_tokens：仅当显式给定 >0 时设置；零值由 omitZeroMaxTokens 从请求体
	// 摘除——不携带即等模型自然结束，不再有任何人写死的人为截断
	if maxTokens > 0 {
		msgParams.MaxTokens = maxTokens
	}
	if systemMsg != "" {
		msgParams.System = []anthropic.TextBlockParam{
			{Text: systemMsg, Type: "text"},
		}
	}
	if len(toolParams) > 0 {
		msgParams.Tools = toolParams
	}
	// 6. 扩展思考（extended thinking）：启用后模型会返回 thinking 块，
	//    其内容经 chat 流式增量转发为 reasoning 帧最终渲染到 TUI。
	if p.thinking {
		msgParams.Thinking = anthropic.ThinkingConfigParamOfEnabled(p.thinkingBudget)
		// output_config.effort：DeepSeek 的 /anthropic 口忽略 thinking.budget_tokens，
		// 改以 effort 控制思考强度；真实 Anthropic 则吃 budget_tokens。两者同时下发互不
		// 冲突。none 已在上游关闭 thinking，此处不再下发 effort；minimal 在 Anthropic/DeepSeek
		// effort 枚举中无对应值，按最接近降级为 low。
		if eff := anthropicEffortForLevel(p.reasoningEffort); eff != "" {
			msgParams.OutputConfig = anthropic.OutputConfigParam{Effort: eff}
		}
	}
	return msgParams, usesBetaHeader
}

// visionEnabled 是否启用图像输入：默认按模型能力自动判断（服务端在 /models
// 上报 input_modalities 含 image 时启用；未上报则默认放行，对齐 DSH）。
// DSC_NO_VISION=1 可显式强制关闭（自动判断失灵时的逃生口）。
// env 解析仍在本函数内（测试可直接调）；生产路径用 dsc.LoadLLMConfig 拿
// cfg.VisionEnabled 后传 envNoVisionOverride=true 跳过重复读 env。
func visionEnabled(baseURL, model string, envNoVisionOverride bool) bool {
	if envNoVisionOverride {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSC_NO_VISION"))) {
	case "1", "true", "on", "yes":
		return false
	}
	return dsc.ModelSupportsImages(baseURL, model)
}

// isDeepSeekEndpoint 按 base URL 是否指向 DeepSeek 判定 Files API 可用性
// （llamacpp 等本地 server 无 Files API，始终内联）。
func isDeepSeekEndpoint(baseURL string) bool {
	return strings.Contains(baseURL, "deepseek.com")
}

// anthropicEffortForLevel 将 DSC 统一推理强度档位映射为 anthropic 路径
// output_config.effort 取值（low/medium/high/max）。
//   - none：思考已在上游关闭，返回空（不携带 effort）。
//   - minimal：Anthropic/DeepSeek 的 effort 枚举无 minimal，按最接近降级为 low。
//   - low/medium/high/max：1:1 透传（与 OpenAI 路径的 reasoning_effort 字面量一致）。
//
// 真实 Anthropic 端点同时仍由 thinking.budget_tokens 控制预算；DeepSeek 忽略
// budget_tokens、只认 effort，于是同一请求对两类端点都生效。
func anthropicEffortForLevel(level string) anthropic.OutputConfigEffort {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "low", "minimal":
		return anthropic.OutputConfigEffortLow
	case "medium":
		return anthropic.OutputConfigEffortMedium
	case "high":
		return anthropic.OutputConfigEffortHigh
	case "max":
		return anthropic.OutputConfigEffortMax
	default:
		return "" // none 或空
	}
}

// fileContentBlocks 构造用户消息的多模态内容块：文本块 + 文件附件块。
// 文本引用（dsc-txt://）读取内容作为纯文本块注入，不受视觉限制；图像引用
// （dsc-img:// 持久附件 / dsc-shot:// 操作截图）按路由策略投影（超限缩放重编码）
// 为 base64 data URL，单图解码后不超过内联上限时用 base64 源、超限且 DeepSeek
// Files API 可用时自动上传并以 file 源引用 file_id（请求需带 anthropic-beta 头）；
// 图像仅当视觉开启（p.vision）。返回内容块与是否使用了 file 源。
func (p *AnthropicProvider) fileContentBlocks(text string, refs []string) ([]anthropic.ContentBlockParamUnion, bool) {
	blocks := make([]anthropic.ContentBlockParamUnion, 0, len(refs)+1)
	usesFile := false
	if text != "" {
		blocks = append(blocks, anthropic.NewTextBlock(text))
	}
	for _, ref := range refs {
		if strings.HasPrefix(ref, dsc.TextRefPrefix) {
			content, err := dsc.ResolveTextRef(ref)
			if err != nil {
				log.Printf("⚠️ 忽略无法解析的文本引用: %v", err)
				continue
			}
			blocks = append(blocks, anthropic.NewTextBlock(content))
			continue
		}
		if !p.vision {
			continue // 视觉关闭：跳过图像引用
		}
		url, err := dsc.ProjectImageRef(ref, dsc.DefaultProjectionMaxSide)
		if err != nil {
			// 引用失效（截图过期/附件缺失）：占位文本留痕而非静默丢弃——
			// 模型须知道该处曾有图（对齐 DSH offloadedImageText 语义）
			log.Printf("⚠️ 图像引用投影失败: %v", err)
			blocks = append(blocks, anthropic.NewTextBlock(imageUnavailableText(ref)))
			continue
		}
		mime, b64, ok := splitDataURL(url)
		if !ok {
			continue
		}
		if p.filesAPI && dataURLSize(url) > maxInlineImageBytes {
			if fileID := p.uploadImage(url); fileID != "" {
				usesFile = true
				blocks = append(blocks, anthropic.NewImageBlock(anthropic.FileImageSourceParam{
					FileID:    fileID,
					MediaType: anthropic.Base64ImageSourceMediaType(mime),
				}))
				continue
			}
		}
		blocks = append(blocks, anthropic.NewImageBlockBase64(mime, b64))
	}
	return blocks, usesFile
}

// toolResultContentBlocks 构造 tool_result 内容块：文本块 + 图像块（vision 门控）。
// 图像引用按路由策略投影（超限缩放重编码）为 data URL，单图不超内联上限时内联
// base64；超限且 Files API 可用时上传后以 file 块引用（与用户消息图像同一套判定），
// usesFile 标记请求须带 anthropic-beta 头；引用失效降级为占位文本块。
func (p *AnthropicProvider) toolResultContentBlocks(text string, refs []string) ([]anthropic.ToolResultBlockParamContentUnion, bool) {
	blocks := make([]anthropic.ToolResultBlockParamContentUnion, 0, len(refs)+1)
	usesFile := false
	if text != "" {
		blocks = append(blocks, anthropic.ToolResultBlockParamContentUnion{OfText: &anthropic.TextBlockParam{Text: text}})
	}
	for _, ref := range refs {
		if !p.vision {
			continue // 视觉关闭：跳过图像引用
		}
		url, err := dsc.ProjectImageRef(ref, dsc.DefaultProjectionMaxSide)
		if err != nil {
			log.Printf("⚠️ 图像引用投影失败: %v", err)
			blocks = append(blocks, anthropic.ToolResultBlockParamContentUnion{
				OfText: &anthropic.TextBlockParam{Text: imageUnavailableText(ref)},
			})
			continue
		}
		mime, b64, ok := splitDataURL(url)
		if !ok {
			continue
		}
		if p.filesAPI && dataURLSize(url) > maxInlineImageBytes {
			if fileID := p.uploadImage(url); fileID != "" {
				usesFile = true
				blocks = append(blocks, anthropic.ToolResultBlockParamContentUnion{
					OfImage: &anthropic.ImageBlockParam{
						Source: anthropic.ImageBlockParamSourceUnion{
							OfFile: &anthropic.FileImageSourceParam{
								FileID:    fileID,
								MediaType: anthropic.Base64ImageSourceMediaType(mime),
							},
						},
					},
				})
				continue
			}
		}
		blocks = append(blocks, anthropic.ToolResultBlockParamContentUnion{
			OfImage: &anthropic.ImageBlockParam{
				Source: anthropic.ImageBlockParamSourceUnion{
					OfBase64: &anthropic.Base64ImageSourceParam{
						Data:      b64,
						MediaType: anthropic.Base64ImageSourceMediaType(mime),
					},
				},
			},
		})
	}
	return blocks, usesFile
}

// imageUnavailableText 图像引用失效（截图过期/附件缺失）时的模型可见占位文本：
// 身份留痕——引用写进占位，模型可据此重新截图或请用户重新提供（对齐 DSH
// offloadedImageText/textOnlyImageText 的稳定占位语义）。
func imageUnavailableText(ref string) string {
	return "[image unavailable: attachment expired or missing; " + ref + "]"
}

// splitDataURL 解析 data:image/<mime>;base64,<b64> 为 (mime, base64 负载)。
func splitDataURL(url string) (string, string, bool) {
	i := strings.Index(url, ";base64,")
	if !strings.HasPrefix(url, "data:") || i < 0 {
		return "", "", false
	}
	return url[len("data:"):i], url[i+len(";base64,"):], true
}

// dataURLSize 返回 data URL 解码后的近似字节数（按 base64 长度估算；非 data URL 返回 0）。
func dataURLSize(url string) int {
	_, b64, ok := splitDataURL(url)
	if !ok {
		return 0
	}
	return len(b64) / 4 * 3
}

// mimeToExt 由图片 MIME 推导文件扩展名（Files API 上传文件名用）。
func mimeToExt(mime string) string {
	switch strings.ToLower(mime) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	}
	return ".img"
}

// uploadImage 把超大图上传到 DeepSeek anthropic 兼容 Files API（purpose=user_data）
// 并返回 file_id。同一 data URL 只上传一次（进程内缓存，多轮复用）；失败返回空串。
func (p *AnthropicProvider) uploadImage(dataURL string) string {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	if id, ok := p.fileCache[dataURL]; ok {
		return id
	}
	mime, b64, ok := splitDataURL(dataURL)
	if !ok {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return ""
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("purpose", "user_data")
	part, err := writer.CreateFormFile("file", "image"+mimeToExt(mime))
	if err != nil {
		return ""
	}
	if _, err := part.Write(raw); err != nil {
		return ""
	}
	if err := writer.Close(); err != nil {
		return ""
	}

	endpoint := strings.TrimRight(p.baseURL, "/") + "/v1/files"
	req, err := http.NewRequest(http.MethodPost, endpoint, &body)
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("x-api-key", p.apiKey)
	req.Header.Set("Authorization", "Bearer "+p.apiKey)
	req.Header.Set("anthropic-beta", "files-api-2025-04-14")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("⚠️ 图片上传 Files API 失败: %v", err)
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		log.Printf("⚠️ 图片上传 Files API 失败（HTTP %d）: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
		return ""
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.ID == "" {
		return ""
	}
	p.fileCache[dataURL] = out.ID
	return out.ID
}

// concatText 拼接消息中所有 text 块的内容
func concatText(blocks []anthropic.ContentBlockUnion) string {
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// concatReasoning 拼接消息中所有 thinking 块的内容（思考过程）
func concatReasoning(blocks []anthropic.ContentBlockUnion) string {
	var b strings.Builder
	for _, block := range blocks {
		// 嘗試作為 thinking 塊訪問
		if thinkingBlock := block.AsThinking(); thinkingBlock.Thinking != "" {
			b.WriteString(thinkingBlock.Thinking)
		}
	}
	return b.String()
}

// extractToolCalls 从消息内容块中提取工具调用
func extractToolCalls(blocks []anthropic.ContentBlockUnion) []dsc.ToolCall {
	var toolCalls []dsc.ToolCall
	for _, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		toolUse := block.AsToolUse()
		// 解析失败时保留原文（RawArguments）：toolUse.Input 是模型交付的原始 JSON 片段，
		// 也是模型唯一能据以自纠的东西——换成空 map 后历史里只剩「调过工具、参数空」。
		argsMap, raw, _ := dsc.DecodeToolArguments(toolUse.Name, string(toolUse.Input))
		toolCalls = append(toolCalls, dsc.ToolCall{
			ID:           toolUse.ID,
			Name:         toolUse.Name,
			Arguments:    argsMap,
			RawArguments: raw,
		})
	}
	return toolCalls
}

// usageFromAnthropic 将 Anthropic usage 转换为 dsc.Usage（nil 返回 nil）。
// 口径契约（见 core.PromptSideTokens）：Usage.PromptTokens 报**完整** prompt（含缓存）。
//
// Anthropic 的 input_tokens 不含缓存部分：命中前缀在 cache_read_input_tokens、本次新写入
// 缓存的在 cache_creation_input_tokens，故完整 prompt = 三者相加。启用提示缓存的
// llama.cpp 等兼容端点的 input_tokens 也只统计未命中部分，但（旧版实现）会把
// cache_read 报得比 input 还大——此时按「input 已含 cache_read」的旧语义兜底，保持
// 原值，避免把同一段前缀算两遍。
// CacheCreation 保持 Anthropic 的原义（写入缓存的 token），不再冒充「未命中」：
// 命中率由 prompt - cacheRead 推出，见 TUI trackTurnUsage。
func usageFromAnthropic(u *anthropic.Usage) *dsc.Usage {
	if u == nil {
		return nil
	}
	// Anthropic 官方语义：input_tokens 只算「未被缓存服务、也没用于建缓存」的部分，
	// 命中与写入分别落在 cache_read / cache_creation，故完整 prompt = 三者之和。
	// llama.cpp 等 Anthropic 兼容端口的 input_tokens 同样只报新增部分（实测 input=4、
	// cache_read=173、真实上下文 177），加法一致。
	promptTokens := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	if promptTokens <= 0 && u.OutputTokens <= 0 {
		return nil
	}
	return &dsc.Usage{
		PromptTokens:             int32(promptTokens),
		CompletionTokens:         int32(u.OutputTokens),
		TotalTokens:              int32(promptTokens + u.OutputTokens),
		CacheReadInputTokens:     int32(u.CacheReadInputTokens),
		CacheCreationInputTokens: int32(u.CacheCreationInputTokens),
	}
}

// maxTokensReserve 是给「我们的估算 vs 服务端分词器」留的余量；maxTokensFloor 是
// 「还值得给」的下限（比它小说明这一帧该由宿主滑窗收口）。
const (
	maxTokensReserve = 512
	maxTokensFloor   = 128
)

// windowHeadroom 返回这一帧还能生成多少 token（窗口 − 消息 − 工具目录 − 余量）。
// 窗口未知时返回 0（不做限制）。
func windowHeadroom(messages []dsc.Message, tools []dsc.LLMTool) int {
	window := dsc.ContextWindow()
	if window <= 0 {
		return 0
	}
	room := window - dsc.EstimateMessageValuesTokens(messages) - dsc.EstimateToolTokens(tools) - maxTokensReserve
	if room < maxTokensFloor {
		return 0
	}
	return room
}

// resolveMaxTokens 计算本请求实际携带的 max_tokens。
//
// anthropic 协议把 max_tokens 当 **required**，缺席时服务端会自填保守默认（llama.cpp
// 兼容口填 4096），一轮长回答直接腰斩。所以这里三态：
//   - 请求级参数（压缩等场景的净余值）> 0：用它；
//   - 插件 env 显式给出（含宿主按启动期探测结果注入的值）：用它；
//   - 都没有：按「窗口 − 本帧输入」现算；
//
// 最后统一用余量夹一次，保证 prompt + max_tokens ≤ 窗口；显式 -1 表示不携带（尊重）。
func (p *AnthropicProvider) resolveMaxTokens(requestMaxTokens int64, messages []dsc.Message, tools []dsc.LLMTool) int64 {
	mt := requestMaxTokens
	if mt <= 0 {
		mt = p.maxTokens
	}
	if mt < 0 {
		return 0 // 明确不携带
	}
	headroom := windowHeadroom(messages, tools)
	if mt == 0 {
		mt = int64(headroom)
		if mt <= 0 {
			return 0
		}
	}
	if headroom > 0 && mt > int64(headroom) {
		mt = int64(headroom)
	}
	return mt
}

func (p *AnthropicProvider) Chat(ctx context.Context, messages []dsc.Message, tools []dsc.LLMTool, maxTokens int) (*dsc.ChatResponse, error) {
	params, beta := p.buildMessageParams(messages, tools, p.resolveMaxTokens(int64(maxTokens), messages, tools))
	opts := p.requestOptions(beta)
	resp, err := p.client.Messages.New(ctx, params, opts...)
	if err != nil {
		return nil, err
	}

	return &dsc.ChatResponse{
		Content:      concatText(resp.Content),
		FinishReason: string(resp.StopReason),
		ToolCalls:    extractToolCalls(resp.Content),
	}, nil
}

// requestOptions 返回本次消息请求的额外选项：引用 Files API file_id 时附加
// anthropic-beta 头（DeepSeek 要求）。
func (p *AnthropicProvider) requestOptions(beta bool) []option.RequestOption {
	if !beta {
		return nil
	}
	return []option.RequestOption{
		option.WithHeader("anthropic-beta", "files-api-2025-04-14"),
	}
}

func (p *AnthropicProvider) ChatStream(ctx context.Context, messages []dsc.Message, tools []dsc.LLMTool) (<-chan *dsc.ChatStreamResponse, error) {
	params, beta := p.buildMessageParams(messages, tools, p.resolveMaxTokens(0, messages, tools))
	stream := p.client.Messages.NewStreaming(ctx, params, p.requestOptions(beta)...)

	ch := make(chan *dsc.ChatStreamResponse)
	dsc.SafeGoroutine(func() {
		defer close(ch)
		acc := newStreamAccumulator()
		prevLen := 0
		prevReasonLen := 0
		for stream.Next() {
			event := stream.Current()
			if err := acc.accumulate(event); err != nil {
				ch <- &dsc.ChatStreamResponse{Error: fmt.Sprintf("stream accumulate error: %v", err)}
				return
			}
			// 仅当文本有新增时才发送增量帧
			text := concatText(acc.msg.Content)
			if len(text) > prevLen {
				ch <- &dsc.ChatStreamResponse{Content: text[prevLen:]}
				prevLen = len(text)
			}
			// 思考过程增量（thinking 块）
			reason := concatReasoning(acc.msg.Content)
			if len(reason) > prevReasonLen {
				// 思维链调试开关（默认关闭）：reasoning 已随流式帧送宿主/TUI，stderr
				// 打印仅用于排查插件本身，且会整段混入宿主日志刷屏——设 DSC_LLM_DEBUG
				// 后才输出。
				if llmStreamDebug {
					fmt.Fprintf(os.Stderr, "[LLM-ANTHROPIC-REASONING] %s\n", reason[prevReasonLen:])
				}
				ch <- &dsc.ChatStreamResponse{Reasoning: reason[prevReasonLen:]}
				prevReasonLen = len(reason)
			}
		}
		if err := stream.Err(); err != nil {
			ch <- &dsc.ChatStreamResponse{Error: err.Error()}
			return
		}
		// 流结束：发送最终帧（工具调用与结束原因；文本已增量发送，不再重复）
		resp := &dsc.ChatStreamResponse{
			FinishReason: string(acc.msg.StopReason),
			ToolCalls:    extractToolCalls(acc.msg.Content),
		}
		// 處理 usage 信息（Anthropic 格式：input_tokens -> prompt_tokens, output_tokens -> completion_tokens；
		// cache_read/cache_creation 直接对应）
		resp.Usage = usageFromAnthropic(&acc.msg.Usage)
		ch <- resp
	})
	return ch, nil
}

// llmStreamDebug 思维链 stderr 调试开关（DSC_LLM_DEBUG），进程启动时读取一次。
var llmStreamDebug = os.Getenv("DSC_LLM_DEBUG") != ""

func (p *AnthropicProvider) Name(ctx context.Context) string {
	return "anthropic"
}

func (p *AnthropicProvider) Version(ctx context.Context) string {
	return "1.2.0" // 版本升级：支持图像输入（视觉）
}

func (p *AnthropicProvider) VisionEnabled() bool { return p.vision }

func (p *AnthropicProvider) HealthCheck(ctx context.Context) error {
	return nil
}

// thinkingEnabled 解析扩展思考开关：DSC_ANTHROPIC_THINKING 优先，旧名
// ANTHROPIC_THINKING 回退（与 DSC_ 前缀约定一致，旧名保留兼容）。0/false/off/no
// 关闭，其余（含未设置）开启。DSC_REASONING_EFFORT=none 是并列的关闭入口，
// 由 main() 单独处理。
func thinkingEnabled() bool {
	switch strings.ToLower(dsc.EnvWithFallback("DSC_ANTHROPIC_THINKING", "ANTHROPIC_THINKING", "1")) {
	case "0", "false", "off", "no":
		return false
	default:
		return true
	}
}

func main() {
	cfg := dsc.LoadLLMConfig("anthropic")
	apiKey := cfg.APIKey
	if apiKey == "" {
		// 对于 llama.cpp server 或测试，可忽略
		apiKey = "sk-laamaafung-not-used"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.deepseek.com/anthropic"
	}
	model := cfg.Model
	if model == "" {
		model = "deepseek-flash"
	}

	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithMiddleware(omitZeroMaxTokens),
	}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}

	// 扩展思考默认开启：DeepSeek 的 anthropic 接口支持 thinking 参数并返回 thinking 块，
	// 其内容经 chat 流式增量转发为 reasoning 帧渲染到 TUI。
	// DSC_ANTHROPIC_THINKING=0 可显式关闭（旧名 ANTHROPIC_THINKING 仍兼容）；
	// DSC_REASONING_EFFORT=none 同样关闭思考；思考 token 预算由 DSC 统一推理强度控制：
	// DSC_ANTHROPIC_THINKING_BUDGET 直接指定，否则 DSC_REASONING_EFFORT
	// （none/minimal/low/medium/high/max）按 DSC 标准档位映射
	// （0/2048/4096/8192/16384/32768），均未设时默认 4096（DeepSeek 忽略 budget 值，
	// 故强度改由下方 output_config.effort 表达，二者同时下发互不冲突）。
	thinking := thinkingEnabled()
	// DSC_REASONING_EFFORT=none 显式关闭扩展思考（对应 OpenAI 路径 reasoning_effort=none）
	if cfg.ReasoningEffort == "none" {
		thinking = false
	}
	thinkingBudget := cfg.ThinkingBudget
	if thinkingBudget == 0 {
		thinkingBudget = 4096
	}
	// 单轮输出上限（插件级默认）：只来自插件 env（ANTHROPIC_MAX_OUTPUT_TOKENS 或通用
	// DSC_MAX_OUTPUT_TOKENS），宿主不再注入窗口值——「缺席是服务端自填 4096」属于服务端
	// 的选择，不该由宿主替用户决定输出预算。均缺席时 0 = 请求不携带，零值字段由
	// omitZeroMaxTokens 中间件摘除。
	maxTokens := cfg.MaxOutputTokens

	provider := &AnthropicProvider{
		client:          anthropic.NewClient(opts...),
		model:           model,
		thinking:        thinking,
		thinkingBudget:  thinkingBudget,
		reasoningEffort: cfg.ReasoningEffort,
		maxTokens:       maxTokens,
		vision:          visionEnabled(baseURL, model, !cfg.VisionEnabled),
		filesAPI:        isDeepSeekEndpoint(baseURL),
		fileCache:       map[string]string{},
		apiKey:          apiKey,
		baseURL:         baseURL,
	}

	// 以公共 SDK（dsc-sdk）声明式启动：SDK 复用宿主 dsc.LLMGRPCPlugin
	// 自动提供 LLMService + 元数据（重写自旧的 plugin.Serve 样板）。
	sdk := dsc.New(dsc.Config{Name: "anthropic", Version: "1.2.0", Type: dsc.TypeLLM})
	sdk.LLM(provider)
	sdk.Serve()
}
