//go:build !windows

package core

// EnableProcessJob 非 Windows 平台为空实现。
//
// Job Object 是 Windows 内核对象；POSIX 侧插件子进程的回收由既有机制负责
// （go-plugin 的 client.Kill / GracefulKill 信号链，以及宿主的 pid 监控）。
// 保留同名空实现，使 main.go 的接线无需按平台分叉。
func EnableProcessJob() error { return nil }

// ProcessJobActive 非 Windows 平台恒为 false（无 Job Object 概念）。
func ProcessJobActive() bool { return false }

// ProcessInJob 非 Windows 平台无对应语义，恒返回 false。
func ProcessInJob(uint32) (bool, error) { return false, nil }
