# AGENTS.md

本文件为 DSC 项目的 AI 开发代理提供工程约束与约定。开发代理在执行任务、修改源码与提交时须遵守《时空可组合性的编程范式》及以下规范；同时在编写本仓库代码时保持一致。

## 项目概况

- Go 语言项目，采用「宿主 + 插件」架构（go-plugin / gRPC），宿主管理 LLM、工具、Agent 等插件子进程的生命周期。

- 目录概览：`main.go`（宿主入口）、`plugin/`（插件管理、Agent/LLM/Tool 契约与 ADMIN API）、`plugins/`（各插件实现）、`proto/`（gRPC 定义与生成代码）、`tui/`（终端界面）、`libs/`（本地依赖模块）、`config/`（预设与配置）。

## 代码规范

### 1. 提交前对全仓库 Go 代码执行 gofmt

所有提交前，必须对整个仓库的 Go 源码统一执行 `gofmt`，保证格式一致：

```bash
gofmt -w .
```

若需校验（不实际改写），使用：

```bash
gofmt -l .
```

- 该操作应覆盖仓库内全部 `.go` 文件（含插件子项目，如 `plugins/tool-lua-host`）。

- 生成代码（`proto/*.pb.go`、`proto/*_grpc.pb.go`）亦须保持 gofmt 整洁；如未按规范排版，一并修正。

- 本仓维护的代码一律同等要求 gofmt，`libs/` 亦然：`libs/vodka` 是本仓自研的 Web 框架，`libs/sh`、`libs/cron`、`libs/go-lua` 等是本地维护模块，均须格式化。仅**纯 vendored 的第三方源码**（如 `libs/anthropic-sdk-go`、`libs/go-openai`）不强制 git 追踪其内部格式化变更，避免与上游无谓分叉、产生无关且不可回溯的噪音提交。

### 2. 有无重复逻辑须要抽取成独立辅助函数

以「是否存在须要抽取的重复逻辑」为判据核心，并结合单文件行数这一观测信号：

- **判据**：同一段逻辑在多处复制粘贴复用、或因文件过大导致可读性下降时，应把该公共逻辑抽成独立辅助函数，置于**同级独立辅助文件**（例如 `helper.go`）。

- **红线**：凡出现「复制-粘贴」式的重复逻辑，即须抽取，不得通过再复制一份来规避；宁可抽成小而专的辅助函数，也不保留多处重复。

- 抽取标准：同一段逻辑在多处复用、或与文件主体职责无关但仍属同一包。

- 文件命名：与主体文件同包同目录，命名需能反映辅助内容（如 `helper.go`、`session_helper.go`）。

- 放置原则：辅助函数保持小、单一职责、命名自解释；不引入无必要的新包层级。

- 抽取时保持行为不变，仅做纯重构；抽取后须重新 `gofmt` 并确保 `go build ./...` 与相关 `go test` 通过。

### 3. 跨层数据传递须补「字段保真往返 + 真实链路集成」测试

宿主「聚合 / 转发 / 适配」层把数据从一个域搬到另一个域时，任何**只返回子集**的适配器都是静默丢字段的隐患，必须用测试守住：

- **判据**：接口返回类型比其接收到的信息更少（如 `Execute(...) (string, error)` 把 `Content` / `ViewJson` / `Error` 压成单个 `string`），即视为「返回子集」适配器。

- **红线**：这类适配器一律须有**字段保真往返测试**——喂入带全部字段的完整负载，断言返回值逐字段存活；不得只断言「能拿到内容」。

- **链路覆盖**：分层单测 / 直连 e2e 若各层用假数据接邻层，会漏掉中间衔接点。对关键链路（如「插件 → 宿主聚合 → agent → TUI」），必须在**宿主聚合层用真实插件进程**补一条端到端集成测试覆盖每一跳，杜绝「两端都测了、中间丢了」的盲点。

- **先例**：结构化视图链路曾因 `RemoteTool.Execute` 只返回 `Content`、在宿主聚合层静默丢弃插件 `ViewJson`，导致真实运行中所有插件视图失效；现由字段保真往返（`TestRemoteToolRoundTripPreservesFields`）与真实插件进程的宿主链集成测试（`TestHostToolChainPropagatesPluginViewJson`）守住。

### 4. 禁止保留 Deprecated 代码

本项目仍在高速迭代，保留 Deprecated 代码只会造成代码膨胀、堆积大量无用代码，并让「旧实现是否仍在使用」的判断混乱。

- **判据**：任何标为废弃的本项目手写代码——`// Deprecated:` 注释、函数/类型/字段命名带 `deprecated`/`legacy`/`old` 的旧实现、以及「用新替代后仅留作兼容」的分支——一律不得留存在仓库中。

- **红线**：发现即删除其实现，并从调用点一并移除，不得通过再包一层或改名来规避废弃状态；删除后重新 `gofmt` 并确保 `go build ./...` 与相关 `go test` 通过。

- **例外**：protoc-gen-go 在生成的 `*.pb.go` 里**自动附带**的 `// Deprecated: Use X.ProtoReflect.Descriptor instead.` 样板注释属生成物自带的固定模板（每个消息都会生成），不是我们需要维护的废弃代码，不在此列；且不得手改生成文件——该样板无法也不应被移除。

### 5. 文档、代码与提交信息不得使用开发阶段代号

本项目高速迭代中，勿用「P2/P3」「方案A/B」「P2.2(b)」「P3-1」「基线」这类**开发阶段的内部代号**去指代某项能力或某个修复——这些代号对外/后续读者无意义，且随迭代不断漂移，反而制造混淆。

- **判据**：文档（README 等面向读者的内容）、提交信息、以及面向维护者的代码注释中出现此类代号，即视为违规。

- **红线**：一律直接描述设计意图与实际行为（如「配置自愈」「插件目录自愈」「goroutine 泄漏修复」），不得用代号缩略。已存在的历史代号，应在接触相关代码或文档时顺手改写为直述。

- **例外**：产品/技术之固有术语不算内部开发代号，不受此限——如「PTC 程序化工具呈现」、语义化「基线版本（baseline）」（指同目录未带版本号后缀的运行文件，见热重载）、「hot reload」等。

### 6. 进入模型请求前缀的有序产物，必须显式按名稳定排序（确定性，不依赖 map 类型或插入序）

LLM 请求的前缀（system prompt、工具目录、上下文片段等）参与提示前缀缓存——前缀字节必须逐次一致才能命中缓存。Go 的 map 遍历序随机（每次进程/运行都可能不同），若直接用其生成工具清单、上下文拼接等**有序**产物，会让请求前缀随进程随机漂移、命中前缀缓存失效。

- **判据**：遍历 map（`for ... := range m`，`m` 为 map 类型）并把结果按序拼接/收集/序列化为列表，且该产物会进入模型请求（system prompt、工具参数、上下文 `ListContext`、run\_code SDK、工具结果的 TOON 化 Content、子代理/主代理工具目录等）。

- **红线**：这类有序产物一律按**名称/键升序**稳定排序（`sort.Slice`/`sort.Strings`，比较统一为 `Name < Name`、`a < b`），不得依赖 map 迭代序。排序不影响功能，但必须确定。

- **红线之确定性本质**：确定性由「**显式按键升序排序**」保障，**不是**靠换用某种「有序 map」数据结构（如 orderedmap 只保证插入序，插入序本身若基于无序遍历/随机装载序则同样漂移）。因此**不要靠更换 map 类型来绕开排序**；一切进模型输入的有序产物都必须显式排序，与所用 map 是否有序无关。

- **覆盖面**：偏好「一处统一排序点」覆盖其全部下游——如 `AllToolsProto` 一处按工具名排序，即覆盖 ListTools、子代理、run\_code SDK、PTC 描述内嵌清单；宿主 `ListContext` 按插件名排序；工具结果 TOON 化的 map 已由 toon 库按键排序；agent 端工具目录也按名升序。

- **红线之算法一致**：同一次请求内的所有片段（tools、ListContext 等）须用**同一排序算法**（统一按名/键升序），避免「此处排序、彼处不排」或「一处升序、一处其它」导致前缀仍不一致。

- **例外**：随会话变化的动态内容（用户输入、历史、记忆）不属固定前缀命中区，不强制作稳定排序；但若某动态段被明确用作缓存前缀的一部分，仍需稳定。已在好状态的点（如 agent 端 `availableTools`、tool-skill `scanSkillDir`）应保持不变，不得退化。

### 7. 跨平台：以下述目标平台集为定义，交叉编译逐平台校验

本项目面向以下**定义的目标平台集**（跨平台编译校验矩阵；对齐另一项目 GhostClaw 的平台约束，含 FreeBSD/GhostBSD 共享单一二进制的口径）：

- 目标平台（GOOS/GOARCH）：`linux/amd64`、`linux/arm64`、`linux/loong64`、`darwin/amd64`、`darwin/arm64`、`windows/amd64`、`freebsd/amd64`（GhostBSD 与 FreeBSD 共享同一二进制）。

上述七个平台（通称「七端」）内又区分**主力三端**：`linux/amd64`、`darwin/amd64`、`windows/amd64`——最常部署运行的平台，也是无障碍交叉编译校验的基准。按模块层级确定跨平台编译校验的强制档位：

- **判据（核心程序 / 核心插件 vs 非核心插件）**：
  - **核心程序与核心插件**：宿主主程序、`core/`、`plugin/`，以及支撑宿主运行的骨架类插件（agent / LLM / 文件系统 / 记忆、进程生命周期与交互所涉插件）——必须在**七端全部**交叉编译通过。
  - **非核心插件**：可加性/外围类插件（音频通知、播放器、专用业务工具等）——至少须在**主力三端**交叉编译通过；若依赖（如 CGO 音频库 ALSA、小众架构无体系的纯 Go 库）在这些平台也无解，则须按其实际受支持的平台集在插件目录下补 `README.md` 注明受支持平台与构建要求（如需 `CGO_ENABLED=1`、需 ALSA 头文件），并保证**主力三端中能编过的平台照常通过**、不因单一平台编译不过而阻塞其余平台。

- **红线**：核心程序/核心插件不得以「某平台小众、暂不校验」为由豁免七端交叉编译；非核心插件的平台降级必须在插件 `README.md` 显式载明受支持平台与理由，不得静默宣称「全平台」。

涉及进程生命周期、信号、控制台、系统调用、路径分隔相关的代码，不得只按单一平台写死：

- **判据**：凡假设「某信号 / 某 API / 某路径写法在另一平台存在且语义一致」即须拆分另判。典型：进程退出/终止信号（`os/signal`、控制台关闭事件）、路径分隔（`/` 与 `\`，必要时用 `filepath`/`filepath.ToSlash` 归一）、进程/超时管理、系统特有 syscall。

- **红线**：同一逻辑在两平台行为不同或无共同实现时，用 build tag（`//go:build windows` / `//go:build !windows`）拆成**独立平台文件**，通用部分留在无 tag 的同包文件；不得把各平台分支全塞进单个通用文件，也不得因仅在一台平台编译通过就以为可移植。不存在的平台 API 应回退到显式、合理的兜底，而非静默假设。

- **先例**：`installShutdownSignals`（配套 `graceful_exit.go` / `signal_unix.go` / `signal_windows.go`）即为范例——Unix 用 `signal.Notify` 捕 SIGINT/SIGTERM/SIGHUP/SIGQUIT；Windows 用 `kernel32.SetConsoleCtrlHandler` 兜底点窗口的 console 关闭事件（`os/signal` 捕获不到），共享收尸逻辑抽在无 tag 的 `gracefulShutdown`。

- **验证红线**：涉及上述能力的改动，提交前除本平台 `go build ./...` 外，必须对**定义的目标平台集**逐对 (GOOS, GOARCH) 各交叉编译一次，确认平台文件都能编过——优先使用**项目自带的 builder**（`builder/builder.go`）执行，而非手敲 `GOOS=... GOARCH=... go build`：

  - **首选：用 builder**（覆盖七端，且封装了 `freebsd/amd64` 的 `purego fakecgo -std` 等平台特有构建标志，手敲会漏）：
    ```bash
    cd builder
    go build -o builder builder.go        # 一次性构建 builder 自身
    ./builder cross                       # 交叉编译并打包所有七端到 dist/dsc-for-<platform>/
    ./builder cross --platforms linux-amd64,windows-amd64   # 仅校验指定子集
    ```
    完成后用 `./builder clean` 及 `bash clean.sh` 清掉 `dist/`目录，及源码目录开发期间的产物而不带入仓库。

  - **外部工具链由 builder 自己找，不要凭「PATH 里没有」就断定缺失**。查找顺序：`DSC_TOOL_ROOTS`（显式，路径分隔符分隔）→ `PATH` → 仓库根（工具链随仓库放置，如 `protoc_dir/bin/protoc.exe`）→ `~/.bun/bin`、`~/.local/bin` → `%LOCALAPPDATA%\Programs`、`C:\Programs`、`C:\Tools` → 各磁盘的 `<盘>:\winkit\share`、`<盘>:\tools`。命中版本化目录（如 `E:\winkit\share\zig-x86_64-windows-0.17.0\zig.exe`）同样算找到。
    - CGO 插件（`tool-computer-use` 等）的编译器链：`CC` 环境变量 → 自动找到的 `zig`（自动拼 `"<path> cc"`）→ `gcc` → `clang` → go 默认 `cc`。全都没有时，错误信息里会列出**已查过的目录**，并提示用 `CC` / `DSC_TOOL_ROOTS` 指定；zig 缺可写缓存目录（受限环境 `APPDATA` 不可见时报 `AppDataDirUnavailable`）时 builder 会自动给临时缓存目录。
    - `tool-harness-webui` 的前端由 `bun` 构建（`bun install` → `bun run build`，产物 `go:embed`）。bun 同样自动发现；找不到时日志会写明查过哪里。
    - 构建前 builder 会用 Go 侧 `RemoveAll` 清空 `webui/dist` 与 `webui/.svelte-kit`：受限环境给 Node 装了 safe-delete 垫片，svelte-kit/adapter 的批量 `rimraf` 会被当成「批量删用户文件」拦下（`SAFE_DELETE_BULK_CONFIRM_REQUIRED`）。清空后它面对空目录，**不需要关闭任何安全机制**。
    - 跳过与失败要分清：插件编译失败仍会让整轮构建失败（避免悄悄少发插件）；日志里的「跳过」必须写明真实原因（缺 bun / 缺前端目录 / 前端构建失败三者不同），不要一律写成「需 bun」。

  - **次选：手敲逐平台**（仅当只改动主模块、无需走 builder 全量打包时）——PowerShell：`$env:GOOS='<goos>'; $env:GOARCH='<goarch>'; go build ./...`；Unix shell：`GOOS=<goos> GOARCH=<goarch> go build ./...`。**注意 `freebsd/amd64` 必须额外附 `-gcflags=github.com/ebitengine/purego/internal/fakecgo=-std`**，否则会编不过——这是优先用 builder 的核心理由。

  - 平台 API/签名用错应在交叉编译期暴露，勿拖到真机。

## 真机测试验证流程

真机（真实模型服务 + 真实工具进程的运行态）验证时，须按下述固定流程产出可运行产物并跑完测评后清理，避免把测得的运行时配置/临时产物带进仓库：

1. **产出 dist**：先在 `builder` 目录内编译构建器（`go build -o builder builder.go`），再直接在该目录内执行 `./builder` 产出 `dist/dsc-for-<platform>/`。勿用 `go run builder/builder.go`——它经临时缓存目录运行，会把仓库根错位到缓存路径，导致构建失败。
2. **写运行时配置**：切换进 dist 平台目录，从 `config/config.example.yaml` 复制出 `config/config.yaml`，按目标模型服务改写（LLM base_url / model / api_key 等）。
3. **跑 agentic-bench 真机测评**：用随 dsc 主程序同级复制进 dist 的 `bench` 程序（或等价命令）启动，加载 `tool-agentic-bench` 插件逐项完成并汇总报告；若本次改动点未被已有用例覆盖，则先在该插件补用例、再重走 agentic-bench 验证。
4. **清理**：测评完成后删除整个 `dist` 目录，临时配置与实际产物不得随本次提交带入仓库。

## 共享模块改动的最小验证面

`proto` / `session` / `core` / `sdk` 是全部插件共用的契约。改动它们的**语义**（字段含义、消息结构、派生行为、事件载荷）时，除本模块测试外必须：

1. 跑所有插件模块的测试：`plugins/*/go.mod` 各自 `go test ./...`；
2. 跑一次 `builder` 打包——它在打包后会调 `tools/backendcheck`，用真实派生历史驱动每个声明 `compaction` 能力的插件**实际二进制**，校验改写结果的结构约束（tool_call ↔ tool_result 配对完整、条数不膨胀、首条 system 不被压掉、消息 id 唯一且不得是位置式 `msg-<数字>`）。该检查失败即构建失败。

理由：这类「改 A 撞 B」的回归只会在另一侧插件自己的测试没覆盖到的路径上漏出来，而它曾经真的漏过（一次是消息身份、一次是压缩切点）。不要靠「我记得那边没用到」来判断。

## 提交约定

- 遵循一步一提交（一个特性/修复一次提交），便于回滚。

- 提交信息简洁，聚焦「为什么」而非「改了什么」。

### 8. 临时方案收拢为通用方案

在实现过程中，探索路径经常产生临时方案（如硬编码插件名、env 快照、绑在特定 Service 上的方法）。当多个插件面临相同需求时，这些临时方案应收拢抽象到 SDK 成为通用方案，而非逐个插件重复实现。

- **判据**：某项能力被两个或以上插件需要，或某个临时方案（env 变量、硬编码名、绑在特定 Service 上的方法）在后续插件开发中被重复遇到——即应收拢为 SDK 通用方案。

- **红线**：
  - 不得在宿主核心代码（`core/`）中硬编码任何特定插件名——必须经能力边界体系（`Provides`/`Requires`）或显式配置字段。
  - 不得用环境变量传递跨进程的运行时状态——env 在子进程 spawn 时快照，动态加载/卸载不同步。运行时状态应经 RPC（如 `ListContext`）或事件（如 `agent/pre-step`）查询。
  - 不得把通用能力绑在特定 Service 上——如 `ListContext` 不应仅绑在 `ToolService` 上，应提升到所有插件类型都能用的 `PluginHookService`。
  - 临时方案在确认需求通用后，须在当次提交中收拢——不得留到后续"再整理"。

- **对齐 DSH Cordis**：DSH 的 `ctx` 是统一入口——`ctx.on()` / `ctx.compaction` / `ctx.systemPrompt.section()` / `ctx.get('tokenMeter')`。DSC 的等价物（`EventBus` / `CompactionEngine` / `Hook.ContextFn` / `HasPluginProvidingCapability`）散落在不同文件中，但 SDK 的 `SDK` struct 是插件开发者的统一入口。新增通用能力时应经 SDK 暴露，而非要求插件开发者直接 import `core/` 包。

### 9. 工具意外不得中断会话（三层 panic recover 防线）

工具执行过程中发生的 panic（无论是宿主内置工具的 nil 指针、插件 Handler 中的 mvdan/sh expand 不支持语法、ViewFn 序列化失败、还是 gRPC 传输异常）都必须被 recover 转为错误返回给模型——绝不允许崩溃宿主进程导致 LLM 连接中断。此规则经三层防线实现，新增工具或修改执行路径时必须保持三层完整：

1. **SDK 层**（`sdk/tool.go::toolServiceServer.ExecuteTool`）：捕获 `Tool.Handler` 与 `Tool.ViewFn` 中的 panic，转为 `ExecuteToolResponse{Error: "tool X panicked: ..."}`。这是插件作者的第一道防线——所有用 SDK 的插件自动获得此保护。

2. **宿主执行层**（`core/tool_pipeline.go::executeToolBody`）：捕获所有宿主侧工具（含 builtin / `run_code` / agent 内部工具等）的 panic，转为 `tool X panicked: ...` 错误。覆盖未用 SDK 的工具（如 Lua 脚本工具、host 内置工具）。

3. **远程边界层**（`core/remote_tool.go::RemoteTool.ExecuteWithView`）：捕获插件 gRPC 客户端调用的 panic（如 nil client、序列化失败），转为 `tool X panicked (remote): ...` 错误。兜底覆盖未用 SDK 的插件与 gRPC 传输异常。

- **判据**：任何工具（无论 builtin、SDK 插件、Lua 脚本工具）panic 都应返回错误给模型，使模型能感知失败并自行纠正；不得让 panic 一路冒泡导致宿主进程退出，使整个会话中断。

- **红线**：修改工具执行路径（如新增 waterfall 阶段、新增工具类型）时必须保证上述三层 recover 完整。删除任一层 recover 视为破坏此红线——除非有同等的替代保护（如新工具路径自带 recover）。

- **工具描述自描述**：内置工具的 `Description()` 必须明确说明：工具的输入格式、何时不该使用、错误时模型的应对策略。模型不应把用途受限的工具误当作通用工具——经描述与 JSON Schema `pattern` 约束双重保证（如参数受格式约束的工具应同时以 schema `pattern` 与描述双重声明）。

### 10. 路径归一化：禁止使用 filepath.ToSlash

DSC 的命令解析使用内置 POSIX shell 实现（mvdan/sh），所有路径输入输出必须始终归一化为正斜杆 `/`，确保跨平台一致性。

- **禁止使用 `filepath.ToSlash`**：它只转换当前平台的路径分隔符。在 Linux 上 `\` 不是分隔符，所以 `D:\a\b.txt` 不会被转换——但 Windows 用户传入的路径可能含反斜杆，必须被转换。

- **必须使用两行连续替换**，处理两种不同情况：

  ```go
  s = strings.ReplaceAll(s, `\\`, "/")  // 先：反引号
  s = strings.ReplaceAll(s, "\\", "/")   // 后：双引号
  ```

  两行**必须先后同时出现**，顺序不可颠倒——先处理双反斜杆（避免被第 2 行拆成两个单反斜杆后再各自转换，产生多余的 `/`），再处理单反斜杆。

- **适用范围**：所有向模型或用户展示的路径字符串（错误消息、工具结果、日志、TUI 显示等），以及从外部输入或错误中提取的路径（必须用上述两行替换）。构造内部路径一律使用 `core.P*` / `dsc.P*` 正斜杆 helper（见规则 11），不得先经原生 filepath 拼接再用 ToSlash 补救。

### 10.5 分層：core ← sdk ← plugins（插件不得 import core）

插件一律 `import dsc "dsc-sdk"`，**不得** `import "dsc/core"`：

- **为什么**：core 是宿主内核（事件总线、会话、计量、协议实现），会持续重构；sdk 是插件的稳定面（类型别名 + 薄转发）。更要紧的是**同一件事不能有第二份实现机会**——DSC 曾让宿主 `core.TokenMeter`（实测口径）成为无人调用的死代码，agent 插件自己用裸估算决定裁剪/压缩，口径一偏（纯 ASCII 高估 4~5 倍），1M 窗口里服务端实测 332K 的会话被判每轮超窗并反复裁剪。
- **怎么做**：缺什么类型/函数就在 `sdk/` 加转发——类型用别名（`type X = core.X`，语义一致、无包装层），函数一行转发。命名冲突处取新名（如发模型用的工具 schema 叫 `dsc.LLMTool`，因 `dsc.Tool` 已是插件定义的工具）。
- **守卫**：`core/plugin_sdk_boundary_test.go::TestPluginsDoNotImportCore` 扫描 `plugins/**`（含测试文件），检出即失败。新增例外须在该文件的映射里写明具体理由与迁移计划。

### 11. 路径 API 黑名单：禁止直接使用 Windows 下返回反斜杆的 filepath 函数

filepath 包在 Windows 上会把路径结果归一化为原生反斜杆（实测 Clean/Dir/Join/Abs/Rel/Split/FromSlash/EvalSymlinks/Glob/WalkDir/Walk 均如此，甚至对正斜杆输入也归一化为反斜杆），与「内部 POSIX shell 统一视窗与 UNIX 都以正斜杆处理路径输入输出」相悖。

- **黑名单**：`filepath.Join / Abs / Clean / Rel / Split / FromSlash / EvalSymlinks / Glob / WalkDir / Walk / Dir / Separator` 一律禁止直接使用；由 `core/filepath_guard_test.go` 哨兵测试全仓守护，检出即报错（注释提及亦算命中，须改写）。

- **`path.Clean` 同样在黑名单**：标准库 path 只认 `/` 作分隔符，把反斜杆当普通字符**原样保留**（它只「清理」不「转换」），对 `O:\a\b` 清理后结果仍含反斜杆，违反规则 10。需要清理时用 `core.PClean` 或包内 `posixClean`（先按平台语义清理、再强制正斜杆）。正则带前置 `\b`，不会误捕 `posixpath.Clean` 这类本地等价实现。

- **正确接入**：本仓主模块（core/session/tui/cron/main 等）用 `core.P*`（PJoin/PAbs/PClean/PRel/PSplit/PDir/PEvalSymlinks/PGlob/PWalkDir/PWalk，先原生计算再归一化为正斜杆）；插件用 SDK 转发 `dsc.P*`；libs/sh、plugin、cron、session 等因独立模块或包级 import 环无法引入 core 的，用其本地等价实现（libs/sh/internal/posixpath、plugin/internal/pathx、cron/posix.go、session/posix.go，均登记守卫测试 fileAllowlist）。`string(filepath.Separator)` 一律用 `"/"`。

- **豁免口径**：目录豁免（skipDirs）仅限 (a) 非直管的 vendored 第三方源码（libs/anthropic-sdk-go、libs/go-openai、libs/go-lua、libs/jig-lisp、libs/toon-go、libs/fasttemplate、libs/bytebufferpool、plugins/tool-2fa-master/vendor）；(b) 仓库内非运行时产物（builder、examples、docs、testdata 等）；(c) 逐条登记理由的语义例外（见下）。**本仓直管的 libs（libs/sh、libs/cron 等）无豁免权**，必须同样遵守黑名单——直管代码不得以「第三方」名义豁免。

  `libs/vodka` 属本仓自研（Web 框架），归属上按直管论，但登记为语义例外：其 filepath 用法集中在 HTTP 框架内部（静态文件服务、session 文件存储、模板加载），不进入模型可见的路径链路，且在该语境下原生分隔符本就是正确选择——改造无收益，反而会改坏框架内部的路径语义。例外须像这样逐条登记具体理由，不得笼统以「第三方」「非直管」名义豁免。

### 12. 数值语义约定：零值必须表示「未定义 / 走默认」

凡 Go 代码中出现「数量 / 档位 / 开关」类数值字段（配置项、环境变量、proto 跨进程字段、JSON/YAML 入参），一律遵守三值编码——**`0` 必须为未定义，`-1` 或负值必须为禁用，正值必须为启用或具体数值**：

- **`0` = 未定义**：交由默认逻辑处理。**绝不**把 `0` 当作「关闭 / 禁用」——Go 的零值就是 `0`，漏写字段、YAML/JSON 反序列化失败、结构体新增字段、任何一次「没设值」都会静默使其生效，是默认踩得到的陷阱。主动关闭行为交给负值。

- **负值（典型 `-1`）= 禁用 / 不注入 / 不携带**：表示「主动关掉某行为」。

- **正值 = 启用或具体数值**：`N > 0` 表示「最近 N 条」「最多 N 页」「注入 N 条」等具体量。

配套红线：

1. **判定分支不得裸用 `== 0` 表达「禁用」**。表达「启用 / 具体值」用 `> 0`，表达「缺省 / 未设置」用 `== 0` 或 `<= 0`。

2. **布尔开关走 opt-out 模型**：默认开启，以「显式标注关闭」表达禁用（如 `ok && v != "false"`），全仓统一经 SDK `EnvBool(key, def)` 解析，**不得**再出现 `== "1"`、`!= "" && != "0"`、`== "1" || EqualFold "true"` 等互不兼容的内联写法（同一 `DSC_XXX=true` 在不同插件里结论不同，即协议层语义分歧）。

3. **跨进程边界尤其要守这条**——零值隐患最常发生在协议交换处：
   - proto 的 `service_id` 等 ID 字段用 1-based（`atomic.AddUint32` 从 0 起），故 `0` 约定为「未提供」是安全的**特例**，不要套用到数量字段；
   - 工具入参结构体必须带 `> 0` / `<= 0` 守卫，不得让真值 `0` 与「缺省」碰撞；
   - JSON/YAML 反序列化时 `omitempty` 与缺省值要区分清楚，避免缺省被当成真值或反之。

4. **先例与反例**：`history_injection`（`README.md` §语义分歧）按此三值编码——`-1`=不注入 / `0`=未定义(默认不限制) / `N>0`=最近 N 条；`max_tokens=0` 在 openai 由 `resolveMaxTokens`、anthropic 由 `omitZeroMaxTokens` 中间件摘除，在 ollama 经 `num_predict` 转发且**绝不以 `0` 上送**（避免被服务端当无限）；反面教材：`DSC_HISTORY_INJECTION` 曾按 DSH 的 `-1=不限制` 反向实现、`num_predict=0` 被 ollama 当无上限、布尔 env 三套写法并存——均属未守此约定所致。

### 13. 压缩是插件能力；agent 只调度，且对压缩是**强依赖**

职责边界（改 agent 插件前先读这条）：

- **agent 不实现压缩**。裁剪、折叠、摘要化、把"已省略 N 条"塞进 system——这些都属于
  "改写请求面"，归当值的压缩后端（声明 `Provides: {compaction: "true"}`）与宿主收口层
  所有。agent 只负责循环与调度。历史上 agent 曾自带 `trimToWindow` / `foldOversizeMessages`
  / `compactHistory` 三套兜底，已全部删除。**「不能裁」的理由不是消息身份**（身份早已是
  宿主下发的稳定 id，裁掉头部不再让压缩块指错消息），而是：①职责不属于它；②它手里是
  **改写前**的列表，真正上线的是后端渲染后 / 宿主滑窗后的视图，拿前者决定丢什么等于替
  后端扔材料；③压缩是增量的（`dsc-system` 按已压前缀的 sha256 指纹复用摘要、
  billion-context 的块覆盖按它见过的历史建立），喂它一份每步都被裁过的列表会让前缀指纹
  每步失配 → 每步重算压缩、请求面在相邻两步之间摆动、前缀缓存反复失效。
- **窗口适配归宿主**：`core/context_ceiling.go`（改写完成后量表面 → 超窗即滑窗到 90%，
  保留段全是原始消息 + 开头一行切除说明）与 `core/overflow_adjust.go`（provider 4xx 收紧
  预算后仍走滑窗）。agent 侧**不得**再判定"单轮超窗"报错——它量的是改写前的列表。
- **依赖必须显式声明**：agent 的 metadata（`plugins/agent-react-loop` 的 `agentSDKConfig()`）
  声明 `requires/dsc/compaction`。宿主据此在依赖未满足时把 agent 留在 PENDING，不按插件名
  或 env 猜。删掉这条声明 = 允许 agent 在没有压缩能力的环境里裸奔，属于回归。
- **启动期闸门**：`main.go` 在插件加载完成后调 `Manager.CompactionRoleProblem()`，岗位空缺
  即 `fail`（说明缺的是"没启用插件 / 点名的没加载 / 点名的没声明能力 / 多候选未点名"）。
  空缺不是可选形态，不要把它改回 warn。
- **岗位解析唯一入口** `core/notify.go ResolveCompactionRole` → `pickCompactionProviderLocked`：
  配置点名且合格者当值；未点名时**唯一候选自动当值**（启用即生效）；多候选未点名则空缺并
  要求点名（两个后端同时改写会层层叠加）。能力依赖解析复用同一函数——两处各写一份就会
  分叉成"岗位有人、agent 却因依赖未满足而 PENDING"。
- **能力可多、岗位唯一**：`Candidates` 列出所有候选仅供诊断与切换；`agent/pre-step` 的完整
  消息列表只发给岗位持有者，`agent/request-error` 也只发给它。
- **压缩的持久化只能写旁挂状态，不得写会话日志**：压缩后端要「重启不重压 + 可审计」，就写
  自己的 `<session>.<plugin>.json`（`dsc-system` 的摘要缓存、`billion-context` 的块与引用），
  内容含 schema 版本、指纹、摘要、审计字段（模型/来源/时间）。**不要**往会话日志追加
  `compaction/summary` + surface 遮蔽（旧 agent 内联压缩的做法，已删）：那会把「压缩」变成
  不可逆的历史改写，滑窗「窗口内每条都是原始消息、日志一字不改、切回大窗口全恢复」的
  不变量随之失效。丢状态文件的正确后果是「下次重压一次」，不是「历史变了」。

### 14. 曝光与审批归工具提供方，宿主只做通用装配

两条契约，改动工具目录或审批门之前先读：

- **工具目录里出现谁，由工具自己说**（`HiddenTool`，`core/tool_registry.go`）。装配点
  （`AllToolsProto`）只做「取 VisibleTools → 转 proto」，**不得**再按工具名前缀/名字
  判断谁能出现。历史教训：MCP 的曝光规则（关闭、已加载、有没有服务器）曾以
  `filterMCPCatalog` 的形式住在这个唯一装配点里，靠 `mcp__` 前缀认人——每加一个「有
  曝光策略」的提供方，就要在这里再加一段特例。
  - 隐藏 ≠ 不可用：隐藏的工具仍在注册表里，后台任务、子代理、按名调用照常；只是不随
    请求发给模型（目录每个请求都要付成本）。
  - 判定每次现读、不缓存：提供方改主意后下一次 `ListTools` 就生效（宿主约 800ms 同步）。

- **审批是能力的权限属性，不是工具的文案**（`proto.Tool.scopes` + `ScopeDeclarer` +
  `ToolService.ApprovalReason`）：
  - **范围**（静态，工具声明）：本工具**可能**触碰什么（`process.spawn` / `net.egress` /
    `fs.write`）。非空即需要审批；空 = 不进入审批。
  - **粒度**（策略，人配置：`DSC_APPROVAL_GRANULARITY=tool|scope`）：工具级每次问、不记账；
    范围级问一次即记账，同一范围的多把工具共享这次授权。**粒度与范围正交**，工具侧两种
    粒度下都不用改代码。
  - **理由**（动态，按本次参数）：`ApprovalReason` RPC；插件未实现时按声明的范围合成说明。
  - **不能因为「问不到理由」就放行**：RPC 失败按范围合成说明，只有「没声明任何范围」才算
    不需要审批。
  - **只声明宿主能真正执行或验证的范围**：无法执行的范围只是自我声明，与一句「这工具
    危险」等价，却披着声明式的外衣。
  - **`never` 与预授予是两件事**：`never` 表示「不要问，需要点头的就拒」；配置里
    `authorization.granted_scopes` 的预授予是人已经做过的决定，`never` 下照常放行。
    两者混为一谈就是「问不到就放行」这种漏洞的来源。

- **岗位是「有名字的坑位」，编制由岗位性质决定**：`ResolveCompactionRole` 的「唯一」是
  那个岗位的性质（压缩只能有一个写者），**不是岗位概念的通则**。管理这类能力天然多提供方
  （编制 N）；呈现层若抢同一端口/路径，则那个岗位编制 1。新增岗位时先声明编制，再谈解析。

- **跨层字段只有一个赋值点**：`remoteToolFromProto`。`listStagedTools`（启动期）与
  `syncPluginTools`（运行期）曾经各拼一遍 `RemoteTool`，加字段时只改一处 → 插件声明了、
  宿主听不见。新增 proto 字段时改这一个函数即可。

## 文档同步

- **README 必须时刻反映代码实现的实际变化**。任何会改变 README 所述内容（架构、目录/模块与依赖增减、关键行为、安全设定）的源码修改，须在本次提交内一并同步更新 README，不允许 README 与实现漂移。

- 尤其要及时反映：vendored 依赖（`libs/`）的增删、沙箱策略档位、管理 API 默认监听地址与认证方式、工具调用超时机制等容易被 README 误导读者的行为变更。

- 若改动较为局部、不影响 README 概述，可标注「README 无需变更」以便复核。

