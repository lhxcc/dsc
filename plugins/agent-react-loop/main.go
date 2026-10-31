package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"dsc-sdk"

	"dsc/proto"
	"dsc/session"

	"github.com/hashicorp/go-hclog"
	"google.golang.org/grpc"
)

// toolMetadataRPCTimeout ListContext / ListTools 等 tool-client 元数据 RPC 超时
// （默认 3 秒，超时即跳过该次元数据刷新，不阻塞主循环）。
//
// 压缩（阈值、保留比例、摘要提示词、请求预算）都不在这里：agent 不实现压缩，
// 那些参数属于声明 Provides compaction 的压缩后端（各自读自己的 env）。
var (
	toolMetadataRPCTimeout = 3 * time.Second
)

type ReactLoopAgent struct {
	// logger 本地 hclog（写 os.Stderr，宿主经 SyncStderr 捕获入日志流，受 -log 门控）。
	// 供 runLoop 等记录运行日志，避免散落 fmt.Fprintf(os.Stderr, …) 直写。
	logger hclog.Logger

	broker        *dsc.AgentBroker // SDK 隔离封装的宿主 broker（Dial 宿主 LLM/Tool/UserQuestions）
	llmServiceID  uint32
	toolServiceID uint32
	mu            sync.Mutex // 保護 serviceID

	// 快取的服務連接（broker.Dial 為一次性握手，需跨 Run 重用）
	connMu     sync.Mutex
	llmConn    *grpc.ClientConn
	toolConn   *grpc.ClientConn
	llmClient  proto.LLMServiceClient
	toolClient proto.ToolServiceClient

	// 用户评审通道（宿主挂载的 UserQuestionsService）：exit_plan_mode 等工具
	// 向宿主询问用户并等待回答；serviceID 由宿主经 SetUserQuestionsService 注入。
	uqServiceID uint32
	uqMu        sync.Mutex
	uqConn      *grpc.ClientConn
	uqClient    proto.UserQuestionsServiceClient

	// 新增字段
	cancelFunc context.CancelFunc // 用於取消當前 Run
	runWg      sync.WaitGroup     // 等待 Run 完成
	shutdownMu sync.Mutex         // 保護關閉狀態
	isShutdown bool

	// 多輪對話記憶：跨 Run 保留會話歷史（事件溯源日志）
	sessMu      sync.Mutex
	sess        *session.Session
	turnCounter int // 轮次编号（跨 Run 递增，对齐 DSH 的 turn 概念）

	// 运行中注入计数：TUI 在模型输出期间经 InjectMessage 实时注入的新用户消息数
	// （sessMu 保护）。runLoop 每轮派生请求历史时快照消费；模型无工具调用收尾前若发现
	// 快照后有新注入，说明存在尚未发送给模型的新消息，须继续下一轮而不是结束本轮。
	pendingInjects int

	// runActive 表示当前是否有 runLoop 正在消费会话（sessMu 保护）。
	// InjectMessage 只在此为真时才有意义：消息写进会话表面后，必须有循环在
	// 下一次派生请求历史时把它发给模型。若循环已退出（本轮收尾与 TUI 收到
	// done 帧之间的窗口、或 TUI 状态判断失误），静默追加只会让这条消息沉睡——
	// 直到用户下次说话才被顺带发出。此时宁可返回错误，让调用方走「唤醒开新轮」，
	// 也不要制造「消息已发出、工作却没发生」的假象。
	runActive bool

	// 多会话事件日志存储（DSC_SESSION_DIR，缺省 ./sessions）；固定使用 default 会话
	store *session.Store

	// 上下文容量管理（由宿主透過 DSC_CONTEXT_WINDOW 傳入）
	contextWindow    int        // 總容量（token 數），0 表示未設置（不做自動壓縮）
	lastPromptTokens int32      // 最近一次 LLM 請求的 prompt token 數 ≈ 當前已用容量
	lastUsage        *dsc.Usage // 最近一次 LLM 請求的完整 usage 信息
	// usageMu 保護 lastPromptTokens/lastUsage：串流 loop 喺唔持 sessMu 時寫入，
	// 而 DebugSnapshot（另一 goroutine）要讀 lastPromptTokens，故用專鎖避免跨 goroutine
	// 競爭。
	usageMu sync.Mutex

	// 沙箱策略（宿主經 DSC_SANDBOX_POLICY 傳入，缺省 workspace-write）：渲染进
	// system prompt 的 sandbox:policy 上下文片段（对齐 DSH），让模型知道当前文件
	// 策略与工作区真实根路径，避免臆造不存在的 /workspace 虚拟路径而陷入死循环。
	sandboxPolicy string

	// 审批策略（宿主演 DSC_APPROVAL_POLICY 傳入，缺省 ask）：渲染进 system prompt 的
	// approval:policy 上下文片段。每 Run 从会话事件日志折疊（approval/policy，resume/fork
	// 后恢復）；无事件時回退部署缺省。审批门 enforcement 由宿主按会话解析，二者经
	// /approval → 宿主广播 → 本 agent 落会话日志保持同步。
	approvalPolicy string

	// 历史注入条数上限，遵循 DSC 三值语义（与 DSH 相反，勿按 DSH 读）：
	// 负值 = 不注入从存档恢复的历史；0 = 未定义（未设置，缺省，按默认 = 不限制，
	// 恢复历史全部注入）；N>0 = 只注入恢复历史中最近 N 条。
	// 会话级设置以 log-only history/limit 事件持久化（每次 Run 折叠还原），
	// 无事件时退回 DSC_HISTORY_INJECTION 环境变量默认。用于控制本地模型预填充长度。
	historyInjection int

	// contextSearchSeen 本会话是否已出现过「历史装不进窗口」。
	//
	// 一旦成立就保持常开：会话检索工具（core/context_search_tool.go）只在滑窗真的
	// 裁掉了历史时才有增量价值，而工具目录是提示缓存前缀的一部分——每步增删会让
	// 整段前缀缓存失效。条件本身是单调的（历史只增不减），所以最多只变一次。
	contextSearchSeen bool

	// lastContextTokens 本步请求的上下文占用投影（core.TokenMeter 的 Project 结果）。
	// 随 usage 帧（Usage.ContextTokens）上报给宿主：宿主侧计量实例据此对外发布，
	// TUI 显示的就是这个数——与宿主的滑窗/压缩决策同源，不再另算一套。
	lastContextTokens int

	// meter 上下文占用的唯一计量（dsc.TokenMeter）：实测锚点 + 启发式增量。
	// 「已用」上报与宿主决策读同一个口径，不得各自再用估算另算一遍。
	meter *dsc.TokenMeter

	// 記錄上次的工具名稱列表，用於檢測模式是否切換
	lastToolNames []string
	// 標記是否需要重置 system prompt（例如模式切換導致工具上下線時）
	sysPromptNeedsUpdate bool

	// 完整 system prompt（基礎指令 + 各工具插件貢獻的上下文片段，如技能索引），
	// 首次對話時構建一次，上下文壓縮後沿用，避免技能索引丢失。
	// 同时作为"上次渲染的 prompt"被 commitSystemPrompt 比较 surface 当前
	// system/message 节点，差异即 emit 一条 SystemMessage 事件（append 或
	// replace node 0）——system prompt 进入事件日志，对齐 DSH v3
	// system/message surface node 0 设计。
	sysPrompt string

	// 單輪模式（-input 自動化測試入口使用）：代理循環僅執行一次，
	// 完成一輪（含工具調用）後自然結束，方便測試後程序自動退出
	singleTurn bool

	// 正常模式下 agent 循環的輪數上限；0 = 不設限（默認，面向長程任務）。
	// 僅當宿主通過 DSC_MAX_ITERATIONS 顯式設置大於 0 的值時才作為上限；
	// 模型不調用工具時循環自然結束，設限僅用於需要控制失控場景。
	maxIterations int

	// persona "你是一個…助手" 身份句，由宿主透過 DSC_PRESET_PERSONA 傳入（預設可配）；
	// 空則回退通用默认（详见 buildSystemPrompt 中的 fallback）
	persona string

	// plan/goal 宿主工具状态（对齐 DSH plan-mode + goal 领域）：
	// planSection 为 plan 模式激活时注入 system prompt 的部署方引导文案（DSC_PLAN_SECTION，
	// 缺省 DSH 示例文案）；planActive 为当前会话的 plan 模式（每次 Run 从事件日志折叠）。
	// goalActivation 是进程本地续行启用状态（绝不持久化：恢复/fork 后停用，需显式 resume
	// 重新启用）；goalRounds 为已准入 Goal Round 数（v1 恒 0，jobs/workflow 落地后推进）。
	planSection                   string
	defaultMaxGoalRounds          int
	blockedAfterConsecutiveRounds int
	planActive                    bool
	goalActivation                bool
	goalRounds                    int

	// todo 任务清单部署配置（对齐 DSH allowParallelInProgress 开关）：
	// todoAllowParallel 为 true 时允许多个任务同时 in_progress（DSC_TODO_ALLOW_PARALLEL，
	// 缺省 false 强制单活跃项纪律）。
	todoAllowParallel bool
	// todoNudges 当前轮已用 TODO 追问次数；truncContinues 当前轮已用截断续行
	// 次数。追问不设预算（用户裁定：待办清单非空就持续追问直到处理完，
	// 修半途收尾留尾巴）；截断续行仍是预算制（每次 turn/start 重置，
	// maxTruncContinuesPerTurn 次）——防退化复读机无限烧 token。
	todoNudges     int
	truncContinues int
}

// 收尾驱动器预算（每次 turn/start 重置）。
//
// 截断续行：finish_reason=max_tokens/length 且无工具调用时，注入「从中断处
// 继续」用户消息再入循环——截断不弃任务（实测 33.8 万 ms 生成 6144 词元的
// 架构报告被端点默认上限拦腰截断后整轮静默收尾，用户侧表现为任务
// 中断）。上限 2 次：正常长报告一次续行即可完成；退化重复（模型复读机）
// 最多白烧 2 段即止损。
//
// TODO 追问：模型收尾（无论自然 end_turn 还是截断）而待办仍有未完成项时，
// 注入追问驱动模型继续——待办清单从「被动展示」升级为「主动监督」（用户
// 裁定：清单非空就持续追问直到逐项处理完，完成或取消；不设次数上限，
// 模型始终有出路：把各项标记 completed 或重写清单移除失效项即可自然收尾）。
// 连续追问达 todoNudgeRepeatWarnThreshold 次升级 Warn 留痕（可观测空转，
// 不熔断——硬熔断会让待办再次沦为无人过问的界面摆设）。
const (
	maxTruncContinuesPerTurn     = 2
	todoNudgeRepeatWarnThreshold = 5
)

func (a *ReactLoopAgent) RegisterServices(ctx context.Context, llmServiceID, toolServiceID uint32) error {
	a.mu.Lock()
	a.llmServiceID = llmServiceID
	a.toolServiceID = toolServiceID
	a.mu.Unlock()

	// 兩個 serviceID 一次性就緒後，立即建立連接（在 broker 的 5 秒超時前）
	if llmServiceID != 0 && toolServiceID != 0 {
		dsc.SafeGoroutine(func() {
			_, _, err := a.ensureConnected(llmServiceID, toolServiceID)
			if err != nil {
				fmt.Printf("[Agent Loop] eager connect failed: %v\n", err)
			} else {
				fmt.Printf("[Agent Loop] eager connect successful\n")
			}
		})
	}
	return nil
}

func (a *ReactLoopAgent) Run(ctx context.Context, input string, images []string) (*dsc.AgentResult, error) {
	return a.runLoop(ctx, input, images, nil)
}

// RunStream 以流式方式执行循环：LLM 文本增量、工具调用提示以帧的形式发送到通道，关闭表示结束
func (a *ReactLoopAgent) RunStream(ctx context.Context, input string, images []string) (<-chan *dsc.RunStreamResponse, error) {
	ch := make(chan *dsc.RunStreamResponse)
	dsc.SafeGoroutine(func() {
		defer close(ch)
		var emittedErr bool
		_, err := a.runLoop(ctx, input, images, func(item *dsc.RunStreamResponse) {
			if item.Status == "error" {
				emittedErr = true
			}
			ch <- item
		})
		// runLoop 在早期失败（serviceID 未设置 / 连接失败等）时不会 emit 錯誤幀，
		// 這裡補發一幀，避免錯誤被吞掉導致 TUI 靜默無響應
		if err != nil && !emittedErr {
			ch <- &dsc.RunStreamResponse{Status: "error", Error: err.Error()}
		}
	})
	return ch, nil
}

// runLoop 是 Agent 的核心循环；emit 非空时输出流式帧（文本增量 / 工具提示 / 结束状态），否则保持非流式
func (a *ReactLoopAgent) runLoop(ctx context.Context, input string, images []string, emit func(*dsc.RunStreamResponse)) (*dsc.AgentResult, error) {
	// 创建一个可取消的 context，保存 cancelFunc
	ctx, cancel := context.WithCancel(ctx)
	a.sessMu.Lock()
	a.runActive = true
	a.sessMu.Unlock()
	a.mu.Lock()
	// 如果已有 cancelFunc，先取消（防止并发 Run）
	if a.cancelFunc != nil {
		a.cancelFunc()
	}
	a.cancelFunc = cancel
	a.mu.Unlock()

	defer func() {
		a.sessMu.Lock()
		a.runActive = false
		a.sessMu.Unlock()
		a.mu.Lock()
		a.cancelFunc = nil
		a.mu.Unlock()
		a.runWg.Done() // 标记 Run 结束
	}()
	a.runWg.Add(1)

	fmt.Printf("[Agent Loop] Starting turn with input: %s\n", input)

	a.mu.Lock()
	llmID := a.llmServiceID
	toolID := a.toolServiceID
	a.mu.Unlock()

	if llmID == 0 || toolID == 0 {
		return nil, fmt.Errorf("service IDs not set, call RegisterServices first")
	}

	// 獲取（或首次建立）LLM 與 Tool 服務連接
	llmClient, toolClient, err := a.ensureConnected(llmID, toolID)
	if err != nil {
		return nil, err
	}

	// 在循环外获取工具列表（一次即可）
	listToolsResp, err := toolClient.ListTools(ctx, &proto.ListToolsRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}
	availableTools := listToolsResp.Tools

	// 提取當前工具名稱列表
	currentToolNames := make([]string, len(availableTools))
	for i, t := range availableTools {
		currentToolNames[i] = t.Name
	}
	// 對工具名稱進行排序，確保比較時不因順序差異而誤判
	sort.Strings(currentToolNames)

	// 檢測工具列表是否變化
	sysPromptChanged := false
	if len(a.lastToolNames) == 0 {
		// 首次初始化
		a.lastToolNames = currentToolNames
	} else {
		// 比較工具列表是否變化（長度不同或內容不同）
		if len(a.lastToolNames) != len(currentToolNames) {
			sysPromptChanged = true
		} else {
			for i, name := range currentToolNames {
				if i >= len(a.lastToolNames) || a.lastToolNames[i] != name {
					sysPromptChanged = true
					break
				}
			}
		}

		if sysPromptChanged {
			a.lastToolNames = currentToolNames
		}
	}

	// 事件溯源会话：首轮从磁盘恢复或新建（多会话 Store，默认会话按当前工作区
	// 项目命名——同项目跨时期共享历史、不同项目隔离，不再硬编码 default.jsonl），
	// 并构建完整 system prompt；工具列表变化时重建 system prompt。
	// 历史以事件追加进 session（对齐 DSH：模型可见即已记录），不再维护独立消息数组。
	a.sessMu.Lock()
	if a.sess == nil {
		projectKey := dsc.ProjectSessionKey()
		restored, err := a.store.Ensure(projectKey)
		if err != nil {
			a.sessMu.Unlock()
			return nil, fmt.Errorf("failed to restore session: %w", err)
		}
		if restored.Len() > 0 {
			a.turnCounter = restored.LastTurn()
			fmt.Printf("[Agent Loop] session restored from store (%d events, last turn %d)\n",
				restored.Len(), a.turnCounter)
		}
		a.sess = restored
		// 重启恢复：把上次成功请求的用量锚点也恢复出来。否则首发请求没有服务端
		// usage 可依，TokenMeter 会退回「全量派生估算」——把模型根本看不到的、已被
		// 压缩后端遮蔽的历史也算进「已用」，标题虚高，还可能误判超窗/误触发压缩。
		// 用锚点后 Project 退化为该实测值，与上次请求的读数一致。
		a.seedUsageFromHistory(restored)
		a.goalActivation = false // 恢复/切换后停用续行（对齐 DSH session-start disarm）
		a.sysPrompt = a.buildSystemPrompt(ctx, toolClient)
		// resume 后 surface 已含上次提交的 system/message 节点（v3）；
		// ProjectSystemPrompt 比对后通常 no-op（内容未变）——若 plugin 状态
		// 变更致 prompt 漂移，则 replace node 0。
		a.commitSystemPrompt(a.turnCounter, 0)
	} else if sysPromptChanged || a.sysPromptNeedsUpdate {
		a.sysPrompt = a.buildSystemPrompt(ctx, toolClient)
		a.sysPromptNeedsUpdate = false
		a.commitSystemPrompt(a.turnCounter, 0)
	}
	// plan 模式每次 Run 从事件日志折叠（SetPlanMode/exit_plan_mode 已提交的变更即时生效）
	a.planActive = session.FoldPlanMode(a.sess.Events())
	// 审批策略每次 Run 从事件日志折叠（/approval 经宿主广播落会话，resume/fork 恢复）；
	// 无事件时保留 newAgent 读入的 DSC_APPROVAL_POLICY 部署缺省（默认 ask）。
	if p := session.FoldApprovalPolicy(a.sess.Events()); p != "" {
		a.approvalPolicy = p
	}
	// 历史注入条数每次 Run 从事件日志折叠（/settings history 已提交的变更即时生效）；
	// 无记录时保留 newAgent 读入的 DSC_HISTORY_INJECTION 部署默认（缺省 0 = 未定义
	// → 不限制，即恢复历史全部注入）。
	if limit, found := session.FoldHistoryLimit(a.sess.Events()); found {
		a.historyInjection = limit
	}
	a.turnCounter++
	turnNo := a.turnCounter
	sess := a.sess
	a.sessMu.Unlock()

	// 宿主托管的工具（plan/goal + ask_user_question + todo_write）追加进模型可见目录（执行时拦截，见下方工具循环）
	availableTools = append(availableTools, a.hostTools()...)

	// 每次 Run 结束（含错误路径）将事件日志落盘（多会话 Store）
	defer func() {
		if err := a.store.Save(sess); err != nil {
			fmt.Printf("[Agent Loop] failed to save session: %v\n", err)
		}
	}()

	// 轮次与用户输入作为会话事件记录（turn/start 为 log-only，user/message 进入 surface）
	sess.Append(session.TurnStart, &session.TurnData{Turn: turnNo}, nil)
	a.todoNudges = 0     // 每轮重置追问计数
	a.truncContinues = 0 // 每轮重置截断续行预算
	sess.Append(session.UserMessage, &session.UserMessageData{Content: input, Source: "user", Images: images}, &session.SurfaceOp{Op: session.SurfaceAppend})

	// cancelLoop 返回被取消的结果
	cancelResult := func() (*dsc.AgentResult, error) {
		return &dsc.AgentResult{
			Output: "Agent canceled",
			Status: "error",
		}, ctx.Err()
	}

	// 迭代上限只来自显式预算（DSC_MAX_ITERATIONS）；默认 0 = 不设限。
	//
	// 这里曾在单轮模式（-input / -headless）下把上限硬设成 1，本意是「自动化跑完一轮
	// 就能自然退出」，实际含义却是**只发一次 LLM 请求**：模型一旦调用工具，这一轮就在
	// 工具结果之后结束，再没有第二次请求去用结果作答。表现为 `dsc -input` 永远只打印
	// 工具卡片、没有答案，多步流程（如 MCP connect→list→load→call）根本无法验证。
	//
	// 「单轮」该约束的是**不自动续行**（goal 轮、待办追问、注入续行、截断续写都按
	// singleTurn 关掉了），而不是不许多步；循环在模型不再调用工具时自然收尾。
	maxIterations := a.maxIterations
	stepNo := 0
	// 回合闭合不变量（对齐 DSH：turn/end 总以某 reason 闭合 turn/start，step 同理）：
	// 正常收尾路径（completed/goal-concluded/max-iterations/单轮成败）自行闭合并
	// 置 turnOpen=false；异常（LLM/压缩失败等）与取消路径由 defer 兜底闭合
	//（reason=error/cancelled），杜绝日志中残留悬空 turn/start / step/start——
	// 排障时 start/end 恒可配对（本 defer 注册晚于落盘 defer，先于其执行）。
	turnOpen := true
	stepOpen := false
	defer func() {
		if stepOpen {
			sess.Append(session.StepEnd, &session.StepData{Turn: turnNo, Step: stepNo}, nil)
		}
		if turnOpen {
			reason := "error"
			if ctx.Err() != nil {
				reason = "cancelled"
			}
			sess.Append(session.TurnEnd, &session.TurnData{Turn: turnNo, Reason: reason}, nil)
		}
	}()
	// 包装 emit：为每帧自动填充对齐 DSH 的轮/步编号（轮=一次受理输入的排空，
	// 步=一次模型请求及其引发的工具执行），供 TUI 状态行实时显示当前进度。
	// 闭包按引用捕获 turnNo/stepNo，故每次发射都取当前步。
	if emit != nil {
		baseEmit := emit
		emit = func(f *dsc.RunStreamResponse) {
			f.Turn = int32(turnNo)
			f.Step = int32(stepNo)
			baseEmit(f)
		}
		// 待办投影帧（对齐 DSH FoldTodos）：turn/start 使上一轮计划失效，
		// 通知 TUI 清空待办面板；面板内容随后由 todo_write 成功结果帧驱动。
		// 无需 /todo 手动清理，也不依赖模型主动清空——新一轮即自动让位。
		emit(&dsc.RunStreamResponse{Status: "todo"})
	}
	// maxIterations == 0 表示不設上限，僅靠 ctx 取消（用戶 Ctrl+C / Shutdown）與模型自然收尾結束
	// pre-step 会话归属追踪：firstRequest 标记回合首个请求（开场输入必为新用户
	// 输入）；seenInjects 为上一请求派生时的注入计数快照（增量 = 运行中插话）。
	firstRequest := true
	seenInjects := 0
	for i := 0; maxIterations == 0 || i < maxIterations; i++ {
		// 检查 ctx.Done()
		select {
		case <-ctx.Done():
			return cancelResult()
		default:
		}

		stepNo++
		// 步骤边界（log-only）
		sess.Append(session.StepStart, &session.StepData{Turn: turnNo, Step: stepNo}, nil)
		stepOpen = true

		// Per-step system prompt reconciliation（对齐 DSH agent.ts step()）：
		// 每步 LLM 请求前重建 system prompt 并提交到事件日志——
		// 检测 plugin 贡献的上下文片段变化（ListContext RPC）、plan 模式
		// 切换、工具列表变化等。ProjectSystemPrompt 比对 surface 当前
		// system/message 节点内容，相同则 no-op（不产生事件），不同则
		// replace node 0。保证事件日志是 system prompt 变更的唯一事实源，
		// 模型派生消息始终从 surface node 0 派生当前生效的 prompt。
		a.sysPrompt = a.buildSystemPrompt(ctx, toolClient)
		a.commitSystemPrompt(turnNo, stepNo)

		// 请求历史由会话 surface 派生（system prompt 前置），不再依赖独立消息数组。
		// 派生与注入计数快照在同一 sessMu 临界区内完成（与 InjectMessage 互斥），
		// 保证「已发送给模型的消息」与「已消费的注入」原子一致，避免注入恰好落在
		// 两者之间被漏检。
		a.sessMu.Lock()
		msgs := a.deriveMessages(sess)
		iterPendingInjects := a.pendingInjects
		a.sessMu.Unlock()

		// 请求历史由会话 surface 派生（system prompt 前置），**原样下发**。
		//
		// 这里不做任何窗口手术：裁剪、折叠、摘要化压缩都是「改写请求面」，属于压缩岗位
		// （声明 Provides compaction 的后端）的职责；agent 只负责循环与调度。历史装不进
		// 窗口时，由宿主在请求发出前统一量表面并按需滑窗（core/context_ceiling.go）；
		// provider 报超窗时同样由宿主滑窗后重试（core/overflow_adjust.go）。
		//
		// 为什么不能让 agent 顺手裁一刀（与消息身份无关——身份早已是宿主下发的稳定 id，
		// 裁掉头部不会再让压缩块指错消息）：
		//
		//  1. 职责：改写请求面是压缩后端的职责，窗口适配是宿主收口层的职责（它看得到
		//     改写之后的表面）。agent 只调度，两边都不该由它代劳。
		//  2. 观测点错位：这里拿到的是**改写前**的列表，可能几倍于窗口；真正上线的是
		//     后端渲染后的视图或宿主滑窗后的视图。拿前者决定丢什么，等于在后端改写之前
		//     替他扔材料——扔掉的正是它可能要用/要保留的那一段。
		//  3. 压缩是增量的：dsc-system 按「已压前缀的 sha256 指纹」复用摘要，billion-context
		//     的块覆盖与保留区也建立在它见过的历史上。喂它一份每步都被裁过的列表，前缀
		//     每步变、指纹每次失配 → 每步重算压缩，视图与请求面在相邻两步之间摆动、
		//     前缀缓存反复失效（早期实测 169K / 26.5K 交替，那时还叠加着按位置寻址的错位；
		//     位置寻址已随稳定 id 消失，但「喂它残次历史就每步重算」这条至今成立）。
		//
		// 压缩能力是 agent 的强依赖（metadata 声明 requires/dsc/compaction）：没有当值的
		// 压缩后端时宿主不会激活 agent（启动即失败并说明怎么补），因此这里不存在、
		// 也不该有「没有后端时自己压」的分支。

		// 会话检索工具只在「历史已经装不进窗口」时才出现在目录里：整个历史都在眼前时，
		// 检索没有增量价值；而一旦滑窗开始裁历史，模型就失去了那部分内容的唯一入口。
		availableTools = a.filterContextSearch(availableTools, dsc.EstimateProtoMessagesTokens(msgs))

		// 「已用容量」只有一个来源：dsc.TokenMeter 的投影值（实测锚点 + 自锚点以来的
		// 启发式增量，对齐 DSH tokenMeter 的 projectedTokens）。
		meter := a.tokenMeter()
		meter.UpdateContextWindow(a.contextWindow)

		// 展示/上报的「已用」= 真正要发出的那份表面（消息 + 工具目录）的投影。
		// 宿主与 TUI 都读这个数，不另算一套。
		sentSurface := dsc.EstimateProtoMessagesTokens(msgs) + dsc.EstimateProtoToolTokens(availableTools)
		a.usageMu.Lock()
		a.lastContextTokens = meter.Project(sentSurface)
		a.usageMu.Unlock()

		// 压缩（改写请求面）不在这里做：它由当值的压缩后端在 agent/pre-step 事件里完成，
		// 宿主按能力（Provides compaction）找到唯一岗位持有者并把完整消息列表发给它。
		// agent 只调度，不实现压缩——否则换后端时行为会分裂成两套。

		// 稳定排序工具目录（按名称），消除无关的顺序差异导致请求前缀字节变化、
		// 命中前缀缓存失效（对齐 REX 的 normalizeToolSchemas；顺序不影响功能）。
		sort.Slice(availableTools, func(i, j int) bool {
			return availableTools[i].Name < availableTools[j].Name
		})

		// 调用 LLM（流式或非流式）；请求面图像预算卸载已外置 dsc-system
		// image-offload 驻留（agent/pre-step 瀑布改写，策略归还插件）——
		// 本循环零请求前预处理，消息列表按会话派生原样下发
		//
		// 会话归属与新用户输入标记随请求透传（宿主在 agent/pre-step 事件载荷
		// 中转发，对齐 DSH agent/pre-step 的 inbox claim 语义）：回合开场输入
		// 与运行中注入都算「新用户输入」，goal/truncation/todo 等续行驱动消息
		// 不算——循环检测类插件据此归属会话并重置循环链（用户插话改变了
		// 上下文，跨插话的重复不是循环）。
		newUserInput := firstRequest || iterPendingInjects > seenInjects
		req := &proto.ChatRequest{
			Messages:     msgs,
			Tools:        availableTools,
			SessionId:    sess.ID(),
			NewUserInput: newUserInput,
		}
		seenInjects = iterPendingInjects
		firstRequest = false
		// 本次请求实际发出的**整体**估算（消息 + 工具目录）：服务端回报用量时与它配对
		// 写入计量锚点，两边必须同源同口径——provider 的 prompt 计数本身就含工具定义，
		// 只算消息会让锚点与后续增量不在同一尺度上。
		sentEstimate := dsc.EstimateProtoMessagesTokens(msgs) + dsc.EstimateProtoToolTokens(availableTools)
		// request/header（log-only）：把本次请求的**信封**落盘（工具目录指纹 + 模型 +
		// 窗口 + 当次估算），与结算的 llm/attempt 按 Turn/Step 配对，构成用量锚点。
		// 请求前写：失败请求也留有信封；宿主据此判断「下次能否沿用这次的用量」。
		// 模型名由宿主经 DSC_MODEL 注入（插件自己不解析 provider 配置）。
		sess.Append(session.RequestHeader, &session.RequestHeaderData{
			Turn: turnNo, Step: stepNo,
			ToolsHash:     dsc.ToolsFingerprint(availableTools),
			Model:         dsc.EnvStr("DSC_MODEL", ""),
			ContextWindow: a.contextWindow,
			SentEstimate:  sentEstimate,
		}, nil)
		// 步开始信号帧：在 LLM 请求真实发出前发射（emit 包装器自动携带
		// Turn/Step 编号），TUI 以此时刻测 TTFT/初速——覆盖 prompt 处理
		// 与排队等待；仅靠首个内容帧打点会把这部分首响等待低估为零。
		if emit != nil {
			emit(&dsc.RunStreamResponse{Status: "step_start"})
		}
		var content string
		var toolCalls []*proto.ToolCall
		var finishReason string
		// llm/attempt 结算留痕（对齐 DSH llm/* + assistant/attempt 的诊断定位）：
		// 无论成败，每次 LLM 调用结算时落一条 log-only 事件——finish_reason/usage/
		// 时长/错误/产出规模一眼可查，截断、provider 报错、上下文溢出、流中断
		// 从此无需审计代码。usage 仅流式协议回传，取本次调用帧内的值
		//（不复用上一步残留，避免误归属）。
		llmStart := time.Now()
		var attemptUsage *proto.Usage
		settleAttempt := func(finishReason string, llmErr error) {
			errMsg := ""
			if llmErr != nil {
				errMsg = llmErr.Error()
			}
			sess.Append(session.LLMAttempt, &session.LLMAttemptData{
				Turn: turnNo, Step: stepNo,
				Streaming:    emit != nil,
				FinishReason: finishReason,
				Usage:        attemptUsage,
				DurationMS:   time.Since(llmStart).Milliseconds(),
				Error:        errMsg,
				Code:         dsc.ClassifyLLMErrorCode(llmErr),
				ContentChars: len(content),
				ToolCalls:    len(toolCalls),
			}, nil)
		}
		if emit == nil {
			resp, err := llmClient.Chat(ctx, req)
			if err != nil {
				settleAttempt("", err)
				return nil, fmt.Errorf("LLM chat failed: %w", err)
			}
			content = resp.Content
			toolCalls = resp.ToolCalls
			finishReason = resp.FinishReason
		} else {
			s, err := llmClient.ChatStream(ctx, req)
			if err != nil {
				emit(&dsc.RunStreamResponse{Status: "error", Error: err.Error()})
				settleAttempt("", err)
				return nil, fmt.Errorf("LLM chat stream failed: %w", err)
			}
			for {
				cr, err := s.Recv()
				if err == io.EOF {
					// 流正常结束——记录日志便于排查「模型停止」问题
					a.logger.Info("stream ended", "turn", turnNo, "step", stepNo, "toolCalls", len(toolCalls), "contentLen", len(content))
					break
				}
				if err != nil {
					emit(&dsc.RunStreamResponse{Status: "error", Error: err.Error()})
					settleAttempt(finishReason, err)
					return nil, fmt.Errorf("LLM chat stream recv failed: %w", err)
				}
				if cr.Error != "" {
					emit(&dsc.RunStreamResponse{Status: "error", Error: cr.Error})
					settleAttempt(finishReason, fmt.Errorf("%s", cr.Error))
					return nil, fmt.Errorf("LLM stream error: %s", cr.Error)
				}
				// 記錄 prompt 用量（≈ 當前上下文已用容量）；該值在 finish 分片由服務端返回
				if cr.Usage != nil {
					a.usageMu.Lock()
					attemptUsage = cr.Usage
					a.lastPromptTokens = cr.Usage.PromptTokens
					a.lastUsage = dsc.UsageFromProto(cr.Usage)
					a.usageMu.Unlock()
					// 锚定上下文计量：实测 prompt 侧用量 + **本次实际发出**的消息估算。
					// 之后 Project(sentSurface) = 实测 + 增量，绝对误差被实测抵消。
					//
					// 不再因为「本步裁剪过」就跳过锚定。跳过会让 meter 永远拿不到增量
					// 基线（只有重启倒查来的一个旧实测值），而滑動窗口模式下每步都在裁剪
					// ——于是 Project 恒等于那个旧值，界面上的「已用」从首帧起就不动
					// （实测：会话涨到 730K，界面一直是 33K/3.29%）。
					//
					// 之所以以前担心「把差额算成增长」：基线取自裁剪后的列表，而投影输入
					// 是裁剪前的全量历史，两者不同源。现在投影输入统一改成裁剪后的表面
					// （见上面的 sentSurface），基线与投影同源，增量才有意义。
					a.tokenMeter().SampleUsage(dsc.PromptSideTokens(dsc.UsageFromProto(cr.Usage)), sentEstimate)
				}
				content += cr.Content
				if cr.Content != "" {
					emit(&dsc.RunStreamResponse{Output: cr.Content, Status: "streaming"})
				}
				if cr.Reasoning != "" {
					emit(&dsc.RunStreamResponse{Reasoning: cr.Reasoning, Status: "reasoning"})
				}
				// 原始流分片记入会话（log-only：回放/UI 保真，不参与派生历史）
				if cr.Content != "" || cr.Reasoning != "" {
					sess.Append(session.AssistantChunk, &session.AssistantChunkData{
						Turn: turnNo, Step: stepNo, Content: cr.Content, Reasoning: cr.Reasoning,
					}, nil)
				}
				if len(cr.ToolCalls) > 0 {
					toolCalls = cr.ToolCalls
				}
				// 捕获 finish_reason：截断（max_tokens/length）此前全链路无人感知，
				// 截断响应被当普通完成收轮——本变量是后续截断防护的判定依据
				if cr.FinishReason != "" {
					finishReason = cr.FinishReason
				}
			}
		}
		settleAttempt(finishReason, nil)
		truncated := isTruncatedFinishReason(finishReason)
		if truncated {
			a.logger.Warn("response truncated", "finish_reason", finishReason, "turn", turnNo, "step", stepNo,
				"toolCalls", len(toolCalls),
				"hint", "若反复出现：检查 LLM 端点侧输出上限（llama.cpp n_predict / 服务默认）；或显式设 ANTHROPIC_MAX_OUTPUT_TOKENS / OPENAI_MAX_OUTPUT_TOKENS 放宽")
			// 截断告警帧：向 TUI 显式呈现截断事实。截断根因已确诊：
			// LLM 插件默认 max_tokens 已从源头移除；DSC 侧仅压缩路径主动携带
			// 窗口净余值，主路径不携带——残余截断来自端点侧：anthropic 兼容口把
			// max_tokens 视为 required，字段缺席时服务端自填保守默认（laamaafung
			// server-chat.cpp 填 4096）。宿主探测命中 LLAMACPP 家族端点时已注入
			// DSC_MAX_OUTPUT_TOKENS=窗口值对抗该默认；截断仍出现则多为主路径残余
			// 场景。收尾侧的预算内自动续行见下方 continuation drivers（截断不弃任务）。
			if emit != nil {
				emit(&dsc.RunStreamResponse{
					Output: fmt.Sprintf("\n[模型输出被截断: finish_reason=%s]\n", finishReason),
					Status: "tool",
				})
			}
		}

		// 组装后的助手消息记入会话（surface；携带 usage 与工具调用，对齐 DSH assistant/message）
		lastUsage := &proto.Usage{}
		if a.lastUsage != nil {
			lastUsage = &proto.Usage{
				PromptTokens:             a.lastUsage.PromptTokens,
				CompletionTokens:         a.lastUsage.CompletionTokens,
				TotalTokens:              a.lastUsage.TotalTokens,
				CacheReadInputTokens:     a.lastUsage.CacheReadInputTokens,
				CacheCreationInputTokens: a.lastUsage.CacheCreationInputTokens,
			}
		}
		sess.Append(session.AssistantMessage, &session.AssistantMessageData{
			Turn:      turnNo,
			Step:      stepNo,
			Content:   content,
			ToolCalls: toolCalls,
			Usage:     lastUsage,
		}, &session.SurfaceOp{Op: session.SurfaceAppend})
		// 增量落盘（对齐 DSH 事件日志即时持久化）：模型响应已发生即持久化，
		// 长轮次中途（工具阻塞/多步循环）崩溃或 /export 时不丢已发生的步
		if err := a.store.Save(sess); err != nil {
			a.logger.Warn("incremental session save failed", "turn", turnNo, "step", stepNo, "error", err)
		}

		// 没有工具调用 → 目标续行驱动器检查（对齐 DSH goal-round-driver）：
		// goal active+armed+预算未耗尽时，准入下一轮 goal-round 用户消息并继续循环。
		// 单轮模式（-input 自动化）不自动续行；人类消息不消耗预算。
		if len(toolCalls) == 0 {
			a.logger.Info("no tool calls, checking continuation drivers", "turn", turnNo, "step", stepNo)
			if !a.singleTurn {
				if g := session.FoldGoal(sess.Events()); goalRoundDriver(g, a.goalActivation, a.goalRounds) {
					a.logger.Info("goal round", "round", a.goalRounds+1, "maxRounds", g.MaxGoalRounds, "turn", turnNo, "step", stepNo)
					a.goalRounds++
					sess.Append(session.UserMessage, &session.UserMessageData{
						Content: goalRoundPrompt(g, a.goalRounds),
						Source:  "goal_round",
					}, &session.SurfaceOp{Op: session.SurfaceAppend})
					if emit != nil {
						emit(&dsc.RunStreamResponse{
							Output: fmt.Sprintf("\n[Goal Round %d/%d]\n", a.goalRounds, g.MaxGoalRounds),
							Status: "tool",
						})
					}
					// 步闭合：续行驱动器开新步前收口当前步（防悬空 step/start）
					sess.Append(session.StepEnd, &session.StepData{Turn: turnNo, Step: stepNo}, nil)
					stepOpen = false
					continue
				}
				// 竞态修复：模型最后一次输出期间 TUI 实时注入了新用户消息（尚未进入本轮
				// 派生的请求历史）。若就此收尾，会出现「消息已发出但工作停止」：消息已写入
				// 会话 surface，却没有任何一轮再去发送给模型。检测到快照后有新注入则继续
				// 下一轮（消息已被下轮派生，不会重复触发，也不会死循环）。
				if a.hasNewInjectedSince(iterPendingInjects) {
					if emit != nil {
						emit(&dsc.RunStreamResponse{
							Output: "\n[已收到新消息，继续处理]\n",
							Status: "tool",
						})
					}
					// 步闭合：续行驱动器开新步前收口当前步（防悬空 step/start）
					sess.Append(session.StepEnd, &session.StepData{Turn: turnNo, Step: stepNo}, nil)
					stepOpen = false
					continue
				}
				// 截断续行（截断不弃任务）：无工具调用的截断响应此前直接收轮——
				// 实测 33.8 万 ms 生成的架构报告被 provider 默认上限拦腰截断后
				// 整轮静默收尾（用户侧表现为任务中断，待办 0/5 无人过问）。预算内
				// 注入「从中断处继续」用户消息再入循环；预算耗尽后放行收尾，
				// 防退化复读机无限烧 token。排在待办追问之前：先把当前输出续完，
				// 待办监督留给下一次收尾判定。
				if truncated && a.truncContinues < maxTruncContinuesPerTurn {
					a.truncContinues++
					contMsg := fmt.Sprintf("上一条回复因 finish_reason=%s 在输出中途被截断（非自然结束）。"+
						"请从中断处继续，不要重复已输出的内容；若确已无可补充，请明确说明。", finishReason)
					a.logger.Info("truncation continue", "count", a.truncContinues, "max", maxTruncContinuesPerTurn,
						"finish_reason", finishReason, "turn", turnNo, "step", stepNo)
					sess.Append(session.UserMessage, &session.UserMessageData{
						Content: contMsg,
						Source:  "truncation_continue",
					}, &session.SurfaceOp{Op: session.SurfaceAppend})
					if emit != nil {
						emit(&dsc.RunStreamResponse{
							Output: fmt.Sprintf("\n[输出被截断，已请求模型继续（%d/%d）]\n", a.truncContinues, maxTruncContinuesPerTurn),
							Status: "tool",
						})
					}
					// 步闭合：续行驱动器开新步前收口当前步（防悬空 step/start）
					sess.Append(session.StepEnd, &session.StepData{Turn: turnNo, Step: stepNo}, nil)
					stepOpen = false
					continue
				}
			}

			// TODO 未完成追问（对齐 DSH 的「模型自然停止后检查未完成 TODO」语义，
			// 用户裁定升级为主动监督且不设预算）：模型收尾（自然 end_turn 或截断
			// 预算耗尽）后，若 TODO 列表中仍有 pending/in_progress 项，系统自动追加
			// 一条用户消息追问，驱动模型逐项处理完（完成或取消）——只要清单非空
			// 就持续追问，杜绝「待办挂在界面上无人过问」。模型始终有出路：把各项
			// 标记 completed 或重写清单移除失效项即可自然收尾。仅非单轮模式触发；
			// 连续追问达 todoNudgeRepeatWarnThreshold 次升级 Warn（可观测空转，
			// 不熔断——硬熔断会让待办重新变成无人过问的界面摆设；用户可随时中断）。
			if !a.singleTurn {
				todos := session.FoldTodos(sess.Events())
				if hasPendingTodos(todos) {
					a.todoNudges++
					if a.todoNudges >= todoNudgeRepeatWarnThreshold {
						a.logger.Warn("todo nudge repeated", "count", a.todoNudges,
							"turn", turnNo, "step", stepNo,
							"hint", "待办持续未清空且模型反复自然收尾，疑似空转；如长时间无进展可中断后检查")
					} else {
						a.logger.Info("todo nudge", "count", a.todoNudges,
							"turn", turnNo, "step", stepNo)
					}
					nudgeMsg := buildTodoNudgePrompt(todos)
					sess.Append(session.UserMessage, &session.UserMessageData{
						Content: nudgeMsg,
						Source:  "todo_nudge",
					}, &session.SurfaceOp{Op: session.SurfaceAppend})
					if emit != nil {
						emit(&dsc.RunStreamResponse{
							Output: nudgeMsg + "\n",
							Status: "tool",
						})
					}
					// 步闭合：续行驱动器开新步前收口当前步（防悬空 step/start）
					sess.Append(session.StepEnd, &session.StepData{Turn: turnNo, Step: stepNo}, nil)
					stepOpen = false
					continue
				}
			}

			a.logger.Info("turn completed", "turn", turnNo, "steps", stepNo, "reason", "completed")
			sess.Append(session.TurnEnd, &session.TurnData{Turn: turnNo, Reason: "completed"}, nil)
			turnOpen = false
			if emit != nil {
				// success 幀攜帶當前已用容量，供 TUI 標題欄顯示「已用/總容量」。
				// 统一走 usageSnapshot：上下文投影（ContextTokens）只在那一处装配，
				// 否则三份字面量各写一遍，漏一个字段就是「标题口径与决策不一致」。
				emit(&dsc.RunStreamResponse{
					Status: "success",
					Usage:  a.usageSnapshot(),
				})
			}
			return &dsc.AgentResult{
				Output: content,
				Status: "success",
			}, nil
		}

		// 执行每个工具并追加结果
		concludeTurn := false // 宿主 goal 工具 complete/blocked 后结束物理轮次（对齐 DSH concludeTurn）
		// 只读批并行：整批都是「非本地 + 工具声明只读」时先并发发起 RPC，结果按源序
		// 落回下面的串行循环（事件顺序、审批与副作用语义不变）。不满足则返回 nil。
		prefetched := a.prefetchReadOnlyBatch(ctx, toolClient, toolCalls, availableTools, truncated)
		for i, tc := range toolCalls {
			// 检查 ctx.Done()：并行批的调用已全部发起，必须继续按序落结果
			// （保证每个 tool_call 都有 result，会话恢复后历史仍合法）。
			select {
			case <-ctx.Done():
				if _, done := prefetched[i]; !done {
					return cancelResult()
				}
			default:
			}

			// 模型请求的工具调用记入会话（log-only，与下方 tool/result 配对）
			sess.Append(session.ToolCallEvent, &session.ToolCallData{
				Turn: turnNo, Step: stepNo, CallID: tc.Id, Name: tc.Name, Arguments: tc.ArgumentsJson,
			}, nil)
			// 调用发起前落盘：此后可能长时间阻塞（如 ask_user_question 等待用户
			// 回答、browser 长任务），调用记录须已持久化（/export 与崩溃恢复可见）
			if err := a.store.Save(sess); err != nil {
				a.logger.Warn("incremental session save failed", "turn", turnNo, "step", stepNo, "error", err)
			}

			// 参数不合法（输出被截断，或模型自己漏了闭合标签——引擎的解析器会把后文折进
			// 第一个值，参数于是断在字符串中间）。不执行工具，落一条合成 tool/result 保持
			// tool_use/tool_result 配对（协议要求），并把**模型自己写下的原文**回给它，请它
			// 重发。原文同时留在会话历史里（assistant 消息携带 ArgumentsJson），模型下一
			// 步能读到，才可能补上缺失的 </parameter>——那是它唯一的自纠依据。
			//
			// 这不再是「截断专用」：截断只是参数不合法的原因之一。早先只有 truncated 才走
			// 这条路，于是模型漏写标签的调用被当成合法调用下发；而在 core 层曾有一次改动把
			// 同一个错误变成「本轮失败」，直接在模型看到之前中止了整轮。
			//
			// 空串不算畸形：provider 对「不收参数的工具」发出的参数文本就是空串（或 {}），
			// 那不是错误；把它当失败会让 ask_user_question 之类再也调不动。
			if strings.TrimSpace(tc.ArgumentsJson) != "" && !json.Valid([]byte(tc.ArgumentsJson)) {
				reason := fmt.Sprintf("工具 %s 的调用没有执行：它的参数不是合法 JSON（%s）。%s\n"+
					"这通常意味着上一条调用漏了闭合标签（例如 </parameter>），解析器于是把后面的内容折进了第一个参数值。"+
					"请重新发出格式完整的调用：参数写成一个 JSON 对象，只转义一层。",
					tc.Name, argsSyntaxProblem(tc.ArgumentsJson), truncationCause(truncated, finishReason))
				sess.Append(session.ToolResult, &session.ToolResultData{
					Turn: turnNo, Step: stepNo, CallID: tc.Id,
					Content: reason,
					Error:   reason,
				}, &session.SurfaceOp{Op: session.SurfaceAppend})
				a.logger.Warn("malformed tool call returned to model", "turn", turnNo, "step", stepNo,
					"tool", tc.Name, "argsLen", len(tc.ArgumentsJson), "truncated", truncated)
				if emit != nil {
					emit(&dsc.RunStreamResponse{Output: fmt.Sprintf("\n[工具结果: %s 错误] %s\n", tc.Name, reason), Status: "tool", ToolName: tc.Name, ToolResult: reason, Error: reason, Usage: a.usageSnapshot()})
				}
				continue
			}

			// 向客户端提示正在调用工具（携带工具名与参数 JSON，供 TUI 渲染 REX 式卡片）
			// Usage 随帧携带：本步模型请求完成后即可刷新容量，不必等轮末 success 帧。
			if emit != nil {
				emit(&dsc.RunStreamResponse{Output: fmt.Sprintf("\n[调用工具: %s]\n", tc.Name), Status: "tool", ToolName: tc.Name, ToolArgs: tc.ArgumentsJson, Usage: a.usageSnapshot()})
			}
			a.logger.Info("tool call", "turn", turnNo, "step", stepNo, "tool", tc.Name, "argsLen", len(tc.ArgumentsJson))

			// 宿主托管的 plan/goal 工具直接本地执行（状态读写会话事件日志），
			// 其余工具经聚合 ToolService 转发到工具插件
			var toolResp *proto.ExecuteToolResponse
			if pre, ok := prefetched[i]; ok {
				// 并行预执行结果：RPC 已按源序发起，这里只消费并落结果。
				toolResp = pre.resp
				if pre.err != nil {
					a.logger.Info("tool RPC error", "turn", turnNo, "step", stepNo, "tool", tc.Name, "error", pre.err)
					sess.Append(session.ToolResult, &session.ToolResultData{
						Turn: turnNo, Step: stepNo, CallID: tc.Id,
						Content: fmt.Sprintf("Error executing tool %s: %v", tc.Name, pre.err),
						Error:   pre.err.Error(),
					}, &session.SurfaceOp{Op: session.SurfaceAppend})
					continue
				}
			} else if isLocalTool(tc.Name) {
				var concluded bool
				toolResp, concluded = a.executeLocalTool(ctx, tc)
				if concluded {
					concludeTurn = true
				}
			} else {
				toolReq := &proto.ExecuteToolRequest{
					ToolName:      tc.Name,
					ArgumentsJson: tc.ArgumentsJson,
					ToolCallId:    tc.Id,
					// 随调用转发本会话审批策略（从事件日志折叠）：宿主审批门优先使用，
					// 使宿主重启后 per-session 策略得以恢复（无需重设 /approval）。
					ApprovalPolicy: a.approvalPolicy,
				}
				// owner 隔离：附带调用方会话标识（job 工具按此授权）
				a.sessMu.Lock()
				if a.sess != nil {
					toolReq.SessionId = a.sess.ID()
				}
				a.sessMu.Unlock()
				var err error
				toolResp, err = toolClient.ExecuteTool(ctx, toolReq)
				if err != nil {
					a.logger.Info("tool RPC error", "turn", turnNo, "step", stepNo, "tool", tc.Name, "error", err)
					// 错误也作为 tool/result 记入（surface）
					sess.Append(session.ToolResult, &session.ToolResultData{
						Turn: turnNo, Step: stepNo, CallID: tc.Id,
						Content: fmt.Sprintf("Error executing tool %s: %v", tc.Name, err),
						Error:   err.Error(),
					}, &session.SurfaceOp{Op: session.SurfaceAppend})
					continue
				}
			}
			if toolResp != nil && toolResp.Error != "" {
				a.logger.Info("tool error", "turn", turnNo, "step", stepNo, "tool", tc.Name, "error", toolResp.Error)
				sess.Append(session.ToolResult, &session.ToolResultData{
					Turn: turnNo, Step: stepNo, CallID: tc.Id,
					Content: fmt.Sprintf("Tool error: %s", toolResp.Error),
					Error:   toolResp.Error,
				}, &session.SurfaceOp{Op: session.SurfaceAppend})
				// 把工具結果輸出到流，供 TUI 渲染 REX 式结果卡片
				if emit != nil {
					emit(&dsc.RunStreamResponse{Output: fmt.Sprintf("\n[工具结果: %s 错误] %s\n", tc.Name, toolResp.Error), Status: "tool", ToolName: tc.Name, ToolResult: toolResp.Error, Error: toolResp.Error, Usage: a.usageSnapshot()})
				}
			} else {
				a.logger.Info("tool result", "turn", turnNo, "step", stepNo, "tool", tc.Name, "contentLen", len(toolResp.Content))
				sess.Append(session.ToolResult, &session.ToolResultData{
					Turn: turnNo, Step: stepNo, CallID: tc.Id,
					Content: toolResp.Content,
					Images:  toolResp.Images,
				}, &session.SurfaceOp{Op: session.SurfaceAppend})
				// 把工具結果輸出到流，供 TUI 渲染 REX 式结果卡片；
				// 成功结果帧附带 ToolArgs（供 TUI 更新待办面板）与 ToolView（结构化视图 spec，TUI 统一渲染）
				if emit != nil {
					emit(&dsc.RunStreamResponse{Output: fmt.Sprintf("\n[工具结果: %s]\n%s\n", tc.Name, toolResp.Content), Status: "tool", ToolName: tc.Name, ToolArgs: tc.ArgumentsJson, ToolResult: toolResp.Content, ToolView: toolResp.ViewJson, Usage: a.usageSnapshot()})
				}
			}
			// 策略 advisory 上下文（对齐 DSH additionalContexts）：宿主从策略裁决
			// 机械收集（如 dsc-system 的重复调用提醒），在工具结果之后以合成
			// user message 投喂模型（source guard）；被拦截/失败的调用同样携带
			//——循环恰恰最常发生在反复重试的被拒调用上。
			for _, n := range toolResp.GetNotices() {
				if n == "" {
					continue
				}
				sess.Append(session.UserMessage, &session.UserMessageData{
					Content: n, Source: "guard",
				}, &session.SurfaceOp{Op: session.SurfaceAppend})
			}
		}
		// 步骤结束（log-only）
		sess.Append(session.StepEnd, &session.StepData{Turn: turnNo, Step: stepNo}, nil)
		stepOpen = false
		// 步收口增量落盘：工具结果/守卫消息已入事件日志，持久化到磁盘边界
		if err := a.store.Save(sess); err != nil {
			a.logger.Warn("incremental session save failed", "turn", turnNo, "step", stepNo, "error", err)
		}

		// 工具执行后刷新工具列表：若本轮调用了 load_dsc_plugin / unload_dsc_plugin 等，
		// 宿主工具注册表已更新，但 availableTools 仍是 RunStream 开始时的快照——
		// 下一次 LLM 请求会带着过时的工具列表，模型看不到新加载的工具。
		// 刷新 availableTools 确保模型在下一轮看到最新工具集。
		//
		// MCP 的 load/unload/connect 同理：那几个工具改的是「哪些 MCP 工具对模型可见」，
		// 归属在 tool-mcp 插件（宿主只按插件上报的 hidden 决定曝光），宿主下一次 ListTools
		// 给的目录已经不同。不刷新的话，模型刚 mcp_load 完就调不了刚加载的工具——它会
		// 以为加载失败，然后重复加载。
		for _, tc := range toolCalls {
			if isToolCatalogChangingCall(tc.Name) {
				if refreshed, err := toolClient.ListTools(ctx, &proto.ListToolsRequest{}); err == nil {
					availableTools = append(refreshed.Tools, a.hostTools()...)
					// 刷新是全量替换：会话检索工具的可见性要重新按同一规则裁定
					availableTools = a.filterContextSearch(availableTools, dsc.EstimateProtoMessagesTokens(msgs))
					// 同步更新 a.lastToolNames，避免下次 RunStream 误判为变更触发不必要的 system prompt 重建
					refreshedNames := make([]string, len(availableTools))
					for i, t := range availableTools {
						refreshedNames[i] = t.Name
					}
					sort.Strings(refreshedNames)
					a.lastToolNames = refreshedNames
					// 重建 system prompt（工具上下文已变，如 ContextFn 贡献的字体清单）
					a.sysPrompt = a.buildSystemPrompt(ctx, toolClient)
					// 提交 prompt 变更到事件日志（对齐 v3 system/message surface node 0）
					a.commitSystemPrompt(turnNo, stepNo)
				}
				break // 一次刷新即可
			}
		}

		// 宿主 goal 工具标记 complete/blocked：物理轮次在本步骤后停止（对齐 DSH concludeTurn）
		if concludeTurn {
			sess.Append(session.TurnEnd, &session.TurnData{Turn: turnNo, Reason: "goal-concluded"}, nil)
			turnOpen = false
			if emit != nil {
				emit(&dsc.RunStreamResponse{Status: "success", Usage: a.usageSnapshot()})
			}
			return &dsc.AgentResult{Output: content, Status: "success"}, nil
		}
	}
	// 超过最大迭代次数：轮次以 max-iterations 关闭（不再持久化独立消息数组，session 即历史）。
	// 预算耗尽一律是「没做完」，与模式无关——以前单轮模式会在这里按「已执行过工具」判成功，
	// 那只是给「单轮=单步」的缺陷兜面子（输出一句占位文案、退出码 0），真正的答案并没有产生。
	sess.Append(session.TurnEnd, &session.TurnData{Turn: turnNo, Reason: "max-iterations"}, nil)
	turnOpen = false
	if emit != nil {
		// Error 字段必須帶上，否則 TUI 只按 Status 判斷、看不到任何提示，造成「莫名停止」
		emit(&dsc.RunStreamResponse{Output: "Max iterations reached", Status: "error", Error: "达到最大轮次上限，自动停止"})
	}
	return &dsc.AgentResult{
		Output: "Max iterations reached",
		Status: "error",
	}, nil
}

// usageSnapshot 生成当前步骤请求的用量快照：优先取服务端返回的 lastUsage，
// 否则退化为仅携带最近一次 prompt 用量。随工具帧携带，供 TUI 在每步工具调用
// 后实时刷新容量显示（对齐 REX 每步 usage 事件即时更新统计行的观感）。
func (a *ReactLoopAgent) usageSnapshot() *dsc.Usage {
	a.usageMu.Lock()
	lastPrompt, lastCtx := a.lastPromptTokens, a.lastContextTokens
	a.usageMu.Unlock()
	usage := &dsc.Usage{PromptTokens: lastPrompt, ContextTokens: int32(lastCtx)}
	if a.lastUsage != nil {
		usage.CompletionTokens = a.lastUsage.CompletionTokens
		usage.TotalTokens = a.lastUsage.TotalTokens
		usage.CacheReadInputTokens = a.lastUsage.CacheReadInputTokens
		usage.CacheCreationInputTokens = a.lastUsage.CacheCreationInputTokens
	} else {
		usage.TotalTokens = lastPrompt
	}
	return usage
}

// buildSystemPrompt 構建完整 system prompt，采用 DSH 的分层结构：
// 身份首行 + persona（預設可配）+ 工具指引 + 各工具插件貢獻的上下文片段（如技能索引）。
// 各層按 DSH 约定以空行 "\n\n" 拼接，空內容直接跳過。
func (a *ReactLoopAgent) buildSystemPrompt(ctx context.Context, toolClient proto.ToolServiceClient) string {
	parts := []string{
		// 身份首行（DSH 风格总开关，品牌名适配为 DSC）
		"You are an AI agent powered by DSC.",
	}
	// persona "你是一個…助手" 身份句：预设可配，空則回退通用默认
	if p := strings.TrimSpace(a.persona); p != "" {
		parts = append(parts, p)
	} else {
		parts = append(parts, "You are a helpful software engineer assistant.")
	}
	// 工具指引（DSC 是工具型 agent，需提示模型按任务选用工具）
	parts = append(parts, "根据任务选择合适的工具，逐步完成用户的请求。")

	// PTC 呈现模式（DSC_PTC=1 开启）：引导用 run_code 组合多步 + 注入 run_code
	// 可调用工具 SDK 清单；个别工具仍可照 call（fallback，不强求隐藏）。
	if s := a.ptcSDKContext(ctx, toolClient); s != "" {
		parts = append(parts, s)
	}

	// plan 模式引导（仅激活时注入；对齐 DSH plan:policy 段落，软引导不强制任何限制）
	if a.planActive {
		if s := strings.TrimSpace(a.planSection); s != "" {
			parts = append(parts, s)
		}
	}

	// goal 策略指引（对齐 DSH tool-goal 固定提示词段；goal 状态本身不注入模型上下文）
	parts = append(parts, fmt.Sprintf(goalPolicyPrompt, a.blockedAfterConsecutiveRounds))

	// 聚合各工具插件貢獻的上下文片段（如技能索引），失敗或為空則跳過
	ctx, cancel := context.WithTimeout(ctx, toolMetadataRPCTimeout)
	resp, err := toolClient.ListContext(ctx, &proto.ListContextRequest{})
	cancel()
	if err == nil {
		listContextContent := strings.TrimSpace(resp.GetContent())
		if listContextContent != "" {
			// 宿主在响应里追加 [DSC_COMPACTION_BACKEND_ACTIVE] 标记（它自己据此确认压缩
			// 岗位有人当值）。标记是宿主内部诊断信息，不进 system prompt（对模型不可见）。
			listContextContent = strings.ReplaceAll(listContextContent, "[DSC_COMPACTION_BACKEND_ACTIVE]", "")
			listContextContent = strings.TrimSpace(listContextContent)
			if listContextContent != "" {
				parts = append(parts, listContextContent)
			}
		}
	}

	// 沙箱策略上下文（对齐 DSH sandbox:policy 运行时上下文快照）：让模型知道当前
	// 文件策略与工作区真实根路径，避免臆造不存在的 /workspace 虚拟路径而陷入
	// 「虚拟路径访问出错、真实路径又被拦」的死循环。
	if policy := a.sandboxPolicyContext(); policy != "" {
		parts = append(parts, policy)
	}
	// 审批策略上下文（对齐 DSH approval:policy）：让模型知道当前是否会被审批提问，
	// 以及升级重试（sandbox_permissions+justification）在 never 下会被自动拒。
	if ap := a.approvalPolicyContext(); ap != "" {
		parts = append(parts, ap)
	}
	return strings.Join(parts, "\n\n")
}

// commitSystemPrompt 把当前渲染的 system prompt 提交到事件日志（对齐 DSH v3
// system/message surface node 0）。比较 a.sysPrompt 与 surface 当前 system
// 节点内容（经 sess.ProjectSystemPrompt），差异即 emit 一条 SystemMessage
// 事件：surface 无 system 节点时 append（成为 node 0）；有 system 节点但内容
// 不同时 replace 覆盖 node 0；内容相同则 no-op。
//
// 该方法在每轮 buildSystemPrompt 之后调用，让事件日志成为 system prompt 变更
// 的唯一事实源——resume/fork 后 surface 已含 system/message 节点，无需重新
// buildSystemPrompt 即可恢复 prompt；模型派生消息时由 surface node 0 派生。
//
// turn/step 用于事件载荷定位：本次 prompt 版本是在哪个回合的哪一步发布的。
// 调用方需先确保 a.turnCounter / a.stepCounter 已对齐当前回合/步骤。
//
// inHistory 参数对齐 DSH SystemPromptProjection 的 inHistory 决策：
//
//	false（默认）→ replace node 0（覆盖旧 prompt，Anthropic/OpenAI 协议
//	  只读取首条 system 消息作为有效 prompt）
//	true → append 到 surface 尾部（DeepSeek in-history 协议支持读取
//	  后续 system 消息作为有效 prompt，保留旧 prompt 在缓存历史中）
//
// 当前 DSC 的 LLM provider（llm-anthropic、llm-openai）均不支持
// in-history，故 inHistory 恒为 false。未来若添加 DeepSeek-native
// provider 声明 systemPromptUpdate='in-history' 能力，可在此传入 true。
func (a *ReactLoopAgent) commitSystemPrompt(turn, step int) {
	if a.sess == nil {
		return
	}
	// 当前 LLM provider 不支持 in-history system message，恒走 replace 路径。
	// 未来 DeepSeek-native provider 可改为 a.llmSupportsInHistory。
	op, _ := a.sess.ProjectSystemPrompt(a.sysPrompt, false)
	if op == nil {
		// surface 已有相同内容的 system 节点，无需提交
		return
	}
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{
		Turn:    turn,
		Step:    step,
		Content: a.sysPrompt,
	}, op)
}

// ptcEnabled 是否开启 PTC 呈现模式：环境变量 DSC_PTC，或处于 ptc preset（DSC_MODE=ptc）。
func ptcEnabled() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("DSC_MODE")), "ptc") {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DSC_PTC"))) {
	case "1", "true", "on", "ptc", "yes":
		return true
	}
	return false
}

// ptcSDKContext 返回 PTC 呈现模式下的系统提示段落：引导模型用 run_code 组合多步，
// 并把 SDK 工具清单注入。宿主在 PTC 折叠下只返回 run_code，其描述已承载「程序内
// 可调工具」（对齐 DSH presentation：直接调用折叠为唯一 run_code，其余经 SDK）。
// 非 PTC 模式返回空串（不注入，保持既有 prompt 不变）。
func (a *ReactLoopAgent) ptcSDKContext(ctx context.Context, toolClient proto.ToolServiceClient) string {
	if !ptcEnabled() {
		return ""
	}
	lctx, cancel := context.WithTimeout(ctx, toolMetadataRPCTimeout)
	resp, err := toolClient.ListTools(lctx, &proto.ListToolsRequest{})
	cancel()
	if err != nil {
		return ""
	}
	var sdk string
	for _, t := range resp.Tools {
		if t.Name == "run_code" {
			sdk = t.Description
			break
		}
	}
	if sdk == "" {
		sdk = formatPTCTools(resp.Tools)
	}
	return "You are in PTC (programmatic tool composition) presentation mode. For multi-step or batched " +
		"work, prefer composing ONE Lua program and running it via run_code instead of issuing many " +
		"individual tool calls; the tools callable inside the program are declared in run_code's SDK.\n\n" +
		sdk + "\n\n" + ptcLanguageSpec()
}

// ptcLanguageSpec 返回 go-lua 严格 Lua 方言的语言规范速查，便于模型快速上手写 run_code 程序。
func ptcLanguageSpec() string {
	return "Strict Lua dialect (go-lua: Lua 5.1 syntax, 5.3 integers, flow-sensitive type checker) quick reference:\n" +
		"- Tables/arrays are 1-based: items[1] is the first element (not items[0]); '#t' is sequence length; absent keys read as nil.\n" +
		"- nil != false; '--' starts a comment; 'local' scopes a variable; no trailing semicolons.\n" +
		"- Typed annotations are supported and recommended: local n:number = 1; local xs:{string} = {...}; " +
		"type P = {x:number, y:number}; local f:(number,number)->number = function(a,b) return a+b end.\n" +
		"- Nullable/optional and union types: T? ; type Event = Exit | Message. The checker narrows after a nil/type test " +
		"(e.g. 'if not x then return end' then use x.field safely; 'assert(x ~= nil)').\n" +
		"- Call a tool as its same-name function with ONE table argument (mytool{arg=...}); a failed tool resolves to nil, not an error."
}

// formatPTCTools 把 run_code 可调用工具（不含 run_code 自身）格式化为 PTC SDK 清单。
func formatPTCTools(tools []*proto.Tool) string {
	var b strings.Builder
	b.WriteString("You are in PTC (programmatic tool composition) presentation mode. For any " +
		"multi-step or batched operation, prefer composing ONE Lua script and running it via " +
		"run_code instead of issuing many individual tool calls (individual tools remain " +
		"available as fallback). Inside run_code every tool below is exposed as a same-name Lua " +
		"function (call it as <name>{args}); the script's top-level return is the result.\n" +
		"run_code SDK (tools callable inside a program):")
	for _, t := range tools {
		if t.Name == "run_code" {
			continue
		}
		desc := oneLinePrompt(t.Description)
		if len(desc) > 90 {
			desc = desc[:87] + "..."
		}
		fmt.Fprintf(&b, "\n- %s: %s", t.Name, desc)
	}
	return b.String()
}

// oneLinePrompt 把多行描述压成单行（用空行分隔，供 prompt 内紧凑展示）。
func oneLinePrompt(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// sandboxPolicyContext 渲染当前沙箱策略上下文片段。workspace-write 时携带工作区
// 真实根路径（对齐 DSH renderPolicyContext 以真实路径呈现根的约定）；/workspace
// 仍作为该根的别名被编辑器工具与 sandbox 接受，但 shell 等原生命令只能访问真实路径。
func (a *ReactLoopAgent) sandboxPolicyContext() string {
	ws := dsc.WorkspaceRoot()
	if ws == "" {
		ws = "."
	}
	ws = filepath.ToSlash(ws)
	// 对齐 DSH renderPolicyContext：明确「别净凭政策拒绝一次必要修改——照常尝试可用工具，
	// 并按工具返回的拒绝/升级指引行事」，避免模型因政策提示而过早放弃。
	// 三档均注入 workspace root，让模型知道真实工作目录——full-access 也注入，
	// 避免 full-access 模式下模型误以为 cwd 是 dsc 可执行目录而路径出错。
	switch strings.ToLower(strings.TrimSpace(a.sandboxPolicy)) {
	case "readonly", "read-only":
		return "Current DSC file policy: read-only. Available tool operations cannot modify files in the standing mode. " +
			"Do not refuse a required modification from this policy alone: try an available tool normally and follow any denial and escalation guidance it returns. " +
			"The session workspace root is " + strconv.Quote(ws) + " (also accessible via the \"/workspace\" prefix)."
	case "full", "full-access":
		return "Current DSC file policy: danger-full-access. The DSC file sandbox does not restrict file modifications by available tool operations. " +
			"The session workspace root is " + strconv.Quote(ws) + " (also accessible via the \"/workspace\" prefix). " +
			"Under full-access, tool operations may read/write any path on the filesystem."
	default: // workspace / workspace-write（缺省，为便利而非对齐 DSH 默认的 read-only）
		return "Current DSC file policy: workspace-write. Available tool operations may modify files under the session workspace: " +
			strconv.Quote(ws) + `. The "/workspace" prefix is an alias for that root; native commands (e.g. shell) can only use the real path.`
	}
}

// approvalPolicyContext 渲染当前审批策略上下文片段（对齐 DSH approval:policy 两句）：
// 由 a.approvalPolicy（会话事件折叠 ?? 部署缺省 DSC_APPROVAL_POLICY）驱动。ask 声明须审批
// 的操作会经评审通道询问；never 声明审批提示关闭、升级重试（sandbox_permissions）自动拒。
func (a *ReactLoopAgent) approvalPolicyContext() string {
	switch strings.ToLower(strings.TrimSpace(a.approvalPolicy)) {
	case "never", "off":
		return "Approval prompts are disabled in this session: actions that require approval are rejected automatically — do not request sandbox escalation (do not set `sandbox_permissions`)."
	default: // ask（缺省）
		return "Approval policy: ask. Operations that require approval may ask through the configured answerer; without an available answerer, the request fails closed."
	}
}

// SetUserQuestionsService 注入宿主挂载在 broker 上的 UserQuestionsService ID。
// exit_plan_mode 评审等场景经 ensureUserQuestionsClient 惰性连接并调用。
func (a *ReactLoopAgent) SetUserQuestionsService(ctx context.Context, serviceID uint32) error {
	a.uqMu.Lock()
	a.uqServiceID = serviceID
	// 热重载会下发新 id（新 broker 上的新服务）：先释放旧连接，避免复用失效客户端。
	if a.uqConn != nil {
		_ = a.uqConn.Close()
	}
	a.uqConn = nil
	a.uqClient = nil
	a.uqMu.Unlock()

	if serviceID == 0 {
		return nil
	}
	// 立即建立连接（与 RegisterServices 同规）：go-plugin broker 的 ConnInfo 是一次性
	// 通告、约 5 秒后过期；此前这里是惰性连接（模型首次提问才 Dial），实际使用时
	// 早已超出窗口，Dial 必然失败并回落到「no user-questions channel is available」。
	if _, err := a.dialUserQuestions(); err != nil {
		fmt.Printf("[Agent Loop] user-questions eager connect failed: %v\n", err)
		return err
	}
	fmt.Printf("[Agent Loop] user-questions service connected (id=%d)\n", serviceID)
	return nil
}

// ensureUserQuestionsClient 取 UserQuestionsService 客户端（现有连接优先，否则重试连接；
// 返回 nil 表示无通道，调用方给出可行动提示）。
func (a *ReactLoopAgent) ensureUserQuestionsClient() proto.UserQuestionsServiceClient {
	c, err := a.dialUserQuestions()
	if err != nil {
		fmt.Printf("[Agent Loop] user-questions unavailable: %v\n", err)
		return nil
	}
	return c
}

// dialUserQuestions 建立/复用 UserQuestionsService 连接。
func (a *ReactLoopAgent) dialUserQuestions() (proto.UserQuestionsServiceClient, error) {
	a.uqMu.Lock()
	defer a.uqMu.Unlock()
	if a.uqServiceID == 0 {
		return nil, fmt.Errorf("user-questions service not injected")
	}
	if a.uqClient != nil {
		return a.uqClient, nil
	}
	if a.broker == nil {
		return nil, fmt.Errorf("agent broker not available")
	}
	conn, err := a.broker.Dial(a.uqServiceID)
	if err != nil {
		return nil, fmt.Errorf("dial user-questions service: %w", err)
	}
	a.uqConn = conn
	a.uqClient = proto.NewUserQuestionsServiceClient(conn)
	return a.uqClient, nil
}

// SwitchSession 切换当前会话：从 store 按 id 加载并接管（事件溯源日志）。
// 下一次 Run 将基于目标会话继续；当前轮次若在进行中由调用方负责确保已结束。
func (a *ReactLoopAgent) SwitchSession(ctx context.Context, sessionID string) error {
	if a.store == nil {
		return fmt.Errorf("session store not initialized")
	}
	sess, err := a.store.Ensure(sessionID)
	if err != nil {
		return fmt.Errorf("switch session: %w", err)
	}
	a.sessMu.Lock()
	a.sess = sess
	a.turnCounter = sess.LastTurn()
	a.sysPromptNeedsUpdate = true // 新会话的 plan/goal 状态不同，下次 Run 重建 system prompt
	a.sessMu.Unlock()
	// 用量属于「上一个会话」：不清掉的话，从 1M 的大会话切到新会话时，上一发的
	// prompt 用量会被当成本会话的已用容量，导致空会话被判定超窗而触发滑窗/压缩。
	a.usageMu.Lock()
	a.lastPromptTokens = 0
	a.lastUsage = nil
	a.usageMu.Unlock()
	a.contextSearchSeen = false // 会话检索工具的可见性随会话重新裁定
	a.tokenMeter().Reset()
	fmt.Printf("[Agent Loop] switched to session %s (%d events, last turn %d)\n",
		sessionID, sess.Len(), a.turnCounter)
	return nil
}

// SetPlanMode 设置当前会话的 plan 模式：追加 log-only plan/mode 事件并落盘
// （事件溯源：恢复/fork/压缩都能直接折叠回 plan 状态），并标记下次 Run 重建
// system prompt 以注入/移除 plan section。对齐 DSH plan-mode 的软引导设计：
// 仅注入引导文案，不强制任何沙箱或批准限制。
func (a *ReactLoopAgent) SetPlanMode(ctx context.Context, active bool) error {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.sess == nil {
		return fmt.Errorf("set plan mode: session not loaded")
	}
	a.sess.Append(session.PlanMode, &session.PlanModeData{Active: active}, nil)
	if err := a.store.Save(a.sess); err != nil {
		return fmt.Errorf("set plan mode: %w", err)
	}
	a.sysPromptNeedsUpdate = true
	fmt.Printf("[Agent Loop] plan mode set to %v\n", active)
	return nil
}

// SetHistoryInjection 设置当前会话的历史注入条数上限：追加 log-only history/limit
// 事件并落盘（事件溯源：恢复/切换/fork 都能折叠还原），无需重建 system prompt。
// 同时立即更新内存值（当前 Run 后续 step 即按新值派生，无需等下一次 Run 折叠）。
// count < 0 不注入恢复历史；count == 0 未定义（按默认 = 不限制）；count > 0 只注入
// 最近 count 条派生消息。与 DSH 相反（DSH：count < 0 不限制、count == 0 不注入）。
func (a *ReactLoopAgent) SetHistoryInjection(ctx context.Context, count int) error {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.sess == nil {
		return fmt.Errorf("set history injection: session not loaded")
	}
	a.sess.Append(session.HistoryLimit, &session.HistoryLimitData{Count: count}, nil)
	if err := a.store.Save(a.sess); err != nil {
		return fmt.Errorf("set history injection: %w", err)
	}
	a.historyInjection = count // 立即生效（负数 = 不注入恢复历史；0 = 未定义/不限制）
	fmt.Printf("[Agent Loop] history injection limit set to %d\n", count)
	return nil
}

// ensureConnected 建立並快取 LLM/Tool 服務連接。
// broker.Dial 對同一 serviceID 是一次性握手，連接信息只會發送一次，
// 因此必須跨 Run 重用連接，否則第二次 Dial 會因收不到連接信息而超時。
func (a *ReactLoopAgent) ensureConnected(llmID, toolID uint32) (proto.LLMServiceClient, proto.ToolServiceClient, error) {
	a.connMu.Lock()
	defer a.connMu.Unlock()

	if a.llmClient == nil || a.toolClient == nil {
		llmConn, err := a.broker.Dial(llmID)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to dial LLM service: %w", err)
		}
		a.llmConn = llmConn
		a.llmClient = proto.NewLLMServiceClient(llmConn)

		toolConn, err := a.broker.Dial(toolID)
		if err != nil {
			_ = a.llmConn.Close()
			a.llmConn = nil
			a.llmClient = nil
			return nil, nil, fmt.Errorf("failed to dial tool service: %w", err)
		}
		a.toolConn = toolConn
		a.toolClient = proto.NewToolServiceClient(toolConn)
	}
	return a.llmClient, a.toolClient, nil
}

func (a *ReactLoopAgent) Name(ctx context.Context) string    { return "react-agent" }
func (a *ReactLoopAgent) Version(ctx context.Context) string { return "1.3.0" } // 请求面图像预算卸载外置 dsc-system image-offload 驻留（pre-step 瀑布投影）；本循环零请求前预处理

// InjectMessage 将一条用户消息实时注入到当前运行中会话的历史末端。
// 运行中的 runLoop 每步都从会话 surface 重新派生请求历史（DeriveMessages），
// 因此这里追加的 UserMessage 表面事件会在下一次 LLM 迭代即被模型看到——
// 无需停止或等待本轮完成（对齐 TUI 正在工作中的实时输入）。
func (a *ReactLoopAgent) InjectMessage(ctx context.Context, content string, images []string) error {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.sess == nil {
		return fmt.Errorf("inject message: session not loaded")
	}
	// 没有活跃循环时追加等于把消息丢进黑洞：写进去了，却没有任何一步会把它
	// 发给模型（模型"看不到"用户明明发过的消息）。调用方（TUI）据此改走唤醒。
	if !a.runActive {
		return fmt.Errorf("inject message: no active run")
	}
	a.sess.Append(session.UserMessage,
		&session.UserMessageData{Content: content, Source: "user", Images: images},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	a.pendingInjects++ // 记录一次尚未被 runLoop 消费的注入（收尾前据此检测是否继续）
	fmt.Printf("[Agent Loop] injected user message (%d chars) into running session\n", len(content))
	return nil
}

// hasNewInjectedSince 返回是否自注入基线（iterInjects，本迭代派生请求历史时快照的
// pendingInjects）之后又有新的 InjectMessage 注入。runLoop 在模型无工具调用收尾前调用：
// 命中说明模型输出期间 TUI 注入了新用户消息，本轮不得结束，须继续下一轮处理。
func (a *ReactLoopAgent) hasNewInjectedSince(iterInjects int) bool {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	return a.pendingInjects > iterInjects
}

// DebugSnapshot 返回 agent 当前运行时的调试快照，供 ADMIN API 的 DEBUGGER 端点
// 与自动化测试观察代理内部状态：会话历史（含实时注入的消息）、token 用量、
// turn 计数与 plan/goal 状态。
func (a *ReactLoopAgent) DebugSnapshot(ctx context.Context) (*dsc.AgentDebugSnapshot, error) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.sess == nil {
		return nil, fmt.Errorf("debug snapshot: session not loaded")
	}

	// lastPromptTokens 由串流 loop 在 usageMu 下寫入（不在 sessMu 下），故分開取鎖讀
	a.usageMu.Lock()
	lastPromptTokens := a.lastPromptTokens
	a.usageMu.Unlock()

	snap := &dsc.AgentDebugSnapshot{
		SessionID:        a.sess.ID(),
		TurnCount:        a.turnCounter,
		PlanActive:       a.planActive,
		LastPromptTokens: lastPromptTokens,
	}

	// goal 状态由事件日志折叠
	if g := session.FoldGoal(a.sess.Events()); g != nil {
		snap.Goal = &dsc.AgentGoalDebugInfo{
			Phase: g.Phase, Revision: g.Revision,
			MaxRounds: g.MaxGoalRounds, Objective: g.Objective,
		}
	}

	// 派生的请求历史（与下次 LLM 请求一致，含实时注入的消息与历史注入条数限制）
	for _, m := range a.sess.DeriveMessagesLimited(a.historyInjection) {
		snap.Messages = append(snap.Messages, &dsc.AgentDebugMessage{Role: m.Role, Content: m.Content})
	}
	return snap, nil
}

// handleHostEvent 订阅宿主事件（SDK Hook.OnEvent）：
//   - 沙箱升级审批审计事件（approval/asked / approval/decided）落进当前会话日志（对齐 DSH）；
//   - 会话审批策略变化（approval/policy）落进当前会话日志并即时刷新 a.approvalPolicy（上下文）。
//
// 仅匹配宿主带上的同一会话 id 的事件。返回 ("", nil)：通知型 listener，不改写事件载荷。
func (a *ReactLoopAgent) handleHostEvent(_ context.Context, eventType, dataJSON string) (string, error) {
	if eventType != string(dsc.EventApprovalAsked) && eventType != string(dsc.EventApprovalDecided) && eventType != string(dsc.EventApprovalPolicy) {
		return "", nil
	}
	var payload struct {
		Session, Tool, Mode, Reason, Outcome, Policy string
	}
	if err := json.Unmarshal([]byte(dataJSON), &payload); err != nil {
		return "", nil
	}
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if a.sess == nil || (payload.Session != "" && payload.Session != a.sess.ID()) {
		return "", nil
	}
	switch eventType {
	case string(dsc.EventApprovalAsked):
		a.sess.Append(session.ApprovalAsked, &session.ApprovalAskedData{
			Tool: payload.Tool, Mode: payload.Mode, Reason: payload.Reason,
		}, nil)
	case string(dsc.EventApprovalDecided):
		a.sess.Append(session.ApprovalDecided, &session.ApprovalDecidedData{
			Tool: payload.Tool, Mode: payload.Mode, Outcome: payload.Outcome,
		}, nil)
	case string(dsc.EventApprovalPolicy):
		a.sess.Append(session.ApprovalPolicy, &session.ApprovalPolicyData{Policy: payload.Policy}, nil)
		if payload.Policy != "" {
			a.approvalPolicy = payload.Policy
		}
	}
	if err := a.store.Save(a.sess); err != nil {
		fmt.Printf("[Agent Loop] failed to save session after approval audit: %v\n", err)
	}
	return "", nil
}

func (a *ReactLoopAgent) Shutdown(ctx context.Context, force bool) error {
	a.shutdownMu.Lock()
	defer a.shutdownMu.Unlock()

	if a.isShutdown {
		return nil
	}

	// 1. 取消當前 Run
	a.mu.Lock()
	if a.cancelFunc != nil {
		a.cancelFunc()
	}
	a.mu.Unlock()

	// 2. 等待 Run 完成（非強制模式）
	if !force {
		done := make(chan struct{})
		dsc.SafeGoroutine(func() {
			a.runWg.Wait()
			close(done)
		})
		select {
		case <-done:
			// 正常完成
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second): // 超時保護
			// 超時後強制繼續
		}
	}

	a.isShutdown = true

	// 關閉快取的服務連接
	a.connMu.Lock()
	if a.llmConn != nil {
		_ = a.llmConn.Close()
		a.llmConn = nil
		a.llmClient = nil
	}
	if a.toolConn != nil {
		_ = a.toolConn.Close()
		a.toolConn = nil
		a.toolClient = nil
	}
	a.uqMu.Lock()
	if a.uqConn != nil {
		_ = a.uqConn.Close()
		a.uqConn = nil
		a.uqClient = nil
	}
	a.uqMu.Unlock()
	a.connMu.Unlock()

	return nil
}

// tokenMeter 返回上下文计量器（懒初始化：窗口可能晚于构造才确定，测试构造也不设它）。
// 全 agent 只有这一个计量入口——裁剪、压缩、提示文案、单轮超窗判定都读它。
func (a *ReactLoopAgent) tokenMeter() *dsc.TokenMeter {
	if a.meter == nil {
		a.meter = dsc.NewTokenMeter(a.contextWindow)
	}
	return a.meter
}

// contextSearchToolName 宿主内置的会话检索工具名（实现在 core/context_search_tool.go）。
// agent 只决定「要不要把它放进目录」，搜索本身在宿主侧执行——数据（会话日志）归宿主。
const contextSearchToolName = "context_search"

// filterContextSearch 按会话状态决定是否暴露 context_search。
//
// 判据：历史（消息 + 工具目录）已经装不进窗口 → 滑窗正在裁掉内容 → 模型需要一个
// 精确回取的手段。反过来，整个历史都塞得下时不暴露：那时检索没有增量价值，白占
// 目录与提示前缀。
//
// 注入被显式关闭（historyInjection < 0）时永不暴露——那是用户明确表示不要让模型
// 翻旧账，再给一个检索工具等于绕过这个决定。
//
// 一旦成立就保持常开（见 contextSearchSeen）：避免工具目录每步抖动导致提示缓存
// 整段失效。
func (a *ReactLoopAgent) filterContextSearch(tools []*proto.Tool, msgsEstimate int) []*proto.Tool {
	if a.contextSearchSeen {
		return tools
	}
	fits := a.contextWindow <= 0 ||
		msgsEstimate+dsc.EstimateProtoToolTokens(tools) <= a.contextWindow
	if fits || a.historyInjection < 0 {
		return dropToolByName(tools, contextSearchToolName)
	}
	a.contextSearchSeen = true
	return tools
}

// isToolCatalogChangingCall 报告一个工具调用是否会改变「模型可见的工具目录」，
// 从而需要在本步结束后重新向宿主取一次目录。
//
// 两类：插件热加载（注册表真的增删了工具）与 MCP 的 connect/load/unload（注册表没变，
// 但宿主侧的曝光集变了，下一次 ListTools 给的目录就不同）。mcp_list 不改目录，不在
// 其中——把它算进来会让每次列举都白跑一次 ListContext + system prompt 重建。
func isToolCatalogChangingCall(name string) bool {
	switch name {
	case "load_dsc_plugin", "unload_dsc_plugin", "install_dsc_plugin", "uninstall_dsc_plugin",
		"mcp_connect", "mcp_load", "mcp_unload":
		return true
	}
	return false
}

// dropToolByName 返回去掉指定名字后的工具目录副本（不改原切片：调用方可能与
// 快照共享底层数组）。名字不存在时原样返回。
func dropToolByName(tools []*proto.Tool, name string) []*proto.Tool {
	idx := -1
	for i, t := range tools {
		if t.GetName() == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return tools
	}
	out := make([]*proto.Tool, 0, len(tools)-1)
	out = append(out, tools[:idx]...)
	return append(out, tools[idx+1:]...)
}

// seedUsageFromHistory 从会话日志恢复上次成功请求的用量锚点（重启后首发请求没有
// 服务端 usage 可依）：
//   - lastPromptTokens/lastUsage 供 usageSnapshot 上报「已用」；
//   - tokenMeter().UpdateUsage 记住这个实测值，供 MeasuredPromptTokens 与
//     「锚点是否已超窗」的判断使用。
//
// 注意它**不**会让 Project 退化为该实测值：没有增量基线的实测值只描述那一次请求，
// 拿它当投影会把界面钉在一个旧数字上（见 core.TokenMeter.projectLocked）。投影在
// 拿到第一发真实响应（SampleUsage 锚定实测 + 当次表面估算）之后才精确。
//
// 倒查逻辑在 dsc.LastPromptUsage（唯一实现，宿主 core 用同一份），此处不再自己
// 遍历事件——同一规则写两遍就会出现「宿主和插件挑到不同锚点」的漂移。
func (a *ReactLoopAgent) seedUsageFromHistory(sess *session.Session) {
	promptTokens, usage, ok := dsc.LastPromptUsage(sess)
	if !ok {
		return
	}
	a.usageMu.Lock()
	a.lastPromptTokens = int32(promptTokens)
	a.lastUsage = usage
	a.usageMu.Unlock()
	a.tokenMeter().UpdateUsage(usage)
}

// newAgent 从宿主注入的环境变量构建 ReactLoopAgent（配置读取 + 会话存储初始化）。
// broker 由 SDK 在 gRPC server 建立时经 AgentBroker 回调注入（仅该阶段可用）。
func newAgent() (*ReactLoopAgent, error) {
	agent := &ReactLoopAgent{}
	// 本地 hclog：写 os.Stderr（宿主 go-plugin SyncStderr 捕获入日志流，受 -log 门控），
	// 与其余运行日志统一经 logger 输出，而非散落直接写 stderr。
	agent.logger = hclog.New(&hclog.LoggerOptions{
		Name:   "agent-react-loop",
		Level:  hclog.Info,
		Output: os.Stderr,
	})
	// 讀取宿主傳入的上下文窗口容量（DSC_CONTEXT_WINDOW，token 數）
	if n := dsc.ContextWindow(); n > 0 {
		agent.contextWindow = n
	}
	// 讀取宿主傳入的單輪模式標記（DSC_SINGLE_TURN=1，-input 自動化測試入口使用）
	if dsc.EnvBool("DSC_SINGLE_TURN", false) {
		agent.singleTurn = true
	}
	// 讀取宿主傳入的沙箱策略（DSC_SANDBOX_POLICY，缺省 workspace-write）
	agent.sandboxPolicy = os.Getenv("DSC_SANDBOX_POLICY")
	if agent.sandboxPolicy == "" {
		agent.sandboxPolicy = "workspace-write"
	}
	// 讀取宿主傳入的审批策略缺省（DSC_APPROVAL_POLICY，缺省 ask）；每个会话可在事件
	// 日志上 override（/approval → 宿主广播落地），Run 时折叠优先于此缺省。
	agent.approvalPolicy = os.Getenv("DSC_APPROVAL_POLICY")
	if agent.approvalPolicy == "" {
		agent.approvalPolicy = "ask"
	}
	// 讀取宿主傳入的历史注入条数默认值（DSC_HISTORY_INJECTION），沿用 DSC 三值语义：
	// 负值（-1 等）不注入恢复历史、0 未定义（未设置，按默认 = 不限制）、N>0 只注入
	// 恢复历史中最近 N 条。会话内可通过 /settings history 以 history/limit 事件覆盖
	// （事件折叠优先于此默认值）。
	// 注意与 DSH 相反：DSH 用 -1 表示不限制、0 表示不注入；Go 体系的 0 是零值，
	// 把它解释成「关闭」会让默认配置意外关掉历史，故 DSC 统一走自己的三值。
	agent.historyInjection = 0
	if v := os.Getenv("DSC_HISTORY_INJECTION"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			agent.historyInjection = n
		}
	}
	// 讀取宿主傳入的循環輪數上限（DSC_MAX_ITERATIONS）。默認 0 = 不設限（面向長程任務），
	// 僅當顯式設置大於 0 的值時才作為循環上限。
	agent.maxIterations = 0
	if v := os.Getenv("DSC_MAX_ITERATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			agent.maxIterations = n
		}
	}
	// 讀取宿主傳入的 preset persona（DSC_PRESET_PERSONA，「你是一個…助手」身份句）
	agent.persona = os.Getenv("DSC_PRESET_PERSONA")
	// plan 模式引导文案（DSC_PLAN_SECTION，缺省 DSH 示例文案）
	agent.planSection = os.Getenv("DSC_PLAN_SECTION")
	if agent.planSection == "" {
		agent.planSection = defaultPlanSection
	}
	// goal 部署默认 Round 上限（DSC_GOAL_MAX_ROUNDS，缺省 256 对齐 DSH）
	agent.defaultMaxGoalRounds = 256
	if v := os.Getenv("DSC_GOAL_MAX_ROUNDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			agent.defaultMaxGoalRounds = n
		}
	}
	// goal 阻塞判定阈值（DSC_GOAL_BLOCKED_AFTER，缺省 3 对齐 DSH；写入策略提示词）
	agent.blockedAfterConsecutiveRounds = 3
	if v := os.Getenv("DSC_GOAL_BLOCKED_AFTER"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			agent.blockedAfterConsecutiveRounds = n
		}
	}
	// todo 任务清单并行开关（DSC_TODO_ALLOW_PARALLEL，缺省 false：最多一个 in_progress）
	if dsc.EnvBool("DSC_TODO_ALLOW_PARALLEL", false) {
		agent.todoAllowParallel = true
	}
	// 多会话事件日志存储：目录解析统一在 SDK（DSC_SESSION_DIR，缺省 <ExecDir>/sessions），
	// 不得回退相对路径 "sessions"——插件 cwd 未必是 ExecDir，宿主会读不到。
	store, err := session.NewStore(dsc.SessionDir())
	if err != nil {
		return nil, fmt.Errorf("failed to open session store: %w", err)
	}
	agent.store = store
	return agent, nil
}

// agentSDKConfig 是 agent 的声明式元数据：名字、类型、**能力依赖**。
//
// 依赖只在 metadata 里说「我需要什么」，不点名具体插件：
//   - requires/llm/llm：一个提供 "llm" 能力的 LLM 插件（所有 LLM 插件默认提供）。
//     宿主据此按能力解析 primary LLM provider。
//   - requires/dsc/compaction：一个提供 "compaction" 能力的压缩插件。这是**强依赖**：
//     agent 只负责循环与调度，不实现压缩（不裁剪、不折叠、不做摘要），请求面的改写
//     全部由当值后端完成。没有它，历史一旦越过窗口就无人能救——所以依赖未满足时宿主
//     让 agent 保持 PENDING，并在启动期明确失败，而不是让用户到某次请求报超窗才发现。
//
// 多候选（同时启用两个压缩后端）由宿主按 config.yaml 的 compaction 字段选一个当值，
// 其余保留为候选——同域唯一 provider 语义在这里落实为「岗位唯一」。
//
// 抽成函数而不是写在 main 里：这是可测的契约（见 agent_deps_test.go），
// 删掉一条依赖就等于允许 agent 在没有压缩能力的环境里裸奔。
func agentSDKConfig() dsc.Config {
	return dsc.Config{
		Name:    "agent-react-loop",
		Version: "1.2.0",
		Type:    dsc.TypeAgent,
		Requires: []dsc.CapabilityRequirement{
			{Type: "llm", Capability: "llm"},
			{Type: "dsc", Capability: "compaction"},
		},
	}
}

// main 以公共 SDK（dsc-sdk）声明式启动：SDK 复用宿主 dsc.AgentGRPCPlugin 提供
// AgentService + 元数据，并经 AgentBroker 回调在 gRPC server 建立时注入宿主 broker
// （重写自旧的 customAgentGRPCServer 样板）。
func main() {
	agent, err := newAgent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-react-loop: %v\n", err)
		os.Exit(2)
	}

	sdk := dsc.New(agentSDKConfig())
	sdk.Agent(agent)
	// 订阅宿主事件：把沙箱升级审批审计（approval/asked + approval/decided）写进当前会话日志。
	sdk.Hook(dsc.Hook{OnEvent: agent.handleHostEvent})
	sdk.AgentBroker(func(b *dsc.AgentBroker) error {
		agent.broker = b
		return nil
	})
	sdk.Serve()
}

// hasPendingTodos 报告 TODO 列表中是否有未完成的项（pending 或 in_progress）。
func hasPendingTodos(todos []session.TodoItem) bool {
	for _, t := range todos {
		if t.Status == session.TodoPending || t.Status == session.TodoInProgress {
			return true
		}
	}
	return false
}

// buildTodoNudgePrompt 构造 TODO 追问提示词，列出未完成项提醒模型继续处理。
func buildTodoNudgePrompt(todos []session.TodoItem) string {
	var pending []string
	for _, t := range todos {
		if t.Status == session.TodoPending || t.Status == session.TodoInProgress {
			status := t.Status
			if status == session.TodoInProgress {
				status = "进行中"
			} else {
				status = "待处理"
			}
			pending = append(pending, fmt.Sprintf("  - [%s] %s", status, t.Content))
		}
	}
	if len(pending) == 0 {
		return ""
	}
	return fmt.Sprintf("你的待办清单中还有 %d 项未完成的任务：\n%s\n"+
		"请逐项继续处理，直到清单清空再结束回复：完成的项、已失效或无需进行的项，"+
		"都必须以 todo_write 更新（标记为 completed 或从清单中移除），不得保留无人处理的待办项；"+
		"若某项暂时无法推进，请改为 in_progress 并说明阻塞原因。", len(pending), strings.Join(pending, "\n"))
}
