package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dsc/jobs"
	"dsc/proto"
)

// 工具执行流水线（对齐 DSH tools/* 事件管线）：
//
//      pre-execute → execute → post-execute
//
// pre/post 均为 waterfall 事件：监听器通过 next 委托给链上后续监听器，
// 不调 next 即 veto。pre 阶段可拦截（阻止执行），post 阶段可观测/改写结果。
// 流水线挂在宿主聚合 Tool 服务上，agent 的所有工具调用都经过它，
// policy 插件等策略逻辑以监听器形式参与（替代旁路）。

const (
	// EventToolPreExecute 工具执行前拦截（waterfall）：veto 返回错误即阻止执行。
	EventToolPreExecute EventName = "tools/pre-execute"
	// EventToolExecute 工具执行（waterfall）：对齐 DSH tools/execute——监听器可改写
	// 执行方式/参数/超时策略（timeout-policy）；不调 next 即 veto。policy 插件经
	// bridgePolicyToPipeline 在此槽获得事件转发：超时裁决的 TimeoutSpec 由宿主
	// 机械安装为活跃续命执行域（WithIdleDeadline），执行 ctx 经 EventContext
	// 回读传播到实际执行体。
	EventToolExecute EventName = "tools/execute"
	// EventToolPostExecute 工具执行后处理（waterfall）：可观测或改写结果。
	EventToolPostExecute EventName = "tools/post-execute"
	// EventToolResult 工具结果（emit）：对齐 DSH tools/result——工具执行完成后广播，
	// 供插件订阅工具调用结果（非拦截，仅通知）。
	EventToolResult EventName = "tools/result"
	// EventToolsChange 工具集变更（emit）：对齐 DSH tools/change——工具加载/卸载时广播，
	// 供依赖工具清单的插件（如 subagent）感知工具集变化。
	EventToolsChange EventName = "tools/change"
)

// ToolInvocation 一次工具调用的流水线上下文（共享指针，监听器可直接改写）。
type ToolInvocation struct {
	ToolName      string
	ArgumentsJSON string
	CallID        string
	Result        string // post-execute 阶段：执行结果
	Err           error  // 执行错误或 pre 阶段 veto 原因
	ViewJSON      string // 工具声明的结构化视图 spec（可选，见 ViewExecutor）
	// Images 工具结果图像引用（dsc-shot:// / dsc-img://，可选，见 ViewImageExecutor）：
	// 插件回传的 data URL 在入库口（admitToolImages）折算为内容寻址引用后才进入
	// 本字段与会话历史；executeBackgroundPipeline 后台路径不携带。
	Images []string
	// Notices 策略插件本次调用产出的 advisory 上下文（对齐 DSH additionalContexts）：
	// post-execute 桥从 PolicyDecision.notice 机械收集，随返回值透传给调用方
	//（agent 在工具结果之后以合成用户消息投喂模型）；执行失败/被拦截的调用同样携带。
	Notices []string
	// SessionID 调用方会话标识（来自 ExecuteToolWithView 的 ctx，agent 每次调用都会带）；
	// 供 per-session 审批策略（approvalPolicyFor）与审计事件归属使用。
	SessionID string
	// ApprovalPolicy 调用方会话随调用转发的审批策略（"ask"/"never"；空 = 未提供）。
	// 审批门优先以此为本会话生效策略，实现宿主重启后 per-session 恢复。
	ApprovalPolicy string

	// Escalated / EscalatedMode 沙箱升级审批（对齐 DSH approveEscalation）字段：
	// 审批门 allowed 后置 Escalated=true，并把本次调用目标更宽档暂存，随后
	// sandboxPolicy 以 EscalatedMode 复审放行（仅作用于这一个调用）。
	Escalated     bool
	EscalatedMode SandboxPolicy
}

// PolicyService 事件种类与裁决动作（宿主与插件共同遵守的字符串约定，
// 与 proto PolicyEvent.kind / PolicyDecision.action 对应）。
const (
	policyEventPreExecute  = "tool/pre-execute"
	policyEventExecute     = "tool/execute"
	policyEventPostExecute = "tool/post-execute"
	policyDecisionDeny     = "deny"
	policyDecisionReplace  = "replace"
)

// ToolTimeoutError 工具调用超时（对齐 DSH TOOL_TIMEOUT 结构化结果）。
type ToolTimeoutError struct {
	Tool string
	Ms   int
}

func (e *ToolTimeoutError) Error() string {
	return fmt.Sprintf("Error: tool call timed out after %dms (TOOL_TIMEOUT)", e.Ms)
}

// ExecuteTool 以流水线方式执行工具：pre-execute(waterfall) → execute → post-execute(waterfall)。
// 任何阶段返回错误即中止；post 阶段的监听器可改写 inv.Result。
// 声明 TimeoutProvider 的工具在 execute 阶段获得协作式单次调用截止时间（timeout-policy）。
// 视图信息（插件 ViewJson / 宿主 ViewExecutor）不在此返回，见 ExecuteToolWithView。
func (m *Manager) ExecuteTool(ctx context.Context, toolName string, argsJSON json.RawMessage) (string, error) {
	result, _, _, _, err := m.ExecuteToolWithView(ctx, toolName, argsJSON)
	return result, err
}

// ExecuteToolWithView 与 ExecuteTool 语义相同，额外返回工具声明的结构化视图 spec
// （ViewJson）：插件工具透传 Tool.ViewFn 产物（经 RemoteTool），宿主工具按需实现
// ViewExecutor。聚合 Tool 服务（ToolGRPCServer）据此把视图与策略 notices 一并回给调用方。
//
// run_in_background 支持（对齐 DSH bash run_in_background）：若工具参数中
// run_in_background=true，宿主在 job 注册表中登记一个后台任务，异步执行完整
// 流水线（pre-execute → execute → post-execute），立即返回 job_id。
// 模型可用 job_output/job_list/job_kill 管理后台任务。
func (m *Manager) ExecuteToolWithView(ctx context.Context, toolName string, argsJSON json.RawMessage) (string, string, []string, []string, error) {
	// 检测 run_in_background 参数（对齐 DSH：模型在参数中声明 run_in_background: true）
	if isBackgroundRequest(argsJSON) && m.jobs != nil {
		caller := CallerFromContext(ctx)
		res, view, err := m.startBackgroundTool(ctx, toolName, argsJSON, caller)
		return res, view, nil, nil, err // 后台路径立即返回 job_id，图像与 notices 不适用
	}

	// 自动后台（声明式能力 auto-background）：声明该能力的工具不再直接同步执行，
	// 而是先登记成后台任务、再给一个前台宽限期。宽限内跑完 = 普通工具结果；
	// 超时 = 返回 job_id 转入后台。目的见 CapabilityAutoBackground 的注释：
	// 长命令占死一个 step 时，任何注入都要等它跑完才可能被模型看到。
	if m.jobs != nil && m.autoBackgroundable(toolName) {
		if res, view, handled, err := m.runWithGrace(ctx, toolName, argsJSON, CallerFromContext(ctx)); handled {
			return res, view, nil, nil, err
		}
	}

	inv := &ToolInvocation{ToolName: toolName, ArgumentsJSON: string(argsJSON), SessionID: CallerFromContext(ctx), ApprovalPolicy: ApprovalPolicyFromContext(ctx)}

	// pre-execute（waterfall）：守卫。不调 next 即 veto（阻止执行，execute 不运行）。
	// 语义对齐 DSH tools/pre-execute guards。
	runErr := m.events.Waterfall(EventToolPreExecute, EventContext{Data: inv, Context: ctx}, func(EventContext) error {
		// 互通机制 3：插件 BeforeTool 钩子（可 veto/改写参数；按加载顺序调用）
		if veto := m.runPluginBeforeTool(ctx, inv); veto != nil {
			inv.Err = veto
			return veto
		}
		return inv.Err
	})
	if inv.Err == nil && runErr != nil {
		inv.Err = runErr // pre 阶段 veto（execute 未运行）
	}

	// execute（waterfall）：真正执行 + 超时策略。对齐 DSH tools/execute dispatch body。
	// 执行 ctx 从 EventContext 回读：监听器（如 policy 桥的超时裁决）可换装
	// 执行域（活跃续命看门狗），未换装时用调用方原始 ctx。
	if inv.Err == nil {
		execErr := m.events.Waterfall(EventToolExecute, EventContext{Data: inv, Context: ctx}, func(evtCtx EventContext) error {
			execCtx := evtCtx.Context
			if execCtx == nil {
				execCtx = ctx
			}
			return m.executeToolBody(execCtx, inv, toolName)
		})
		if inv.Err == nil && execErr != nil {
			inv.Err = execErr
		}
	}

	// post-execute（waterfall）：观测/改写。对齐 DSH tools/post-execute finalize。
	if err := m.events.Waterfall(EventToolPostExecute, EventContext{Data: inv}, func(EventContext) error {
		// 互通机制 3：插件 AfterTool 钩子（可改写结果/错误）
		m.runPluginAfterTool(ctx, inv)
		return inv.Err
	}); err != nil {
		m.emitToolResult(inv)
		return "", "", nil, inv.Notices, err
	}

	// result（emit）：结果广播（非拦截），对齐 DSH tools/result。
	m.emitToolResult(inv)
	return inv.Result, inv.ViewJSON, inv.Images, inv.Notices, inv.Err
}

// executeToolBody 实际执行工具（可被 tools/execute 的 waterfall 监听器改写/包围）。
// 返回执行错误；调用方负责据此置 inv.Err。超时策略在此协作式应用（对齐 DSH timeout-policy）。
//
// 通用 panic recover：覆盖所有宿主侧工具（含 builtin / run_code / agent
// 内部工具等）与 RemoteTool 调用——任何工具 panic 都被转为错误返回，避免宿主进程崩溃
// 导致 LLM 连接中断。这是「工具意外不中断会话」的最后一道防线——插件 SDK 层（sdk/tool.go）
// 已先行 recover 一次，本层兜底覆盖未用 SDK 的工具（如 Lua 脚本工具、host 内置工具）。
func (m *Manager) executeToolBody(ctx context.Context, inv *ToolInvocation, toolName string) (errRet error) {
	// run_code 是 PTC presentation transport：native 模式不对模型暴露，也不可执行
	// （对齐 DSH“native agent must not find run_code”；ptc 折叠时才允许经其组合）。
	if toolName == runCodeToolName && !m.isPTC() {
		return fmt.Errorf("tool %s is only available in PTC (programmatic tool composition) mode", runCodeToolName)
	}
	start := time.Now()
	tool, ok := m.toolRegistry.Get(toolName)
	if !ok {
		m.logger.Warn("tool execution failed", "tool", toolName, "duration_ms", time.Since(start).Milliseconds(), "error", "tool not found")
		return m.toolNotFoundError(toolName)
	}
	// timeout-policy：声明 timeoutMs 的工具设置协作式截止时间（对齐 DSH）
	execCtx, timeoutMs := ctx, 0
	if tp, ok := tool.(TimeoutProvider); ok {
		if ms := tp.TimeoutMs(); ms > 0 {
			timeoutMs = ms
			var cancel context.CancelFunc
			execCtx, cancel = context.WithTimeout(ctx, time.Duration(ms)*time.Millisecond)
			defer cancel()
		}
	}
	// 通用 panic recover：任何工具执行 panic 都转为错误返回，避免宿主进程崩溃。
	// 覆盖 host 内置工具（runCodeTool / cron 管理工具等）与 RemoteTool（虽插件 SDK
	// 已 recover 一次，但若插件未用 SDK或 RemoteTool 自身 gRPC 调用 panic 仍兜底）。
	defer func() {
		if r := recover(); r != nil {
			err := fmt.Errorf("tool %s panicked: %v", toolName, r)
			inv.Result, inv.Err = "", err
			errRet = err
			m.logger.Error("tool panicked", "tool", toolName,
				"duration_ms", time.Since(start).Milliseconds(), "panic", fmt.Sprint(r))
		}
	}()
	args := json.RawMessage(inv.ArgumentsJSON)
	invoke := func() (string, string, []string, error) {
		if ev, ok := tool.(ViewImageExecutor); ok {
			return ev.ExecuteWithViewAndImages(execCtx, args)
		}
		if ev, ok := tool.(ViewExecutor); ok {
			res, view, err := ev.ExecuteWithView(execCtx, args)
			return res, view, nil, err
		}
		res, err := tool.Execute(execCtx, args)
		return res, "", nil, err
	}
	result, viewJSON, images, err := invoke()
	if errors.Is(err, context.DeadlineExceeded) {
		err = &ToolTimeoutError{Tool: toolName, Ms: timeoutMs}
	}
	// 只读调用的连接级瞬态错误重试：写语义/未知写、取消、超时、panic 一律不重试，
	// 避免重复副作用或把超时预算翻倍（见 core/tool_retry.go）。
	if retries := toolRetryAttempts(); retries > 0 && isTransientToolError(err) &&
		execCtx.Err() == nil && toolCallRetryable(tool, inv.ArgumentsJSON) {
		for attempt := 1; attempt <= retries; attempt++ {
			m.logger.Warn("tool transient failure, retrying",
				"tool", toolName, "attempt", attempt, "retries", retries, "error", err.Error())
			select {
			case <-execCtx.Done():
			case <-time.After(toolRetryBackoff * time.Duration(attempt)):
			}
			if execCtx.Err() != nil {
				break
			}
			result, viewJSON, images, err = invoke()
			if errors.Is(err, context.DeadlineExceeded) {
				err = &ToolTimeoutError{Tool: toolName, Ms: timeoutMs}
			}
			if !isTransientToolError(err) {
				break
			}
		}
	}
	// 结果净化：工具输出可能携带非法 UTF-8（如 Windows 原生命令的 OEM 码页输出、
	// GBK 编码的文件内容），原样进入会话日志/LLM 请求后，proto string 字段会拒绝
	// marshal（string field contains invalid UTF-8），整个工具结果与后续请求全部
	// 丢失。此处与 SDK 层（sdk/tool.go）双重设防：SDK 覆盖插件工具，本层覆盖宿主
	// 内置工具与一切绕过 SDK 的路径。非法字节退化为 U+FFFD，不中断会话。
	inv.Result, inv.ViewJSON, inv.Err = sanitizeUTF8(result), sanitizeUTF8(viewJSON), err
	inv.Images = m.admitToolImages(sanitizeUTF8All(images))
	// 结算留痕（-log 启用时的全链路诊断能力）：每个模型请求的工具调用在此
	// 统一计时——成功 Info、失败/超时 Warn，与 llm request 日志配套成完整链路。
	if err != nil {
		m.logger.Warn("tool execution failed", "tool", toolName,
			"duration_ms", time.Since(start).Milliseconds(), "error", err.Error())
	} else {
		m.logger.Info("tool executed", "tool", toolName,
			"duration_ms", time.Since(start).Milliseconds(), "result_chars", len(inv.Result))
	}
	return err
}

// sanitizeUTF8 把可能含非法 UTF-8 的字符串净化为合法 UTF-8（非法字节序列替换为
// U+FFFD）。已合法时零开销原样返回。
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "\uFFFD")
}

// sanitizeUTF8All 批量净化字符串切片（如工具图像附件 data URL），nil/空安全。
func sanitizeUTF8All(ss []string) []string {
	if len(ss) == 0 {
		return nil
	}
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = sanitizeUTF8(s)
	}
	return out
}

// ToolResultInfo tools/result 事件的载荷（对齐 DSH tools/result）。
type ToolResultInfo struct {
	ToolName string `json:"tool_name"`
	Result   string `json:"result"`
	Error    string `json:"error,omitempty"`
	ViewJSON string `json:"view_json,omitempty"`
	// Images 工具结果图像引用（dsc-shot:// / dsc-img://，内容寻址，见 admitToolImages）。
	Images []string `json:"images,omitempty"`
}

// emitToolResult 广播工具执行结果事件（非拦截）。
func (m *Manager) emitToolResult(inv *ToolInvocation) {
	info := ToolResultInfo{ToolName: inv.ToolName, Result: sanitizeUTF8(inv.Result), ViewJSON: sanitizeUTF8(inv.ViewJSON), Images: sanitizeUTF8All(inv.Images)}
	if inv.Err != nil {
		info.Error = sanitizeUTF8(inv.Err.Error())
	}
	m.events.Emit(EventToolResult, EventContext{Data: info})
}

// bridgePolicyToPipeline 把已加载的 policy 插件通用策略服务桥接为工具流水线监听器：
// pre-execute 转发事件，deny 即占槽拦截（reason 原文透传模型，对齐 DSH 单决策槽
// veto 语义）；execute 转发事件，deny 同样拦截，TimeoutSpec 裁决由宿主机械安装为
// 活跃续命执行域（WithIdleDeadline + TouchActivity，预算与文案全在插件）；
// post-execute 转发事件，replace 即改写模型可见结果。宿主不解读任何
// 领域语义——参数提取、工具类别判断、观察状态全部在插件侧（对齐 DSH「policy
// 插件持有策略，宿主只派发与执行裁决」）。策略服务不可用时不阻塞执行（best-effort：
// 策略缺失降级为无策略，而非工具不可用）。返回监听器的移除函数（卸载 policy 时调用）。
func (m *Manager) bridgePolicyToPipeline(name string, pc proto.PolicyServiceClient) []func() {
	var off []func()
	off = append(off, m.events.OnWaterfall(EventToolPreExecute, func(ctx EventContext, next func(EventContext) error) error {
		inv, _ := ctx.Data.(*ToolInvocation)
		if inv == nil {
			return next(ctx)
		}
		dec, err := pc.OnEvent(context.Background(), &proto.PolicyEvent{
			Kind:          policyEventPreExecute,
			Tool:          inv.ToolName,
			ArgumentsJson: inv.ArgumentsJSON,
			Session:       inv.SessionID,
		})
		if err != nil {
			m.logger.Warn("policy pre-execute forward failed; allowing", "policy", name, "tool", inv.ToolName, "err", err)
			return next(ctx)
		}
		if dec.GetAction() == policyDecisionDeny {
			reason := dec.GetReason()
			if reason == "" {
				reason = fmt.Sprintf("tool call denied by policy %s", name)
			}
			m.logger.Info("policy denied tool call", "policy", name, "tool", inv.ToolName, "reason", reason)
			return errors.New(reason)
		}
		return next(ctx)
	}))
	// execute 槽：deny 同占槽拦截；TimeoutSpec 裁决由宿主机械安装为活跃续命
	// 执行域（WithIdleDeadline）：换装执行 ctx 经 EventContext 传播到实际执行体，
	// 执行方（工具实现/子代理循环）以 TouchActivity 上报活动；看门狗触发
	// （cause=ErrIdleDeadline）时以裁决文案替换执行错误（对齐 deny reason 的
	// 透传语义：超时预算与模型可见文案全部在插件，宿主只负责执行裁决）。
	off = append(off, m.events.OnWaterfall(EventToolExecute, func(ctx EventContext, next func(EventContext) error) error {
		inv, _ := ctx.Data.(*ToolInvocation)
		if inv == nil {
			return next(ctx)
		}
		dec, err := pc.OnEvent(context.Background(), &proto.PolicyEvent{
			Kind:          policyEventExecute,
			Tool:          inv.ToolName,
			ArgumentsJson: inv.ArgumentsJSON,
			Session:       inv.SessionID,
		})
		if err != nil {
			m.logger.Warn("policy execute forward failed; allowing", "policy", name, "tool", inv.ToolName, "err", err)
			return next(ctx)
		}
		if dec.GetAction() == policyDecisionDeny {
			reason := dec.GetReason()
			if reason == "" {
				reason = fmt.Sprintf("tool call denied by policy %s", name)
			}
			m.logger.Info("policy denied tool call at execute", "policy", name, "tool", inv.ToolName, "reason", reason)
			return errors.New(reason)
		}
		spec := dec.GetTimeout()
		if spec == nil || spec.GetIdleMs() <= 0 {
			return next(ctx)
		}
		base := ctx.Context
		if base == nil {
			base = context.Background()
		}
		// 后台任务不装空闲看门狗：它的语义是「已交给后台，模型可以干别的」，
		// 用「一段时间没输出」判死它既有害（构建/测试几十秒不吐字是常态），
		// 也和前台宽限期自相矛盾（那时它正被允许安静地跑）。终止只走 job_kill。
		if IsBackgroundJob(base) {
			m.logger.Info("idle watchdog skipped for background job", "policy", name, "tool", inv.ToolName)
			return next(ctx)
		}
		idle := time.Duration(spec.GetIdleMs()) * time.Millisecond
		execCtx, cancel := WithIdleDeadline(base, idle)
		defer cancel()
		ctx.Context = execCtx
		err = next(ctx)
		if err != nil && IdleDeadlineExceeded(execCtx) {
			msg := spec.GetMessage()
			if msg == "" {
				msg = fmt.Sprintf("tool call idle timeout (no activity for > %s)", idle)
			}
			m.logger.Warn("tool idle timeout", "policy", name, "tool", inv.ToolName, "idle", idle.String())
			err = errors.New(msg)
			inv.Err = err
		}
		return err
	}))
	off = append(off, m.events.OnWaterfall(EventToolPostExecute, func(ctx EventContext, next func(EventContext) error) error {
		inv, _ := ctx.Data.(*ToolInvocation)
		if inv == nil {
			return next(ctx)
		}
		runErr := next(ctx)
		// 失败亦转发（失败是权威观察：如读到不存在的路径须记录 confirmed absent
		// 以授权后续创建），随后原样上抛保持流水线错误语义。
		ev := &proto.PolicyEvent{
			Kind:          policyEventPostExecute,
			Tool:          inv.ToolName,
			ArgumentsJson: inv.ArgumentsJSON,
			Result:        sanitizeUTF8(inv.Result),
			Session:       inv.SessionID,
		}
		if inv.Err != nil {
			ev.Error = sanitizeUTF8(inv.Err.Error())
		} else if runErr != nil {
			ev.Error = sanitizeUTF8(runErr.Error())
		}
		dec, err := pc.OnEvent(context.Background(), ev)
		if err != nil {
			m.logger.Warn("policy post-execute forward failed", "policy", name, "tool", inv.ToolName, "err", err)
			return runErr
		}
		// advisory 上下文机械收集：不占决策槽，成功与失败（含被拦截）调用同样透传
		//（对齐 DSH additionalContexts 折叠到 block 与非 block 两种决策的语义）。
		if n := dec.GetNotice(); n != "" {
			inv.Notices = append(inv.Notices, n)
		}
		if dec.GetAction() == policyDecisionReplace && dec.GetResult() != "" {
			inv.Result = dec.GetResult()
		}
		return runErr
	}))
	return off
}

// isBackgroundRequest 检测工具参数 JSON 中是否声明 run_in_background: true。
// 对齐 DSH bash 的 run_in_background 参数——任何工具都可以声明后台运行。
func isBackgroundRequest(argsJSON json.RawMessage) bool {
	var p struct {
		RunInBackground bool `json:"run_in_background"`
	}
	if err := json.Unmarshal(argsJSON, &p); err != nil {
		return false
	}
	return p.RunInBackground
}

// startBackgroundTool 在 job 注册表中登记一个后台任务，异步执行完整工具流水线，
// 立即返回 job_id（对齐 DSH bash run_in_background）。
// 模型可用 job_output/job_list/job_kill 管理后台任务。
func (m *Manager) startBackgroundTool(ctx context.Context, toolName string, argsJSON json.RawMessage, caller string) (string, string, error) {
	jobID, err := m.startBackgroundJob(ctx, toolName, argsJSON, caller, nil)
	if err != nil {
		return "", "", err
	}
	result := fmt.Sprintf("Background job started: %s. Track it with job_output (job_id: %s). Stop with job_kill.", toolName, jobID)
	return result, "", nil
}

// startBackgroundJob 登记后台任务并返回 job id（不生成对模型的提示文本）。
// 显式 run_in_background 与自动后台（宽限期超时）共用这一条登记路径。
// sink 非空时把管道产生的结构化视图（ViewJson）记录进去——宽限期路径要在
// job 之外把视图原样交回调用方（否则 TUI 的工具卡会丢掉结构化视图）。
func (m *Manager) startBackgroundJob(ctx context.Context, toolName string, argsJSON json.RawMessage, caller string, sink *bgResult) (string, error) {
	label := toolName
	// 尝试从参数中提取 command 作为 label 更友好的展示
	var p struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(argsJSON, &p) == nil && p.Command != "" {
		label = fmt.Sprintf("%s: %s", toolName, truncateStr(p.Command, 60))
	}

	// 后台任务与当前回合解耦：调用方 ctx 在回合被中断（LLM 流错误、用户 Ctrl+C、
	// 会话切走）时会被取消，若直接沿用它，后台作业会被一起判为 failed
	// （"rpc error: code = Canceled desc = context canceled"），输出也随之丢失——
	// 而 `run_in_background` 的语义正是「脱离本轮继续跑」。这里复制归属信息
	//（会话、审批策略）到独立 ctx，生命周期交给 job 注册表（job_kill/进程退出）。
	jobCtx := detachJobContext(ctx)

	// 观察通道：能流式上报的工具（插件把自己的输出登记进 GetActivity）在这里被探测出
	// 尾部输出与最近活动时间，job_output 于是在任务落定前也能回答「它在说什么」。
	// 探测不到就是 nil——任务退回原来的 final-output 行为。
	tailFn, activityFn := m.jobProbes(toolName, ToolCallIDFromContext(ctx))

	jobID, err := m.jobs.Start(jobs.StartSpec{
		Kind:  toolName,
		Label: label,
		Owner: caller,
		Start: func() (jobs.JobHooks, error) {
			doneCh := make(chan jobs.JobOutcome, 1)
			// job_kill 需要能终止流水线；jobCtx 已脱离回合取消（detachJobContext），
			// 这里只叠加一层可取消性，终止来源仅限 job_kill。
			runCtx, cancel := context.WithCancel(jobCtx)

			var mu sync.Mutex
			cancelReason := ""

			go func() {
				defer cancel()
				// 异步执行完整流水线（pre-execute → execute → post-execute）
				result, view, err := m.executeBackgroundPipeline(runCtx, toolName, argsJSON)
				// 视图与管道错误交回宽限期路径（jobs.JobOutcome 只带输出文本）。
				sink.set(view, err)
				mu.Lock()
				reason := cancelReason
				mu.Unlock()
				if runCtx.Err() != nil {
					detail := "cancelled"
					if reason != "" {
						detail = reason
					}
					doneCh <- jobs.JobOutcome{Status: jobs.StatusKilled, Detail: detail, Output: result}
					return
				}
				// 终态由 settleBackgroundOutcome 统一决定：流水线报错即 failed，
				// 结果里的 [exit_code: N] 非 0 同样判 failed，让模型能从 [status: ...]
				// 直接看出脚本失败（命令非零退出不算 Go error）。
				status, detail := settleBackgroundOutcome(result, err)
				doneCh <- jobs.JobOutcome{Status: status, Detail: detail, Output: result}
			}()

			return jobs.JobHooks{
				Cancel: func(reason string) {
					mu.Lock()
					if cancelReason == "" {
						cancelReason = reason
					}
					mu.Unlock()
					cancel()
				},
				Done: doneCh,
				// 最终输出仍以 Done.Output 为权威；TailOutput 只服务「任务还没结束」的观察。
				ReadOutput: nil,
				TailOutput: tailFn,
				Activity:   activityFn,
			}, nil
		},
	})
	if err != nil {
		return "", fmt.Errorf("start background job: %w", err)
	}
	return jobID, nil
}

// exitCodeMarkRe 匹配工具结果尾部追加的退出码标记（Shell 工具的结果约定：
// 命令非零退出时在结果里附加 "[exit_code: N]"）。后台任务流水线只看 execute
// 返回的 Go error 来决定终态，而命令非零退出码不被视为 Go 错误（err 恒为 nil，
// 退出码只写进结果文本），故从这里把「命令失败」还原成任务失败状态——否则
// 模型看到 [status: completed] 却无法知道脚本其实以非 0 退出。正则只认 DSC
// 约定格式，不依赖任何插件名。
var exitCodeMarkRe = regexp.MustCompile(`\[exit_code:\s*(-?\d+)\]`)

// exitCodeFromResult 从工具结果文本提取首个 [exit_code: N] 标记的整数；未找到返回 0。
func exitCodeFromResult(result string) int {
	if m := exitCodeMarkRe.FindStringSubmatch(result); len(m) == 2 {
		if n, err := strconv.Atoi(m[1]); err == nil {
			return n
		}
	}
	return 0
}

// settleBackgroundOutcome 决定后台任务的终态：流水线自身报错（如超时裁决上抛）即
// failed；否则从结果文本的 [exit_code: N] 标记还原命令退出码——N 非 0 也判 failed，
// 让模型直接由 [status: ...] 看出脚本失败。同步路径语义不变：命令失败不视为工具
// error，结果文本仍携退出码，模型可自行判断；这里只补「状态机可见性」这一跳。
func settleBackgroundOutcome(result string, runErr error) (jobs.JobStatus, string) {
	if runErr != nil {
		return jobs.StatusFailed, runErr.Error()
	}
	if code := exitCodeFromResult(result); code != 0 {
		return jobs.StatusFailed, fmt.Sprintf("exit code: %d", code)
	}
	return jobs.StatusCompleted, ""
}

// detachJobContext 把后台任务从「回合 ctx」解绑：保留归属信息（会话 id、审批策略），
// 但不继承取消信号。回合被中断（LLM 流错误 / 用户中断 / 会话切走）时，若后台任务
// 仍挂在同一 ctx 上，会被判 failed 且输出丢失——而 run_in_background 的语义正是
// 脱离本轮继续跑，终止只能经 job_kill 或进程退出。
func detachJobContext(ctx context.Context) context.Context {
	dctx := context.Background()
	if c := CallerFromContext(ctx); c != "" {
		dctx = WithCaller(dctx, c)
	}
	if pol := ApprovalPolicyFromContext(ctx); pol != "" {
		dctx = WithApprovalPolicy(dctx, pol)
	}
	// 工具调用 id 必须一起带过去。
	//
	// 它是插件侧活跃上报的键：sdk 用 CallInfo.ToolCallID 把「这次调用还在动」记进
	// GetActivity 的表里，宿主的前台宽限期轮询的正是这个键。此前这里只复制了会话与
	// 审批策略，id 被丢成空串 → 插件把活动记在 "" 上（recordActivity 直接 return）→
	// 宿主永远读到 0 → 任何长命令都在固定预算（30s）后被判 "no output" 转后台，
	// 哪怕它一直在打印。这正是「几乎所有长命令都被无谓转后台」的根因。
	if id := ToolCallIDFromContext(ctx); id != "" {
		dctx = WithToolCallID(dctx, id)
	}
	// 打上后台任务标记：装空闲看门狗的一律跳过（见 background_ctx.go）。
	// 后台任务已经交出去了，用「无输出」判死它既有害也自相矛盾——几十秒不吐字的
	// 构建/测试是常态，而前台等待阶段本来就是允许它安静跑着的。
	return WithBackgroundJob(dctx)
}

// executeBackgroundPipeline 在后台执行完整工具流水线（不含 run_in_background 检测，
// 避免递归）。供 startBackgroundTool 的 goroutine 调用。
func (m *Manager) executeBackgroundPipeline(ctx context.Context, toolName string, argsJSON json.RawMessage) (string, string, error) {
	inv := &ToolInvocation{ToolName: toolName, ArgumentsJSON: string(argsJSON), SessionID: CallerFromContext(ctx), ApprovalPolicy: ApprovalPolicyFromContext(ctx)}

	runErr := m.events.Waterfall(EventToolPreExecute, EventContext{Data: inv, Context: ctx}, func(EventContext) error {
		if veto := m.runPluginBeforeTool(ctx, inv); veto != nil {
			inv.Err = veto
			return veto
		}
		return inv.Err
	})
	if inv.Err == nil && runErr != nil {
		inv.Err = runErr
	}

	if inv.Err == nil {
		execErr := m.events.Waterfall(EventToolExecute, EventContext{Data: inv, Context: ctx}, func(evtCtx EventContext) error {
			// 执行 ctx 从 EventContext 回读：policy 桥的超时裁决可换装
			// 活跃续命执行域，未换装时保持调用方原始 ctx。
			execCtx := evtCtx.Context
			if execCtx == nil {
				execCtx = ctx
			}
			return m.executeToolBody(execCtx, inv, toolName)
		})
		if inv.Err == nil && execErr != nil {
			inv.Err = execErr
		}
	}

	if err := m.events.Waterfall(EventToolPostExecute, EventContext{Data: inv}, func(EventContext) error {
		m.runPluginAfterTool(ctx, inv)
		return inv.Err
	}); err != nil {
		m.emitToolResult(inv)
		return "", "", err
	}

	m.emitToolResult(inv)
	return inv.Result, inv.ViewJSON, inv.Err
}

// truncateStr 截断字符串到 max 字节（用于 job label 展示）。
func truncateStr(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}
