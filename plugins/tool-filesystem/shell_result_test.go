package main

import (
	"strings"
	"testing"
)

// formatShellResult 是 shell 工具交给模型的最终文本，进度帧必须在这里折叠掉：
// 实测一条 `git worktree add` 结果 2415 字符里 2112 是再也看不见的中间帧，
// 模型照单全收既费 token 又干扰判断。
func TestFormatShellResultCollapsesProgress(t *testing.T) {
	out := formatShellResult("", "Updating files:  38% (1819/4780)\rUpdating files: 100% (4780/4780), done.\nHEAD is now at abc\n", 0)
	if strings.ContainsRune(out, '\r') {
		t.Fatalf("结果仍含 CR: %q", out)
	}
	if !strings.Contains(out, "100% (4780/4780), done.") || !strings.Contains(out, "HEAD is now at abc") {
		t.Fatalf("应保留最终帧与后续行: %q", out)
	}
	if strings.Contains(out, "38%") {
		t.Fatalf("中间帧不应保留: %q", out)
	}
}

// CRLF（Windows 文本）归一为 LF：与 Unix 侧行为一致，模型不会看到 \r 噪声。
func TestFormatShellResultNormalizesCRLF(t *testing.T) {
	out := formatShellResult("", "a\r\nb\r\n", 0)
	if out != "a\nb\n" {
		t.Fatalf("CRLF 应归一为 LF, got %q", out)
	}
}

// 失败路径：归一后仍要正确附上退出码；纯进度输出失败时也一样。
func TestFormatShellResultKeepsExitCode(t *testing.T) {
	out := formatShellResult("", "10%\r20%\rfatal: nope\r\n", 128)
	if !strings.HasPrefix(out, "fatal: nope\n") {
		t.Fatalf("应保留最终帧, got %q", out)
	}
	if !strings.Contains(out, "[exit_code: 128]") {
		t.Fatalf("应保留退出码, got %q", out)
	}
	if strings.ContainsRune(out, '\r') {
		t.Fatalf("结果仍含 CR: %q", out)
	}
}

// 只有 \r 的输出归一后为空 → 走「(no output)」分支，而不是吐出空行。
func TestFormatShellResultOnlyCRIsEmpty(t *testing.T) {
	out := formatShellResult("", "\r", 0)
	if !strings.Contains(out, "(no output)") {
		t.Fatalf("纯 CR 输出应视为空, got %q", out)
	}
}

// TestFormatShellResultMarksShortCircuit 命令链在 && 处中断时必须明说「后面的没跑」。
//
// 现实触发：`cd <不存在目录> && python3 << 'PY' …` —— 整条链只有 cd 失败、后续一条
// 都没执行，输出里只剩一句找不到目录。模型看到空/极短输出会误判成「这个环境不支持
// heredoc / 多行脚本」，于是换着写法反复重试同一个错误。
func TestFormatShellResultMarksShortCircuit(t *testing.T) {
	out := formatShellResult("cd /nope && python3 << 'PY'\nprint(1)\nPY",
		"cd: no such file or directory: /nope\n", 1)
	if !strings.Contains(out, "commands after && were not run") {
		t.Fatalf("应标明命令链中断: %q", out)
	}
	if !strings.Contains(out, "[exit_code: 1]") {
		t.Fatalf("退出码标记不得丢: %q", out)
	}
	// 不含 && 的命令不加这条提示
	if plain := formatShellResult("cd /nope", "cd: no such file or directory: /nope\n", 1); strings.Contains(plain, "commands after &&") {
		t.Fatalf("无 && 时不应出现中断提示: %q", plain)
	}
	// 成功路径不加提示
	if ok := formatShellResult("a && b", "done\n", 0); strings.Contains(ok, "commands after &&") {
		t.Fatalf("成功时不应出现中断提示: %q", ok)
	}
}
