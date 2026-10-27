package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newProfileManager 造一个指向临时目录 config/config.yaml 的 Manager。
func newProfileManager(t *testing.T, active string) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	activePath := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(activePath, []byte(active), 0644); err != nil {
		t.Fatal(err)
	}
	m := NewManager(&ManagerConfig{ExecDir: dir})
	m.SetConfigPath(activePath)
	return m, activePath
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

// TestListConfigProfilesFilters 只认 config.<name>.yaml/.yml：
// 忽略 config.yaml 自身、忽略任意大小写组合含 example 的模板、忽略目录与其他文件名。
func TestListConfigProfilesFilters(t *testing.T) {
	m, activePath := newProfileManager(t, "providers: []\n")
	dir := filepath.Dir(activePath)
	for _, name := range []string{
		"config.DeepSeek.yaml",
		"config.local.yml",
		"config.example.yaml",
		"config.Example.Prod.yaml",
		"config.EXAMPLE.yaml",
		"config..yaml",
		"other.yaml",
		"config.README.txt",
	} {
		writeFile(t, filepath.Join(dir, name), "providers: []\n")
	}
	if err := os.MkdirAll(filepath.Join(dir, "config.dir.yaml"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := m.ListConfigProfiles()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	names := make([]string, 0, len(got))
	for _, p := range got {
		names = append(names, p.Name)
	}
	want := []string{"config.DeepSeek.yaml", "config.local.yml"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("配置档 = %v, want %v（example 任意大小写都必须忽略）", names, want)
	}
}

// TestListConfigProfilesMarksActive 与当前 config.yaml 同内容的档标 Active，
// 让菜单能指出「现在用的就是它」。
func TestListConfigProfilesMarksActive(t *testing.T) {
	m, activePath := newProfileManager(t, "model: same\n")
	dir := filepath.Dir(activePath)
	writeFile(t, filepath.Join(dir, "config.Same.yaml"), "model: same\n")
	writeFile(t, filepath.Join(dir, "config.Other.yaml"), "model: other\n")

	got, err := m.ListConfigProfiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("档数 = %d, want 2", len(got))
	}
	for _, p := range got {
		if wantActive := p.Name == "config.Same.yaml"; p.Active != wantActive {
			t.Fatalf("%s Active=%v, want %v（按文件名排序，不看下标）: %+v", p.Name, p.Active, wantActive, got)
		}
	}
}

// TestSwitchConfigProfile 切换 = 先备份当前 config.yaml（config-backups/），
// 再把目标档原子写回；切完 Active 翻转。
func TestSwitchConfigProfile(t *testing.T) {
	m, activePath := newProfileManager(t, "model: old\n")
	dir := filepath.Dir(activePath)
	target := filepath.Join(dir, "config.DeepSeek.yaml")
	writeFile(t, target, "model: deepseek\nplugins: []\n")

	backup, err := m.SwitchConfigProfile("config.DeepSeek.yaml")
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	got, err := os.ReadFile(activePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "model: deepseek\nplugins: []\n" {
		t.Fatalf("config.yaml 未被替换: %q", got)
	}
	// 备份必须落在 config-backups/ 且内容是切换前的那份
	if !strings.Contains(filepath.ToSlash(backup), "/config-backups/") {
		t.Fatalf("备份路径不在 config-backups/: %s", backup)
	}
	old, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("读备份: %v", err)
	}
	if string(old) != "model: old\n" {
		t.Fatalf("备份内容 = %q, want 切换前内容", old)
	}
	// Active 翻转
	profiles, _ := m.ListConfigProfiles()
	for _, p := range profiles {
		wantActive := p.Name == "config.DeepSeek.yaml"
		if p.Active != wantActive {
			t.Fatalf("%s Active=%v, want %v", p.Name, p.Active, wantActive)
		}
	}
}

// TestSwitchConfigProfileRejects 坏输入一律拒绝，且**不动**当前配置：
// 不存在的档、路径穿越、非 YAML、空映射。
func TestSwitchConfigProfileRejects(t *testing.T) {
	m, activePath := newProfileManager(t, "model: old\n")
	dir := filepath.Dir(activePath)
	writeFile(t, filepath.Join(dir, "config.Broken.yaml"), "model: [unclosed\n")
	writeFile(t, filepath.Join(dir, "config.Empty.yaml"), "{}")
	writeFile(t, filepath.Join(dir, "config.example.yaml"), "model: template\n")

	cases := []string{
		"config.Missing.yaml",     // 不在列表
		"../config.Broken.yaml",   // 路径穿越
		"config.example.yaml",     // 模板被过滤
		"config.Broken.yaml",      // 非 YAML
		"config.Empty.yaml",       // 空映射
		"config.Broken.yaml/../x", // 混入分隔符
	}
	for _, name := range cases {
		if _, err := m.SwitchConfigProfile(name); err == nil {
			t.Fatalf("%q 应被拒绝", name)
		}
	}
	got, _ := os.ReadFile(activePath)
	if string(got) != "model: old\n" {
		t.Fatalf("被拒绝的切换改动了 config.yaml: %q", got)
	}
	// 校验在备份之前：拒绝的切换不应留下备份
	if entries, _ := os.ReadDir(filepath.Join(dir, "config-backups")); len(entries) != 0 {
		t.Fatalf("被拒绝的切换留下了备份: %d", len(entries))
	}
}
