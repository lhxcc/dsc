# tool-browser-use

DSC 插件：为模型提供网页浏览能力（抓取、搜索、点击、输入、截图），并尽量让浏览器**看起来像人开的**。

## 工具

| 工具 | 作用 |
| --- | --- |
| `web_fetch` | 打开 URL 取正文。`return_html=true` 时返回原始 HTML，否则返回纯文本（版面模式） |
| `web_search` | 用搜索引擎（google / duckduckgo / baidu / so360）搜索并提取结果条目 |
| `browser_click` | 按 CSS 选择器点击元素 |
| `browser_type` | 向输入框写入文本，可选回车提交 |
| `browser_screenshot` | 截图（可视区或整页） |

`user_mode`（默认 `true`）控制**是否复用系统 Chrome 的 user-data-dir**（带上登录态），
它不影响有没有窗口——浏览器默认无头运行，不会弹窗。会话一旦建立，该参数即固定；
要换 profile 得先关会话（`session_id` 变化会新建）。

## 取文：保住视觉版面

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
| `navigator.webdriver` | 带 `--enable-automation` → `true` | `Delete("enable-automation")` + `disable-blink-features=AutomationControlled` |
| 平台自相矛盾 | `rod.New()` 默认模拟 MacBook：Windows 上 UA 报 Macintosh + Chrome/114 | `NoDefaultDevice()`，UA/视口回到浏览器真实值 |
| 版本错配导致 profile 打不开 | 找不到浏览器就下载自带旧 Chromium，用户 v155 profile 被拒 → `user_mode` 静默降级 | 优先 `DSC_BROWSER_BIN` → 系统 Chrome → Edge |

实测（Chrome 155，`--headless=new` 对比有头）：`navigator.plugins=5`、WebGL 渲染器是真实显卡
（ANGLE/NVIDIA，不是 SwiftShader）、`window.chrome=object`、`outer/inner` 1280×900 / 1264×805、
`visibilityState=visible`、`webdriver=false`、UA 为真实 Windows Chrome——两者一致，差别只剩
有没有一个窗口。所以默认**无头不弹窗**。

无头会话的 UA 里 Chrome 会写 `HeadlessChrome/xx`，建页时用页面自身 UA 去掉该标记再回设
（版本号与平台保持真实，不另编 UA）。

## 环境变量

| 变量 | 作用 |
| --- | --- |
| `DSC_BROWSER_BIN` | 指定浏览器可执行文件（优先级最高） |
| `DSC_BROWSER_HEADLESS=1` | 强制无头（默认即无头；优先级最高） |
| `DSC_BROWSER_HEADED=1` | 有头且**可见**，用于观察模型在做什么 |
| `DSC_BROWSER_OFFSCREEN=1` | 有头但窗口挪到 `-32000,-32000`（看不见，任务栏留有图标）——真浏览器、无弹窗 |
| `DSC_BROWSER_CDP_URL` | 不启动浏览器，直接连已存在的 CDP 端点（`http://127.0.0.1:9222`）。用自己手动起的 Chrome 时最接近真人：没有自动化开关，也无 `--enable-automation` |

优先级：`HEADLESS` > `HEADED` > `OFFSCREEN` > 默认（无头）。

启动失败会逐级降级：用户 profile + 有头 → 用户 profile + 无头 → 隔离 profile（无登录态）。
每次命中的组合都记在插件日志里（`user-mode session ... headed=true/false`）。

## 测试

```sh
go test ./...                                  # 单测（版面排版、指纹策略、浏览器候选）
DSC_BROWSER_E2E=1 go test -run 'Layout|Stealth' -v   # 需要本机 chromium，默认跳过
```
