//go:build windows

package main

import (
	"os/exec"
	"strconv"

	dsc "dsc-sdk"
)

// killProcessTree 结束浏览器及其所有子进程。
//
// 只 kill 主进程不够：Chrome 的渲染/网络子进程会多活一会儿，仍占着 user-data-dir 里的
// 文件（临时 profile 目录因此删不掉）。taskkill 自己是控制台程序，同样经统一防护网
// 隐藏控制台，否则关会话时会闪一下黑框——正是整条「自管进程」要避免的东西。
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	dsc.ConfigureChildProcessAttrs(k)
	_ = k.Run()
	_ = cmd.Process.Kill()
}
