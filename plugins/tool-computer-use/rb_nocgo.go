//go:build !cgo

package main

import (
	"errors"
	"fmt"
	"image"
	"os"
)

// inputBackendReady 报告本构建是否带真实输入/截图后端（无 cgo 版恒为 false）。
const inputBackendReady = false

// errNoInputBackend 无 CGO 构建下每次输入/截图调用的错误。
//
// 为什么要有这个变体：robotgo 是 cgo 依赖（需要 C 工具链 + 系统输入库）。在没有工具链
// 的机器上，包级编译失败会把**所有**单元测试一起拖死——连 clampInt、normalizeKey、
// 截图编码这些纯逻辑用例都跑不了。这里用同名同型的报错替身让包能编译：
//
//   - 纯逻辑测试照常跑（它们本来就不碰 robotgo）；
//   - 真实输入/截图的用例自己 t.Skip（见 main_test.go 的 requireInputBackend）；
//   - 真跑到工具上时，每次调用返回一条能读懂的错误，而不是编译期失败或静默无效。
//
// 发布包不会走到这里：builder 走 cgo 构建（工具链自动探测），打进 dist 的是真实后端。
var errNoInputBackend = errors.New(
	"computer_use 不可用：本二进制构建时未启用 CGO（需要 C 工具链与系统输入库）")

// 与 rb_cgo.go 同名同型的替身。签名必须与真实机器人库一致，否则 main.go 的调用点
// 会在某一变体下编译失败——这是刻意的（漂移立刻可见）。
var (
	rbCapture     = func(args ...int) (*image.RGBA, error) { return nil, errNoInputBackend }
	rbScreenSize  = func() (int, int) { return 0, 0 }
	rbSysScale    = func(displayID ...int) float64 { return 1 }
	rbDisplaysNum = func() int {
		return 0
	}
	rbMousePos   = func() (int, int) { return 0, 0 }
	rbMoveClick  = func(x, y int, args ...interface{}) {}
	rbClick      = func(args ...interface{}) {}
	rbMoveMouse  = func(x, y int) {}
	rbMoveSmooth = func(x, y int, args ...interface{}) bool { return false }
	rbScrollDir  = func(x int, direction ...interface{}) {}
	rbTypeStr    = func(str string, args ...int) {}
	rbKeyTap     = func(key string, args ...interface{}) error { return errNoInputBackend }
	rbMouseDown  = func(key ...interface{}) error { return errNoInputBackend }
	rbMouseUp    = func(key ...interface{}) error { return errNoInputBackend }
	rbWriteAll   = func(text string) error { return errNoInputBackend }
	rbReadAll    = func() (string, error) { return "", errNoInputBackend }
	rbMilliSleep = func(tm int) {}
)

func init() {
	// 启动即说明一次：否则部署到无 CGO 构建的用户只会看到工具调用报错，不知道是构建问题。
	if os.Getenv("DSC_COMPUTER_USE_SILENT_NOCGO") == "" {
		fmt.Fprintf(os.Stderr, "tool-computer-use: %v\n", errNoInputBackend)
	}
}
