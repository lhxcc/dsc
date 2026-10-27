package main

import (
	"strings"
	"testing"
)

// formatShellResult 是 shell 工具交给模型的最终文本，进度帧必须在这里折叠掉：
// 实测一条 `git worktree add` 结果 2415 字符里 2112 是再也看不见的中间帧，
// 模型照单全收既费 token 又干扰判断。
func TestFormatShellResultCollapsesProgress(t *testing.T) {
	out := formatShellResult("Updating files:  38% (1819/4780)\rUpdating files: 100% (4780/4780), done.\nHEAD is now at abc\n", 0)
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
	out := formatShellResult("a\r\nb\r\n", 0)
	if out != "a\nb\n" {
		t.Fatalf("CRLF 应归一为 LF, got %q", out)
	}
}

// 失败路径：归一后仍要正确附上退出码；纯进度输出失败时也一样。
func TestFormatShellResultKeepsExitCode(t *testing.T) {
	out := formatShellResult("10%\r20%\rfatal: nope\r\n", 128)
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
	out := formatShellResult("\r", 0)
	if !strings.Contains(out, "(no output)") {
		t.Fatalf("纯 CR 输出应视为空, got %q", out)
	}
}
