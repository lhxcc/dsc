package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateStartupArgs 未知 flag 必须响亮失败（否则会静默落入 TUI，
// 在管道/嵌套调用下阻塞不返回）；带值 flag 的取值不得被当作 flag 校验。
func TestValidateStartupArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string // 空串表示应通过
	}{
		{name: "无参数", args: nil},
		{name: "带值 flag", args: []string{"-input", "hi", "-mode", "minimal"}},
		{name: "无值 flag", args: []string{"-headless", "-debugger"}},
		{name: "-log 无取值", args: []string{"-log"}},
		{name: "-log 带文件", args: []string{"-log", "/tmp/dsc.log"}},
		{name: "可重复 patch", args: []string{"-patch", "a.yaml", "-patch", "b.yaml"}},
		{name: "版本 flag 形式", args: []string{"--version"}},
		{name: "位置参数 setup", args: []string{"setup"}},
		{name: "未知长 flag", args: []string{"--verbose"}, wantErr: `unknown flag "--verbose"`},
		{name: "未知短 flag", args: []string{"-h"}, wantErr: `unknown flag "-h"`},
		{name: "取值以 - 开头按未知 flag 处理", args: []string{"-input", "-foo"}, wantErr: `unknown flag "-foo"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateStartupArgs(c.args)
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("validateStartupArgs(%q) = %v, want nil", c.args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("validateStartupArgs(%q) = %v, want error containing %q", c.args, err, c.wantErr)
			}
		})
	}
}

// TestStartupUsageMentionsVersionAndInput 用法摘要必须给出可用的非交互入口。
func TestStartupUsageMentionsVersionAndInput(t *testing.T) {
	for _, want := range []string{"dsc version", "-input <task>", "-headless"} {
		if !strings.Contains(startupUsage, want) {
			t.Errorf("startupUsage 缺少 %q", want)
		}
	}
}

// TestIsTerminalFileRejectsNonTTY 管道/常规文件/dev/null 都不是终端。
// 注意不能用 os.ModeCharDevice 判断——/dev/null 也是字符设备。
func TestIsTerminalFileRejectsNonTTY(t *testing.T) {
	if isTerminalFile(nil) {
		t.Error("nil file must not be a terminal")
	}

	f, err := os.Create(filepath.Join(t.TempDir(), "regular.txt"))
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer f.Close()
	if isTerminalFile(f) {
		t.Error("regular file must not be a terminal")
	}

	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open dev null: %v", err)
	}
	defer devNull.Close()
	if isTerminalFile(devNull) {
		t.Error("dev null must not be a terminal")
	}
}
