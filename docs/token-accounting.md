# DSC 词元计量：算法、数据流与规范

> 面向后来者：读完这一篇应当能回答——某个数字是谁算的、什么时候算的、能不能信、
> 想改的时候该动哪里。文中所有公式与行为都以当前代码为准，并标注了文件与函数。

---

## 1. 先记住三个数

DSC 里与「token 用量」有关的数只有三类，**用途完全不同，不能互相替代**：

| 数 | 谁产生 | 精度 | 用途 |
|---|---|---|---|
| **估算（estimate）** | `core/token_meter.go` 的纯函数 | ±10%~30%（看内容） | 没有实测值时的绝对值；有实测值时的**差量** |
| **实测（usage）** | provider 上报的 `usage.prompt_tokens` | 精确（但只对"那一次请求"精确） | 压力底数、界面"已用" |
| **投影（projection）** | `TokenMeter.Project` / `core.projectPressure` | 实测底数 + 估算差量 | 决策：要不要裁剪/压缩 |

一句话：**服务端是唯一的绝对真值，估算负责"没有真值时"和"真值之后的增量"。**

---

## 2. 分层与唯一真源

```
core/token_meter.go        估算公式（唯一实现）+ TokenMeter
        ▲
sdk/tokenmeter.go          一行转发（插件唯一入口 dsc.Estimate* / dsc.TokenMeter）
        ▲
plugins/*                  只能调 dsc.*，不得自带公式
```

| 关注点 | 唯一实现位置 |
|---|---|
| 文本/消息/工具目录估算 | `core/token_meter.go`：`EstimateTextTokens`、`EstimateMessageTokens`、`EstimateMessagesTokens`、`EstimateProtoMessage*`、`EstimateToolTokens`、`EstimateProtoToolTokens` |
| 单个工具的估算公式 | `core/token_meter.go:estimateOneToolTokens`（core.Tool 与 proto.Tool 共用，两形态不得算出两个数） |
| 压力合成策略 | `core/llm_router.go:projectPressure`（宿主）+ `plugins/dsc-system` 的**自身表面**测量（见 §8） |
| 用量锚点倒查 | `session/usage.go:LastUsageAnchor` |
| 请求信封指纹 | `core/tool_fingerprint.go:ToolsFingerprint` |
| 缓存口径 | `core/token_meter.go:PromptSideTokens` |

**硬约束**：核心/SDK/前端不得出现插件名（`plugin_names_guard_test.go` 会扫）；插件不得
再写一份 `/4`、`max(bytes/4,runes)` 之类公式——历史上两份口径打架，CJK 低估 4 倍、
ASCII 高估 4~5 倍，压缩疯狂误触发。

---

## 3. 估算算法（局部）

### 3.1 文本：按字符分类累加

```go
// core/token_meter.go:EstimateTextTokens
wide, narrow := 0, 0
for _, r := range s {
    if isWideRune(r) { wide++ } else { narrow += utf8.RuneLen(r) }
}
return wide + (narrow+3)/4
```

- **宽字符**（CJK 表意/假名/韩文/全角/emoji，`isWideRune` 覆盖 East Asian Wide 区段）：**1 token/字**
- **其余**（ASCII/拉丁/代码）：**4 字节/token**

为什么不是 `chars/4` 一刀切：中文按字节/4 会低估到 0.75/字；而按 rune 数当 token 数（旧实现）
会让纯 ASCII 代码按 1 token/字符算，是真实值的 4~5 倍。这里必须分类。

> DSH 用固定的 `CHARS_PER_TOKEN=4`，其 README 自认「CJK text and JSON schemas underprice
> badly」。我们保留 CJK 感知版，是有意为之，不是没对齐。

### 3.2 消息：文本 + 结构性开销

```go
// EstimateProtoMessageTokens
toks  = EstimateTextTokens(content)
toks += 6  // role=="tool"：tool 角色 + tool_call_id
    或 += 4  // 其余：role + 分隔符
toks += Σ imageStructuralTokens(ref)   // 4 + ceil(len(ref)/4)，图片只算引用、不算像素
toks += Σ (EstimateTextTokens(name+arguments_json) + 8)  // 每次工具调用
```

### 3.3 工具目录：每次请求都带的固定成本

```go
// estimateOneToolTokens
name + description + parameters_json 的文本估算 + 16（JSON 结构开销）
```

**必须计入**。工具目录每次请求都发、且不随会话增长消失。本机实测：13 个插件 46 个工具
≈ **10,037 tokens**，占压缩后 ~42K 请求的 **24%**。漏算它等于压力永远少一截，窗口快满
仍判「还宽裕」。

因此任何"请求大小"都必须是：

```go
EstimateProtoMessagesTokens(msgs) + EstimateProtoToolTokens(tools)
```

宿主 pre-step 的 `TokenCount`、agent 的投影/单轮超窗/压缩预算、压缩后端自测表面都遵循此式。

---

## 4. 计量器 `TokenMeter`

```go
type TokenMeter struct {
    pressureTokens  int   // 实测：上一次成功请求的 prompt 侧用量
    sampledEstimate int   // 取锚点那一刻，对所发请求（消息+工具）的估算
    hasSample       bool
    contextWindow   int
}
```

两种建锚方式，语义不同：

| 方法 | 设定 `sampledEstimate` | 之后 `Project(x)` |
|---|---|---|
| `SampleUsage(usage, sentEstimate)` | 有（必须与该次请求同源） | `usage + (x − sentEstimate)`，即**实测底数 + 差量** |
| `UpdateUsage(usage)` | 0（不设基线） | **退回 `x`**（实测值只描述那一次请求，不能当投影） |

`Project` 的差量**可以为负**：压缩让表面变小时，"已用"要跟着回落，否则永远在喊"该压缩了"。
`ShouldCompact(currentEstimate, ratio)` 只在 `window>0` 时给出判断。

**没有增量基线时绝不能拿实测值当投影**（`hasSample && sampledEstimate <= 0 → x`）。
实测值描述的是"那一次请求"，`x` 描述的是"即将发出的这一发"，两者之间没有可换算的关系。
曾经这里返回实测值当"保守下界"，在小窗口读超长会话（滑动窗口，每步都在裁剪）时就是
界面卡死：裁剪后不重新锚定 → 基线永远是 0 → `Project` 恒等于启动倒查到的旧用量。
实测现场是会话已到 730K（provider 实测），界面「已用」从首帧起一直是 33K / 3.29%，
而速率、缓存命中照常刷新（它们读的是另一个数）。

所以还有两条配套约束：

- **`UpdateUsage` 的实测值只用于 `MeasuredPromptTokens()`**（展示兜底、锚点超窗判断），
  不参与投影换算。
- **锚定必须无条件做**：agent 侧 `SampleUsage(promptTokens, sentEstimate)` 不再因为
  "本步裁剪过"跳过。跳过就等于永远拿不到基线。前提是投影输入也换成**裁剪后真正发出的
  表面**（否则基线与投影不同源，差量会把被裁掉的那段算成增长）。
- **展示兜底读 `MeasuredPromptTokens()`，不读 `Project(0)`**：后者算的是
  `实测 − 该次请求的估算`，即启发式的**误差**（几万量级、几乎不随会话增长），
  不是规模。

**锚点自身超窗时退回纯估算**（`window>0 && pressureTokens > window`）：上一次实测已经
大于窗口，说明窗口配置（或探测值）小于 provider 实际接受的规模。继续以它为底数，会把
之后每一次请求都判成超窗——历史裁到只剩最近几条也照样报错，而 provider 明明一直在接收。
这种锚点不再描述"现在要发的请求"，判定退回 `currentEstimate`。窗口未知（0）时无从比较，
维持差量语义。

`PromptSideTokens` 返回 **prompt_tokens**，不叠加 `cacheRead/cacheWrite`：OpenAI 兼容端点
的 prompt 已含缓存命中，相加会把命中缓存的请求算成近两倍（历史 bug，commit `1de5494`）。

---

## 5. 信封与锚点：让"上一次用量"变得可比

用量只对**产生它的那个请求**有效。换了模型、窗口、工具目录，或上一次请求被压缩改写，
那个数字就不再描述现在要发的表面。DSC 用一个独立事件记录这点。

### 5.1 `request/header`（log-only，请求发出前写）

```go
type RequestHeaderData struct {
    Turn, Step    int
    ToolsHash     string // core.ToolsFingerprint
    Model         string // 宿主经 DSC_MODEL 注入
    ContextWindow int
    SentEstimate  int    // 对该请求（消息+工具）的启发式估算
}
```

- 在 agent 构造完 `ChatRequest` 后、真正调用前 `Append`（`plugins/agent-react-loop`）。
- **失败请求也留下信封**（诊断用）；不进 surface、不派生模型消息。
- 新增事件类型**必须**同时在 `session/persist.go` 的解码表注册，否则加载会话时报
  `unknown event type`（写的时候不报、读的时候炸）。

### 5.2 `llm/attempt`（log-only，结算时写）

只记结果：`Usage` / `Error` / `FinishReason` / 耗时 / 产出规模。**不含信封**——同一事实
不存两处。

### 5.3 配对与可比性

```go
// session/usage.go:LastUsageAnchor
// 倒查最近一条「成功且 prompt>0」的 attempt，再向前找同 Turn/Step 的**最近一条** header
```

同一步重试会写多条 header，取最近那条才是这次尝试实际用的信封。之后：

```go
// UsageAnchor.Comparable
ToolsHash 一致  &&  模型一致（任一侧为空则跳过该条件） &&  窗口一致（0 视为未知）
```

加一条实测值的可信度判据（见 §7）：`usage >= SentEstimate`。低于它说明那次请求被压缩
或改写，用量描述的不是这个表面。

**旧日志没有 header → ToolsHash 为空 → 恒不可比**。这是刻意的保守方向：宁可退回估算，
也不拿不可比的 42K 当 900K 表面的底数。

### 5.4 指纹 `ToolsFingerprint`

按名称排序后，逐个写入 `name\0description\0parameters_json\0\x01`，取 sha256 前 16 hex。
顺序无关、任一字段变化即变。空工具目录返回空串（= 无信封信息，判不可比）。

---

## 6. 宿主投影 `projectPressure`

```go
// core/llm_router.go
projected, matched := projectPressure(anchor, curEstimate, curToolsHash, curModel, window)
```

三分支（`curEstimate = 消息 + 工具目录`）：

1. **信封不可比**（指纹/模型/窗口不同，或无 header）→ `curEstimate`
2. **可比但 `anchor.usage < anchor.sentEstimate`**（那次请求被改写）→ `curEstimate`
3. **可比且用量不低于该信封估值** → `anchor.usage + (curEstimate − anchor.sentEstimate)`

结果随 `agent/pre-step` 事件下发：

| 字段 | 含义 |
|---|---|
| `token_count` | 原始请求表面（消息 + 工具）的估算 |
| `last_usage_tokens` | 上一次成功的实测 prompt 用量（0 = 无） |
| `projected_tokens` | 上述三分支的结果，**针对"原始请求表面"** |
| `envelope_matched` | 锚点与本次请求是否同一信封 |

---

## 7. 表面所有权：`projected_tokens` 不是万能的

这是本文档最重要的一条规范：

> **压力必须量在那个"真正会被发出去"的表面上。谁能改写表面，谁就必须自己量。**

`projected_tokens` 描述的是**原始请求表面**（agent 派生出的完整历史 + 工具）。它对
"不改写请求"的消费者是精确的；但对**会改写请求的压缩后端**是错的——因为它压缩之后
只发「摘要 + 保留尾部」，而宿主看不到这次改写。

实测代价（`dsc-system`，窗口 10k、阈值 8k、保留 16%）：

| 场景 | 用 `projected_tokens`（原始 19k） | 自测渲染后表面 |
|---|---|---|
| 首次（尚无压缩记录） | 压缩 ✅ | 压缩 ✅ |
| 尾部只增 2 条（渲染后 ≈2.4k） | **又摘要一次** ❌ | 不新增摘要 ✅ |
| 尾部增到渲染后 >8k | 压缩 ✅ | 压缩 ✅ |

也就是说：直接用投影会把 80% 阈值退化成「保留区一满就压」，每步多打一次 LLM。

所以 `dsc-system` 的规则是：

```go
switch {
case len(records) > 0:  // 表面已被我们改写
    pressure = EstimateProtoMessagesTokens(rewritten) + toolTokens
case ev.ProjectedTokens > 0: // 尚未改写，表面 == 请求表面
    pressure = ev.ProjectedTokens
default:
    pressure = ev.TokenCount
}
```

`toolTokens`：优先用 `ev.ToolTokens`（宿主直接给出）；只有旧宿主没带该字段时才反推
`ev.TokenCount − EstimateProtoMessagesTokens(msgs)`。反推只作兜底——同一条瀑布里若别的
插件先改写过消息，这个减法会把差额算成工具开销，甚至算成负数。

另一个后端 `dsc-billion-context` 同理但更极端：它把绝大部分历史遮蔽成引用，真实载荷是
渲染后的视图（1M 窗口的真实会话实测 765K vs 原始 1.66M；早期样本 40K vs 900K），因此它用
`last_usage_tokens`（上一次真正发出去的量），**不用** `projected_tokens`，代码注释里写明
理由，防止后来者"顺手统一"。

### 7.1 有压缩后端时，agent 不做请求级裁剪

承接上一条「谁能改写表面，谁就必须自己量」：**既然后端把表面改小了，agent 手里的
投影就不再描述将要发出的东西**，任何基于它的裁剪都是瞎猜。

`TokenMeter.Project` 的锚点一头是**改写后**的 usage（比如 169K），一头是**改写前**的
估算（比如 932K）——两者描述的是两个不同的列表。`usage + (x − sentEstimate)` 于是把
后端省下的量当成「历史缩水」，投影在相邻两步之间来回摆：实测同一会话里 agent 的发送面
在 932K / 610K 交替（对应 provider 的 169K / 26.5K），裁剪深度也跟着每步翻转，前缀缓存
每次全废。

因此裁定（`plugins/agent-react-loop` 的 step 循环）：

- 有后端（`hasCompactionBackend`）时：**不裁剪、不折叠、不做「单轮内容超出上下文窗口」
  硬报错**。这三件事量的都是改写前的列表，而真正上线的是后端的渲染结果（实测 765K vs
  原始 1.66M），拿前者判后者会把完全装得下的请求判死。
- 窗口适配是后端的职责：它看得到完整的 append-only 历史，也只有它能决定保留什么。
- 兜底不是盲发：provider 4xx → 宿主 `agent/request-error` → 后端紧急压缩 → 重试
  （`notify` 只采纳选定后端的 `retry`）。
- 无后端时裁剪照旧（§11「动裁剪逻辑」），两条路径不要互相「统一」。

配套硬约束：**消息身份必须是宿主下发的稳定 id**（`proto.Message.id`，派生时取 surface
事件的 seq）。后端的压缩块用 `mNNNNN` 指认消息，而 `mNNNNN` 由消息 id 映射而来；一旦
用列表下标当 id，`history_injection` 只带最近 N 条、fork、或任何裁剪都会让同一个引用
指到另一条消息——不是「压得不准」，是**遮错内容**（实测最坏时整段对话只剩 2 条）。

状态文件带 `schema` 版本，与当前不符即整体丢弃、由事件日志重建（`compress` 调用自带
范围与摘要）。不写逐字段迁移：旧格式里记的是位置式 id，硬迁等于给已废弃的寻址方式
留实现分支（AGENTS.md §4）。

---

## 8. 数据流全景

```
① agent 组装请求
   msgs = sess.DeriveMessagesLimited(historyInjection)
   sentEstimate = Estimate(msgs) + EstimateTools(tools)
   sess.Append(request/header, {Turn,Step,ToolsHash,Model,Window,SentEstimate})

② 宿主 pre-step（每次请求都会发生）
   tokenCount = Estimate(msgs) + EstimateTools(tools)
   anchor     = 该会话上一次成功请求的 {usage, sentEstimate, ToolsHash, Model, Window}
   projected  = projectPressure(anchor, tokenCount, hash(tools), model, window)
   → 广播 agent/pre-step{messages_json, token_count, tool_tokens, last_usage_tokens,
                          projected_tokens, envelope_matched}
   → 压缩后端（waterfall）改写 messages 或返回空（不改写）

③ 宿主发出请求（改写后的 messages + tools）→ provider

④ 结算
   宿主：recordSessionAnchor{usage, Estimate(改写后 req), hash, model, window}
   agent：sess.Append(llm/attempt, {Usage, Error, ...})

⑤ 下一轮回到 ②，此时 anchor 来自 ④（或重启后由 session.LastUsageAnchor 从日志恢复）
```

三个消费者读不同的数：

| 消费者 | 读什么 | 为什么 |
|---|---|---|
| agent（裁剪/单轮超窗/显示） | 自己的 `TokenMeter`（`SampleUsage` + `Project`） | 决策在 agent 进程，窗口裁剪必须本地判断 |
| 宿主 pre-step | `projectPressure` | 给插件一个统一的、含工具目录与信封判据的投影 |
| `dsc-system` | 无记录时用 `projected_tokens`，有记录时自测 `rewritten` | §7 表面所有权 |
| `dsc-billion-context` | `last_usage_tokens` | 它的真实载荷是自己渲染出的视图 |
| 宿主 TUI | `HostContextMeters.ContextTokens`（agent 随帧上报的投影） | 只保管与发布，不重算 |

---

## 9. 规范清单

**必须**

- 估算只用 `core/token_meter.go` 的公式，插件经 `dsc.Estimate*`。
- 「请求大小」= 消息 + 工具目录，任何压力/裁剪/预算都不得漏工具。
- **消息身份 = 宿主下发的稳定 id**（`proto.Message.id`，派生时取 surface 事件 seq）。
  任何跨请求指认同一条消息的东西（压缩块的覆盖集合、`mNNNNN` 映射）都用它；列表下标
  不是身份，只是位置（§7.1）。
- 压力必须量在"将要发出的表面"上（§7）；改写者自测。
- **裁剪与超窗判定必须同一口径**（仅无压缩后端时，见 §7.1）：裁剪按消息估算选起点，
  判定读投影值，两者可能差数倍，故裁剪要闭环（裁完用 `projectedFit` 复核，不满足就收紧
  预算再裁，见 §11「动裁剪逻辑」）。
- **压缩提示词必须命令模型在摘要里保留用户「当前最紧要任务/事项」**：`dsc-system`
  的 `llmSummary` 与 `dsc-billion-context` 的 `compress` 描述都写明——压缩后模型仍须知道
  现在要做什么、卡在哪、下一步是什么。当前任务靠模型自己的压缩选择保留，不靠程序钉死某条。
- **展示口径必须跟随「裁剪/折叠之后真正发出的表面」**：agent 的 `Usage.ContextTokens`
  在裁剪完成后按 `msgs + tools` 计算（`sentSurface`），不是按裁剪前的全量历史。小窗口读
  超长会话时两者差着一整段被裁掉的历史。
- **没有增量基线的实测值不得当投影**（见 §4）：它会冻结在界面上。锚定必须无条件做，
  且投影输入与锚定基线同源（都是发出去的那份表面）。
- **展示值一律夹在当前窗口内**：超过容量的部分发不出去（agent 会先裁掉），显示「714K/128K」
  只是在描述一个永远不会被发出的请求。这条对**启动预置**尤其要紧——预置值来自上一次请求，
  而那次可能跑在 1M 云模型上，这次换成了 128K 本地模型；且 `Seed` 发生在窗口已知之前
  （`LoadHistoryReplay` 先跑，`tui.New` 才 `SetContextWindow`），所以夹取必须在读取时做。
- 新增会话事件：`EventType` 常量 + `Data` 结构 + `session/persist.go` 解码注册，三处齐。
- 用量锚点只能通过 `session.LastUsageAnchor` 取（宿主与 agent 共用一份）。
- log-only 事件必须以 `Append(..., nil)` 写入（不进 surface）。

**禁止**

- 在插件里写 `len/4`、`max(bytes/4,runes)` 之类公式。
- 把 `llm/attempt` 当信封用，或把信封字段塞回 `llm/attempt`。
- 用 `projected_tokens` 代替"自己渲染后的表面"（会压过头）。
- 拿 `Project(0)` 当「已用」展示值：它算的是 `实测 − 该次请求的估算`（启发式误差），
  不是规模，量级几万且几乎不随会话增长——界面会永远停在一个假数字上。
- 用 `UpdateUsage(usage)` 的实测值当投影（无基线，见 §4）。
- 裁一次就判定超窗（估算与投影分叉时会误报；见 §10）。
- 靠程序僵硬地「钉死首条消息 / 首条 user 不被裁不压缩」：超长会话（数十 MB 级）的「首条
  user 消息」大概率是几周前的失效旧需求，强行保留既无效又架空模型自身的压缩判断。裁剪/压缩
  只需保 `system` + 最近 N 条（保留尾），早期历史随滑动窗口自然淘汰即可。
- 用列表下标（`msg-<n>`）当消息身份：`history_injection`、请求级裁剪、fork 都会让
  同一个引用指到另一条消息——那是「遮错内容」，不是「压得不准」。
- 在有压缩后端时于 agent 里做请求级裁剪、折叠，或拿改写前的估算做「单轮超窗」硬报错
  （§7.1）。这三件事量的都不是将要发出的那份表面。
- 宿主按插件名分发用量相关能力（应走声明式/事件）。

**守卫测试**（改坏了会红）：`plugin_names_guard_test.go`、`TestToolsFingerprint`、
`TestProjectPressure`、`TestLastUsageAnchor`、`TestRequestHeaderLogOnly`、
`TestEstimateProtoToolTokens`、`TestCompactionBasicMeasuresItsOwnSurface`、
`TestLsPipedIsOnePerLine`、`TestBackendActiveSkipsHistoryTrim`（有后端时不裁）、
`TestStableIDsSurviveHeadTrimming`（id 不随列表起点变）、
`TestRetainBoundaryKeepsToolPairing`（压缩切点不切断 tool 配对）。

**构建期契约检查**：`tools/backendcheck`（builder 打包后自动跑，见 AGENTS.md
「共享模块改动的最小验证面」）。它按 `compaction` 能力扫全部插件，用真实派生历史驱动
**实际二进制**，校验结构约束（配对完整 / 条数不膨胀 / 首条 system 保留 / id 唯一且非
位置式）。这条通道专门抓「改了一处、另一处静默变形」——各插件自己的单测手搓消息，
天然绕过共享派生路径，抓不到。

---

## 10. 误差与实测基线

同一会话、同一份配置下的真实对照（未发额外请求，取自历史记录与本地函数计算）：

| 场景 | 我们估 | provider 实报 | 偏差 |
|---|---|---|---|
| 对话型内容（历史样本） | 318,562 | 332K | **−4%** |
| 压缩后稳态请求（37,790 含工具） | 37,790 | 42,098 | −10% |
| 全量历史（923,391 含工具） | 923,391 | 1,246,855 | −26% |

结论：

- **4 字节/token 对 JSON/代码/工具输出偏乐观**（那份请求约 3.18 字节/token），密集工具
  输出的会话误差可达 −26%；对话内容仍在 4% 量级。
- 有锚点时只用差量，这部分固定偏差会抵消；误差真正致命的场景是**无锚点的首次请求**。
- 若将来要收窄：优先做"按会话实测比例自适应"（用 `usage ÷ 该请求估算` 做修正），
  而不是改 divisor——后者会让纯对话内容变成高估。

---

## 11. 常见改动指南

**加一个估算维度**（如视频引用）：只需在 `EstimateProtoMessageTokens` 里加一项，
所有调用方自动生效；如需单独统计，再导出一个 `EstimateXxxTokens` 并让 SDK 转发。

**加一个会话事件**：见 §9「三处齐」；并在 `session/usage.go` 或相应投影里决定是否参与计量。

**换压缩后端**：它必须自己回答"我要发的表面是什么"，然后按 §7 决定读哪个数；
新后端不得直接照抄 `dsc-system` 的分支，而要先说清自己的表面是否被改写。

另外三条硬要求：

- 消息身份用 `proto.Message.id`（稳定 id），**不要**按列表下标编号（§7.1）。
- 状态文件带 `schema` 版本；格式变了就加一，旧文件整体丢弃、由事件日志重建，
  不写逐字段迁移（AGENTS.md §4）。
- 打包后跑 `builder`，`tools/backendcheck` 会按能力找到你并验结构约束；配对被切断、
  条数膨胀、首条 system 丢失、出现位置式 id 都会让构建失败。

**调阈值**：窗口 `DSC_CONTEXT_WINDOW`（宿主注入）/ `DSC_COMPACTION_BASIC_CONTEXT_WINDOW`（插件专属，优先）；
触发比例 `DSC_COMPACTION_BASIC_THRESHOLD`（默认 0.80）；保留尾部 `DSC_COMPACTION_BASIC_RETAIN_RATIO`（默认 0.16）。
注意阈值比较的是"投影压力"，不是原始历史大小。

**动裁剪逻辑**（`trimToWindow` / `fitMessagesToWindow*`）：**先确认没有压缩后端**——
有后端时这条路径整段不执行（§7.1），把它改成无条件执行就会重现「发送面 932K/610K
交替、前缀缓存每步失效」。以下仅适用于无后端的兜底路径。

裁剪按消息估算选起点，超窗判定
却读投影值，两者口径不同（差两三倍并不罕见）。所以裁剪是**闭环**的：裁一次 → 用
`projectedFit`（投影口径，含工具目录）复核 → 不满足就按
`oversizeTrimFallbackRatios`（0.90 → 0.60 → 0.35 → 0.15）逐档收紧再裁，直到装得进或
无历史可裁。改动时不得退回"裁一次就判超窗"——那正是「省略六千多条之后还报 441195 /
131072」的成因；也不得让提示帧回显裁剪前的占用（那会让人以为裁剪没执行）。

---

## 12. 与 DSH 的对照

| DSH 做法 | DSC 现状 | 取舍理由 |
|---|---|---|
| `TokenMeter.measure(session, header)` 单一服务，用 session projection 重放 | 宿主 `projectPressure` + agent 本地 `TokenMeter` | 进程隔离：agent 必须本地决策 |
| `request/header` 独立事件 | 已引入，字段对齐（tools 指纹 + model + window） | 已对齐 |
| node 级表面差分（逐节点定价，替换只扣该段） | 整表估算 + 差量 | 结构不同（DSC 无 node 概念），精度略粗但判据一致 |
| usage 仅在 header 相同且不低于该表面启发式价时复用 | 同（§5.3、§6） | 已对齐 |
| `CHARS_PER_TOKEN=4` 固定密度 | CJK 感知分类累加 | DSH 自认该口径对 CJK/JSON 严重低估 |

---

## 13. 术语

| 术语 | 含义 |
|---|---|
| **表面（surface）** | 本次请求真正会被模型看到的内容 = 消息 + 工具目录（可能被压缩后端改写） |
| **锚点（anchor）** | 上一次成功请求的实测用量，附其信封指纹 |
| **信封（envelope）** | 产生该用量的请求身份：工具目录指纹 + 模型 + 窗口 |
| **投影（projection）** | 锚点 + 表面差量得到的"下一次大概多少"，用于决策 |
| **可比（comparable）** | 锚点信封与当前请求一致，且用量不低于该信封的启发式估值 |
| **稳定消息 id** | `proto.Message.id`，形如 `event-3921`（派生时取 surface 事件 seq）：唯一、与派生范围无关，是跨请求指认消息的唯一身份 |
