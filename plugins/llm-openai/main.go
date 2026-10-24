package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	dsc "dsc-sdk"

	openai "github.com/sashabaranov/go-openai"
)

type OpenAIProvider struct {
	client *openai.Client
	model  string
	// maxTokens 单轮输出上限（插件级默认）：取值来源显式 DSC_OPENAI_MAX_OUTPUT_TOKENS
	//（旧名 OPENAI_MAX_OUTPUT_TOKENS 回退）> 宿主注入 DSC_MAX_OUTPUT_TOKENS
	//（一律注入=有效上下文窗口值：探测命中 LLAMACPP 时=探测窗口，云端=配置 context_window）
	// > -1 = 明确不携带（必须写 -1；写 0 仍视为「未设置」走兜底，`>0 才携带` 故负值被忽略）；
	// 请求级参数（压缩等场景）经 resolveMaxTokens 优先于本值。
	maxTokens int
	// vision 是否启用图像输入：默认按模型能力自动判断，DSC_NO_VISION=1 强制关闭。
	vision bool
	// filesAPI 是否可把超大图自动上传 DeepSeek Files API（base URL 为 deepseek.com 时启用）。
	filesAPI bool
	// reasoningEffort DeepSeek 等端点的推理强度（reasoning_effort）；空=不携带。
	// 来自 DSC_OPENAI_REASONING_EFFORT，或 DSC_REASONING_EFFORT 档位映射（max→high）。
	reasoningEffort string
	// fileCache 已上传图片的 data URL → file_id 缓存（同一图多轮复用，避免重复上传）。
	fileMu    sync.Mutex
	fileCache map[string]string
	// maxTokensLimit 服务端 max_tokens 合法上限（自适应学习；0=未知）。
	// 各端点上限不同且随模型变化（DeepSeek 393216、部分端点更低），硬编码不可靠，
	// 故在服务端以 400 拒绝时从报文区间读出并缓存，后续请求直接按其钳制。
	maxTokensLimit atomic.Int64
	// noMaxTokens 服务端只报「max_tokens 非法」但未给出区间时置位：此后一律不携带
	// 该字段（等模型自然结束），避免每个请求都先吃一次 400。
	noMaxTokens atomic.Bool
}

// maxInlineImageBytes 内联 base64 单图大小上限（对齐 DeepSeek 32 MiB 内联限制，
// 预留余量避免请求体逼近 48 MiB 上限；超出则走 Files API 上传）。
const maxInlineImageBytes = 20 << 20 // 20 MiB

// toolCallDeltaAccumulator 用於累積工具調用的增量信息
type toolCallDeltaAccumulator struct {
	ID           string
	Name         string
	ArgumentsStr string
}

// usageFromOpenAI 將 OpenAI usage 轉換為 dsc.Usage（nil 返回 nil）。
// cached_tokens（DeepSeek/llama.cpp 的 prompt_tokens_details.cached_tokens）映射为
// CacheReadInputTokens；命中率计算需要 miss 侧，按 REX 语义以
// CacheCreationInputTokens = prompt - cached 近似（无缓存报告时保持 0）。
func usageFromOpenAI(u *openai.Usage) *dsc.Usage {
	if u == nil {
		return nil
	}
	var cacheRead int32
	if u.PromptTokensDetails != nil {
		cacheRead = int32(u.PromptTokensDetails.CachedTokens)
	}
	// 口径契约（见 core.PromptSideTokens）：PromptTokens 报**完整** prompt。
	// OpenAI 兼容端点的 prompt_tokens 本就含命中缓存的部分（prompt = miss + cached），
	// 故此处不做任何相加；CacheRead 只是其中「由缓存承担」的部分，供显示命中率。
	// CacheCreation 在 OpenAI 侧没有对应计数（写入缓存不单独上报），留 0——曾经把
	// 「miss」塞进这个字段，导致下游 prompt+cacheRead+cacheCreation 算成 2×prompt。
	return &dsc.Usage{
		PromptTokens:             int32(u.PromptTokens),
		CompletionTokens:         int32(u.CompletionTokens),
		TotalTokens:              int32(u.TotalTokens),
		CacheReadInputTokens:     cacheRead,
		CacheCreationInputTokens: 0,
	}
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

// toOpenAIMessages 把 dsc.Message 转换为 OpenAI 请求消息。用户消息携带图像且
// 视觉开启时，构造多模态 content（文本 + image_url / file 块）；assistant 消息
// 回带工具调用；tool 消息回带 tool_call_id 并在连续工具消息段结束后以一条 user
// 图像消息补发其图像附件（OpenAI 协议工具消息不支持图像内容，且须紧跟
// assistant tool_calls，故不能原地内嵌）。
func (p *OpenAIProvider) toOpenAIMessages(messages []dsc.Message) []openai.ChatCompletionMessage {
	openaiMessages := make([]openai.ChatCompletionMessage, 0, len(messages)+2)
	// pendingToolImages 缓冲连续 tool 消息的图像附件（如 computer-use 截图），
	// 段结束（下一条非 tool 消息或历史末尾）时统一补发；视觉关闭时
	// fileContentBlocks 过滤全部图像引用，补发退化为空、不产生消息。
	var pendingToolImages []string
	flushToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		refs := pendingToolImages
		pendingToolImages = nil
		parts := p.fileContentBlocks("", refs)
		if len(parts) > 0 {
			openaiMessages = append(openaiMessages, openai.ChatCompletionMessage{Role: "user", MultiContent: parts})
		}
	}
	for _, m := range messages {
		if m.Role != "tool" {
			flushToolImages()
		}
		msg := openai.ChatCompletionMessage{Role: m.Role}
		if m.Role == "user" && len(m.Images) > 0 {
			// 多模态分支：文本块 + 每张图像的块（image 受 p.vision 门控；
			// dsc-txt 文本引用不受视觉限制，始终注入）
			msg.MultiContent = p.fileContentBlocks(m.Content, m.Images)
		} else if m.Role == "tool" && len(m.Images) > 0 {
			// 工具结果图像：工具消息本体走纯文本，图像缓冲到段末统一补发
			msg.Content = m.Content
			pendingToolImages = append(pendingToolImages, m.Images...)
		} else {
			msg.Content = m.Content
		}
		if m.Role == "tool" {
			// OpenAI 协议要求 tool 消息回带 tool_call_id 关联原调用
			msg.ToolCallID = m.ToolCallID
		}
		// assistant 消息需回带工具调用（OpenAI 格式要求 tool_calls 与后续 tool 结果匹配）
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			msg.ToolCalls = make([]openai.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				argsJSON, _ := json.Marshal(tc.Arguments)
				msg.ToolCalls[j] = openai.ToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openai.FunctionCall{
						Name:      tc.Name,
						Arguments: string(argsJSON),
					},
				}
			}
		}
		openaiMessages = append(openaiMessages, msg)
	}
	flushToolImages()
	return openaiMessages
}

// fileContentBlocks 构造用户消息的多模态 content：文本块 + 文件附件块。
// 图像引用（dsc-img:// 持久附件 / dsc-shot:// 操作截图）按路由策略投影（超限
// 缩放重编码）为 base64 data URL，单图解码后不超过内联上限时用 image_url、
// 超限且 DeepSeek Files API 可用时自动上传并以 file 块引用 file_id（避免请求体
// 超限）；图像仅当视觉开启（p.vision）时嵌入；引用失效降级为占位文本 part。
// 文本引用（dsc-txt://）读取内容作为纯文本块注入，不受视觉限制。
func (p *OpenAIProvider) fileContentBlocks(text string, refs []string) []openai.ChatMessagePart {
	parts := make([]openai.ChatMessagePart, 0, len(refs)+1)
	if text != "" {
		parts = append(parts, openai.ChatMessagePart{Type: openai.ChatMessagePartTypeText, Text: text})
	}
	for _, ref := range refs {
		if strings.HasPrefix(ref, dsc.TextRefPrefix) {
			content, err := dsc.ResolveTextRef(ref)
			if err != nil {
				log.Printf("⚠️ 忽略无法解析的文本引用: %v", err)
				continue
			}
			parts = append(parts, openai.ChatMessagePart{Type: openai.ChatMessagePartTypeText, Text: content})
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
			parts = append(parts, openai.ChatMessagePart{Type: openai.ChatMessagePartTypeText, Text: imageUnavailableText(ref)})
			continue
		}
		part := openai.ChatMessagePart{
			Type:     openai.ChatMessagePartTypeImageURL,
			ImageURL: &openai.ChatMessageImageURL{URL: url},
		}
		if p.filesAPI && dataURLSize(url) > maxInlineImageBytes {
			if fileID := p.uploadImage(url); fileID != "" {
				part = openai.ChatMessagePart{
					Type: openai.ChatMessagePartTypeFile,
					File: &openai.ChatMessageFile{FileID: fileID},
				}
			}
		}
		parts = append(parts, part)
	}
	return parts
}

// imageUnavailableText 图像引用失效（截图过期/附件缺失）时的模型可见占位文本：
// 身份留痕——引用写进占位，模型可据此重新截图或请用户重新提供（对齐 DSH
// offloadedImageText/textOnlyImageText 的稳定占位语义，与 llm-anthropic 同文案）。
func imageUnavailableText(ref string) string {
	return "[image unavailable: attachment expired or missing; " + ref + "]"
}

// dataURLSize 返回 data URL 解码后的近似字节数（按 base64 长度估算；非 data URL 返回 0）。
func dataURLSize(url string) int {
	const marker = ";base64,"
	i := strings.Index(url, marker)
	if i < 0 {
		return 0
	}
	return len(url[i+len(marker):]) / 4 * 3
}

// parseDataURL 解析 data:image/<mime>;base64,<data> 为 (mime, 原始字节)。
func parseDataURL(url string) (string, []byte, error) {
	i := strings.Index(url, ";base64,")
	if !strings.HasPrefix(url, "data:") || i < 0 {
		return "", nil, os.ErrInvalid
	}
	mime := url[len("data:"):i]
	raw, err := base64.StdEncoding.DecodeString(url[i+len(";base64,"):])
	if err != nil {
		return "", nil, err
	}
	return mime, raw, nil
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

// uploadImage 把超大图上传到 DeepSeek Files API（purpose=user_data）并返回 file_id。
// 同一 data URL 只上传一次（进程内缓存，多轮复用）；上传失败返回空串（回退内联）。
func (p *OpenAIProvider) uploadImage(dataURL string) string {
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	if id, ok := p.fileCache[dataURL]; ok {
		return id
	}
	mime, raw, err := parseDataURL(dataURL)
	if err != nil {
		return ""
	}
	file, err := p.client.CreateFileBytes(context.Background(), openai.FileBytesRequest{
		Name:    "image" + mimeToExt(mime),
		Bytes:   raw,
		Purpose: openai.PurposeUserData,
	})
	if err != nil {
		log.Printf("⚠️ 图片上传 Files API 失败，回退内联: %v", err)
		return ""
	}
	p.fileCache[dataURL] = file.ID
	return file.ID
}

func (p *OpenAIProvider) Chat(ctx context.Context, messages []dsc.Message, tools []dsc.LLMTool, maxTokens int) (*dsc.ChatResponse, error) {
	// 转换消息格式
	openaiMessages := p.toOpenAIMessages(messages)

	// 转换工具格式
	openaiTools := make([]openai.Tool, len(tools))
	for i, t := range tools {
		openaiTools[i] = openai.Tool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  json.RawMessage(t.ParametersJSON),
			},
		}
	}

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: openaiMessages,
		Tools:    openaiTools,
	}
	// max_tokens：请求级参数（压缩等场景的窗口净余值）优先，其次插件级默认
	//（显式 env > 宿主注入）；<=0 不携带，等模型自然结束。
	if mt := p.resolveMaxTokens(maxTokens); mt > 0 {
		req.MaxTokens = mt
	}
	// reasoning_effort：DeepSeek 等推理端点的思考强度；空=不携带（非推理模型忽略）。
	if p.reasoningEffort != "" {
		req.ReasoningEffort = p.reasoningEffort
	}

	resp, err := p.client.CreateChatCompletion(ctx, req)
	if err != nil {
		// 服务端因 max_tokens 超范围拒绝时按其上限重试一次：宿主注入的
		// DSC_MAX_OUTPUT_TOKENS=context_window 可能大于端点上限（例如
		// context_window=1048576 而 DeepSeek 上限 393216）。
		if limit, ok := maxTokensRejectionLimit(err); ok {
			p.learnMaxTokensLimit(limit)
			req.MaxTokens = limit // limit=0 表示报文未给出区间 → 不携带该字段
			resp, err = p.client.CreateChatCompletion(ctx, req)
		}
		if err != nil {
			return nil, err
		}
	}

	if len(resp.Choices) == 0 {
		return &dsc.ChatResponse{Content: "", FinishReason: "stop"}, nil
	}

	choice := resp.Choices[0]
	result := &dsc.ChatResponse{
		Content:      choice.Message.Content,
		FinishReason: string(choice.FinishReason),
	}

	// 处理工具调用
	if len(choice.Message.ToolCalls) > 0 {
		result.ToolCalls = make([]dsc.ToolCall, len(choice.Message.ToolCalls))
		for i, tc := range choice.Message.ToolCalls {
			var args map[string]interface{}
			json.Unmarshal([]byte(tc.Function.Arguments), &args)
			result.ToolCalls[i] = dsc.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: args,
			}
		}
	}

	return result, nil
}

// resolveMaxTokens 计算本请求实际携带的 max_tokens：请求级参数（压缩等场景的
// 窗口净余值）优先，其次插件级默认（显式 env > 宿主注入），<=0 表示不携带。
// 已知服务端上限时提前钳制，避免每个新进程的首个请求都先吃一次 400。
func (p *OpenAIProvider) resolveMaxTokens(requestMaxTokens int) int {
	if p.noMaxTokens.Load() {
		return 0
	}
	mt := requestMaxTokens
	if mt <= 0 {
		mt = p.maxTokens
	}
	if mt <= 0 {
		return 0
	}
	if limit := int(p.maxTokensLimit.Load()); limit > 0 && mt > limit {
		mt = limit
	}
	return mt
}

// learnMaxTokensLimit 记录服务端拒绝时给出的上限：limit>0 缓存上限；limit=0 表示
// 报文只提到 max_tokens 却未给区间，改为此后一律不携带该字段。
func (p *OpenAIProvider) learnMaxTokensLimit(limit int) {
	if limit > 0 {
		p.maxTokensLimit.Store(int64(limit))
		return
	}
	p.noMaxTokens.Store(true)
}

// maxTokensRangeRE 从服务端 400 报文里提取 max_tokens 合法上限，例如 DeepSeek：
// "the valid range of max_tokens is [1, 393216]"。
var maxTokensRangeRE = regexp.MustCompile(`(?i)max_tokens[^0-9]*\[\s*[0-9]+\s*,\s*([0-9]+)\s*\]`)

// apiErrorStatusMessage 取 HTTP 状态码与报文正文，兼容非流式 APIError 与流式
// RequestError 两种错误载体（go-openai 两条路径返回的类型不同）。
func apiErrorStatusMessage(err error) (int, string) {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode, apiErr.Message
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		msg := string(reqErr.Body)
		if reqErr.Err != nil {
			msg += " " + reqErr.Err.Error()
		}
		return reqErr.HTTPStatusCode, msg
	}
	return 0, err.Error()
}

// maxTokensRejectionLimit 判断错误是否为「max_tokens 超出合法范围」，并返回可用上限：
//   - 报文给出区间 → 区间上限；
//   - 仅提到 max_tokens 但无区间 → 0（表示改发不带该字段的请求）；
//   - 其它错误 → ok=false，调用方原样返回错误。
func maxTokensRejectionLimit(err error) (int, bool) {
	status, msg := apiErrorStatusMessage(err)
	if status != http.StatusBadRequest {
		return 0, false
	}
	if !strings.Contains(strings.ToLower(msg), "max_tokens") {
		return 0, false
	}
	if m := maxTokensRangeRE.FindStringSubmatch(msg); len(m) == 2 {
		if n, convErr := strconv.Atoi(m[1]); convErr == nil && n > 0 {
			return n, true
		}
	}
	return 0, true
}

func (p *OpenAIProvider) Name(ctx context.Context) string       { return "openai" }
func (p *OpenAIProvider) Version(ctx context.Context) string    { return "1.2.0" } // 支持图像输入（视觉）+ 插件级 max_tokens 默认
func (p *OpenAIProvider) HealthCheck(ctx context.Context) error { return nil }
func (p *OpenAIProvider) VisionEnabled() bool                   { return p.vision }

// ChatStream 實現 LLMProvider.ChatStream 接口
func (p *OpenAIProvider) ChatStream(ctx context.Context, messages []dsc.Message, tools []dsc.LLMTool) (<-chan *dsc.ChatStreamResponse, error) {
	// 轉換消息格式
	openaiMessages := p.toOpenAIMessages(messages)

	// 轉換工具格式
	openaiTools := make([]openai.Tool, len(tools))
	for i, t := range tools {
		openaiTools[i] = openai.Tool{
			Type: "function",
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  json.RawMessage(t.ParametersJSON),
			},
		}
	}

	req := openai.ChatCompletionRequest{
		Model:    p.model,
		Messages: openaiMessages,
		Tools:    openaiTools,
		Stream:   true,
		// 请求流式 usage：服务端（含 llama.cpp）会在最后一个分片返回整轮 token 统计
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
	}
	// max_tokens：流式主路径此前从不携带——llama.cpp 等 OpenAI 兼容端点会把缺席
	// 解释为服务端默认（n_predict=-1 无限，但 anthropic 兼容口同类场景自填 4096）。
	// 宿主一律注入 DSC_MAX_OUTPUT_TOKENS=有效上下文窗口值（不分本地/云端），在此
	// 显式携带（LLAMACPP 侧受上下文自然钳制，无害；云端值可经 context_window 或
	// 插件 env 调整）。
	if mt := p.resolveMaxTokens(0); mt > 0 {
		req.MaxTokens = mt
	}
	// reasoning_effort：DeepSeek 等推理端点的思考强度；空=不携带（非推理模型忽略）。
	if p.reasoningEffort != "" {
		req.ReasoningEffort = p.reasoningEffort
	}

	stream, err := p.client.CreateChatCompletionStream(ctx, req)
	if err != nil {
		// 同 Chat：max_tokens 超范围时按服务端给出的上限重试一次。
		if limit, ok := maxTokensRejectionLimit(err); ok {
			p.learnMaxTokensLimit(limit)
			req.MaxTokens = limit
			stream, err = p.client.CreateChatCompletionStream(ctx, req)
		}
		if err != nil {
			return nil, err
		}
	}

	ch := make(chan *dsc.ChatStreamResponse)
	dsc.SafeGoroutine(func() {
		defer close(ch)

		var textAccumulator strings.Builder
		var toolCallAccums map[int]*toolCallDeltaAccumulator
		streamFinished := false

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				ch <- &dsc.ChatStreamResponse{Error: err.Error()}
				return
			}

			// 處理 usage 數據（llama.cpp 等會在流式響應中攜帶 usage）
			if resp.Usage != nil {
				ch <- &dsc.ChatStreamResponse{
					Usage: usageFromOpenAI(resp.Usage),
				}
			}

			if len(resp.Choices) == 0 {
				// llama.cpp 在正文結束後會追加一個空 choices、僅含 usage 的結尾分片；
				// 該分片送達即代表流已結束，處理完 usage 即可退出。
				if streamFinished {
					return
				}
				continue
			}

			choice := resp.Choices[0]
			delta := choice.Delta

			// 處理文本增量
			if delta.Content != "" {
				textAccumulator.WriteString(delta.Content)
				ch <- &dsc.ChatStreamResponse{
					Content: delta.Content,
				}
			}

			// 處理思考過程增量（DeepSeek reasoning_content 等）
			if delta.ReasoningContent != "" {
				ch <- &dsc.ChatStreamResponse{
					Reasoning: delta.ReasoningContent,
				}
			}

			// 處理工具調用增量
			if len(delta.ToolCalls) > 0 {
				if toolCallAccums == nil {
					toolCallAccums = make(map[int]*toolCallDeltaAccumulator)
				}
				for _, tc := range delta.ToolCalls {
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					}
					if toolCallAccums[idx] == nil {
						toolCallAccums[idx] = &toolCallDeltaAccumulator{
							ID: tc.ID,
						}
					}
					acc := toolCallAccums[idx]
					// 部分服务端（如 llama.cpp）在后续分片才携带 ID，迟到时补上，
					// 避免工具调用 ID 为空导致 agent 无法关联 tool result。
					if acc.ID == "" && tc.ID != "" {
						acc.ID = tc.ID
					}
					if tc.Function.Name != "" {
						acc.Name = tc.Function.Name
					}
					if tc.Function.Arguments != "" {
						acc.ArgumentsStr += tc.Function.Arguments
					}
				}
			}

			// 處理完成原因
			if choice.FinishReason != "" && choice.FinishReason != "null" {
				if streamFinished {
					continue
				}
				streamFinished = true

				// 轉換工具調用為 dsc.ToolCall：按 index 排序，保证多工具调用顺序稳定
				// （map 遍历顺序随机，直接遍历会让同一流在不同运行下顺序漂移）。
				idxList := make([]int, 0, len(toolCallAccums))
				for idx := range toolCallAccums {
					idxList = append(idxList, idx)
				}
				sort.Ints(idxList)
				var toolCalls []dsc.ToolCall
				for _, idx := range idxList {
					acc := toolCallAccums[idx]
					var args map[string]interface{}
					json.Unmarshal([]byte(acc.ArgumentsStr), &args)
					toolCalls = append(toolCalls, dsc.ToolCall{
						ID:        acc.ID,
						Name:      acc.Name,
						Arguments: args,
					})
				}

				finishReason := string(choice.FinishReason)

				// 把 usage 一并转发（部分服務會在 finish 分片攜帶 usage）；
				// 若該分片同時含正文/工具調用，則轉發完後仍繼續讀流，
				// 由後續的空 choices 結尾分片（或 io.EOF）收尾退出。
				ch <- &dsc.ChatStreamResponse{
					Content:      "",
					FinishReason: finishReason,
					ToolCalls:    toolCalls,
					Usage:        usageFromOpenAI(resp.Usage),
				}
				// 不在這裡返回：llama.cpp 等會緊接著發送僅含 usage 的空 choices 結尾分片，
				// 稍後由上方空 choices 分支收尾退出。
			}
		}
	})

	return ch, nil
}

func main() {
	cfg := dsc.LoadLLMConfig("openai")
	apiKey := cfg.APIKey
	if apiKey == "" {
		// 對於 llama.cpp server，API key 通常是可選的或接受任意值
		apiKey = "sk-laamaafung-not-used"
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://api.deepseek.com"
	}
	model := cfg.Model
	if model == "" {
		model = "deepseek-flash"
	}

	config := openai.DefaultConfig(apiKey)
	config.BaseURL = baseURL

	provider := &OpenAIProvider{
		client:          openai.NewClientWithConfig(config),
		model:           model,
		maxTokens:       int(cfg.MaxOutputTokens),
		vision:          visionEnabled(baseURL, model, !cfg.VisionEnabled),
		filesAPI:        isDeepSeekEndpoint(baseURL),
		reasoningEffort: cfg.OpenAIReasoningEffort,
		fileCache:       map[string]string{},
	}

	// 以公共 SDK（dsc-sdk）声明式启动：SDK 复用宿主 dsc.LLMGRPCPlugin
	// 自动提供 LLMService + 元数据（重写自旧的 plugin.Serve 样板）。
	sdk := dsc.New(dsc.Config{Name: "openai", Version: "1.2.0", Type: dsc.TypeLLM})
	sdk.LLM(provider)
	sdk.Serve()
}
