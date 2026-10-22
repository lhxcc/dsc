# DSC Agent Loop 加固：SPEC 与实施规划

本文把三项待办形成可评审的技术规格，并给出实施顺序与后续优化路线：

1. 压缩切点吸附工具对 + 孤立 tool 消息断言
2. 只读工具并行 + 工具级瞬态重试
3. bench 基线对比并接入 CI + CI 开启 `-race`

> 术语约定：文中「工具对」指一条带 `tool_calls` 的 assistant 消息与其后连续对应的 `tool` 结果消息构成的不可分割单元。`tool_call_id` 指 proto 的 `tool_call_id` / 内部 `ToolCallID`。

---

## 1. 背景

三项都来自与同目录 `pigo` 的机制对比，但落点与动机各自独立：

- **压缩切点**：DSC 的压缩按 token 预算机械切分，没有「不得切开工具对」的约束。一旦切点落在 assistant(`tool_calls`) 与它的 `tool` 结果之间，投影出的请求历史就会以孤立 tool 消息开头，OpenAI 兼容端点会直接拒绝。这与刚修复的 `protoMessagesToPlugin` 丢失 `ToolCallId` 属于同一类「协议配对断裂」，但成因在压缩边界，尚未有约束守住。
- **工具并行与重试**：DSC 主循环串行执行同一 assistant 消息里的全部工具调用；工具层没有瞬态错误重试，长尾延迟与偶发连接错误都直接暴露给模型。DSC 已具备判定材料（工具自声明的写能力 token、审批接口），可以比照搬更精确。
- **评测与 CI**：`tool-agentic-bench` 只有绝对得分，改动后无法判断 agent 变强还是变弱；CI 没有 `-race`，而并发面（事件总线 `parallel`、跨进程派发、后台 job、热重载、增量落盘）正在扩大。

---

## 2. 范围与非目标

**范围内**

- 压缩边界的统一吸附函数、三类压缩路径接入、工具对合法性校验与兜底修复。
- 单批工具的只读并行调度、结果按源序回填、取消时的结果补齐。
- 宿主工具执行的瞬态重试（含门禁）。
- bench 报告的基线对比、独立比较 CLI、CI 的 `-race` 与比较回归作业。

**非目标**

- 不改插件协议（`proto/*.proto`）与 SDK 契约。
- 不改沙箱档位语义、审批交互形态；并行只覆盖「无需审批的只读调用」。
- 不做写工具的并行，不做跨 assistant 消息的流水线并发。
- 不把真实模型 agentic-bench 纳入 PR CI（需要真实端点与密钥），只把**比较逻辑**纳入。

---

## 3. SPEC

### 3.1 压缩边界不变量

#### 3.1.1 现状与缺陷

| 路径 | 代码 | 切分方式 | 风险 |
| -- | -- | -- | -- |
| 插件压力压缩 | [compaction-basic.go](../plugins/dsc-system/compaction-basic.go) `retainBoundary` / `applyRecords` | 从末尾按 token 累积，返回 `i+1` | `UpTo` 可能落在工具对中间，尾部以 `tool` 开头 |
| 插件溢出紧急压缩 | 同文件 `handleRequestError` | 固定 `end := len(msgs)-1`，只保留最后 1 条 | 最后一条若是 `tool` 结果，尾部就是孤立 tool 消息 |
| agent 内联压缩 | [main.go](../plugins/agent-react-loop/main.go) 压缩段 | 从末尾按 token 累积得到 `keep`，`SurfaceReplace` 覆盖 `[nodes[1], nodes[keep-1]]` | `nodes[keep]` 可能正是 `tool` 消息节点 |
| 历史注入 | [session.go](../session/session.go) `DeriveMessagesLimited` | 回退到最近的 `user` 边界 | 已受保护，不在此项范围 |

#### 3.1.2 不变量

对任意「将要进入模型请求」的消息列表 `msgs`：

- **I1 工具消息必有归属**：`msgs[i].Role == "tool"` ⇒ `msgs[i].ToolCallId != ""`，且存在 `j < i`，`msgs[j].Role == "assistant"` 且其 `ToolCalls` 含同 id，且 `j` 与 `i` 之间只有 `tool`/`user`（advisory notice）消息。
- **I2 工具对不跨切点**：压缩后保留区起点不得是 `tool` 消息；被压缩区不得以「assistant(`tool_calls`) 后跟结果」的一半结尾。
- **I3 每个 tool_call 有结果**：若 assistant 的 `tool_calls` 在保留区内，则其每个 id 在保留区内都有对应 `tool` 消息（截断跳过路径已合成结果，仍成立）。
- **I4 system 头不变**：沿用既有 Head 不变量（node 0 恒为 system，压缩不得覆盖），此项不重复设计。

I1–I3 只对「发往模型的投影」生效；事件日志仍 append-only 全量保留，回放/恢复语义不变。

#### 3.1.3 接口契约

新增 `core/compaction_boundary.go`（纯函数，不依赖 Manager）：

```go
// SnapRetainStart 把保留区起点向后（向更早）吸附，直到不切开工具对。
// 压力压缩的 retainIdx 与紧急压缩的尾部起点 end 语义一致，共用本函数。
// 返回 (newIdx, ok)：ok=false 表示吸附会越过 base（无可安全切点），调用方应放弃本次压缩。
func SnapRetainStart(msgs []*proto.Message, retainIdx, base int) (int, bool)

// ValidateToolPairing 按 I1–I3 校验一条即将发往模型的消息列表。
func ValidateToolPairing(msgs []*proto.Message) error

// RepairToolPairing 在 Validate 失败时做最小修复：
//   - 丢弃无归属的 tool 消息；
//   - 保留 assistant 文本，剥离无结果的 tool_call。
// 返回修复后的列表与被修复项数（0 表示未改动，调用方直接沿用原切片）。
func RepairToolPairing(msgs []*proto.Message) ([]*proto.Message, int)
```

吸附规则（实现即规范）：

1. 若 `retainIdx` 处是 `tool`，向前回退到该工具对所属 assistant（含）——整对留在保留区，宁可略微超出保留预算。
2. 若回退会越过 `base`，返回 `ok=false`。
3. 紧急压缩的 `end` 用同一规则处理；若 `end<=base` 则 `ok=false`（`end` 与 `retainIdx` 都表示「保留区第一个下标」）。
4. 吸附只影响本次请求的改写与压缩区间，不写回事件日志。

#### 3.1.4 接入点

- `plugins/dsc-system/compaction-basic.go`
  - `handlePreStep`：`retainIdx := s.retainBoundary(msgs)` 之后调用 `core.SnapRetainStart`；`ok=false` 时按「无可压缩段」处理（保留既有记录改写，不新增记录）。
  - `handleRequestError`：`end := len(msgs)-1` 之后调用 `core.SnapRetainStart`；`ok=false` 时不产出记录、不返回 `retry`，避免重试同样的坏切分。
- `plugins/agent-react-loop/main.go` 内联压缩：算出 `keep` 后调用 `core.SnapRetainStart(msgs, keep, 1)`；`ok=false` 时跳过本次内联压缩（保留 request-error 兜底）。
- 请求前兜底：`deriveMessages` 在 `DeriveMessagesLimited` 之后调用 `ValidateToolPairing`；失败时 `RepairToolPairing` + 一条 Warn 日志（含被修复条数、会话 id），不阻断请求。修复只作用于请求副本，不改事件日志。

#### 3.1.5 失败语义

- 吸附失败**不压缩**，而不是产出非法历史；空间压力由 `agent/request-error` 的紧急压缩与 provider 自身报错兜底。
- 修复函数必须幂等：对已合法列表调用返回 `n=0` 且切片不变。

#### 3.1.6 测试与验收

- `core/compaction_boundary_test.go`
  - 表驱动：切点落在 `assistant(tool_calls)`↔`tool` 之间、落在连续 tool 中间、落在 `user`、落在 `system`、`base` 边界、无可安全切点。
  - `ValidateToolPairing`：缺失 `tool_call_id`、孤儿 tool、assistant 有未应答 tool_call、notice 插入在工具对之间（允许）。
  - `RepairToolPairing`：幂等、最小改动、日志计数。
- `plugins/dsc-system/compaction-basic_test.go`
  - 回归：构造「assistant + 3 个 tool 结果」的消息列表，`retainBoundary` 恰好落在第 2 个 tool 时，断言改写结果以 assistant 开头、且 `ValidateToolPairing` 通过。
  - 紧急压缩：最后一条为 tool 结果时，断言保留区不以 tool 开头（或放弃压缩）。
- `plugins/agent-react-loop/`：内联压缩路径的同类回归（surface replace 后重新派生，断言 I2）。
- 验收：以上测试全绿；`go test ./core/ ./session/ ./plugins/...`（各模块）通过。

---

### 3.2 只读工具并行 + 工具级瞬态重试

#### 3.2.1 并发分类契约

分类发生在 agent 侧（[parallel_tools.go](../plugins/agent-react-loop/parallel_tools.go)），依据只有工具自声明的能力标签——agent 与宿主分属不同进程，拿不到宿主的工具注册表与写能力裁决；因此能力必须随 `ListTools` 的 `proto.Tool.capabilities` 下发（[grpc_tool.go](../core/grpc_tool.go) `AllToolsProto`）。

```go
// core/sandbox.go（与沙箱裁决共用同一解析逻辑）
const CapabilityReadOnly = "read-only"

// ToolCallReadOnly 按能力标签判定一次调用是否只读。
// 未声明任何写能力的工具一律返回 false（能力未知即不可并行，fail-closed）。
func ToolCallReadOnly(capabilities []string, argsJSON string) bool
```

新增能力 token 语义：

- `read-only`：工具任何调用都不写（本仓已为 `lisp_eval`、`memory_search`、`memory_list`、`skill` 声明；`str_replace_editor` 由既有 `read-only-when:command=view` 覆盖）。
- 与 `writes-*` 同时声明时以 `writes-*` 为准（保守）。
- 未声明能力的工具（含 MCP 工具与宿主本地工具）一律串行。

#### 3.2.2 调度语义（agent 侧）

作用域：**同一条 assistant 消息内的 `toolCalls`**（[main.go](../plugins/agent-react-loop/main.go) 工具段）。

- **整批判定**：仅当该批全部只读、非宿主本地工具、且响应未被截断、数量 ≥2 时并行；任一不满足则整批串行（第一版不做分段混排，降低审批与副作用交错的风险）。
- **并发上限**：`DSC_TOOL_PARALLELISM`，默认 4，`0/1` 表示关闭并行（等价旧行为）。信号量实现，不引入新依赖。
- **并发范围**：只并发 `ExecuteTool` RPC 本身。`tool/call` 事件、增量落盘、「开始调用」帧与结果落地仍由原串行循环按源序执行，事件日志顺序确定、可回放。
- **取消语义**：批内调用全部发起后，即使 `ctx` 取消也继续按序落结果（每个调用都有 `tool/result`，会话恢复后历史仍合法）；串行路径保持原有「取消即返回」语义。
- **异常隔离**：并行 goroutine 内 panic 转为 `tool X panicked (parallel)` 错误结果，不崩溃宿主。
- **不做的事**：不并发弹审批、不并发写会话、不并发改 `availableTools`、不并发触发 `load/unload_dsc_plugin` 刷新。

#### 3.2.3 重试契约（宿主侧）

落点：`core/tool_pipeline.go` `executeToolBody`，只包裹「一次工具真正执行」这一段；`pre-execute`（含审批）与 `post-execute` 仍各执行一次。

- **可重试错误**（全部满足才重试）：
  - 工具本次调用被判定为只读（复用 `resolveWriteCall`）；
  - 错误满足 `isTransientToolError`：`net.Error`（Timeout/Temporary）、`io.EOF`/`io.ErrUnexpectedEOF`、`syscall.ECONNRESET/ECONNREFUSED/ETIMEDOUT/EPIPE`、gRPC `codes.Unavailable`；
  - `ctx.Err() == nil`（调用方未取消）。
- **不可重试**：`context.Canceled`、`ToolTimeoutError`/`context.DeadlineExceeded`（超时是策略预算，重试会翻倍墙钟）、panic（recover 后直接返回错误）、写语义或无法判定写语义的调用。
- **次数与退避**：默认 2 次重试（共 3 次尝试），`DSC_TOOL_RETRY` 可调（0 关闭）；退避 `200ms × attempt`，用 `select` 感知 ctx。
- **可观测**：每次重试写一条 Info 日志（`tool`、`attempt`、`error`）；最终失败仍是原有 Warn。

#### 3.2.4 配置

| 变量 | 默认 | 语义 |
| -- | -- | -- |
| `DSC_TOOL_PARALLELISM` | 4 | 单批只读工具的最大并发；0/1 关闭 |
| `DSC_TOOL_RETRY` | 2 | 只读工具瞬态错误重试次数；0 关闭 |

两值均 fail-soft：非法值回退默认，不阻断启动。

#### 3.2.5 测试与验收

- `core/tool_concurrency_test.go`
  - 只读工具（`read`）、`read-only-when` 命中（`str_replace_editor command=view`）、写工具（`write`）、`writes-unknown`（`shell`）、未注册工具、参数非法 → 期望 `safe` 取值。
  - `ApprovalRequester` 非空 reason → `safe=false`。
- `core/tool_retry_test.go`
  - 注入「前 N 次返回 `ECONNRESET`」的桩工具：只读时重试至成功；写工具零重试；`context.Canceled` 零重试；超时零重试；panic 零重试且不 crash。
  - 断言 `pre-execute`/`post-execute` 各只触发一次（用事件计数）。
- `plugins/agent-react-loop/parallel_tools_test.go`
  - 用假 `toolClient` 记录并发峰值：全只读批并发 >1、混合批并发 =1。
  - 结果与帧顺序按源序（对比事件 seq）。
  - ctx 取消：每个 `toolCall` 都有结果，`ValidateToolPairing` 通过。
- 验收：`go test -race ./core/ ./plugins/agent-react-loop/`；同一批只读调用的墙钟时间低于串行基线（测试用 2×100ms 桩断言 <180ms）。

---

### 3.3 bench 基线对比 + CI

#### 3.3.1 当前能力与缺口

[report.go](../plugins/tool-agentic-bench/report.go) `buildReport` 产出 `report.json`：`passed/failed/total/score/ratio/duration_ms/cases[]`。缺少：报告版本、与历史基线的比较、可被 CI 直接消费的退出码。

#### 3.3.2 报告 schema 扩展（向后兼容）

新增字段，不删改既有字段：

```json
{
  "schema_version": 2,
  "harness": "dsc-tool-agentic-bench",
  "score": "93.75",
  "passed": 15, "failed": 1, "total": 16,
  "duration_ms": 240974,
  "cases": [ { "id": "...", "status": "pass", "earned": 1, "weight": 1, "duration_ms": 4877, "feedback": "", "artifact_content": "" } ],
  "baseline": { "path": "...", "score": "87.50", "passed": 14, "failed": 2, "duration_ms": 260000 },
  "diff": {
    "score_delta": "+6.25",
    "regressions": ["prime_below_20"],
    "improvements": ["sqrt_2"],
    "unchanged": 13,
    "cases": [ { "id": "...", "base_status": "pass", "cur_status": "fail", "delta_ms": 320, "kind": "regression" } ]
  }
}
```

规则：

- `DSC_BENCH_BASELINE` 指向一份既有 `report.json` 时启用比较；未设置则完全不产出 `baseline`/`diff`，行为与现在一致。
- 比较按用例 id 对齐；基线缺失的用例标记 `new`，当前缺失的标记 `missing`。
- `diff.cases` 与 `regressions`/`improvements` 一律**按 id 升序**（确定性，便于 diff 与缓存）。
- 同时写 `<benchRoot>/bench-out/report.diff.json`（仅 diff 部分），供 CI 收集。
- `bench_report` 视图在有基线时增加一列「变化」（`+1`/`-1`/`=`/`new`/`missing`）。

#### 3.3.3 独立比较 CLI

新增 `plugins/tool-agentic-bench/cmd/benchcmp/main.go`（纯 Go、无 CGO，可与插件共享同一比较包）：

```
benchcmp --baseline <report.json> --current <report.json> [--fail-on-regression] [--fail-on-new-fail] [--json]
```

- 退出码：`0` 无回归 / `1` 命中断言（回归或新增失败）/ `2` 用法或 IO 错误。
- `--json` 输出机器可读 diff，字段与 `report.diff.json` 一致。
- 回归定义：基线 `pass` 且当前 `fail`；新增失败：基线不存在或为 `skip` 且当前 `fail`（默认不计回归，仅 `--fail-on-new-fail` 时失败）。

#### 3.3.4 CI 变更

1. **`-race` 作业**（新 job `race`，与 `test` 并列，避免拖慢主反馈）：
   ```yaml
   - run: go test -race -timeout 15m ./core/... ./session/... ./jobs/... ./workflow/... ./cron/... ./coderuntime/...
   ```
   只覆盖并发核心包；`tui` 等 UI 包后续按稳定性再纳入。
2. **bench 比较作业**（新 job `bench-compare`）：
   - `cd plugins/tool-agentic-bench && go test ./...`；
   - `go run ./cmd/benchcmp --baseline testdata/baseline.json --current testdata/current_regressed.json --fail-on-regression` 期望退出码 1（断言检测有效，用 `!` 反转）；
   - `--current testdata/current_improved.json` 期望退出码 0。
3. **真实 agentic-bench**：仍为人工/定时任务（需要真实模型端点与密钥），在 README 记录运行方式；PR CI 不做。

#### 3.3.5 测试与验收

- 比较逻辑单测：回归/改进/新增/缺失/无变化五类；排序确定性；空基线；重复 id（报错）。
- 报告往返：`buildReport` 在有无基线两种模式下，既有字段逐字段存活（沿用 `TestBuildReportRoundTripPreservesFields` 风格）。
- CLI 退出码表驱动测试。
- 验收：CI 两个新作业通过；本地 `go run ./cmd/benchcmp ...` 退出码符合契约。

---

## 4. 实施规划

每一步独立可提交、可回滚；提交信息沿用仓库风格（英文 conventional commit，聚焦「为什么」）。

### 步骤 1：压缩边界不变量（纯函数 + 测试）

- 新增 `core/compaction_boundary.go`、`core/compaction_boundary_test.go`。
- 不改任何生产路径行为（只加函数与测试）。
- 验证：`gofmt -l` 无输出；`go test ./core/ -run 'CompactionBoundary|ToolPairing'`。
- 建议提交：`feat(core): add tool-pair boundary helpers for compaction`

### 步骤 2：接入三条压缩路径 + 请求前兜底

- `plugins/dsc-system/compaction-basic.go`（`handlePreStep`、`handleRequestError`）
- `plugins/agent-react-loop/main.go`（内联压缩 + 请求前 `Validate/Repair`）
- 回归测试落在各自模块。
- 验证：`go test ./plugins/dsc-system/ ./plugins/agent-react-loop/`；`go test ./core/`。
- 建议提交：`fix(compaction): never cut between a tool call and its result`

### 步骤 3：并发分类与只读并行

- `core/sandbox.go`：新增 `CapabilityReadOnly` 与 `ToolCallReadOnly`（与沙箱裁决共用 `resolveWriteCallFromDecl`）；`core/grpc_tool.go` 的 `AllToolsProto` 透传能力标签；单测 `core/tool_readonly_test.go`。
- `plugins/agent-react-loop/parallel_tools.go`：整批判定、信号量、panic 隔离；`main.go` 工具段按源序消费并行结果。
- 本仓只读工具补声明：`lisp_eval`、`memory_search`、`memory_list`、`skill`。
- 验证：`go test -race ./core/ ./plugins/agent-react-loop/`；并发峰值与取消补齐断言。
- 建议提交：`feat(agent): run read-only tool calls in parallel within a step`

### 步骤 4：工具级瞬态重试

- `core/tool_pipeline.go`：把执行体抽成 `invoke` 闭包，在 `executeToolBody` 内循环重试。
- `core/tool_retry.go` + 单测（错误分类、门禁、pre/post 只跑一次）。
- 验证：`go test -race ./core/`。
- 建议提交：`feat(tools): retry transient failures for read-only tool calls`

### 步骤 5：bench 基线对比

- `plugins/tool-agentic-bench/internal/benchreport`（Report/Diff/Load，插件与 CLI 共用）、`report.go` 接入、`cmd/benchcmp`、`testdata/` 固件。
- README 增补基线用法与退出码。
- 验证：`cd plugins/tool-agentic-bench && go test ./...`；手工跑一次 `benchcmp`（回归 1、改进 0）。
- 建议提交：`feat(bench): compare runs against a baseline report`

### 步骤 6：CI 加固

- `.github/workflows/ci.yml`：新增 `race` 与 `bench-compare` 作业。
- 验证：本地等价命令全绿；PR 上观察两个作业。
- 建议提交：`ci: add race detector and bench baseline regression checks`

### 步骤 7：整体验证（按 AGENTS.md）

1. `gofmt -w .`（受控文件）与 `gofmt -l .` 校验。
2. `go build ./...`、`go vet ./...`、`go test ./...`（根模块）。
3. 子模块：`sdk`、`plugin`、`plugins/dsc-system`、`plugins/agent-react-loop`、`plugins/tool-agentic-bench` 各 `go build ./...` + `go test ./...`。
4. 七端交叉编译：`cd builder && go build -o builder builder.go && ./builder cross`；完成后 `./builder clean` 与 `bash clean.sh`。
5. 真机验证（真实模型 + 真实工具进程）：
   - 构造「工具执行后继续下一步」的对话，确认第 5/6 轮不再出现 `missing field tool_call_id`；
   - 触发一次压缩（调小 `DSC_COMPACTION_BASIC_CONTEXT_WINDOW`），确认改写后请求仍合法；
   - 用 `DSC_TOOL_PARALLELISM=4` 跑一批只读调用，观察日志中并发与结果顺序；
   - 跑一次 agentic-bench 并用 `benchcmp` 对比前次报告。
6. 清理 `dist/` 与临时配置，不带入提交。

---

## 5. 风险与回滚

| 风险 | 影响 | 缓解 |
| -- | -- | -- |
| 吸附导致压缩量略减 | 上下文略增（最多一个工具对） | 只影响单次请求；压力仍由阈值与紧急压缩兜底 |
| 并行打乱事件顺序 | 回放/缓存/调试困难 | 结果统一在 join 后按源序写入，绝不并发写会话 |
| 并行遇到隐式写工具 | 数据竞争/副作用交错 | `safe` 判定 fail-closed；只对声明只读的工具并行 |
| 重试放大副作用 | 重复写 | 只重试只读；写/未知写一律不重试 |
| 重试拖长超时 | 回合变慢 | 超时不重试；次数与退避可配、可关 |
| 修复函数掩盖真实缺陷 | 问题不暴露 | 每次修复写 Warn（含计数与上下文），并保留 `ValidateToolPairing` 用例守护 |
| `-race` 作业耗时上升 | CI 反馈变慢 | 独立 job、限定并发核心包、`-timeout 15m` |

回滚粒度：步骤 3/4 各有独立环境变量开关（`DSC_TOOL_PARALLELISM=1`、`DSC_TOOL_RETRY=0`），无需回退代码即可恢复旧行为；步骤 1/2/5/6 按提交回滚。

---

## 6. 后续优化方向（下一步）

按「收益 / 依赖」排序，前三项建议在本轮加固落地后紧接着做。

### 6.1 收尾驱动器抽成显式 seam（结构债）

- 现状：[main.go](../plugins/agent-react-loop/main.go) 1833 行，goal 轮次、运行中注入、截断续行、todo 追问四套驱动器硬编码在循环体。
- 目标：抽出 `turnDriver` 接口（`ShouldContinue(ctx, state) (action, reason)`），循环体只负责「取动作并执行」。
- 收益：四个驱动器可独立单测；新增驱动器不改主循环；主循环回到可审计规模。
- 判断依据：主循环文件行数、驱动器测试覆盖率、新增驱动器的 diff 行数。

### 6.2 工具结果预算的「单一权威」

- 现状：spill（4000 字符外置）、billion-context `truncate`（头尾各 2000 字符）、TOON 化三处各自改写结果，缺少优先级表。
- 目标：固化 `工具自身限制 → spill 外置 → 截断兜底 → TOON 投影` 的顺序，并加一条「同一结果不被两处同时改写」的集成测试。
- 收益：模型看到的结果形态不再取决于加载了哪些插件。
- 判断依据：同一会话在不同插件组合下的结果一致性测试。

### 6.3 会话恢复 + 快照重建

- 现状：事件溯源已在，`Store.Fork` 已有，但没有「从压缩检查点重建前缀」的能力（对照 pigo 的 checkpoint/rebuild）。
- 目标：压缩摘要落盘为可复用检查点，resume 时按检查点折叠前缀而非重放全量；提供 `/rebuild`。
- 收益：长会话恢复更快、token 更省；压缩历史可追溯。
- 判断依据：恢复耗时、恢复后首请求 prompt tokens。

### 6.4 文件级回滚

- 现状：只有会话 fork，没有文件快照。
- 目标：在 write/edit 工具前后记录文件快照，支持按轮回滚（与事件日志的 turn 边界对齐）。
- 收益：显著降低「让 agent 改代码」的心理成本。
- 判断依据：回滚成功率与跨平台行为（Windows 路径/权限）。

### 6.5 子代理进程隔离档

- 现状：子代理在宿主进程内运行，共享资源与权限。
- 目标：可选进程隔离（复用 go-plugin 机制），崩溃即工具错误。
- 收益：子代理失控不拖垮主进程；资源上限可独立设置。

### 6.6 hooks 契约补齐

- 现状：Lua/exec 钩子可执行但缺 matcher、输出上限、显式超时语义。
- 目标：按工具名正则匹配、stdout/stderr 上限、超时失败策略（fail-open/fail-closed）显式化。
- 收益：脚本 hook 不再能挂死回合或刷爆日志。

### 6.7 机器可读事件流

- 现状：`RunStreamResponse` 是内部结构，外部消费只能抓 TUI。
- 目标：定义带版本号的事件流 schema（对齐 pigo 的 stream-json），供 CI、脚本、外部 UI 使用。
- 收益：自动化与可观测性解耦于 TUI 实现。

### 6.8 真机 bench 常态化

- 目标：定时任务跑真实 agentic-bench，归档 `report.json` 作为下一轮基线；回归自动告警。
- 前置：6.3 之外无硬依赖；需要稳定的模型端点与预算。
- 判断依据：连续多轮得分方差、回归发现时延。

---

## 7. 附：验证命令速查

```bash
# 格式
gofmt -l core plugins/dsc-system plugins/agent-react-loop plugins/tool-agentic-bench

# 根模块
go build ./... && go vet ./... && go test ./...
go test -race -timeout 15m ./core/... ./session/... ./jobs/... ./workflow/... ./cron/... ./coderuntime/...

# 子模块
(cd plugins/dsc-system && go test ./...)
(cd plugins/agent-react-loop && go test ./...)
(cd plugins/tool-agentic-bench && go test ./... && go run ./cmd/benchcmp --baseline testdata/baseline.json --current testdata/current_improved.json)

# 七端交叉编译
(cd builder && go build -o builder builder.go && ./builder cross)
(cd builder && ./builder clean) && bash clean.sh
```
