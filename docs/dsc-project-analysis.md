# DSC 项目分析报告：主要功能、扩展点、完成度与演进方向

> 分析对象：`/Users/apple/workspace/golang/dsc`（版本 `VERSION` = v3.0.0）
> 初版快照：HEAD = `8bd6e15`（修复前）
> 同步修订：HEAD = `7fa76e4`（修复后），2026-10-22
> 方法：全仓静态阅读（core / session / tui / sdk / plugin / plugins / proto / config / builder）+ `go build ./...` + 主模块单元测试实跑
> 说明：本报告为独立文档，不依赖对话上下文；所有结论均标注可复核的源码位置

---

## 修订状态（修复后同步）

初版基于 `8bd6e15`。其后共 14 个提交完成报告所列的高危/中危缺陷修复，本版已同步更新摘要、量化指标、成熟度矩阵、缺陷状态与演进路线图。

| 批次 | 提交 | 内容 |
|---|---|---|
| 契约正确性 | `cb05c53` `4c9592a` `ecfa775` `e34f505` `c952d1c` | 后台任务取消、macOS 大小写沙箱、会话 Fork 落盘、admin 路由/reload/ListContext/metadata/Shutdown、veto/parallel/重试取消/热重载退避/依赖状态 |
| 开发者体验 | `582a0ea` `420e3e7` | SDK 示例可编译、CI 工作流（gofmt + 根模块/子模块 + 七端交叉编译） |
| 平台能力 | `b70487d` `8821369` `90c1c84` `fdcaf95` | 沙箱写能力自声明、会话增量保存、MCP stdio、LSP 完整握手 |
| 文档与修正 | `d51a86f` `7aafcd6` `7fa76e4` | `Requires` 口径对齐、`default_llm` 合并透传修复、`tool-computer-use` macOS 15 ScreenCaptureKit fork |

> 阅读提示：5.3 与 6.x 中的**行号仍是初版快照**（修复后代码已变化），请以“状态”列为准；仍存在的条目标注为“仍存在”，可直接按位置复核当前代码。

---

## 摘要

DSC 是一套用 Go 实现的「编程代理（coding agent）」运行时，设计哲学与 DeepSeek Harness（DSH）同源：**一切皆插件**。它用 go-plugin + gRPC 把 LLM、Agent、Tool、Policy、通用 dsc 五类能力拆成独立子进程，由宿主统一装配、聚合与仲裁，并提供热插拔、热重载、能力边界声明、事件总线、工具流水线、事件溯源会话、TUI 与管理 API。

**核心判断：**

- **架构完成度高**：宿主/插件分层、五类插件契约、聚合服务、能力依赖、热重载、工具流水线三阶段、会话事件溯源均已落地并可编译运行（`go build ./...` 通过）。
- **功能面接近“可用产品”**：20 个插件覆盖 3 家 LLM、13 个工具域、压缩/技能/通知等核心能力；TUI 具备 15+ 斜杆命令；管理 API 提供 SSE 观测；内置 agentic-bench 评测插件。
- **工程质量中上，关键裂缝已封堵**：主模块与插件约 11.1 万行 Go（含测试），1039 个 Test/Benchmark 函数；主模块测试全绿，CI 已覆盖 gofmt、根模块/子模块构建与七端交叉编译。初版列出的后台取消、admin 路由、Fork 落盘、会话全量重写等缺陷已修复。
- **扩展性设计优秀、生态仍早期**：工具/LLM/Agent/Policy/Hook/视图/互联/脚本级（Lua、SQL）扩展面齐全，第三方插件无需改宿主；SDK 示例已恢复可编译，但插件脚手架、契约测试套件与拆分文档仍缺。
- **平台能力补齐**：MCP 已支持 HTTP/SSE 与 stdio，LSP 完成 initialize/didOpen/diagnostics 握手；沙箱写能力改为工具自声明；`tool-computer-use` 通过本地 ScreenCaptureKit fork 在 macOS 15 SDK 下恢复构建。
- **演进主线**：正确性批次基本收口，下一步重点转向开发者生态（脚手架/契约测试/文档拆分）、安全边界（admin 认证、插件隔离、OS 级沙箱）与多 Agent/远程执行。

---

## 1. 项目定位与设计哲学

### 1.1 定位

DSC 是一个**终端优先、插件化的编程代理运行时**，对标 DSH（TypeScript / Node.js / cordis），但选择 Go 全栈：

| 维度 | DSH | DSC |
|---|---|---|
| 语言/框架 | TypeScript + cordis | Go 1.26 + go-plugin/gRPC |
| 插件形态 | 同进程插件（fiber/scope） | 独立子进程（gRPC） |
| 隔离方式 | 内核级沙箱（bwrap/Landlock） | 工具级拦截（Windows 兼容） |
| UI | Web UI | TUI（bubbletea）+ 可选 Web UI 插件 |
| 会话 | 事件日志 + derive | 事件溯源 + `DeriveMessages` |

（对照详见 `docs/cordis-alignment.md` 与 `README.md`）

### 1.2 设计哲学

仓库把「时空可组合性」论文（`docs/A_Programming_Paradigm_for_Spatiotemporal_Composability.md`）作为理论背景，落到工程上是四条约束：

1. **能力边界而非硬编码**：插件用 `Config.Provides` / `Config.Requires` 自描述依赖，宿主扫描匹配（`core/capability_deps.go:12-22`），禁止在 `core/` 硬编码插件名（`AGENTS.md` 第 8 条）。
2. **事件驱动而非直接调用**：宿主 EventBus 提供 `emit / waterfall / serial / bail / parallel` 五种分发（`core/eventbus.go:197-280`），跨进程经 `PluginHookService.OnEvent`（`core/notify.go:218`）。
3. **失败不牵连**：插件 panic 三层 recover（SDK 层 / 宿主执行层 / 远程边界层），热重载失败不触碰旧实例。
4. **确定性前缀**：进入模型请求的有序产物按名稳定排序，保障提示前缀缓存命中（`AGENTS.md` 第 6 条）。

---

## 2. 架构总览

### 2.1 启动链路

`main.go:278 main()` 的主干：

1. `dsc version` / `dsc setup` 短路；注入 `DSC_*_DIR` 目录契约（`main.go:304-329`）。
2. 解析 `-log / -mode / -input / -headless / -debugger / -admin / -hooks / -patch`。
3. 装配日志扇出（`core.NewLogFanout`），使管理 API 的 `/plugins/logs` 在默认静默时仍可观测。
4. 读取 preset（`config/presets/<mode>.yaml`）与 `config/config.yaml`，`-patch` 叠加，计算 workspace 根（正斜杆）。
5. `core.NewManager`（`core/manager.go:213`）→ 注册内置工具、沙箱/审批瀑布、LLM 重试监听。
6. `assembleMerged`（`main_assemble.go:76`）合并插件集：LLM/agent 从 config 或目录发现；tool/policy/dsc 按「preset 优先、config 补缺」去重。
7. `LoadFromConfig` 加载插件；失败则「备份坏版 → 还原最近正常备份 → 重建插件集重试」一次（`main.go:759-806`），并做插件目录快照恢复。
8. 启动成功即备份配置与插件目录；非 headless 时启动 MCP 自动连接、热重载 watcher、cron、按需管理 API。
9. 进入 `runInputMode` / `-headless` / `tui.Run`；退出时 `mgr.Shutdown()` 与信号处理（`signal_unix.go` / `signal_windows.go`）。

### 2.2 插件运行时

**五类插件 + 正交服务：**

| 类型 | 契约 Service | 关键行为 | 宿主加载点 |
|---|---|---|---|
| `tool` | `ToolService`（ExecuteTool/ListTools/ListContext/SetInterconnect） | 聚合进 ToolRegistry，供模型调用 | `core/manager.go:2114` |
| `llm` | `LLMService`（Chat/ChatStream/HealthCheck） | 聚合为 primary + fallback 路由 | `core/llm_router.go:21` |
| `agent` | `AgentService`（RunStream/RegisterServices/...） | agent 是 broker 提供者，宿主反向注册聚合服务 | `core/manager.go:1895` |
| `policy` | `PolicyService.OnEvent` | 桥接到工具流水线三槽位 | `core/tool_pipeline.go:289` |
| `dsc` | 无独立 RPC；按 `PluginInfo.services` 机械桥接 tool/policy/hook | 核心能力混合体（压缩、技能、超时、外置、提醒） | `core/manager.go:2181` |

**生命周期状态机**：`PENDING → SPAWNED → CONNECTING → READY → ACTIVE → UNLOADING → DISPOSED`（`core/lifecycle.go:17-41`），迁移白名单 + 事件发布（`core/manager.go:375`）。

**聚合与 broker**：每个连接有独立 `(serviceID, addr)`，宿主把聚合 LLM / Tool / UserQuestions / Notify / AgentBridge 挂到 broker 上，再以 service ID 注入 agent（`core/aggregate_attach.go:27`、`core/notify.go:31`、`core/agent_bridge.go:61`）。热重载换进程后必须在新 broker 上重挂。

### 2.3 事件、钩子与上下文

- **宿主内**：`EventBus.Emit/Serial/Bail/Waterfall/Parallel`（`core/eventbus.go`），监听器 panic 一律 recover。
- **跨进程**：`dispatchEventToPlugins` 按 `eventDispatchMode` 表（`core/agent_events.go:102`）分发给 `PluginHookService.OnEvent`；waterfall 语义由宿主按插件顺序串联 `result_json` 实现（`core/notify.go:218`）。
- **工具钩子**：`BeforeTool` 可 veto/改写参数，`AfterTool` 可改写结果（`core/notify.go:104/152`）。
- **上下文贡献**：`Hook.ContextFn` / `ListContext` 聚合为 system prompt 片段（`core/manager.go:1303`）。
- **外部脚本钩子**：`-hooks` 指向 `hooks.json`，支持严格 Lua（进程内 go-lua）或原生可执行文件（`core/lua_hooks.go:70`）。

### 2.4 会话模型

事件溯源：`Session` = append-only `events` + `surfaceNodes` 投影（`session/session.go:182-192`）。

- **surface 事件**（进入模型历史）：`user/message`、`assistant/message`、`tool/result`、`compaction/summary`、`system/message`；其余为 log-only（`session/session.go:22-87`）。
- **不变量**：surface node 0 必须是 `system/message`；违反直接 panic（`session/session.go:264`）。
- **派生**：`DeriveMessages` 支持 `history_injection` 三态与 TOON 化工具结果（`session/session.go:318/500`）。
- **持久化**：JSONL，`.tmp` + rename 原子替换（`session/persist.go:174`）。
- **多会话**：按项目路径生成 key（`session/store.go:29`），`/session new|list|switch|delete` 管理。
- **派生状态**：plan/goal/todo 由事件折叠恢复（`session/plan.go`、`goal.go`、`todo.go`）。

---

## 3. 主要功能

### 3.1 插件运行时与生命周期能力

| 能力 | 实现 | 说明 |
|---|---|---|
| 热插拔 | `LoadPlugin` / `UnloadPlugin` / `SwitchMode` | 运行期加载/卸载，工具注册表实时增删 |
| 热重载 | `core/hot_reload_watch.go:33`、`hot_reload_version.go:14`、`manager.HotReload:628` | `<name>-v<semver>` 版本化二进制；fsnotify + 5s 轮询；准备阶段不持锁、交换阶段短临界区 |
| 配置自愈 | `main.go:759-806`、`core/config_backup.go` | config 与 preset 各自备份/还原，坏版留档，降级启动 |
| 插件目录自愈 | `core/plugins_backup.go` | “上次正常”快照回拷，孤立插件只告警不删除 |
| 能力依赖 | `core/capability_deps.go`、`core/inject.go` | `requires/<type>/<cap>` 编码；多 provider fail-loud；PENDING 反应式重算 |
| 凭据隔离 | `core/manager.go:2429` | 子进程 env 白名单，仅 LLM 插件放行 `*_API_KEY` 等 |
| 宿主存活看门狗 | `sdk/host_watchdog.go` | 宿主 PID 检测，连续 3 次失败插件自杀，防孤儿进程 |
| 模型自助插件管理 | `core/dsc_plugin_tool.go` | 6 个工具：install/upgrade/uninstall/list/load/unload |

### 3.2 Agent 与 LLM

- **Agent**：`agent-react-loop` 插件实现 ReAct 主循环、流式输出、事件溯源会话、plan/goal、todo 追问、内联压缩兜底、`llm/attempt` 诊断留痕、输出截断防护（约 2.4k 行）。
- **LLM provider**：`llm-openai` / `llm-anthropic` / `llm-ollama`，均支持流式、工具调用、usage 与缓存命中统计；前两者支持视觉输入与本地 llama.cpp 端点探测（`meta.n_ctx`）。
- **统一推理强度**：`DSC_REASONING_EFFORT=low|medium|high|max` 跨 provider 映射（README “统一推理强度”）。
- **压缩**：`dsc-system` 内驻 `compaction-basic`（80% 阈值、16% 尾部、聚合 LLM 摘要、溢出紧急压缩），`dsc-billion-context` 提供 ACP 分层压缩与检索（约 4.1k 行）。
- **token 计量**：服务端 usage 优先，缺失时字节级启发式估算（`core/token_meter.go`）。
- **多 Agent 编排**：`subagent`（宿主内置）+ `workflow`（Lua 编排，扇出 `agent/parallel/pipeline`，支持后台）+ `jobs` 管理。

### 3.3 工具系统

- **内置工具**：`subagent`、`workflow`、`run_code`、`job_output/list/kill`、6 个插件管理工具、4 个 cron 工具（`core/manager.go:269-283`）。
- **工具流水线**：`tools/pre-execute` → `tools/execute` → `tools/post-execute` → `tools/result`（`core/tool_pipeline.go:111`），支持 veto、结果改写、notice 注入、三层 panic recover。
- **审批**：全局/会话策略 `ask|never`；沙箱升级审批（`sandbox_permissions` + `justification`）；工具声明式审批（`ApprovalRequester`）；审计写入会话日志（`core/approval.go`）。
- **沙箱**：三档 `read-only / workspace-write / full-access`，pre-execute fail-closed；workspace 包含判定用真实路径解析防 symlink/junction 穿越（`core/sandbox.go:62-179`）。
- **结构化视图**：插件可声明 `view_json`（card/table/plain），TUI 单一渲染器统一绘制（`sdk/view.go`、`tui/`）。
- **PTC（程序化工具呈现）**：`run_code` 用严格 Lua 一把过组合多步工具；`-mode ptc` 时折叠为唯一工具（`core/run_code.go:24`、`coderuntime/`）。
- **图像**：`ImagesFn` 返回 data URL，宿主做图像准入与投影（`core/image_admission.go`）。

### 3.4 会话、任务与调度

- 多会话、项目级隔离、`/export` 导出、`fork`、`resume`。
- plan / goal / todo 三个领域，均由事件折叠恢复。
- cron：宿主调度器 + 模型工具 + TUI `/cron` + 管理 API。
- jobs：后台任务注册表（`jobs/`），TUI `/jobs list|output|kill`。

### 3.5 交互与可观测

- **TUI**：流式渲染、Markdown、diff 视图、拖选复制、状态行指标（tokens/s、缓存命中）、15 个斜杆命令（`/approval /clear /cron /crons /exit /export /help /jobs /plan /quit /sandbox /session /sessions /settings /skills`）。
- **管理 API**（`core/admin.go:40-103`）：插件 load/unload/reload/list/tools/metadata、events/domain-events/logs 三条 SSE、debugger snapshot、cron、credentials、feedback、LSP、MCP。
- **评测**：`bench.go` + `tool-agentic-bench` 插件（18 个用例），支持真机 agentic 评测。
- **构建**：`builder/builder.go` 七端交叉编译、CGO 插件本机构建、附带资源目录、内部插件可选打包。

### 3.6 插件清单（20 个）

| 插件 | 类型 | 作用 | 规模（非测试，约） | 状态 |
|---|---|---|---|---|
| `agent-react-loop` | agent | 主 Agent 循环 | 2.4k | 完整 |
| `dsc-system` | dsc | 超时/外置/提醒/技能/压缩/图像外置 | 2.3k | 完整 |
| `dsc-billion-context` | dsc | ACP 分层压缩与检索 | 4.1k | 较完整 |
| `dsc-notify` | dsc | 回合音效通知 | 0.4k | 完整 |
| `llm-openai` / `llm-anthropic` / `llm-ollama` | llm | 三家 provider | 1.7k | 完整 |
| `tool-filesystem` | tool | shell + 进程内 POSIX 命令 | 3.0k | 完整 |
| `tool-str-replace-editor` | tool | 文件编辑 | 0.5k | 完整 |
| `tool-browser-use` | tool | 浏览器/抓取/搜索 | 1.1k | 完整 |
| `tool-pdf` | tool | 13 个 PDF 工具 | 4.0k | 完整（CJK 字体需自备） |
| `tool-ssh` | tool | SSH 持久会话 | 0.3k | 完整 |
| `tool-lua-host` | tool | Lua 脚本工具宿主 | 1.5k | 完整 |
| `tool-sql-host` | tool | `.dsp` SQLite 插件宿主 | 3.4k | 完整 |
| `tool-memory-service` | tool | 记忆检索/自动记忆 | 0.7k | 完整 |
| `tool-lisp-eval` | tool | 精确有理数 Lisp 求值 | 0.4k | 完整 |
| `tool-musicplayer` | tool | 音乐播放 | 0.6k | 完整 |
| `tool-computer-use` | tool | 桌面自动化（CGO） | 0.9k | 可用；macOS 15 经 ScreenCaptureKit fork 构建 |
| `tool-agentic-bench` | tool | 评测驱动 | 0.9k | 完整 |
| `tool-harness-webui` | tool | SvelteKit Web UI | 0.3k + 前端 | 原型/取巧 |

---

## 4. 扩展点

### 4.1 扩展点总表

| 扩展形态 | 契约入口 | SDK 入口 | 需要改宿主？ | 成熟度 |
|---|---|---|---|---|
| Tool 插件 | `proto.ToolService` | `sdk.Tool(...)` / `sdk.ToolProvider(...)` | 否 | 成熟 |
| LLM 插件 | `proto.LLMService` | `sdk.LLM(core.LLMProvider)` | 否（版本范围写死 `>=1.0,<2.0`） | 成熟 |
| Agent 插件 | `proto.AgentService` | `sdk.Agent(...)` + `sdk.AgentBroker` | 否（但宿主仅支持 1 个 agent） | 成熟但受限 |
| Policy 插件 | `proto.PolicyService` | `sdk.Policy(...)` | 否 | 可用（无 interconnect） |
| 通用 dsc 插件 | `PluginInfo.services` 正交声明 | `sdk.New(TypeDsc)` | 否 | 成熟 |
| 工具前后钩子 | `PluginHookService.BeforeTool/AfterTool` | `sdk.BeforeTool/AfterTool` | 否 | 成熟 |
| 事件订阅 | `PluginHookService.OnEvent` | `sdk.Hook.OnEvent` | 新事件需宿主 Emit | 成熟 |
| system prompt 贡献 | `PluginHookService.ListContext` | `sdk.Context(fn)` | 否 | 成熟 |
| 结构化视图 | `view_json` | `sdk.CardView/TableView/PlainView` | 否 | 成熟 |
| 结果图像 | `images[]` | `sdk.Tool.ImagesFn` | 否 | 成熟 |
| 插件互联 | `SetInterconnect` | `sdk.Interconnect`（LLM/Tool/Notify/Agent） | 否 | 成熟 |
| 事件上报 | `PluginNotifyService.Notify` | `ic.Notify(...)` | 否 | 成熟 |
| 能力声明/依赖 | `Config.Provides/Requires` | `sdk.Config` | 否 | 部分生效（见 5.3） |
| 审批声明 | `ApprovalRequester`（宿主接口） | 无 SDK 对应字段 | 是 | 缺口 |
| 脚本级工具 | `tool-lua-host` | `dsc.register_tool(...)` | 否 | 成熟 |
| 数据级插件 | `tool-sql-host` `.dsp` | SQLite + Lua | 否 | 成熟 |
| 技能 | `SKILL.md` 目录 | `skill`/`install_skill`/`uninstall_skill` | 否 | 成熟 |
| MCP | 宿主内置 | — | 是（HTTP/SSE + stdio） | 可用 |
| LSP | 宿主内置 | — | 是（握手/诊断已完成；hover 等未工具化） | 可用 |
| Web UI | 独立插件 | SvelteKit + go:embed | 否 | 原型 |
| 外部脚本钩子 | `hooks.json` | Lua / 可执行文件 | 否 | 成熟 |
| 模式/preset | `config/presets/*.yaml` | — | 否 | 成熟 |
| 平台打包 | `builder/builder.go` | — | 是 | 成熟 |

### 4.2 五类插件契约要点

**Tool**（`sdk/tool.go:23`）：`Name / Description / Schema / Handler / ViewFn / ImagesFn / Context(ContextFn) / Capabilities`。Wire 请求含 `session_id`、`approval_policy`，但宿主 `RemoteTool` 未填充、SDK Handler 不暴露（`core/remote_tool.go:70`），插件既拿不到会话身份也无法参与审批对话。

**LLM**（`core/grpc_llm.go:20`）：`Chat / ChatStream / Name / Version / HealthCheck / VisionEnabled`。响应携带 `usage`（含 cache 读写）与 `reasoning`。

**Agent**（`core/interface.go:35`）：`RunStream / RegisterServices / SwitchSession / SetPlanMode / SetHistoryInjection / SetUserQuestionsService / Shutdown / InjectMessage / DebugSnapshot`。

**Policy**（`proto/policy`）：三槽位 `tool/pre-execute`（deny）、`tool/execute`（allow + `TimeoutSpec` 看门狗）、`tool/post-execute`（replace），并可返回 advisory `notice`。

**Dsc**（`sdk/dsc.go:30`）：唯一可同时注册 Tool + Policy + Hook 的类型；宿主按 `PluginInfo.services` 机械桥接。

### 4.3 扩展点设计评价

**优点：**

1. **零宿主改动即可加工具/模型/钩子**——插件目录 + config 条目即可生效，热加载支持运行期增删。
2. **横切关注点齐全**——超时、外置、重复提醒、压缩、技能都做成了插件/策略，而不是写死在 core。
3. **多级扩展粒度**——从「独立进程插件」到「Lua 脚本工具」到「SQLite 数据插件」到「SKILL.md 技能」，覆盖不同门槛的贡献者。
4. **契约有演进机制**——语义版本范围 + 版本化二进制热重载 + 元数据校验。

**限制：**

1. **类型与服务只正交了一半**：只有 `TypeDsc` 能同时提供 tool+policy+hook；`TypeTool` 调 `sdk.Policy` 不会注册；`TypePolicy` 无 `SetInterconnect`，策略插件不能调 LLM/工具；`TypeAgent/TypeLLM` 不能自带工具。
2. **`services` 字段只对 dsc 类型填充**，与 `metadata.proto` 注释不一致。
3. **插件审批不可声明**：`ApprovalRequester` 是宿主侧接口，远程插件无对应协议字段。
4. **`ListContext` 无会话上下文**，宿主固定 3 秒超时；`ToolService.ListContext` 成为历史残留。
5. **事件分发跨进程缺 `parallel`**，且 `OnAny` 同步串行分发会让慢插件阻塞高频事件。
6. **MCP / LSP 位于宿主而非插件**，与“一切皆插件”存在张力，新增传输/能力必须改宿主。

---

## 5. 完成度评估

### 5.1 量化指标

| 指标 | 数值 | 说明 |
|---|---|---|
| 提交数 | 745 | 2026-08-15 ~ 2026-10-22；其中初版后新增 14 个修复提交 |
| 版本 | v3.0.0 | `VERSION` |
| Go 版本 | 1.26.0 | `go.mod` |
| 主模块 + 插件 Go 代码 | ≈11.1 万行（含测试） | 不含 `libs/` |
| 其中测试代码 | ≈3.95 万行 | 占比约 36% |
| Test/Benchmark 函数 | 1039 | core/session/tui/cron/jobs/sdk/plugins/coderuntime |
| 插件数 | 20 | 1 agent + 3 llm + 3 dsc + 13 tool（`tool-computer-use` 已可在 macOS 15 构建） |
| `go build ./...` | 通过 | 实测 exit 0 |
| `go test`（主模块） | **全部通过** | `core`（约 64s）与 `session/tui/cron/jobs/coderuntime/lualib` 均 ok；初版失败的 macOS 沙箱用例已转绿 |
| CI | 已建立 | `.github/workflows/ci.yml`：gofmt、根模块 build/vet/test、24 项子模块矩阵、七端交叉编译 |
| 文档 | README（约 74KB）+ 2 篇设计文档 + 本报告 | 仍缺插件开发指南、API 参考、架构图 |

> 说明：`plugins/*` 为独立 Go module。CI 已对 18 个插件模块执行 `go build`（跳过 CGO 的 `tool-computer-use` 与需 bun 预构建的 `tool-harness-webui`）；插件单测仍未纳入矩阵。七端交叉编译（含 freebsd gcflags）已实际执行并通过。

### 5.2 分层成熟度矩阵

| 层次 | 成熟度 | 判断依据 |
|---|---|---|
| 插件运行时（加载/聚合/卸载） | 成熟 | 五类契约齐备，20 个插件在跑，热插拔可用 |
| 工具流水线 / 审批 / 沙箱 | 成熟 | 三阶段 + 双入口审批 + 三档沙箱；沙箱写能力已改为工具自声明 capability（`writes-unknown` / `writes-path:<arg>` / `read-only-when:<arg>=<value>`），旧插件保留 legacy 回退 |
| 会话事件溯源 | 成熟 | 不变量严格、派生稳定；`Store.Fork` 可落盘；保存改为增量 append（异常自动回退全量） |
| Agent / LLM | 成熟 | ReAct、三家 provider、流式、工具调用、usage；`default_llm` 已正确透传为 primary |
| 压缩 / 技能 / 记忆 | 成熟 | 两个压缩后端、SKILL.md、SQLite 记忆 |
| 事件与钩子 | 成熟 | 五种模式齐备；跨进程 `parallel` 已实现；pre-step veto 真正阻断请求 |
| TUI | 成熟 | 15 个斜杆命令、流式/Markdown/diff/状态指标 |
| 管理 API | 可用 | 路由前缀已修复，metadata 返回 capabilities；仍未认证默认化，`/plugins/load` 风险仍在 |
| 热重载 | 可用 | 两阶段切换 + 失败指数退避（上限 5 分钟）；LLM per-provider 服务不刷新仍遗留 |
| MCP / LSP | 可用 | MCP 支持 HTTP/SSE + stdio；LSP 完成握手、文档同步与诊断推送；尚无 hover/definition 工具化 |
| Web UI | 原型 | 依赖 bun 构建、空工具集取巧 |
| 开发者生态 | 基础可用 | CI 与可编译示例已就位；仍缺插件脚手架、契约测试套件与拆分文档 |

### 5.3 已验证的缺陷与风险

按初版影响排序；“状态”列反映修复后（HEAD `7fa76e4`）的结论，位置为初版快照。

| 严重度 | 位置（快照） | 现象 | 影响 | 状态 |
|---|---|---|---|---|
| 高 | `core/tool_pipeline.go:437-457` | `cancelCh` 只写不读，执行 ctx 不可取消 | `job_kill` 对后台工具无效 | ✅ 已修复：取消真实 job ctx，并与回合生命周期解耦 |
| 高 | `core/admin.go:83-103` + `admin_credentials_feedback.go:43` / `admin_lsp_mcp.go:22,36` | `Group(prefix)` 与路由路径重复拼接 | 实际路径 404 | ✅ 已修复：子路由相对注册，路由表测试守护 |
| 高 | `core/manager.go:1749-1769`、`core/inject.go:80` | 启动路径不按依赖门控，与 README 的 PENDING 语义不一致 | 契约漂移 | ✅ 已按方案 A 对齐文档：明确“启动期先加载后解析” |
| 高 | `core/manager.go:1303-1324` | `clients` 过滤 nil 后与未过滤 `names` 同下标配对 | `ListContext` 片段错归属 | ✅ 已修复：直接按 `toolHookOrder` 配对 |
| 高 | `session/fork.go:36` | Fork 返回空 id，无法保存 | Fork 结果不可用 | ✅ 已修复：新增 `Store.Fork` 分配 id 并落盘 |
| 高 | `core/manager.go:1394-1397` + `admin.go:183-199` | reload = unload（含移除配置）+ load | 失败即内存/配置双丢 | ✅ 已修复：`ReloadPlugin` 快照旧条目并按需回滚 |
| 中 | `core/hot_reload_watch.go:129` | 无失败计数/退避 | 坏版本每 5s 重试 | ✅ 已修复：按候选指数退避（上限 5 分钟） |
| 中 | `core/manager.go:1685` vs `2111` | 启动路径全程持写锁并做慢 RPC | 启动期全局阻塞 | ⚠️ 仍存在 |
| 中 | `core/path_canon.go:54-64` | macOS 大小写不敏感卷未处理 | 大小写变体路径被误拒 | ✅ 已修复：文件身份（dev/ino）兜底，自带用例转绿 |
| 中 | `core/sandbox.go:89-102` | 只识别 `str_replace_editor` 与 `shell` | 新写文件工具不受约束 | ✅ 已修复：工具自声明写能力 token，legacy 回退 |
| 中 | `core/notify.go:237-319` | 跨进程分发无 `parallel` | 静默降级为 emit | ✅ 已修复：并发分发 + 错误聚合 |
| 中 | `core/llm_router.go:186-189` | pre-step veto 仅 warn | 插件无法拦截请求 | ✅ 已修复：返回 error 即中止请求 |
| 中 | `core/manager.go:1172-1184` | `Shutdown` 未清理完整运行态 | 复用 Manager 时残留工具/serviceID | ✅ 已修复：归零注册表、broker、LSP/MCP 等 |
| 中 | `session/persist.go:174` | 每次保存全量重写 | 长会话 O(n²) | ✅ 已修复：增量 append + 校验失败回退全量 |
| 中 | `core/manager.go:269-283` | 内置工具注册错误全部忽略 | 同名冲突时静默消失 | ⚠️ 仍存在 |
| 中 | `plugins/tool-harness-webui` | `ToolProvider` 返回 nil、依赖不可解析的 `dsc/admin` | 形态取巧，发布需 bun | ⚠️ 仍存在 |
| 中 | `sdk/examples/*`、`sdk/README.md` | 示例签名过期、replace 失效 | 示例不可编译 | ✅ 已修复：示例可离线构建，文档同步 |
| 低 | `builder/builder.go:83` | 引用 3 个不存在的内部插件 | 发布清单不一致（自动跳过） | ⚠️ 仍存在 |
| 低 | `core/lsp.go`、`main_mcp_autoload.go:49` | LSP 握手 TODO；MCP 仅 HTTP/SSE | 能力名不副实 | ✅ 已修复：LSP 握手/诊断、MCP stdio |
| 低 | `core/admin.go:21` | 未设 token 即无认证；load 接受任意 BinaryPath | 暴露到 0.0.0.0 时可远程执行 | ⚠️ 仍存在 |
| 低 | `core/mcp_client.go` | MCP 工具不经插件隔离 | 与“插件进程隔离”模型不一致 | ⚠️ 仍存在 |

### 5.4 工程质量观察

**做得好的：**

- 测试密度高（1039 个用例），且有针对“字段保真往返”和“真实插件进程链路”的集成测试（`core/host_*_chain_test.go`、`core/listcontext_test.go`）。
- 三层 panic 防线完整，工具异常不会杀宿主。
- 路径黑名单有哨兵测试（`core/filepath_guard_test.go`），确定性排序有明确规范。
- 平台差异用 build tag 拆分（信号、进程属性、看门狗）。
- 初版高危缺陷均已补回归测试：后台取消、admin 路由、ListContext、Fork、reload、macOS 大小写、MCP stdio、LSP 握手、热重载退避。
- CI 已覆盖 gofmt、根模块 `build/vet/test`、18 个子模块构建与七端交叉编译；示例可在离线环境构建。
- 新增两个 vendored fork（`libs/robotgo`、`libs/screenshot`）解决 macOS 15 SDK 删除 CoreGraphics 截图 API 的问题，并在哨兵测试与 CI 中逐条登记豁免理由。

**需要补的：**

- **测试盲区收窄但未清零**：启动期锁边界、审批门顺序、`NewManager` 注册错误仍无测试覆盖。
- **插件测试未进 CI**：子模块矩阵只做 `go build`（`sdk`/`plugin`/示例例外），插件单测仍依赖本地执行。
- **文档单点**：README 74KB 承担了全部说明，缺拆分后的开发者文档；`docs/` 目前为本报告 + 2 篇设计文档。
- **死代码/重复路径**：`LoadToolsAndPoliciesFromConfig`、`serveUserQuestionsLocked` 无调用；两套 LLM 服务路径并存（`llm_provider_server` 与聚合路由）。
- **安全默认值**：管理 API 未设 token 即无认证、MCP 工具不经插件隔离、沙箱仍是工具级拦截而非 OS 级边界。

---

## 6. 后续演进方向

演进建议按“先正确、再易用、后强大”的顺序组织。每条都给出触发依据与验收标准。

### 6.1 阶段一：契约正确性与可靠性（已基本收口）

| 事项 | 依据 | 验收标准 | 状态 |
|---|---|---|---|
| 后台任务真正可取消 | `cancelCh` 无读取方 | `job_kill` 后工具在超时内停止，且有 e2e 测试 | ✅ 已完成 |
| 修复 4 组 admin 路由前缀 | vodka `Group` 拼接语义 | `/credentials/list` 等实际可访问，路由表测试守护 | ✅ 已完成 |
| `Requires` 门控口径 | 启动加载不检查依赖 | 明确语义并消除文档漂移 | ✅ 按方案 A（文档对齐）关闭 |
| 修复 `ListContext` 索引错位 | nil 过滤后仍按下标配对 | 多插件含失效客户端用例 | ✅ 已完成 |
| 会话 Fork 可落盘 | `fork.go` 空 id | `Store.Fork` 可保存，测试覆盖 | ✅ 已完成 |
| reload 事务化 | 失败即丢配置 | 加载失败回滚配置条目 | ✅ 已完成 |
| 热重载失败退避 | 坏版本无限重试 | 连续失败指数退避并上报 | ✅ 已完成 |
| 启动期锁边界 | 持锁做慢 RPC | 共用“准备无锁 + 交换短锁”路径 | ⚠️ 仍待办 |
| 审批顺序修正 | toolApproval 先于 never | `requires-never-approval` 工具不先弹窗 | ⚠️ 仍待办 |
| 沙箱写能力自声明 | 硬编码工具名 | 工具声明写能力，未声明按 legacy/fail-closed | ✅ 已完成（token 实为 `writes-unknown` / `writes-path:<arg>` / `read-only-when:<arg>=<value>`） |
| 修复 macOS 大小写不敏感判定 | 自带回归用例失败 | 用例转绿 | ✅ 已完成 |
| 会话保存增量化 | 全量重写 | 无变化时 no-op、异常回退全量 | ✅ 已完成（原列于阶段三，提前完成） |

### 6.2 阶段二：开发者体验与可观测性

| 事项 | 依据 | 预期收益 | 状态 |
|---|---|---|---|
| 建立 CI（GitHub Actions） | 无 `.github/` | gofmt + build/vet/test + 七端交叉编译 | ✅ 已完成（插件单测尚未纳入） |
| 修复 SDK 示例与 README | 示例编译失败、文档漂移 | 新插件可照示例跑通 | ✅ 已完成 |
| 提供插件脚手架 | 手写 main.go + go.mod + replace 容易错 | `dsc plugin new tool-xxx` 生成模板 | ⚠️ 待办 |
| 插件契约测试套件 | 20 个插件重复写 e2e | SDK 提供统一契约校验 | ⚠️ 待办 |
| 拆分文档 | README 74KB 单点 | 架构/插件开发/运维文档 + API 参考 | ⚠️ 待办 |
| 补齐事件 `parallel` 与超时 | 跨进程分发缺分支 | 慢插件不阻塞高频事件 | ✅ `parallel` 已完成；超时仍为固定 30s |
| 管理 API 补认证默认值与 capabilities | 无 token 暴露风险、无法观测能力 | 默认 token + metadata 返回能力 | ✅ capabilities 已完成；默认 token 仍待办 |
| 结构化可观测 | 只有日志与 SSE | 接入 OpenTelemetry | ⚠️ 待办 |

### 6.3 阶段三：安全与平台能力

| 事项 | 依据 | 说明 | 状态 |
|---|---|---|---|
| 沙箱升级为 OS 级 | 目前是工具级拦截 | Linux Landlock/bwrap、macOS seatbelt、Windows 工具级 | ⚠️ 待办 |
| 插件通道 mTLS | fork 支持 `AutoMTLS` 但宿主未启用 | 多用户/远程插件场景必需 | ⚠️ 待办 |
| MCP 支持 stdio / WebSocket | 原仅 HTTP/SSE | 与主流 MCP 生态对齐 | ✅ stdio 已完成；WebSocket 待办 |
| LSP 完整握手 + 工具化 | 原 `admin_lsp_mcp.go` TODO | hover/definition/references 变成模型工具 | ✅ 握手/诊断已完成；hover/definition 工具化待办 |
| 会话存储增量 + 索引 | 原全量重写、List 全文件扫描 | 追加写 + 元数据 sidecar | ✅ 增量已完成；索引 sidecar 待办 |
| 多 Agent 并存 | 宿主只取第一个 agent | 按任务/角色选 agent | ⚠️ 待办 |
| 插件类型与服务完全正交 | 只有 dsc 类型可组合 | 任意类型可声明多服务 | ⚠️ 待办 |

### 6.4 阶段四：生态与差异化

| 事项 | 说明 |
|---|---|
| 插件市场/registry | 当前插件只能本地安装；可做索引 + 签名 + 版本约束 |
| 技能生态 | `skills/builtin` 仅 3 个、`installed` 1 个；可引入技能包与依赖声明 |
| 评测体系扩展 | `tool-agentic-bench` 18 用例；建议加入插件兼容性、压缩保真、沙箱逃逸回归集 |
| Web UI 产品化 | 当前为原型；与管理 API 统一鉴权与事件流 |
| 远程/多机执行 | 插件已是独立进程，天然适合远程 worker；可做 `tool-remote` 与分布式 jobs |
| 成本与缓存治理 | 已有 token meter 与前缀缓存意识；可做按会话成本报表与压缩策略自适应 |

### 6.5 建议的路线图节奏

```
阶段一（正确性）  ✅ 基本完成（剩余：启动期锁边界、审批门顺序）
        │
        ▼
阶段二（开发者体验）  🔄 进行中（CI、可编译示例已完成；脚手架/契约测试/文档拆分待办）
        │
        ▼
阶段三（安全/平台）  ⏳ 部分完成（MCP stdio、LSP 握手、会话增量已落地；OS 级沙箱、mTLS、多 Agent 待办）
        │
        ▼
阶段四（生态）      ⏳ 未开始（插件市场、远程 worker、成本治理）
```

---

## 7. 结论

1. **DSC 已经不是一个“架构演示”，而是一套可运行、可扩展的编程代理运行时**：五类插件、20 个实现、热重载、事件溯源会话、工具流水线、审批与沙箱、TUI 与管理 API 都已落地。
2. **它最强的资产是“扩展点的密度与一致性”**：从进程插件到 Lua 脚本到 SQLite 数据插件到 SKILL.md，几乎每一层都留了扩展缝，且多数不需要改宿主。
3. **它最大的风险曾是“契约漂移”，当前已大幅收敛**：Requires 门控、后台取消、admin 路由、Fork 落盘、会话保存等已在 14 个修复提交中收口，并配套回归测试与 CI。剩余风险集中在安全默认值（admin 无 token、MCP 工具隔离）与两处未改的并发/顺序问题（启动期锁边界、审批门顺序）。
4. **下一步的最优路径是开发者生态，而不是继续补功能**：CI 与示例已就位，但脚手架、契约测试套件与拆分文档仍缺；这三项决定外部插件能否规模化生长。
5. **差异化机会在于“Go + 进程插件 + 多形态扩展”**：相比 DSH 的同进程模型，DSC 天然适合远程插件、脚本插件与多语言插件宿主；把这条路线做深（远程 worker、插件市场、内核级沙箱）比追赶 Web UI 更有价值。

---

## 附录 A：关键文件索引

| 主题 | 文件 |
|---|---|
| 宿主入口/组装 | `main.go`、`main_assemble.go`、`setup.go` |
| 插件管理 | `core/manager.go`、`core/lifecycle.go`、`core/grpc_*.go` |
| 能力依赖 | `core/capability_deps.go`、`core/inject.go` |
| 事件/钩子 | `core/eventbus.go`、`core/agent_events.go`、`core/notify.go`、`core/lua_hooks.go` |
| 工具流水线 | `core/tool_pipeline.go`、`core/remote_tool.go`、`core/approval.go`、`core/sandbox.go` |
| PTC/编排 | `core/run_code.go`、`coderuntime/`、`core/workflow.go`、`core/subagent.go`、`jobs/` |
| MCP / LSP | `core/mcp_client.go`、`core/mcp_stdio.go`、`core/lsp.go`、`core/lsp_rpc.go` |
| 会话 | `session/session.go`、`persist.go`、`store.go`、`fork.go`、`goal.go`、`plan.go`、`todo.go` |
| 契约 | `proto/dsc.proto`、`proto/metadata/metadata.proto` |
| SDK | `sdk/sdk.go`、`tool.go`、`hook.go`、`agent.go`、`policy.go`、`view.go`、`interconnect.go` |
| 插件 fork | `plugin/client.go`、`grpc_client.go`、`runner/runner.go` |
| vendored fork | `libs/robotgo`、`libs/screenshot`（ScreenCaptureKit 补丁） |
| CI | `.github/workflows/ci.yml` |
| 管理 API | `core/admin.go`、`admin_cron.go`、`admin_credentials_feedback.go`、`admin_lsp_mcp.go` |
| 配置/构建 | `config/presets/*.yaml`、`builder/builder.go` |

## 附录 B：分析方法与局限

- **方法**：全仓静态阅读；`go build ./...` 实测通过；`go test ./core/ ./session/ ./tui/ ./cron/ ./jobs/ ./coderuntime/ ./lualib/` 实跑；七端交叉编译（含 freebsd gcflags）实测通过；`builder` 本机全量打包通过；对关键缺陷做了源码复核，并对修复后的 MCP stdio / LSP 握手 / tool-computer-use 做了真实子进程集成测试。
- **局限**：
  1. 插件单测仍未全部执行（`plugins/*/go.mod` 的 CI 仅 `go build`；`sdk`、`plugin`、示例模块跑了测试）。
  2. 真实模型端到端只做了冒烟（`-headless -input` + `computer_use_screen_size`），未跑完整 agentic-bench。
  3. 行数为 `wc -l` 统计口径，含空行/注释；插件规模为近似值。
  4. `libs/` 下 vendored 第三方（go-openai、anthropic-sdk-go、go-lua、sh、robotgo、screenshot 等）未纳入功能评价；后两者仅评估其补丁面。
  5. 部分结论依赖静态推断（如锁竞争、性能复杂度），需压测确认；5.3 中标“仍存在”的条目已复核，标“已修复”的均有对应测试或实跑验证。
