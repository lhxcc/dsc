//go:build !windows

package mcp

import "os/exec"

// configureChildProcessAttrs 非 Windows 平台无控制台窗口概念，no-op。
func configureChildProcessAttrs(_ *exec.Cmd) {}
