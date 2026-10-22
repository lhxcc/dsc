package core

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetHistoryInjectionConfigEncoding 校验 /settings history 持久化编码往返：
// agent 值（0=off、-1=unlimited、N=条数）→ config 三态（-1 禁止、0 未定义、N 启用）。
func TestSetHistoryInjectionConfigEncoding(t *testing.T) {
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

	// agent off(0) → config -1（禁止）
	if err := m.SetHistoryInjectionConfig(0); err != nil {
		t.Fatalf("SetHistoryInjectionConfig(0): %v", err)
	}
	if got := load(); got != -1 {
		t.Fatalf("off → config = %d, want -1", got)
	}

	// agent unlimited(-1) → config 0（未定义/默认不限制）
	if err := m.SetHistoryInjectionConfig(-1); err != nil {
		t.Fatalf("SetHistoryInjectionConfig(-1): %v", err)
	}
	if got := load(); got != 0 {
		t.Fatalf("unlimited → config = %d, want 0", got)
	}

	// agent N(5) → config 5（启用 5 条）
	if err := m.SetHistoryInjectionConfig(5); err != nil {
		t.Fatalf("SetHistoryInjectionConfig(5): %v", err)
	}
	if got := load(); got != 5 {
		t.Fatalf("N=5 → config = %d, want 5", got)
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

// TestPluginEntryFromConfig 校验 replay 回滚所需的旧条目读取：命中同名条目并保留
// 字段，缺失条目返回 false（配置中不存在的插件不应产出可回滚条目）。
func TestPluginEntryFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	content := `plugins:
  - name: tool-x
    type: tool
    enabled: true
    binary_path: ./plugins/tool-x/tool-x
  - name: dsc-system
    type: dsc
    enabled: false
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newRouterManager()
	m.configPath = cfgPath

	entry, ok := m.pluginEntryFromConfigLocked("tool-x")
	if !ok {
		t.Fatal("tool-x should be found in config")
	}
	if entry.Type != "tool" || !entry.Enabled || entry.BinaryPath != "./plugins/tool-x/tool-x" {
		t.Fatalf("unexpected entry: %+v", entry)
	}

	if _, ok := m.pluginEntryFromConfigLocked("missing"); ok {
		t.Fatal("missing plugin should not be found")
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
