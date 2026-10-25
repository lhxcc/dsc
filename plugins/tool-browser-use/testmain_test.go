package main

import (
	"os"
	"testing"
)

// TestMain 保证**测试进程永远不会弹出浏览器窗口**。
//
// 浏览器类测试里有一部分走 CreateSession（由环境变量决定有头/无头）。开发者在自己的
// shell 里如果恰好导出了 DSC_BROWSER_HEADED=1 或 DSC_BROWSER_OFFSCREEN=1（这两个开关
// 本来就是给人工排查用的），那么 `go test` 会真的弹出一个 Chrome——测试只要验证行为，
// 不需要窗口。所以在进程入口把「要窗口」的开关清掉、强制无头，一次性兜住所有用例，
// 而不是靠每个测试自己记得设。
func TestMain(m *testing.M) {
	os.Setenv("DSC_BROWSER_HEADLESS", "1")
	os.Unsetenv("DSC_BROWSER_HEADED")
	os.Unsetenv("DSC_BROWSER_OFFSCREEN")
	os.Exit(m.Run())
}
