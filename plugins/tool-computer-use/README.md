# tool-computer-use

桌面自动化插件：截图、鼠标移动/点击、键盘输入等（基于 robotgo）。

## 平台与构建

- **macOS（本仓主要目标）**：需要 `CGO_ENABLED=1` 与 Xcode Command Line Tools。
  截屏走 **ScreenCaptureKit**（macOS 14+），因为 macOS 15 SDK 已将
  `CGDisplayCreateImage*` / `CGWindowListCreateImage` 标记为 obsolete。
  本仓通过 `libs/screenshot`、`libs/robotgo` 两个最小补丁 fork 提供实现
  （见插件 `go.mod` 的 `replace`）。
- **Windows / Linux**：沿用 robotgo 上游实现；Linux 需要 X11 开发库，
  且不在 CI 构建矩阵内（本机按平台构建）。

## 运行要求

- macOS 需在「系统设置 → 隐私与安全性」中为终端（或宿主 dsc 进程）授予：
  - **屏幕录制**（截图；未授权时截屏返回失败）
  - **辅助功能**（鼠标/键盘事件注入）
- 无权限时插件仍可加载，但调用截图/输入工具会返回错误。
- 截图坐标以最新一次 `computer_use_screenshot` 的画面为基准；多显示器时
  通过 `display` 参数选择。

## 构建示例

```bash
cd plugins/tool-computer-use
CGO_ENABLED=1 go build -o tool-computer-use .
```

宿主发布包由 `builder/builder.go` 的 `cgoPlugins` 在本机平台构建（不做交叉编译）。
