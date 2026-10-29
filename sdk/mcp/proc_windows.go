//go:build windows

package mcp

import (
	"os/exec"
	"syscall"
)

// createNoWindow 对齐 Windows SDK 的 PROCESS_CREATION_FLAGS（同
// golang.org/x/sys/windows 的 CREATE_NO_WINDOW）：子进程拿不可见控制台而非新建可见
// 窗口。标准库 syscall 未定义该常量，故本地声明。
//
// 这里自带一份而不是调 SDK 根的 ConfigureChildProcessAttrs：本包刻意不依赖 SDK 根的
// 任何东西——根里 re-export 了 core 的类型，一旦本包引用它，core 反过来用本包就成环。
const createNoWindow = 0x08000000

// configureChildProcessAttrs 隐藏 stdio 服务器的控制台窗口（否则每拉一个 MCP 服务器
// 都会弹一个黑框）。调用方已自定义 SysProcAttr 时不覆盖。
func configureChildProcessAttrs(cmd *exec.Cmd) {
	if cmd.SysProcAttr != nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
