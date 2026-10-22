// Package main 实现 dsc-billion-context 插件：以 acp-kernel 算法接管 DSC 的上下文压缩。
//
// 接管方式（零 agent 改动）：
//  1. 注册 Hook.OnEvent 订阅 agent/pre-step 事件
//  2. 每次进入 LLM 请求前，插件收到当前消息列表 JSON
//  3. 插件经 bcacp.ProcessTurn 跑 pipeline：分配 ref、注入 <acp> 标签、应用 prune、决策 nudge
//  4. 返回改写后的消息列表（{"messages": [...]}）给宿主，宿主透传给 LLM
//  5. 模型调用 compress 工具时触发压缩：插件调 bcacp.ApplyCompression 持久化块、遮蔽范围
//
// 4 个模型可见工具：
//   - compress: 把消息范围替换为摘要（模型生成 summary）
//   - decompress: 恢复压缩块内容
//   - search_context: 在压缩块摘要中搜索关键词
//   - acp_status: 查看上下文使用率与可压缩范围
//
// state 持久化：每个 session 一个 JSON 文件，与宿主会话存档同目录
// （ExecDir/sessions/<session>.acp.json，后缀区分）。
// fork/恢复时经 session 事件日志重放重建（后续实现）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"

	dsc "dsc-sdk"
	"dsc/core"
	"dsc/proto"

	bcacp "dsc-billion-context/acp"
	bcadapter "dsc-billion-context/adapter"
)

// BillionContext 插件主结构。
type BillionContext struct {
	mu     sync.Mutex
	store  *bcadapter.StateStore
	config bcacp.Config
	// sessionID 当前会话 ID（须持 mu 读写）。
	// 取值优先级：pre-step 事件里的 Session（宿主真实会话 ID，见 handlePreStep）>
	// DSC_SESSION_ID env > 工作区派生的项目级键。此前只用后两者，而宿主从不注入
	// DSC_SESSION_ID，于是永远落在工作区键上——同一工作区开多个会话/fork 时共用
	// 一份 state（refs、blocks、nudge 基线互相污染，压缩会命中别的会话的消息）。
	sessionID string

	// logWindowOnce 首次 pre-step 打印生效窗口一次（启动期 stderr 尚未被宿主转发）。
	logWindowOnce sync.Once

	// lastMessages 缓存最近一次 pre-step 收到的 CoreMessage 列表，供 handleCompress
	// 解析 mNNNNN 边界引用（解决"compress 工具调用时无消息列表"的致命 bug）。
	// 每次 handlePreStep 更新此缓存；handleCompress 从中读取。
	lastMessages []bcacp.CoreMessage
}

func main() {
	// state 与会话存档同目录，靠 <id>.acp.json 后缀区分，不再单独开 billion-context/。
	// 目录一律经 SDK 解析（DSC_SESSION_DIR 优先），与宿主 Store 同一来源。
	store := bcadapter.NewStateStore(dsc.SessionDir())

	// 上下文窗口（默认 128K，可经 DSC_CONTEXT_WINDOW 覆盖——宿主按配置 context_window
	// 或端点探测值把该键注入所有插件，故云端 1M 窗口下应为 1048576；实际值在首个
	// pre-step 打印一次，见 handlePreStep）
	contextWindow := 131072
	if n := dsc.ContextWindow(); n > 0 {
		contextWindow = n
	}

	bc := &BillionContext{
		store:  store,
		config: bcacp.DefaultConfig(contextWindow),
	}

	// 即时工具结果吸收开关（默认关闭，DSC_ACP_ABSORB=1 启用）：
	// 启用后达标大工具结果被追加强制提示，模型经 absorb 蒸馏后消息对被隐藏
	if os.Getenv("DSC_ACP_ABSORB") == "1" {
		bc.config.Absorb.Enabled = true
		if n := os.Getenv("DSC_ACP_ABSORB_MIN_TOKENS"); n != "" {
			if v, err := parseInt(n); err == nil && v >= 0 {
				bc.config.Absorb.MinToolTokens = v
			}
		}
	}

	sdkInst := dsc.New(dsc.Config{
		Name:    "dsc-billion-context",
		Version: "0.3.0",
		Type:    dsc.TypeDsc,
		// 声明提供 "compaction" 能力：宿主检测到任何插件 Provides compaction 后，
		// 对齐 DSH preset 不挂 compaction-basic 改挂 billion-context 的后端替换模式——
		// 不硬编码插件名，任何声明 Provides compaction 的插件都接管。
		Provides: map[string]string{
			"compaction": "true",
		},
	})

	// 注册 4 个工具
	sdkInst.Tool(dsc.Tool{
		Name:        "compress",
		Description: "Replace a contiguous range of older conversation with a detailed summary you write. Use when content is genuinely consumed (no longer needed for the current task step). Batch form: content=[{startId,endId,summary,topic?}].",
		Handler:     bc.handleCompress,
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "content": {
      "type": "array",
      "description": "Batch of ranges to compress. Each range becomes one summary block (T1). Use bN block refs (e.g. b0..b4) to trigger T2 distillation.",
      "items": {
        "type": "object",
        "properties": {
          "startId": {"type": "string", "description": "Message ref of range start (mNNNNN or bN for tier escalation)"},
          "endId":   {"type": "string", "description": "Message ref of range end (inclusive)"},
          "summary": {"type": "string", "description": "Your detailed summary of the range (preserves key facts, decisions, code references)"},
          "topic":   {"type": "string", "description": "Optional topic label for the block (helps later search)"}
        },
        "required": ["startId", "endId", "summary"]
      }
    }
  },
  "required": ["content"]
}`),
	})
	sdkInst.Tool(dsc.Tool{
		Name:        "decompress",
		Description: "Restore a previously compressed block's content. Use when you need exact details lost in compression.",
		Handler:     bc.handleDecompress,
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "blockId": {"type": "string", "description": "The bN block id returned from a previous compress call"},
    "full":    {"type": "boolean", "description": "If true, restore full original content; false (default) returns only the summary", "default": false}
  },
  "required": ["blockId"]
}`),
	})
	sdkInst.Tool(dsc.Tool{
		Name:        "search_context",
		Description: "Search through compressed block summaries by keyword. Use BEFORE decompressing to find the right block.",
		Handler:     bc.handleSearch,
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Keyword or phrase to search in compressed summaries and history (hybrid BM25 + fuzzy match)"},
    "limit": {"type": "integer", "description": "Max results to return (default 5)", "default": 5, "minimum": 1, "maximum": 50}
  },
  "required": ["query"]
}`),
	})
	sdkInst.Tool(dsc.Tool{
		Name:        "acp_status",
		Description: "Show context usage and compressible ranges. No args = overview. Use to find what to compress next.",
		Handler:     bc.handleStatus,
		Schema: json.RawMessage(`{
  "type": "object",
  "properties": {},
  "additionalProperties": false
}`),
	})

	// 吸收工具（启用即时吸收时注册——模型侧第五个工具）
	if bc.config.Absorb.Enabled {
		sdkInst.Tool(dsc.Tool{
			Name:        bc.config.Absorb.ToolName,
			Description: "Distill a large tool result into a short summary. Call this IMMEDIATELY for any result marked [ACP absorb]; the original output is removed afterwards and your summary becomes the durable record.",
			Handler:     bc.handleAbsorb,
			Schema: json.RawMessage(`{
                          "type": "object",
                          "properties": {
                            "ref": {"type": "string", "description": "The mNNNNN ref of the tool result to absorb"},
                            "summary": {"type": "string", "description": "Distilled essentials: outcome, key values, exact paths:lines, error text verbatim, decisions"}
                          },
                          "required": ["ref", "summary"]
                        }`),
		})
	}

	// Hook: 拦截 agent/pre-step 改写消息列表 + ContextFn 贡献 ACP system prompt
	sdkInst.Hook(dsc.Hook{
		OnEvent: bc.onEvent,
		ContextFn: func() string {
			if bc.config.Absorb.Enabled {
				return bcacp.ACPSystemPrompt + "\n\n" + bcacp.BuildAbsorbSystemPrompt(bc.config.Absorb.ToolName)
			}
			return bcacp.ACPSystemPrompt
		},
	})

	sdkInst.Serve()
}

// onEvent 处理宿主事件。
// - agent/pre-step：改写消息列表（注入 ACP system prompt、prune、nudge）
// - agent/request-error：上下文溢出时触发紧急压缩后返回 {"retry": true} 让宿主重试
// 其余事件忽略（返回空 result，不影响宿主）。
func (bc *BillionContext) onEvent(ctx context.Context, eventType, dataJSON string) (string, error) {
	switch eventType {
	case string(core.EventAgentPreStep):
		return bc.handlePreStep(ctx, dataJSON)
	case string(core.EventAgentRequestError):
		return bc.handleRequestError(ctx, dataJSON)
	default:
		return "", nil
	}
}

// handleRequestError 处理 agent/request-error 事件：上下文溢出时触发紧急压缩，
// 返回 {"retry": true} 让宿主重新走 pre-step（已压缩的消息列表）+ 重新调 provider。
//
// 仅对 context_window_exceeded 错误码触发紧急压缩——其他错误（rate_limited 等）
// 不触发压缩，返回空 result（不重试）。
//
// 紧急压缩策略：把最近的可压缩范围（跳过保留区与受保护工具）全部压缩为一条摘要。
// 这比 nudge 路径更激进——nudge 在 45% 阈值触发；这里是溢出时的最后安全阀。
func (bc *BillionContext) handleRequestError(ctx context.Context, dataJSON string) (string, error) {
	var ev core.AgentRequestErrorEvent
	if err := json.Unmarshal([]byte(dataJSON), &ev); err != nil {
		return "", fmt.Errorf("billion-context: parse request-error event: %w", err)
	}

	// 仅对上下文溢出触发紧急压缩
	if ev.Code != "context_window_exceeded" {
		return "", nil
	}

	sessionID := bc.getSessionID()
	state, err := bc.store.Load(sessionID)
	if err != nil {
		return "", fmt.Errorf("billion-context: load state for emergency compress: %w", err)
	}

	// 从缓存获取消息列表
	bc.mu.Lock()
	cachedMsgs := bc.lastMessages
	bc.mu.Unlock()
	if len(cachedMsgs) == 0 {
		return "", nil // 无缓存消息，无法压缩
	}

	// 计算可压缩范围（跳过保留区与受保护工具）
	ranges := bcacp.ComputeCompressibleRanges(cachedMsgs, state, bc.config)
	if len(ranges) == 0 {
		// 无可压缩范围：不能救，不重试
		return "", nil
	}

	// 紧急压缩：把最大的可压缩范围压为一条摘要
	// 选 tokens 最大的 range
	best := ranges[0]
	for _, r := range ranges[1:] {
		if r.Tokens > best.Tokens {
			best = r
		}
	}

	// 生成紧急摘要（用固定模板，不调 LLM——溢出时可能已经无法再调 LLM）
	summary := fmt.Sprintf("[Emergency compaction: %d messages (~%d tokens) compressed due to context overflow. "+
		"Key details preserved from compressed range %s..%s.]",
		best.Count, best.Tokens, best.StartRef, best.EndRef)

	pruneRanges := []bcacp.PruneRange{
		{StartRef: best.StartRef, EndRef: best.EndRef, Summary: summary, Topic: "emergency-overflow"},
	}
	_, result := bcacp.ApplyCompression(pruneRanges, cachedMsgs, state, bc.config)
	if result.BlocksCreated == 0 {
		return "", nil // 压缩失败，不重试
	}

	// 持久化
	if err := bc.store.Save(sessionID, state); err != nil {
		fmt.Fprintf(os.Stderr, "[billion-context] emergency compress save state failed: %v\n", err)
	}

	// 返回 {"retry": true}：让宿主重新走 pre-step（插件已压缩消息）+ 重新调 provider
	out, _ := json.Marshal(map[string]any{"retry": true})
	return string(out), nil
}

// handlePreStep 处理 agent/pre-step 事件：跑 acp pipeline，返回改写后的消息列表。
//
// 协议：dataJSON 为 AgentPreStepEvent 的 JSON（含 MessagesJSON 字段）
// 返回：resultJSON 为 {"messages": [...]} 形式（改写后的 proto.Message 数组 JSON）
func (bc *BillionContext) handlePreStep(ctx context.Context, dataJSON string) (string, error) {
	// 首个 pre-step 打印实际生效的窗口（此时 gRPC 已握手，stderr 会被宿主转发到日志），
	// 用于核对云端 1M 窗口是否真的取代了 128K 默认值。
	bc.logWindowOnce.Do(func() {
		fmt.Fprintf(os.Stderr, "[billion-context] context window=%d state=%s/<id>%s\n",
			bc.config.ModelContextLimit, dsc.SessionDir(), dsc.ACPStateSuffix)
	})
	var ev core.AgentPreStepEvent
	if err := json.Unmarshal([]byte(dataJSON), &ev); err != nil {
		return "", fmt.Errorf("billion-context: parse pre-step event: %w", err)
	}

	// 记录宿主真实会话 ID：pre-step 载荷已带 req.GetSessionId()，但宿主从不注入
	// DSC_SESSION_ID env，故必须以事件为准，否则同工作区多会话共用一份 state。
	bc.setSessionID(ev.Session)

	// 解析消息列表
	var protoMsgs []*proto.Message
	if ev.MessagesJSON != "" {
		if err := json.Unmarshal([]byte(ev.MessagesJSON), &protoMsgs); err != nil {
			return "", fmt.Errorf("billion-context: parse messages: %w", err)
		}
	}

	// 加载（或创建）session state
	sessionID := bc.getSessionID()
	state, err := bc.store.Load(sessionID)
	if err != nil {
		return "", fmt.Errorf("billion-context: load state: %w", err)
	}

	// 转换为 acp 核心消息格式
	coreMsgs := bcadapter.ToCoreMessages(protoMsgs, nil)

	// 缓存消息列表供 handleCompress 使用（解决"compress 工具调用时无消息列表"的致命 bug）
	bc.mu.Lock()
	bc.lastMessages = coreMsgs
	bc.mu.Unlock()

	// fork/恢复重建：state 空白而历史已有 ACP 压缩痕迹（状态文件缺失/损坏、
	// 会话 fork 换 ID、跨机恢复）——消息列表即事件日志，重放 compress 调用链
	if len(state.Blocks) == 0 && state.Stats.CompressionCount == 0 && bcacp.HasACPStateTrace(coreMsgs) {
		if rebuilt, n := bcacp.RebuildStateFromMessages(coreMsgs, bc.config); n > 0 {
			state = rebuilt
			fmt.Fprintf(os.Stderr, "[billion-context] state rebuilt from history: %d block(s) replayed\n", n)
		}
	}

	// 回塔块的 compress 调用 ID：handleCompress 执行时调用对尚不在历史中，
	// 下一轮 pre-step 依「范围精确匹配」把历史调用的 callId 记到块上
	// （hide-compress-calls 依此区分已消费调用与孤儿调用）
	backfillCompressCallIDs(coreMsgs, state)

	// token 计数：优先用事件携带的宿主估算值（含工具结构开销），回退本地估算
	tokenCount := ev.TokenCount
	if tokenCount <= 0 {
		tokenCount = estimateTokens(protoMsgs)
	}

	// 跑 pipeline（renderTags=false：不注入 <acp> 标签到消息文本——标签的 tokens
	// 属性每轮估算会变化，破坏前缀缓存。改为仅用 mNNNNN ref 映射，标签信息经
	// acp_status 工具按需查询）
	result := bcacp.ProcessTurn(coreMsgs, state, bc.config, tokenCount)

	// 持久化更新后的 state
	if err := bc.store.Save(sessionID, result.State); err != nil {
		// 持久化失败不阻塞请求（已记内存）
		fmt.Fprintf(os.Stderr, "[billion-context] save state failed: %v\n", err)
	}

	// ACP system prompt 经 Hook.ContextFn 贡献（agent 的 buildSystemPrompt
	// 经 ListContext 拉取，宿主聚合后拼到 system prompt 中）。
	// 不再在 pre-step 改写消息列表注入——前缀缓存稳定性更好（system prompt
	// 经 ListContext 聚合后位置固定在消息列表头部，每轮不变）。
	renderedMsgs := result.Messages

	// nudge 作为尾部追加的 user 消息（不影响前缀缓存）
	if result.Nudge != nil && result.Nudge.ShouldInject {
		nudgeText := bcacp.FormatNudgeText(*result.Nudge)
		if nudgeText != "" {
			renderedMsgs = append(renderedMsgs, bcacp.CoreMessage{
				ID:          "acp_nudge",
				Role:        bcacp.RoleUser,
				ContentType: bcacp.ContentTypeText,
				Text:        nudgeText,
			})
		}
	}

	// 转回 proto.Message
	rewrittenProto := bcadapter.FromCoreMessages(renderedMsgs)
	rewrittenJSON, _ := json.Marshal(rewrittenProto)

	// 返回改写后的消息列表（宿主会用此替换原消息列表发给 LLM）
	return fmt.Sprintf(`{"messages": %s}`, string(rewrittenJSON)), nil
}

// handleCompress 处理 compress 工具调用。
func (bc *BillionContext) handleCompress(ctx context.Context, args json.RawMessage) (string, error) {
	// 宽松解析（代码围栏 / 尾逗号 / 输出截断救援 / 字段变体）：compress 的 summary
	// 常是大段自然语言，严格 json.Unmarshal 失败会让整轮压缩作废、上下文继续膨胀。
	ranges := bcacp.ParseCompressArgs(string(args))
	if len(ranges) == 0 {
		return "", fmt.Errorf("content is required: at least one range with startId/endId/summary")
	}

	sessionID := bc.getSessionID()
	state, err := bc.store.Load(sessionID)
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}

	// 从缓存获取最近一次 pre-step 的消息列表（解决"compress 工具调用时无消息列表"的致命 bug）
	bc.mu.Lock()
	cachedMsgs := bc.lastMessages
	bc.mu.Unlock()
	if len(cachedMsgs) == 0 {
		return "", fmt.Errorf("no cached messages: compress must be called after at least one LLM request (pre-step event)")
	}

	_, result := bcacp.ApplyCompression(ranges, cachedMsgs, state, bc.config)

	// 持久化
	if err := bc.store.Save(sessionID, state); err != nil {
		return "", fmt.Errorf("save state: %w", err)
	}

	out, _ := json.Marshal(map[string]any{
		"ok":               true,
		"blocksCreated":    result.BlocksCreated,
		"tokensCompressed": result.TokensCompressed,
		"errors":           result.Errors,
	})
	return string(out), nil
}

// handleDecompress 处理 decompress 工具调用。
func (bc *BillionContext) handleDecompress(ctx context.Context, args json.RawMessage) (string, error) {
	var req struct {
		BlockID string `json:"blockId"`
		Full    bool   `json:"full,omitempty"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if req.BlockID == "" {
		return "", fmt.Errorf("blockId is required")
	}

	sessionID := bc.getSessionID()
	state, err := bc.store.Load(sessionID)
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}

	summary, err := bcacp.Decompress(req.BlockID, state, req.Full)
	if err != nil {
		return "", fmt.Errorf("decompress: %w", err)
	}

	out, _ := json.Marshal(map[string]any{
		"ok":      true,
		"blockId": req.BlockID,
		"summary": summary,
	})
	return string(out), nil
}

// handleSearch 处理 search_context 工具调用。
func (bc *BillionContext) handleSearch(ctx context.Context, args json.RawMessage) (string, error) {
	var req struct {
		Query string `json:"query"`
		Limit int    `json:"limit,omitempty"`
	}
	if err := json.Unmarshal(args, &req); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	if req.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	if req.Limit <= 0 {
		req.Limit = 5
	}

	sessionID := bc.getSessionID()
	state, err := bc.store.Load(sessionID)
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}

	// 混合检索（BM25+fuzzy 归一化，对齐上游默认 hybrid）：文档集 = 块摘要
	// + 历史消息原文（命中消息携带归属块——模型知道 decompress 哪个块）
	bc.mu.Lock()
	cachedMsgs := bc.lastMessages
	bc.mu.Unlock()
	docs := bcacp.BlockDocs(state)
	if len(cachedMsgs) > 0 {
		docs = append(docs, bcacp.MessageDocs(cachedMsgs, state)...)
	}
	results := bcacp.SearchBlocks(docs, req.Query, bcacp.SearchOptions{Limit: req.Limit})
	out, _ := json.Marshal(map[string]any{
		"ok":      true,
		"query":   req.Query,
		"results": results,
		"count":   len(results),
	})
	return string(out), nil
}

// handleStatus 处理 acp_status 工具调用。
func (bc *BillionContext) handleStatus(ctx context.Context, args json.RawMessage) (string, error) {
	sessionID := bc.getSessionID()
	state, err := bc.store.Load(sessionID)
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}

	// 估算当前 token 数（无消息列表时用 state 估算）
	tokenCount := state.Stats.TokensCompressed + 1000 // 粗略估算
	report := bcacp.BuildStatus(nil, state, bc.config, tokenCount)
	text := bcacp.FormatStatus(report)

	out, _ := json.Marshal(map[string]any{
		"ok":     true,
		"report": text,
		"usage":  report.ContextUsage,
	})
	return string(out), nil
}

// getSessionID 获取当前会话 ID。
// 优先级：pre-step 事件透传的宿主真实会话 > 项目级键（SDK 的 ProjectSessionKey，
// 与宿主 session 存储同一实现——同一项目同名、不同项目隔离）。
// 不用环境变量承载会话 id：env 在子进程 spawn 时快照，中途切换会话不会同步。
func (bc *BillionContext) getSessionID() string {
	bc.mu.Lock()
	cur := bc.sessionID
	bc.mu.Unlock()
	if cur != "" {
		return cur
	}
	return dsc.ProjectSessionKey()
}

// setSessionID 记录当前会话 ID（pre-step 事件携带的宿主真实会话）。
func (bc *BillionContext) setSessionID(id string) {
	if id == "" {
		return
	}
	bc.mu.Lock()
	bc.sessionID = id
	bc.mu.Unlock()
}

// estimateTokens 估算 proto.Message 列表的总 token 数。
func estimateTokens(msgs []*proto.Message) int {
	total := 0
	for _, m := range msgs {
		total += len(m.Content) / 4
		if len(m.ToolCalls) > 0 {
			total += 8 // tool-call 结构开销
		}
	}
	return total
}

// containsStr 报告 s 是否包含 sub（轻量实现，避免引入 strings 包）。
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && strings.Contains(s, sub)
}

// handleAbsorb 处理 absorb 工具调用：记录吸收，消息对下轮 pre-step 隐藏。
func (bc *BillionContext) handleAbsorb(ctx context.Context, args json.RawMessage) (string, error) {
	parsed := bcacp.ParseAbsorbInput(args, "")
	if parsed == nil {
		return "", fmt.Errorf("invalid args: need ref (string) + summary (string)")
	}
	sessionID := bc.getSessionID()
	state, err := bc.store.Load(sessionID)
	if err != nil {
		return "", fmt.Errorf("load state: %w", err)
	}
	bc.mu.Lock()
	cachedMsgs := bc.lastMessages
	bc.mu.Unlock()
	outcome := bcacp.ApplyAbsorb(parsed.Ref, parsed.Summary, parsed.AbsorbCallID, cachedMsgs, state, bc.config)
	if outcome.OK {
		if err := bc.store.Save(sessionID, state); err != nil {
			fmt.Fprintf(os.Stderr, "[billion-context] absorb save state failed: %v\n", err)
		}
	}
	out, _ := json.Marshal(map[string]any{
		"ok":     outcome.OK,
		"result": outcome.ResultText,
	})
	return string(out), nil
}

// backfillCompressCallIDs 依范围精确匹配把历史 compress 调用的 callId 回塔到
// 尚无调用标记的 active 块（refs 按首次见到顺序分配且永不改变，
// 范围对即唯一键；同范围二次压缩会被块消费判定拒绝，不产生歧义）。
func backfillCompressCallIDs(msgs []bcacp.CoreMessage, state *bcacp.CompressionState) {
	pending := map[string]bool{}
	for i := range state.Blocks {
		b := &state.Blocks[i]
		if b.Active && b.CompressCallID == "" && b.StartRef != "" {
			pending[b.StartRef+"::"+b.EndRef] = true
		}
	}
	if len(pending) == 0 {
		return
	}
	for i := range msgs {
		m := &msgs[i]
		if m.ContentType != bcacp.ContentTypeToolCall || m.ToolName != "compress" || m.ToolCallID == "" {
			continue
		}
		for _, r := range bcacp.ParseCompressArgs(m.Text) {
			if pending[r.StartRef+"::"+r.EndRef] {
				for j := range state.Blocks {
					b := &state.Blocks[j]
					if b.CompressCallID == "" && b.StartRef == r.StartRef && b.EndRef == r.EndRef {
						b.CompressCallID = m.ToolCallID
					}
				}
			}
		}
	}
}

// parseInt 轻量 string → int。
func parseInt(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("invalid digit %q", c)
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
