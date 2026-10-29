# 插件全面体检（2026-10-29）

起因：最近几轮把「压缩」从 agent 移到插件/宿主、「消息身份」换成稳定 id、「超窗」统一为
宿主滑窗、「压缩岗位」加了强依赖与启动闸门、「MCP」改为按需曝光。核心语义动得多，插件的
假设可能悄悄过期。这份记录是逐项核对的结果——**只记实际发生的事**，包括最后什么都没问题
的那些检查项，免得下次又从零开始怀疑。

## 一、运行面（机械检查）

| 项 | 结果 |
|---|---|
| 23 个插件模块 `go test -count=1 ./...` | 22 绿；`tool-computer-use` 在本机 shell 里编译失败（`gcc` 不在 PATH，robotgo 是 cgo 插件）。`builder` 自带工具链探测，能正常产出它的 exe（16.7 MB），所以这是**检查环境**的限制，不是插件缺陷。 |
| 根模块 `core` / `tui` / `session` | 绿 |
| `builder windows/amd64`（含 `tools/backendcheck`） | 20 插件、2 个 compaction 后端跑完结构检查、契约违规 0 |
| 仓库守卫（`TestFilepathBlacklistGuard` / `TestSlashTerminologyGuard` / `TestPluginsDoNotImportCore` / 插件名守卫） | 绿（本次修了 2 处自己引入的违规，见下） |

## 二、发现的问题（按严重度）

### 1. `tool-browser-use`：指定了外部 CDP 端点，却先报「找不到浏览器」（真 bug，已修）

`launchBrowserRod` 先查本地候选浏览器，空了就 `return error`，**之后**才看
`DSC_BROWSER_CDP_URL`。于是「用户自己起 Chrome + 指定端点」这条路径，在没有安装
Chrome/Edge 的机器上完全不可用；`TestE2EWithMockChromium`（mock CDP，本意就不依赖本机
浏览器）也因此红。

修法：把 CDP 端点分支提到候选检查之前，并在错误信息里补上 `DSC_BROWSER_CDP_URL`。
`TestE2EWithMockChromium` 现在绿。

### 2. `agent-react-loop`：版本号没跟上行为变更（已修）

agent 的性质变了（不再裁剪/折叠/摘要，且对压缩是强依赖），版本仍是 1.1.0。
→ 1.2.0。（`dsc-system` 已在上一轮升到 1.7.0。）

### 3. `dsc-billion-context` README 与实现不符（文档漂移，已修）

README 仍写「FNV-1a 内容 hash 生成稳定 ID」与「request-error 紧急压缩 + retry」。
实现早已改成：ref 以宿主下发的 `proto.Message.id`（surface 事件 seq）为键，缺 id 的消息
不参与压缩（**不**退回位置或内容 hash）；`agent/request-error` 不再订阅（超窗归宿主滑窗）。
两处已按实现重写。

### 4. `dsc-system` 注释仍在描述已删除的「溢出紧急压缩」（文档漂移，已修）

`main.go` 的驻留清单、事件分流注释、hook 订阅说明，以及 `compaction-basic.go` 的一处
行内注释，都还写着 `agent/request-error` 在做事。均已改为「超窗归宿主滑窗」。

### 5. 分层守卫有盲区：只匹配 `"dsc/core"` 精确串（守卫漏洞，已修）

`TestPluginsDoNotImportCore` 用 `strings.Contains(data, "\"dsc/core\"")`，**漏掉所有子包**
（`dsc/core/llmclient` 等）。实测有 3 个文件依赖 core 子包，而守卫仍打印「全部插件均已脱离
core」，例外表也写着「当前为空」——守卫在说谎。

修法：
- 匹配改前缀 `"dsc/core`；
- `dsc-system` 的 `attachLLM` 改收 `llmChat` 接口，去掉对 `core/llmclient` 的依赖（它本来就
  只用 `Chat` 一个方法，接口早就在那儿）；
- `tool-lua-host` / `tool-sql-host` 的脚本/SQL 绑定确实需要具体客户端类型，登记为例外并
  写明迁移计划（在 SDK 补齐别名后删除）。

### 6. 测试假设 POSIX（不是产品缺陷，已修）

- `tool-musicplayer`：期望值用 `filepath.Join`（Windows 反斜杆），产品输出统一正斜杆 →
  在 Windows 上必红。改为用同一口径 `dsc.PJoin` 构造期望值。
- `tool-pdf`：断言 `/tmp/xxx.pdf` 真实存在，但 `/tmp` 是宿主虚拟根，插件会映射到自己的临时
  目录。改为从工具输出里取实际落盘路径再断言（顺带让回读也用同一路径）。

### 7. 我自己在本次改动里撞的两条守卫（已修，记录以示守卫有效）

- `compaction-state.go` 用了 `filepath.Join` 拼状态文件路径（禁：Windows 反斜杆）→ 改 `dsc.PJoin`。
- 测试注释里写了「斜杠」（项目统一用「斜杆」）→ 改词。

## 三、核对过、确认没问题的点

| 检查项 | 结论 |
|---|---|
| 会话日志的写者 | 只有 `agent-react-loop` 写 `session.Append`；`tool-memory-service`、`dsc-billion-context` 只读（tail/重建）。压缩后端都不碰会话日志。 |
| 旧压缩路径残留 | 除 agent 里一行「剔除宿主标记」外，插件中已无 `trimToWindow` / `foldOversize` / `compactHistory` / `[DSC_COMPACTION_BACKEND_ACTIVE]` 的行为依赖。 |
| 位置式消息身份 | 无（`mNNNNN` 只是显示别名，`AssignRefs` 以 `msg.ID` 为键；`backendcheck` 也会拦 `msg-<数字>`）。 |
| 私有估算公式 | 无（全部走 `dsc.Estimate*`；billion 的旧公式已删，注释说明保留原因）。 |
| `pre-step` 轻载荷 | 4 个订阅者都处理了「没有 messages」：`dsc-system`（len==0 直接返回，不写状态）、`image-offload`、`billion`（`MessagesJSON==""` 返回）、`tool-memory-service`（只用 session/user_input）。 |
| 两阶段（compaction → projection） | `dsc-system` 按 `dsc.PreStepStageOf` 分流；非投影插件不会被叫第二遍。 |
| 能力声明 | 只有 `dsc-system` / `dsc-billion-context` 声明 `compaction`；`pre-step-transform` 只有 `dsc-system`；`auto-background` 只有 `tool-filesystem` 的 `shell`。无插件引用已删除的宿主 API（`WarnIfCompactionBackendMissing` 等）。 |
| 审批语义 | 插件只声明 `requires-never-approval` 能力，没有把 `approval=never` 当「自动放行」的地方；`agent` 只负责把策略渲染成提示文案。 |
| `mcp_*` 工具名 | 只在 agent 的「工具目录会变，需要重新取目录」判定里出现（`mcp_connect` / `mcp_load` / `mcp_unload`），宿主侧仍是唯一注册点。 |

## 四、层间依赖：LUA/SQL 绑定层已改走 SDK

`tool-lua-host` / `tool-sql-host` 的绑定层原来直接 `import dsc/core/llmclient|toolclient|notify`
（因为它们需要把聚合客户端存进 `Services` 结构体、再交给脚本侧使用）。按既定分层
（**core ← sdk ← plugins**）补齐了 SDK 面：

- `sdk/clients.go` 新增四个**类型别名**：`dsc.AgentClient` / `dsc.LLMClient` /
  `dsc.ToolClient` / `dsc.NotifyClient`（`type X = core.X`，方法集与类型身份一致，无包装层、
  无转换）；
- 两个绑定层改用别名，只 `import dsc-sdk`；
- `TestPluginsDoNotImportCore` 的例外表**清空**——现在全部插件确实只依赖 SDK，守卫那句
  「全部插件均已脱离 core」才成立。

## 五、`tool-computer-use` 在无 C 工具链机器上不再阻塞测试套件

robotgo 是 cgo 依赖，没有 C 工具链时整个包编译不过——连 `clampInt`、`normalizeKey`、
截图编码这些**根本不碰 robotgo** 的纯逻辑用例也一起跑不了。这属于「一个环境缺陷拖死整份
测试报告」，会让真正的回归淹没在噪音里。

改法：把 `rb*` 薄封装拆成两个构建变体（调用点保持不变，签名必须一致，漂移会立刻编译失败）：

- `rb_cgo.go`（`//go:build cgo`）：真实 robotgo，`inputBackendReady = true`；
- `rb_nocgo.go`（`//go:build !cgo`）：同名同型的替身，每次输入/截图返回一条能读懂的错误
  （`computer_use 不可用：本二进制构建时未启用 CGO…`），启动时向 stderr 说明一次
  （`DSC_COMPUTER_USE_SILENT_NOCGO` 可静音），`inputBackendReady = false`；
- 5 个真实输入/截图用例加 `requireInputBackend(t)` → 无 cgo 时 `t.Skip`；9 个纯逻辑用例照跑。

结果：无 cgo 环境下 `go test` = 9 PASS + 5 SKIP + ok；**发布包不受影响**——builder 走 cgo
构建（工具链自动探测），打进 dist 的仍是真实后端。

**另外记录一次构建偶发**：同一台机器同一命令，一次 zig 编译失败（`所有编译器均失败`）、
立刻重试即成功（`zig.exe cc 编译成功`）。不是代码问题，像是 zig 缓存/并发锁；再遇到同样的
红，先重试一次再怀疑代码。
