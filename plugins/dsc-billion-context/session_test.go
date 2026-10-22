package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	bcacp "dsc-billion-context/acp"
	bcadapter "dsc-billion-context/adapter"
)

// TestGetSessionIDPrefersEventSession 会话 ID 优先级回归。
//
// 背景：宿主从不注入 DSC_SESSION_ID，此前 getSessionID 只能回落到「工作区派生键」，
// 同一工作区开多个会话/fork 会共用一份 state（refs/blocks/nudge 基线互相污染）。
// 宿主在 pre-step 事件里本来就带真实会话 ID，必须优先采用。
func TestGetSessionIDPrefersEventSession(t *testing.T) {
	bc := &BillionContext{}
	t.Setenv("DSC_SESSION_ID", "env-session")
	if got := bc.getSessionID(); got != "env-session" {
		t.Fatalf("未收到 pre-step 时应回退 DSC_SESSION_ID，got %q", got)
	}

	bc.setSessionID("sess-from-event")
	if got := bc.getSessionID(); got != "sess-from-event" {
		t.Fatalf("pre-step 事件里的会话应优先，got %q", got)
	}

	// 空值不得覆盖已有会话（避免偶发空 Session 把状态键打回工作区派生值）
	bc.setSessionID("")
	if got := bc.getSessionID(); got != "sess-from-event" {
		t.Fatalf("空会话不应覆盖已有值，got %q", got)
	}
}

// TestHandlePreStepAdoptsEventSession 端到端：pre-step 载荷的 session 成为 state 键，
// 即不同会话各自落一个 state 文件（会话隔离）。
func TestHandlePreStepAdoptsEventSession(t *testing.T) {
	dir := t.TempDir()
	bc := &BillionContext{
		store:  bcadapter.NewStateStore(dir),
		config: bcacp.DefaultConfig(200000),
	}

	payload := `{"session":"sess-A","messages":"[]","tokenCount":10}`
	if _, err := bc.handlePreStep(context.Background(), payload); err != nil {
		t.Fatalf("pre-step 失败: %v", err)
	}
	if got := bc.getSessionID(); got != "sess-A" {
		t.Fatalf("应采用事件会话，got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "sess-A"+bcadapter.StateFileSuffix)); err != nil {
		t.Fatalf("state 应写入 sess-A.json（会话隔离）: %v", err)
	}

	// 切到另一会话：写入新文件，旧文件保留
	payload = `{"session":"sess-B","messages":"[]","tokenCount":10}`
	if _, err := bc.handlePreStep(context.Background(), payload); err != nil {
		t.Fatalf("pre-step(B) 失败: %v", err)
	}
	for _, name := range []string{"sess-A" + bcadapter.StateFileSuffix, "sess-B" + bcadapter.StateFileSuffix} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("应保留 %s: %v", name, err)
		}
	}
}
