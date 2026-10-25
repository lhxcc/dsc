//go:build windows

package main

import "os/exec"

// configureProcessGroup Windows 上把子进程放进独立进程组没有意义（整棵结束走
// taskkill /T，按 PID 追子树），控制台隐藏已由 dsc.ConfigureChildProcessAttrs
// 统一负责（HideWindow + CREATE_NO_WINDOW）。
func configureProcessGroup(_ *exec.Cmd) {}
