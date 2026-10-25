# tool-browser-use

DSC 插件：为模型提供网页浏览能力（抓取、搜索、点击、输入、截图），并尽量让浏览器**看起来像人开的**。

## 工具

| 工具 | 作用 |
| --- | --- |
| `web_fetch` | 打开 URL 取内容。`format` 三选一：`layout`（默认，保留视觉行列）／`text`（原始 innerText，紧凑）／`html`（原始标记） |
| `web_search` | 用搜索引擎（google / duckduckgo / baidu / so360）搜索并提取结果条目 |
| `browser_click` | 按 CSS 选择器点击元素 |
| `browser_type` | 向输入框写入文本，可选回车提交 |
| `browser_screenshot` | 截图（可视区或整页） |

`user_mode`（默认 `true`）控制**是否复用系统 Chrome 的 user-data-dir**（带上登录态），
它不影响有没有窗口——浏览器默认无头运行，不会弹窗。会话一旦建立，该参数即固定；
要换 profile 得先关会话（`session_id` 变化会新建）。

## 取文：三种形态（`format`）

| 值 | 内容 | 何时用 |
| --- | --- | --- |
| `layout`（默认） | 按视觉版面还原的文本：同一行的内容仍在同一行、列大致对齐 | 读文章、看表格/卡片/导航，需要「谁和谁并排」这层信息 |
| `text` | 原始 `document.body.innerText`：紧凑、无补白，但并排的块会被拆成多行 | 只要文字、不在意版面（省 token） |
| `html` | 原始 HTML 标记 | 自己解析结构（选择器、属性、JSON-LD） |

别名宽容：`visual`/`default` → `layout`，`plain`/`innertext` → `text`，`raw`/`markup` → `html`；
大小写与首尾空白忽略。旧参数 `return_html: true` 仍然接受，等价于 `format=html`（不再出现在 schema 里）。
非法值直接报错并列出可选值——静默回退会让模型以为拿到了 `layout`。

## 取文：版面是怎么还原的

`web_fetch` 的纯文本模式不再用 `document.body.innerText`——它按 DOM 块结构断行，flex/grid
页面上并排的导航项、表格单元格会被拆成一词一行。现在的做法分两步：

1. **测量（页面内 JS）**：遍历文本节点，取每段在视口里的矩形、页面实测字宽、所属「排布容器」
   （最近的 flex/grid/table/table-row 祖先，否则最近的块级祖先）。被自动换行拆成多行的文本节点
   按各行框宽度比例切分，段内内联链接因此能留在它原来那一行。
2. **排版（Go，`layout.go`）**：按容器分组（容器间保持 DOM 顺序 = 阅读顺序），容器内按垂直重叠
   聚成视觉行，行内按 x 补白对齐列，列位用全页「列带」量化吸收表格 1~2px 的抖动。

约束与取舍：

- 跨容器永不合并。正文行与同高度的侧栏项属于不同容器，强行并排会读成病句（维基百科是典型）。
- 一行里只要有长块（> 60 列），整行改为逐块成行，不做拼接。
- 补白会让门户首页的输出约一半字符是空格（保留列位的成本）；文章页几乎没有。
- 任何一步失败（脚本异常、页面太怪）都退回 `innerText`，取文不会整体失败。

## 反爬

三条要求，都在插件侧解决（rod 的默认值恰好全踩）：

| 问题 | rod 默认 | 我们的做法 |
| --- | --- | --- |
| `navigator.webdriver` | `true` | `disable-blink-features=AutomationControlled`（真正起作用的一条）+ 删掉 `--enable-automation` |
| 平台自相矛盾 | `rod.New()` 默认模拟 MacBook：Windows 上 UA 报 Macintosh + Chrome/114 | `NoDefaultDevice()`，UA/视口回到浏览器真实值 |
| 版本错配导致 profile 打不开 | 找不到浏览器就下载自带旧 Chromium，用户 v155 profile 被拒 → `user_mode` 静默降级 | 优先 `DSC_BROWSER_BIN` → 系统 Chrome → Edge |

`navigator.webdriver` 的四个组合实测（Chrome 155）：

| 组合 | `navigator.webdriver` |
| --- | --- |
| 只带 `--enable-automation`（rod 默认） | `true` |
| 带 `--enable-automation` + `disable-blink-features=…` | `false` |
| 只删 `--enable-automation` | `true` ← 删了也没用 |
| 两者都做（现状） | `false` |

只删命令行开关不够：该属性挂在 Blink 的 `AutomationControlled` 运行时特性上，页面一旦被
CDP 客户端接管就会被打开。删 `--enable-automation` 是为了另一件事——它还会挂「正受自动
测试软件控制」提示条、关掉翻译/恢复页面等交互，是明面上的自动化痕迹。

实测（Chrome 155，`--headless=new` 对比有头）：`navigator.plugins=5`、WebGL 渲染器是真实显卡
（ANGLE/NVIDIA，不是 SwiftShader）、`window.chrome=object`、`outer/inner` 1280×900 / 1264×805、
`visibilityState=visible`、`webdriver=false`、UA 为真实 Windows Chrome——两者一致，差别只剩
有没有一个窗口。所以默认**无头不弹窗**。

无头会话的 UA 里 Chrome 会写 `HeadlessChrome/xx`，建页时用页面自身 UA 去掉该标记再回设
（版本号与平台保持真实，不另编 UA）。

## 进程启动（Windows 弹窗问题）

浏览器由插件自己 `exec` 起，不走 `rod` 的 `launcher.MustLaunch`。原因在 Windows 上：

- rod 给子进程只设 `CREATE_NEW_PROCESS_GROUP`，**不设 `HideWindow` / `CREATE_NO_WINDOW`**。
  当 DSC 从「没有控制台」的上下文（快捷方式、GUI 宿主、某些终端分屏）拉起时，Windows 会给
  控制台子系统子进程新建一个控制台窗口，屏幕上一闪而过一个黑框；
- rod 默认开 leakless：把浏览器二进制复制到临时目录、经一个 Go 小进程转启，退出时再调
  `taskkill`——都是控制台进程，闪烁概率更高。

现在 `launch_proc.go` 自己起进程：`SysProcAttr{HideWindow: true, CreationFlags: CREATE_NO_WINDOW}`
（Linux/macOS 用 `Setpgid`），参数仍由 rod 的 `FormatArgs()` 生成（flag 集合不变），leakless 彻底不用。
附带一个修复：进程句柄归我们管，会话关闭时 `CloseSession` 会真正结束浏览器，不再残留 chrome 进程。

## 环境变量

| 变量 | 作用 |
| --- | --- |
| `DSC_BROWSER_BIN` | 指定浏览器可执行文件（优先级最高） |
| `DSC_BROWSER_HEADLESS=1` | 强制无头（默认即无头；优先级最高） |
| `DSC_BROWSER_HEADED=1` | 有头且**可见**，用于观察模型在做什么 |
| `DSC_BROWSER_OFFSCREEN=1` | 有头但窗口挪到 `-32000,-32000`（看不见，任务栏留有图标）——真浏览器、无弹窗 |
| `DSC_BROWSER_CDP_URL` | 不启动浏览器，直接连已存在的 CDP 端点（`http://127.0.0.1:9222`）。用自己手动起的 Chrome 时最接近真人：没有自动化开关，也无 `--enable-automation` |

优先级：`HEADLESS` > `HEADED` > `OFFSCREEN` > 默认（无头）。

启动失败会逐级降级：用户 profile（当前模式）→ 无头 → 隔离 profile（无登录态）。每次命中的组合
都记在插件日志里（`user-mode session ... headed=true/false`）。

**降级会告知模型**：请求了 `user_mode`、实际却拿到隔离会话时（典型是默认 profile 被运行中的
Chrome 占用），`web_fetch` 结果里会带 `note` 字段说明「本次为隔离会话，页面要求登录属正常」。
否则模型只会看到一张登录页，然后把原因误判成站点反爬。

## 测试

```sh
go test ./...                                  # 单测（版面排版、指纹策略、浏览器候选）
DSC_BROWSER_E2E=1 go test -run 'Layout|Stealth' -v   # 需要本机 chromium，默认跳过
```
