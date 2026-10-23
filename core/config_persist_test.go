package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetHistoryInjectionConfigRoundTrip 校验 /settings history 持久化原值写入：
// config 与 agent 共用 DSC 三值语义（负值=不注入恢复历史、0=未定义、N 条），
// 不做任何编码换算。
func TestSetHistoryInjectionConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("default_llm: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m := newRouterManager()
	m.configPath = cfgPath

	load := func() int {
		t.Helper()
		cfg, err := LoadConfig(cfgPath)
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}
		return cfg.HistoryInjection
	}

	for _, want := range []int{-1, 0, 5} {
		if err := m.SetHistoryInjectionConfig(want); err != nil {
			t.Fatalf("SetHistoryInjectionConfig(%d): %v", want, err)
		}
		if got := load(); got != want {
			t.Fatalf("config = %d, want %d（原值写入）", got, want)
		}
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
