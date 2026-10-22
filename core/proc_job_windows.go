//go:build windows

package core

import (
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	jobOnce   sync.Once
	jobHandle windows.Handle // 故意保持打开到进程结束，见 EnableProcessJob 注释
	jobErr    error
)

// EnableProcessJob 把当前进程纳入一个带 JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE 的
// Job Object，为插件子进程及其孙进程提供「宿主消失即终止」的内核级兜底。
// 幂等；非 Windows 平台为空实现（见 proc_job_other.go）。
//
// 为什么是「宿主自加入」而不是逐个插件 AssignProcessToJobObject：
//   - Job 成员关系沿进程树自动继承——宿主进 job 后，之后 CreateProcess 出来的所有
//     后代（插件 exe、插件再拉起的 shell / 浏览器 / LSP 等）自动成为成员，无需在
//     每个拉起点接线，将来新增的拉起点也不会漏；
//   - 只用一个句柄，避免 N 个插件 N 次 OpenProcess+Assign 的权限与竞态问题
//     （进程刚创建时可能尚不可 Assign）。
//
// 兜底语义：句柄随宿主进程退出由 OS 回收，最后一个句柄关闭时内核按
// KILL_ON_JOB_CLOSE 终止 job 内全部进程。因此：
//   - 正常退出：client.Kill() / GracefulKill 已先做优雅关闭，job 只兜住漏网者；
//   - panic / os.Exit / 任务管理器强杀 / 宿主机断电前的强终止：Kill 链路根本来不及
//     执行，只有内核兜底能保证不残留（这正是本机制存在的理由）。
//
// 已知限制：Windows 8 之前不支持嵌套 job；若宿主已被某个不允许嵌套的 job 包住
// （部分 CI/终端宿主会这么做），AssignProcessToJobObject 会失败。此时返回错误由
// 调用方降级为告警——既有 Go 侧清理机制仍然有效，不影响启动。
func EnableProcessJob() error {
	jobOnce.Do(func() { jobErr = enableProcessJob() })
	return jobErr
}

func enableProcessJob() error {
	h, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return fmt.Errorf("CreateJobObject: %w", err)
	}

	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(
		h,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("SetInformationJobObject: %w", err)
	}

	self, err := windows.GetCurrentProcess()
	if err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("GetCurrentProcess: %w", err)
	}
	if err := windows.AssignProcessToJobObject(h, self); err != nil {
		_ = windows.CloseHandle(h)
		return fmt.Errorf("AssignProcessToJobObject: %w", err)
	}

	jobHandle = h // 不关闭：句柄存活即兜底存活
	return nil
}

// ProcessJobActive 报告兜底 Job Object 是否已生效（运行时自检与测试用）。
func ProcessJobActive() bool { return jobHandle != 0 }

// processJobHandle 供同包测试查询 job 配置（未启用时为 0）。
func processJobHandle() windows.Handle { return jobHandle }

// ProcessInJob 判断指定 PID 是否属于该兜底 job（测试与诊断用；pid=0 表示当前进程）。
// 未启用兜底 job 时返回错误，避免把「IsProcessInJob(h, NULL) 询问是否属于任意 job」
// 的语义误当成「属于本兜底 job」。
// 走 IsProcessInJob + OpenProcess：x/sys/windows 未导出 IsProcessInJob 封装。
func ProcessInJob(pid uint32) (bool, error) {
	if jobHandle == 0 {
		return false, fmt.Errorf("process job not enabled")
	}
	var proc windows.Handle
	if pid == 0 {
		self, err := windows.GetCurrentProcess()
		if err != nil {
			return false, err
		}
		proc = self
	} else {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
		if err != nil {
			return false, err
		}
		defer func() { _ = windows.CloseHandle(h) }()
		proc = h
	}
	var inJob int32
	isProcessInJob := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob")
	r, _, callErr := isProcessInJob.Call(uintptr(proc), uintptr(jobHandle), uintptr(unsafe.Pointer(&inJob)))
	if r == 0 {
		return false, callErr
	}
	return inJob != 0, nil
}
