package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/mattn/go-isatty"
)

// 本文件负责启动参数的两类「响亮失败」，避免打错参数或非终端调用静默落入
// 交互式 TUI（历史表现：`dsc --version` 直接进 TUI，在管道/嵌套调用下永远
// 不返回，只能等空闲超时或被手动取消）：
//
//  1. 未知 flag：解析循环此前静默忽略，现直接报用法错误并以 2 退出；
//  2. 非终端启动：stdin/stdout 任一不是终端时不进 TUI，提示改用 -input。

// knownFlags 是全部可接受的启动参数。新增参数时必须与 main.go 的解析循环
// 同步登记，否则会被当作未知 flag 拒绝。
// 版本相关的三种 flag 形式由 isVersionCommand 在解析循环之前处理，这里登记
// 是为了让校验口径与「实际接受的命令」一致。
var knownFlags = map[string]bool{
	"-log":      true,
	"-mode":     true,
	"-input":    true,
	"-headless": true,
	"-debugger": true,
	"-admin":    true,
	"-hooks":    true,
	"-patch":    true,
	"-version":  true,
	"--version": true,
	"-v":        true,
}

// flagTakesValue 报告该 flag 是否可能携带取值（-log 的取值可选）。
func flagTakesValue(flag string) bool {
	switch flag {
	case "-log", "-mode", "-input", "-admin", "-hooks", "-patch":
		return true
	}
	return false
}

// validateStartupArgs 对未知 flag 响亮报错。位置参数（version / setup）由
// 更早的分支处理，这里跳过；带值 flag 的取值也跳过，避免把取值当 flag 校验。
func validateStartupArgs(args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}
		if !knownFlags[a] {
			return fmt.Errorf("unknown flag %q", a)
		}
		if flagTakesValue(a) && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
		}
	}
	return nil
}

// startupUsage 是参数错误时打印的用法摘要。
const startupUsage = `usage:
  dsc [options]
  dsc version            print version (-version / --version / -v 等价)
  dsc setup              interactive config wizard

options:
  -input <task>          one-shot run, no TUI
  -mode <preset>         preset under config/presets/
  -admin <addr>          admin API listen address
  -hooks <path>          external script hooks config
  -patch <file>          config overlay (repeatable)
  -log [file]            enable logging (file, or screen when omitted)
  -headless              headless mode for CI (requires -input)
  -debugger              expose /debugger routes
`

// stdioIsTerminal 报告 stdin/stdout 是否都连接到真实终端。
// 交互式 TUI 需要可读的终端输入与可渲染的终端输出；任一不满足时进入 TUI
// 会打开 /dev/tty 或直接阻塞（嵌套调用即卡死），应改为提示非交互入口。
func stdioIsTerminal() bool {
	return isTerminalFile(os.Stdin) && isTerminalFile(os.Stdout)
}

// isTerminalFile 是 isatty 的薄封装（含 Windows 的 Cygwin/MSYS 终端）。
// 注意不能用 os.ModeCharDevice 判断：/dev/null 也是字符设备。
func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	fd := f.Fd()
	return isatty.IsTerminal(fd) || isatty.IsCygwinTerminal(fd)
}
