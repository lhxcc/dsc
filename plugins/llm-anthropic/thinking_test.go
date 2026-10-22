package main

import (
	"os"
	"testing"
)

// TestThinkingEnabled 守护「DSC_ANTHROPIC_THINKING 优先、旧名 ANTHROPIC_THINKING 回退」
// 的解析语义（此前只认旧名，与 DSC_ 前缀约定不一致）。
func TestThinkingEnabled(t *testing.T) {
	// 都未设置 → 默认开启。
	os.Unsetenv("DSC_ANTHROPIC_THINKING")
	os.Unsetenv("ANTHROPIC_THINKING")
	if !thinkingEnabled() {
		t.Fatal("未设置时应默认开启扩展思考")
	}

	// 关闭语义：0/false/off/no（大小写不敏感）。
	for _, off := range []string{"0", "false", "OFF", "No"} {
		t.Setenv("DSC_ANTHROPIC_THINKING", off)
		if thinkingEnabled() {
			t.Fatalf("%q 应关闭扩展思考", off)
		}
	}

	// DSC_ 前缀优先于旧名：DSC_=1 时即使旧名=0 也应开启。
	t.Setenv("DSC_ANTHROPIC_THINKING", "1")
	t.Setenv("ANTHROPIC_THINKING", "0")
	if !thinkingEnabled() {
		t.Fatal("DSC_ANTHROPIC_THINKING 应优先于旧名")
	}

	// 无 DSC_ 前缀时回退旧名。
	os.Unsetenv("DSC_ANTHROPIC_THINKING")
	t.Setenv("ANTHROPIC_THINKING", "0")
	if thinkingEnabled() {
		t.Fatal("无 DSC_ 时应回退旧名 ANTHROPIC_THINKING=0 关闭")
	}
}
