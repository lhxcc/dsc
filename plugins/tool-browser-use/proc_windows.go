//go:build windows

package main

import "syscall"

// CREATE_NO_WINDOW 在 Go 的 syscall 包里没有导出（只有 x/sys/windows 有），
// 这里按 Win32 文档给值：0x08000000。
const createNoWindow = 0x08000000

// hiddenProcAttr 让浏览器子进程不弹任何窗口。
//
// HideWindow 是 Go 专门为此提供的开关（等价于 STARTF_USESHOWWINDOW + SW_HIDE）；
// CREATE_NO_WINDOW 再往前一步：禁止为控制台子系统进程分配控制台——否则当 DSC 从
// 「没有控制台」的上下文（快捷方式、GUI 宿主、某些终端的分屏）拉起时，Windows 会给
// 子进程新建一个控制台窗口，屏幕上就是一閃而過的黑框。
func hiddenProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
}
