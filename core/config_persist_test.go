package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetHistoryInjectionConfigRoundTrip 校验 /settings history 持久化原值往返：
// config 与 agent 用同一套取值（-1 不限制、0 不注入跨轮历史、N 条），不做任何转码。
// 两侧曾经编码相反，预设里的 -1 被译成「不注入」，于是第二轮就看不到第一轮的历史。
func TestSetHistoryInjectionConfigEncoding(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("default_llm: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newRouterManager()
	m.configPath = cfgPath

	load := func() (int, bool) {
		t.Helper()
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		if cfg.HistoryInjection == nil {
			return 0, false
		}
		return *cfg.HistoryInjection, true
	}

	// off(0) → config 0（原值）
	if err := m.SetHistoryInjectionConfig(0); err != nil {
		t.Fatalf("SetHistoryInjectionConfig(0): %v", err)
	}
	if got, ok := load(); !ok || got != 0 {
		t.Fatalf("off → config = %d(present=%v), want 0", got, ok)
	}

	// unlimited(-1) → config -1（原值，不再被译成 0）
	if err := m.SetHistoryInjectionConfig(-1); err != nil {
		t.Fatalf("SetHistoryInjectionConfig(-1): %v", err)
	}
	if got, ok := load(); !ok || got != -1 {
		t.Fatalf("unlimited → config = %d(present=%v), want -1", got, ok)
	}

	// N(5) → config 5
	if err := m.SetHistoryInjectionConfig(5); err != nil {
		t.Fatalf("SetHistoryInjectionConfig(5): %v", err)
	}
	if got, ok := load(); !ok || got != 5 {
		t.Fatalf("N=5 → config = %d(present=%v), want 5", got, ok)
	}

	// 文件其余内容保留
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), "default_llm: x") {
		t.Fatalf("config lost unrelated content:\n%s", data)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
