//go:build !windows

package main

import "os/exec"

// killProcessTree 结束浏览器进程。
//
// Unix 不假设进程组：本插件启动浏览器时不改 SysProcAttr（统一防护网在非 Windows 是
// no-op），因此只结束主进程；浏览器自身的子进程会随它的退出被回收。
func killProcessTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
