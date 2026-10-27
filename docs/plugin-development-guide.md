# 插件开发实战：以 agent-react-loop 为范例

本文用 [agent-react-loop/main.go](../plugins/agent-react-loop/main.go) 讲解 DSC 插件的完整开发路径：如何声明能力、如何实现契约、如何使用宿主挂载的其他插件、如何把状态落进会话日志，以及哪些规范不能破。

## 1. 为什么拿它当范例

DSC 有五类插件（`agent` / `llm` / `tool` / `dsc` / `policy`），其中 agent 的契约最宽：既要实现 `AgentService` 的全部 RPC，又要经 broker 反向调用宿主挂载的 LLM 与工具服务，还要订阅宿主事件、维护会话历史、产出流式帧。看懂它，其余四类都是它的子集：

| 插件类型 | 与 agent-react-loop 相比缺什么 |
| --- | --- |
| `tool` | 不需要 broker 回调与事件订阅，只要 `sdk.Tool(...)` 或 `sdk.ToolProvider(...)` |
| `llm` | 只需实现 `Chat` / `ChatStream`，无需会话与循环 |
| `dsc` | 通常无自有工具，靠 `Hook` / `ListContext` / 事件接管宿主行为 |
| `policy` | 只实现 `PolicyService.OnEvent` 一个 RPC |

它同时覆盖了 SDK 的全部常见用法：`Config` 声明、`sdk.Agent`、`sdk.Hook`、`sdk.AgentBroker`、`sdk.Serve`，以及 `dsc.*` 转发工具（类型别名与函数转发，见 [sdk/](../sdk)）。

## 2. 文件地图

| 文件 | 职责 |
| --- | --- |
| [main.go](../plugins/agent-react-loop/main.go) | agent 契约实现、run loop、能力连接、system prompt 构建 |
| [message_guard.go](../plugins/agent-react-loop/message_guard.go) | 派生请求历史时校验并最小修复工具调用配对 |
| [parallel_tools.go](../plugins/agent-react-loop/parallel_tools.go) | 只读工具批次的并行预执行与并发上限 |
| [plan_goal.go](../plugins/agent-react-loop/plan_goal.go) | `exit_plan_mode` / goal / `ask_user_question` / `todo_write` 等宿主托管工具 |
| [truncation_guard.go](../plugins/agent-react-loop/truncation_guard.go) | 输出截断判定（`max_tokens` / `length`）与续行策略 |
| `*_test.go` | 15 个测试文件，覆盖循环、压缩、注入竞态、视图帧、end-to-end 启动 |

## 3. 启动：main 只做四件事

[main.go#L1969-L1995](../plugins/agent-react-loop/main.go#L1969-L1995)：

```go
func main() {
	agent, err := newAgent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent-react-loop: %v\n", err)
		os.Exit(2)
	}

	sdk := dsc.New(dsc.Config{
		Name:    "agent-react-loop",
		Version: "1.1.0",
		Type:    dsc.TypeAgent,
		Requires: []dsc.CapabilityRequirement{
			{Type: "llm", Capability: "llm"},
		},
	})
	sdk.Agent(agent)
	sdk.Hook(dsc.Hook{OnEvent: agent.handleHostEvent})
	sdk.AgentBroker(func(b *dsc.AgentBroker) error {
		agent.broker = b
		return nil
	})
	sdk.Serve()
}
```

四个动作对应四件事：

1. **`dsc.New(Config)`**：声明身份（`Name` / `Version` / `Type`）与能力依赖（`Requires`）。这是插件与宿主之间唯一的静态契约。
2. **`sdk.Agent(agent)`**：把 agent 实现注册进 SDK；SDK 复用 `dsc.AgentGRPCPlugin` 自动提供 `AgentService` 与插件元数据，插件作者不写任何 gRPC 样板。
3. **`sdk.Hook(...)`**：顺带订阅宿主事件。一个插件可以同时是 agent 与 listener。
4. **`sdk.AgentBroker(...)`**：在宿主侧 gRPC server 建立时拿到 broker，用于反向 Dial 宿主挂载的 LLM / Tool / UserQuestions 服务。这一步替代了「用环境变量传 serviceID」的旧做法——env 在子进程 spawn 时快照，动态加载/卸载不同步。

注意 `Config` 里没有 `Provides`：agent 由宿主按配置选定，不作为能力被其他插件获取。而 `Requires: llm` 只影响**加载期**的 provider 解析（宿主据此选定 primary LLM），工具服务不在 `Requires` 里——它在运行期由宿主的 `RegisterServices` 注入，因为工具是聚合的，不是单选。

## 4. Agent 契约：实现哪些方法

`dsc.Agent` 的每个方法都有明确调用时机，实现时按「宿主何时调」思考：

| 方法 | 何时被调 | 实现要点 |
| --- | --- | --- |
| `Name` / `Version` | 插件加载 | 返回身份信息，不读配置 |
| `RegisterServices(llmID, toolID)` | 宿主聚合服务就绪后 | 存 ID 并**立即**建连（[#L208-L226](../plugins/agent-react-loop/main.go#L208-L226)） |
| `Run` / `RunStream` | 每轮用户输入 | 共用 `runLoop`；流式版把帧写进 channel 并补发错误帧 |
| `SetUserQuestionsService(id)` | 宿主挂载评审通道后 | 独立连接，同样一次性握手（[#L1554-L1580](../plugins/agent-react-loop/main.go#L1554-L1580)） |
| `SwitchSession` | 用户切换会话 | 切换 `session.Store` 中的当前会话，重置轮次计数 |
| `SetPlanMode` | 用户切换 plan 模式 | 只改状态，不直接改历史；下一轮渲染 system prompt 时生效 |
| `SetHistoryInjection(count)` | 用户调整历史注入 | 遵循三值语义：负值不注入 / 0 未定义 / N>0 最近 N 条 |
| `InjectMessage` | 模型输出期间用户追加输入 | 写入会话末尾并计数，供循环判断是否继续下一轮 |
| `DebugSnapshot` | ADMIN API / 测试观察 | 返回会话、turn、plan、goal、prompt token 快照 |
| `Shutdown(force)` | 宿主退出 | 取消当前 Run，非强制时等待 `runWg` |

`RunStream` 的实现值得单独看（[#L232-L251](../plugins/agent-react-loop/main.go#L232-L251)）：它把 `runLoop` 的 `emit` 转成 channel，并在 `runLoop` 早期失败（serviceID 未设置、连接失败）时补发一帧 `status="error"`——否则错误会被吞掉，TUI 静默无响应。

## 5. 使用宿主能力：serviceID → 连接 → 缓存

agent 不直接创建 LLM 或工具进程，而是通过 broker 连接宿主挂载的聚合服务。三个要点：

**一是尽早建连。** `RegisterServices` 收到两个非零 ID 后立刻 eager connect（[#L214-L224](../plugins/agent-react-loop/main.go#L214-L224)），因为 broker 的 `ConnInfo` 是一次性握手且有 5 秒超时，拖到第一次 `Run` 才连容易超时。

**二是缓存连接。** `broker.Dial` 不能反复调用，`ensureConnected` 把 `*grpc.ClientConn` 与 typed client 缓存在结构体里，用独立互斥锁保护（[#L1681-L1706](../plugins/agent-react-loop/main.go#L1681-L1706)）。

**三是连接与状态分离。** `llmServiceID` / `toolServiceID` 由 `mu` 保护，`llmClient` / `toolClient` 由 `connMu` 保护，避免「持锁 Dial」造成长时间阻塞。

## 6. 一次 Run 的完整时序

`runLoop`（[#L254](../plugins/agent-react-loop/main.go#L254)）是整份代码的主干，按顺序做这些事：

1. **建立可取消 ctx**：新 Run 先取消上一个，保存 `cancelFunc`，`runWg.Add(1)`。
2. **校验并取连接**：`serviceID == 0` 直接报错，随后 `ensureConnected`。
3. **拉一次工具目录**：`toolClient.ListTools`，对工具名 `sort.Strings` 后与上次比较，只有变化时才重建 system prompt（[#L290-L326](../plugins/agent-react-loop/main.go#L290-L326)）。
4. **恢复或新建会话**：事件溯源 `session.Session` + `session.Store`，从磁盘恢复历史并折叠出审批策略、历史注入上限等会话状态。
5. **构建 system prompt**：基础身份 + 预设 persona + PTC 指引 + plan/goal 段落 + **聚合的 `ListContext` 片段** + sandbox/approval 策略快照（[#L1198-L1260](../plugins/agent-react-loop/main.go#L1198-L1260)）。
6. **派生请求历史**：`DeriveMessagesLimited(historyInjection)`，再用 `dsc.ValidateToolPairing` / `dsc.RepairToolPairing` 守住工具配对（[message_guard.go#L9-L24](../plugins/agent-react-loop/message_guard.go#L9-L24)）。修复只作用于本次请求副本，事件日志仍是 append-only 事实源。
7. **算上下文占用并裁剪**：只读 `dsc.TokenMeter` 的投影值；真超窗时用 `dsc.SnapRetainStart` 找不破坏工具配对的切点（[#L599](../plugins/agent-react-loop/main.go#L599)）。
8. **流式请求 LLM**：`llmClient.ChatStream`（[#L711](../plugins/agent-react-loop/main.go#L711)），把文本增量发成 `streaming` 帧、思考内容发成 `reasoning` 帧。
9. **执行工具批次**：只读工具按 `dsc.ToolCallReadOnly` 判定后并行（[parallel_tools.go](../plugins/agent-react-loop/parallel_tools.go)），其余串行；统一经 `toolClient.ExecuteTool` 转发（[#L1038](../plugins/agent-react-loop/main.go#L1038)）。
10. **落盘并产出帧**：每次调用都写一条 `ToolResult` 事件；成功帧携带 `ToolArgs` 与 `ToolView`（结构化视图透传），失败帧携带错误；策略 `notices` 以 `source=guard` 的合成用户消息追加。
11. **收尾**：无工具调用且无新注入时发 `success` 帧，带 `usageSnapshot()`；达到迭代上限发 `error` 帧。

`emit` 为 nil 时同一套逻辑就是非流式 `Run`——不需要维护两份循环。

## 7. 上下文与压缩：只认一个计量口径

这个插件最容易被写错的地方是「上下文占用怎么算」。它的做法是：

- **唯一来源** `dsc.TokenMeter`（[#L130-L132](../plugins/agent-react-loop/main.go#L130-L132)）：服务端实测锚点 + 自锚点以来的启发式增量。裁剪、压缩、提示文案、超窗判定全部读它，不允许各处再用估算另算一遍。
- **帧里上报投影值**：`Usage.ContextTokens` 就是 `meter.Project()` 的结果，宿主与 TUI 显示的上下文占用与插件决策同源。
- **压缩后端优先**：`ListContext` 返回内容里若含 `[DSC_COMPACTION_BACKEND_ACTIVE]` 标记，说明 `dsc-system` 或 `dsc-billion-context` 已接管，循环跳过内联 `compactHistory`；标记被剥离后再进 system prompt，对模型不可见（[#L1235-L1246](../plugins/agent-react-loop/main.go#L1235-L1246)）。
- **切点安全**：压缩保留区用 `dsc.SnapRetainStart` 计算，保证不会切在 assistant `tool_call` 与 tool 结果之间。

这体现了「能力边界」的意义：压缩策略归插件，agent 只判断「有没有后端」，不解读后端的领域语义。

## 8. 事件订阅：Hook.OnEvent 的正确姿势

`handleHostEvent`（[#L1773-L1812](../plugins/agent-react-loop/main.go#L1773-L1812)）是一个典型的 emit 型 listener：

```go
func (a *ReactLoopAgent) handleHostEvent(_ context.Context, eventType, dataJSON string) (string, error) {
	if eventType != string(dsc.EventApprovalAsked) &&
		eventType != string(dsc.EventApprovalDecided) &&
		eventType != string(dsc.EventApprovalPolicy) {
		return "", nil
	}
	// 解析载荷 → 校验会话归属 → Append 事件 → Save
	return "", nil
}
```

规则有三条：

1. **只处理关心的事件**，其余立即返回 `("", nil)`。
2. **必须校验会话归属**：跨进程事件带 `Session` 字段，不属于当前会话就跳过，否则会把别人的审批审计写进自己的日志。
3. **返回 `("", nil)` 表示不改写载荷**。emit 模式下宿主忽略返回值；waterfall 模式才需要在 `result_json` 里透传改写结果，插件侧不感知 `next()`。

分发模式由宿主按事件名查 `eventDispatchMode` 表决定，listener 不需要也不应该自己判断模式。

## 9. 规范对照：这份代码怎么守住约束

| 规范 | 在本插件中的体现 |
| --- | --- |
| 插件不 import `core` | import 只有 `dsc-sdk`、`dsc/proto`、`dsc/session`；需要的能力经 SDK 转发 |
| 依赖能力，不依赖插件名 | `Requires: {Type:"llm", Capability:"llm"}`，换 LLM 实现零改动 |
| 运行态状态走 RPC / 事件 | serviceID 经 `RegisterServices` / `SetUserQuestionsService` 注入，审批策略经 `approval/policy` 事件同步 |
| env 仅作部署期配置 | `DSC_CONTEXT_WINDOW`、`DSC_SANDBOX_POLICY` 等在 `newAgent()` 启动时读一次（[#L1892-L1955](../plugins/agent-react-loop/main.go#L1892-L1955)），不承担运行期同步 |
| 有序产物显式排序 | 工具名 `sort.Strings` 后再比较，避免 prompt 前缀漂移 |
| 三值数值语义 | `historyInjection`：负值不注入 / 0 未定义 / N>0 最近 N 条 |
| 结构化视图不丢字段 | 工具结果帧同时携带 `ToolArgs`、`ToolResult`、`ToolView` |
| 工具 panic 不拖垮会话 | 依赖 SDK 与宿主三层 recover，插件只对自建 goroutine 用 `dsc.SafeGoroutine` |
| 路径规范 | 会话目录一律 `dsc.SessionDir()`，不写相对路径 `sessions`，也不用 `filepath` 黑名单 API |

## 10. 如何测试

```bash
cd plugins/agent-react-loop
go test ./...
```

测试分层值得照抄：

- **单元层**：`message_guard_test.go`（工具配对修复）、`parallel_tools_test.go`（并行判定）、`history_limit_test.go`（三值语义）、`oversize_trim_test.go`（超窗裁剪）。
- **并发层**：`inject_race_test.go` 覆盖「模型输出中注入消息」的竞态；`inject_message_test.go` 验证消息在下一轮可见。
- **链路层**：[e2e_test.go](../plugins/agent-react-loop/e2e_test.go) 用宿主同款 `go-plugin` 配置真实 spawn 插件进程，验证 `Name` / `Version` / `SwitchSession` / `SetPlanMode`。完整 `Run` 需要宿主挂载 LLM + Tool 聚合服务，属宿主集成测试范畴。
- **视图层**：`view_frame_test.go` 断言 `ToolView` 一路透传到帧。

跨层搬运数据时，务必像 `view_frame_test.go` 那样做**字段保真往返**断言，而不是只断言「能拿到内容」。

## 11. 从零写一个新插件的最小清单

1. `plugins/<name>/go.mod`，`require dsc-sdk`（本地 replace）。
2. `main()` 里 `dsc.New(dsc.Config{Name, Version, Type, Provides?, Requires?})`。
3. 按类型注册：`sdk.Tool` / `sdk.ToolProvider` / `sdk.Hook` / `sdk.LLM` / `sdk.Agent` / `sdk.Policy`。
4. 需要宿主能力时，用 `sdk.SetInterconnect` 或 `sdk.AgentBroker` 取客户端，不用 env 传 serviceID。
5. 工具必须写清 `Description()`（输入格式、何时不该用、出错如何应对）与 `Capabilities`（`read-only` / `writes-path:<arg>` / `writes-unknown`）。
6. 需要贡献 system prompt 时实现 `Hook.ContextFn`；需要改工具行为时实现 `BeforeTool` / `AfterTool` / `OnEvent`。
7. `sdk.Serve()` 交给 SDK 组装 gRPC server 与 go-plugin 握手。
8. 加测试后 `gofmt -w .`，再对本插件支持的平台做交叉编译。

## 12. 常见坑

| 坑 | 症状 | 正确做法 |
| --- | --- | --- |
| 拖到首次 Run 才 Dial | broker 5 秒握手超时，插件收不到服务 | `RegisterServices` 里立即 eager connect |
| 每次调用重新 Dial | 连接失败或句柄泄漏 | 缓存 `*grpc.ClientConn`，加锁复用 |
| 把 serviceID 塞进 env | 动态加载/卸载后状态不同步 | 经 `RegisterServices` / `SetUserQuestionsService` 注入 |
| 事件不校验会话 | 串会话写入，日志互相污染 | 比对 `payload.Session` 与 `sess.ID()` |
| 自己估算 token | 与服务端实测口径分叉，反复误裁剪 | 只读 `dsc.TokenMeter` |
| 压缩切点随意取 | 切断 `tool_call` 与结果，端点 422 | 用 `dsc.SnapRetainStart` |
| 工具结果只转 `Content` | 结构化视图在宿主层静默丢失 | 同时透传 `ViewJson` |
| panic 不 recover | 宿主进程退出，会话中断 | 保持 SDK / 宿主 / 远程边界三层 recover |
| 工具名 / 片段遍历 map | prompt 前缀逐次漂移，缓存全失效 | 按名升序显式排序 |

## 参考

- [cordis-alignment.md](cordis-alignment.md)：DSC 与 Cordis 的逐项对照
- [agent-loop-hardening.md](agent-loop-hardening.md)：循环健壮性设计记录
- [../sdk/](../sdk)：SDK 类型别名与转发实现
- [../proto/dsc.proto](../proto/dsc.proto)：跨进程契约
- [../AGENTS.md](../AGENTS.md)：工程约束全文
