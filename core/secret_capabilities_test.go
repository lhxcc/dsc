package core

import (
	"strings"
	"testing"

	"dsc/proto/metadata"
)

// declaredPlugins 造一份「插件 → 能力声明」的元数据。
func declaredPlugins(m *Manager, caps map[string]map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, kv := range caps {
		m.coreMetadata[name] = &metadata.PluginInfo{Name: name, Capabilities: kv}
	}
}

// TestMissingCapabilityGrants 只有「宿主设了凭据 + 插件声明了依赖该能力的"
// "声明 + 没被显式授予」三条同时成立才告警——这类静默 401 是最难自查的问题。
func TestMissingCapabilityGrants(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	declaredPlugins(m, map[string]map[string]string{
		"consumer":      {"requires/dsc/admin": "true"},
		"bystander":     {"cron": "true"},
		"also-consumes": {"requires/dsc/admin": "true", "cron": "true"},
	})

	// 1) 宿主没设凭据：该能力当前无需认证，告警是噪声
	t.Setenv("DSC_ADMIN_TOKEN", "")
	if got := m.MissingCapabilityGrants(nil); len(got) != 0 {
		t.Fatalf("宿主未设置凭据时不应告警，got %v", got)
	}

	// 2) 宿主设了凭据、插件声明了依赖、但没有授予：两个消费者各报一条
	t.Setenv("DSC_ADMIN_TOKEN", "secret")
	got := m.MissingCapabilityGrants(nil)
	if len(got) != 2 {
		t.Fatalf("应告警 2 条（两个消费者），got %d: %v", len(got), got)
	}
	for _, msg := range got {
		if !strings.Contains(msg, "DSC_ADMIN_TOKEN") || !strings.Contains(msg, "config.yaml") {
			t.Fatalf("告警应点名凭据与修法，got %q", msg)
		}
		if strings.Contains(msg, "bystander") {
			t.Fatalf("没声明该能力的插件不应被告警：%q", msg)
		}
	}

	// 3) 显式授予后不再告警（授予写在插件自己的 config.yaml env: 里）
	grants := GrantMap(map[string]map[string]string{
		"consumer":      {"DSC_ADMIN_TOKEN": "secret"},
		"also-consumes": {"DSC_ADMIN_TOKEN": "secret"},
	})
	if got := m.MissingCapabilityGrants(grants); len(got) != 0 {
		t.Fatalf("已授予时不应告警，got %v", got)
	}

	// 4) 只授予其中一个：只剩另一个告警（授予是按插件、按键的）
	partial := GrantMap(map[string]map[string]string{"consumer": {"DSC_ADMIN_TOKEN": "secret"}})
	got = m.MissingCapabilityGrants(partial)
	if len(got) != 1 || !strings.Contains(got[0], "also-consumes") {
		t.Fatalf("应只报未授予的那个，got %v", got)
	}
}

// TestCapabilitySecretKeys 能力→凭据的对应关系是宿主侧契约，按能力名查，不认插件名。
func TestCapabilitySecretKeys(t *testing.T) {
	if keys := CapabilitySecretKeys("admin"); len(keys) != 1 || keys[0] != "DSC_ADMIN_TOKEN" {
		t.Fatalf("admin 能力应要求 DSC_ADMIN_TOKEN，got %v", keys)
	}
	if keys := CapabilitySecretKeys("cron"); len(keys) != 0 {
		t.Fatalf("未登记的能力不应凭空要求凭据，got %v", keys)
	}
	// 契约表里不得出现插件名（能力名允许出现；这里只做一次粗检查）
	for capability := range capabilitySecrets {
		if strings.Contains(capability, "-") {
			t.Fatalf("能力名不应写成插件目录名形态：%q", capability)
		}
	}
}

// TestMissingCapabilityGrantsNilSafe 空管理器不得 panic（启动路径会无条件调用）。
func TestMissingCapabilityGrantsNilSafe(t *testing.T) {
	t.Setenv("DSC_ADMIN_TOKEN", "secret")
	var m *Manager
	if got := m.MissingCapabilityGrants(nil); got != nil {
		t.Fatalf("nil Manager 应返回 nil，got %v", got)
	}
	if got := NewManager(&ManagerConfig{}).MissingCapabilityGrants(nil); len(got) != 0 {
		t.Fatalf("没有插件时不应告警，got %v", got)
	}
}
