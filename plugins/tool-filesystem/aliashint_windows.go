//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// aliasStubHint 报告 name 解析到的是不是 Windows「应用执行别名」占位符。
//
// 这类占位符住在 %LOCALAPPDATA%\Microsoft\WindowsApps\，未安装对应 Store 应用时
// 启动即以非零码退出、且**什么都不打印**（实测 python3 别名 exit 49、stdout/stderr 均为空）。
// 对模型来说这是最难自查的一种失败：既不是「命令不存在」（那会 exit 127 并报错），
// 也不是脚本写错（那会有 traceback），只剩「非零退出 + 无输出」，只能盲目重试。
//
// 判据用目录而不是文件大小：别名的桩文件是 reparse point，实测 121 字节。
func aliasStubHint(name string) string {
	if name == "" || strings.ContainsAny(name, `/\`) {
		return "" // 显式路径：调用方自己指定了要跑的文件，不做猜测
	}
	local := os.Getenv("LOCALAPPDATA")
	if local == "" {
		return ""
	}
	aliasDir := strings.ToLower(local + `\Microsoft\WindowsApps\`)
	p, err := exec.LookPath(name)
	if err != nil || !strings.HasPrefix(strings.ToLower(p), aliasDir) {
		return ""
	}
	return fmt.Sprintf("shell: %s 解析到 Windows 应用执行别名（%s）。"+
		"未安装对应 Store 应用时，它不输出任何内容、直接以非零码退出——不是命令写错，也不是输出被吞。"+
		"请改用真正安装的可执行文件（which 可列出候选，或直接写全路径）。", name, p)
}
