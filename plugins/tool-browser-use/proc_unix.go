//go:build !windows

package main

import "syscall"

// hiddenProcAttr Linux / macOS 没有「弹出控制台窗口」这回事，只需把子进程放进独立
// 进程组，便于结束时整组回收（Chrome 会派生一堆子进程）。
func hiddenProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
