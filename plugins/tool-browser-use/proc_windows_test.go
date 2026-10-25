//go:build windows

package main

import "testing"

// TestHiddenProcAttr 子进程必须带「别弹窗」的两个属性：
//   - HideWindow：STARTF_USESHOWWINDOW + SW_HIDE；
//   - CREATE_NO_WINDOW：不给控制台子系统进程分配控制台（否则从无控制台的上下文
//     启动时会冒出一个黑框，一閃而過）。
//
// 这条曾经就是缺的：rod 的 launcher 在 Windows 上只设 CREATE_NEW_PROCESS_GROUP。
func TestHiddenProcAttr(t *testing.T) {
	a := hiddenProcAttr()
	if !a.HideWindow {
		t.Fatal("HideWindow 必须为 true")
	}
	if a.CreationFlags&createNoWindow == 0 {
		t.Fatalf("CreationFlags 必须含 CREATE_NO_WINDOW(0x%X)，实际 0x%X", createNoWindow, a.CreationFlags)
	}
}
