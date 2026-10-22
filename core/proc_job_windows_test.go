//go:build windows

package core

import (
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TestEnableProcessJobConfiguresKillOnClose 验证宿主已加入带 KILL_ON_JOB_CLOSE 的
// job：这是「宿主消失即终止全部插件进程」的兜底前提。
func TestEnableProcessJobConfiguresKillOnClose(t *testing.T) {
	if err := EnableProcessJob(); err != nil {
		// 部分 CI/终端宿主会把进程放进不允许嵌套的 job（Win8 之前语义），此时降级
		// 为告警而非失败——既有 Go 侧清理机制仍有效。
		t.Skipf("当前环境不支持 Job Object 嵌套：%v", err)
	}
	if !ProcessJobActive() {
		t.Fatal("EnableProcessJob 成功后 ProcessJobActive 应为 true")
	}
	if err := EnableProcessJob(); err != nil {
		t.Fatalf("重复调用应幂等成功: %v", err)
	}

	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(processJobHandle(),
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil); err != nil {
		t.Fatalf("QueryInformationJobObject: %v", err)
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatalf("LimitFlags 缺 KILL_ON_JOB_CLOSE: %#x", info.BasicLimitInformation.LimitFlags)
	}

	in, err := ProcessInJob(0)
	if err != nil {
		t.Fatalf("IsProcessInJob: %v", err)
	}
	if !in {
		t.Fatal("宿主进程未进入兜底 job")
	}
}

// TestProcessJobInheritedByChild 验证后代进程自动继承 job——这是「宿主崩溃/被强杀
// 也不残留插件及其孙进程」的语义基础（逐个 AssignProcessToJobObject 覆盖不到孙进程）。
func TestProcessJobInheritedByChild(t *testing.T) {
	if err := EnableProcessJob(); err != nil {
		t.Skipf("当前环境不支持 Job Object 嵌套：%v", err)
	}
	cmd := exec.Command("cmd", "/c", "ping -n 6 127.0.0.1 > NUL")
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动子进程失败: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		in, err := ProcessInJob(uint32(cmd.Process.Pid))
		if err != nil {
			t.Fatalf("查询子进程 job 归属失败: %v", err)
		}
		if in {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("子进程未继承兜底 job（KILL_ON_JOB_CLOSE 将覆盖不到该进程）")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
