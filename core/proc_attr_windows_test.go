//go:build windows

package core

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestConfigureChildProcessAttrsHidesConsole 宿主唯一 chokepoint 的实际效果：
// 子进程必须带 HideWindow + CREATE_NO_WINDOW，否则从「没有控制台」的上下文（快捷方式、
// GUI 宿主、计划任务）拉起的 console 子进程会新建一个可见控制台窗口，屏幕上闪一下。
//
// 这条断言原本只存在于某个插件里（它当时自己写了份 SysProcAttr）；既然属性装配收拢到
// 本函数，守卫也该在这里——否则 chokepoint 的实现被改坏时没有任何测试会报。
func TestConfigureChildProcessAttrsHidesConsole(t *testing.T) {
	cmd := exec.Command("cmd")
	ConfigureChildProcessAttrs(cmd)

	if cmd.SysProcAttr == nil {
		t.Fatal("Windows 上必须设置 SysProcAttr")
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("必须设置 HideWindow")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("CreationFlags 必须含 CREATE_NO_WINDOW(0x%X)，实际 0x%X",
			createNoWindow, cmd.SysProcAttr.CreationFlags)
	}

	// 约定：调用方已自带 SysProcAttr 时不覆盖（合并语义由调用方负责）。
	own := exec.Command("cmd")
	own.SysProcAttr = &syscall.SysProcAttr{}
	ConfigureChildProcessAttrs(own)
	if own.SysProcAttr.HideWindow || own.SysProcAttr.CreationFlags != 0 {
		t.Fatalf("不应覆盖调用方已有的 SysProcAttr：%+v", own.SysProcAttr)
	}
}
