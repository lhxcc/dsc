//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup Linux / macOS 没有「弹出控制台窗口」这回事（隐藏窗口由
// dsc.ConfigureChildProcessAttrs 统一处理，此处为 no-op），只需把浏览器放进独立
// 进程组，便于结束时整组回收（Chrome 会派生一堆子进程）。
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
