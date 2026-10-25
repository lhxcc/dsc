//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAliasStubHint 只有「解析到 %LOCALAPPDATA%\Microsoft\WindowsApps\ 下的占位符」
// 才提示；普通目录里的同名程序、显式路径都不该被误报。
//
// 背景：python3 在本机就是这种占位符（未装 Store 版），启动即 exit 49、零输出——
// 模型只能看到「非零退出 + 无输出」，无法判断是命令写错还是输出被吞。
func TestAliasStubHint(t *testing.T) {
	local := t.TempDir()
	aliasDir := filepath.Join(local, "Microsoft", "WindowsApps")
	if err := os.MkdirAll(aliasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(aliasDir, "python3.exe")
	if err := os.WriteFile(stub, []byte("reparse-point-placeholder"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("PATH", aliasDir+string(os.PathListSeparator)+`C:/Windows/System32`)

	hint := aliasStubHint("python3")
	if hint == "" {
		t.Fatalf("应识别为应用执行别名：stub=%s", stub)
	}
	for _, want := range []string{"应用执行别名", stub, "which"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("提示应包含 %q，实际 %q", want, hint)
		}
	}

	// 显式路径不猜（调用方已点名要跑哪个文件）
	if got := aliasStubHint(stub); got != "" {
		t.Fatalf("显式路径不该提示：%q", got)
	}

	// 同名程序放在普通目录里，不该被当成别名
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "python3.exe"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", plain)
	if got := aliasStubHint("python3"); got != "" {
		t.Fatalf("普通目录里的程序不该提示：%q", got)
	}
}
