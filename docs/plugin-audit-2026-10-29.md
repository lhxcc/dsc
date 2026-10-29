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

## 四、留给以后的两件事（本次未动）

1. `tool-lua-host` / `tool-sql-host` 对 core 子包的依赖：需要先在 `sdk/` 为 `llmclient` /
   `toolclient` / `notify` 补别名，再删例外。属于重构，不是缺陷。
2. `tool-computer-use` 在本机测试需要 C 工具链（`zig`/`gcc`/`clang` 之一）：CI 或无工具链
   机器上它会红，而 `builder` 会正常构建（本机走 `E://winkit//share//zig-...//zig.exe cc`）。
   要不要在测试里检测工具链并 `t.Skip`，由部署策略定。

   顺带记录一次**构建偶发**：同一台机器同一命令，一次 zig 编译失败（`所有编译器均失败`）、
   立刻重试即成功（`zig.exe cc 编译成功`）。不是代码问题，像是 zig 缓存/并发锁；若再遇到
   同样的红，先重试一次再怀疑代码。
