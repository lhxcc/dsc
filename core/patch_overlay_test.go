// patch_overlay_test.go — 测试 -patch overlay 加载与合并
package core

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPatchOverlayTopLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patch.yaml")
	content := `- name: mcp-memory
  type: dsc
  config:
    mcp:
      server_name: memory
      endpoint: http://localhost:3100/mcp
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadPatchOverlay(path)
	if err != nil {
		t.Fatalf("LoadPatchOverlay: %v", err)
	}
	if len(ov.Entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(ov.Entries))
	}
	if ov.Entries[0].Name != "mcp-memory" {
		t.Errorf("name = %q", ov.Entries[0].Name)
	}
	mcp, ok := ov.Entries[0].Config["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("config.mcp not found or wrong type: %T", ov.Entries[0].Config["mcp"])
	}
	if mcp["server_name"] != "memory" {
		t.Errorf("server_name = %v", mcp["server_name"])
	}
}

func TestLoadPatchOverlayInsert(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "patch.yaml")
	content := `- insert:
    - name: mcp-memory
      type: dsc
      config:
        mcp:
          server_name: memory
          endpoint: http://localhost:3100/mcp
    - name: mcp-filesystem
      type: dsc
      config:
        mcp:
          server_name: fs
          endpoint: http://localhost:3101/mcp
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	ov, err := LoadPatchOverlay(path)
	if err != nil {
		t.Fatalf("LoadPatchOverlay: %v", err)
	}
	if len(ov.Entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(ov.Entries))
	}
	if ov.Entries[0].Name != "mcp-memory" {
		t.Errorf("entry[0].name = %q", ov.Entries[0].Name)
	}
	if ov.Entries[1].Name != "mcp-filesystem" {
		t.Errorf("entry[1].name = %q", ov.Entries[1].Name)
	}
}

func TestApplyPatchOverlaysReplaceConfig(t *testing.T) {
	base := &Config{
		Plugins: []PluginEntry{
			{Name: "mcp-memory", Type: "dsc", Enabled: true, Config: map[string]any{"old": true}},
		},
	}
	overlay := &PatchOverlay{
		Entries: []PluginEntry{
			{Name: "mcp-memory", Config: map[string]any{"mcp": map[string]any{"server_name": "memory"}}},
		},
	}
	merged := ApplyPatchOverlays(base, []*PatchOverlay{overlay})
	if len(merged.Plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(merged.Plugins))
	}
	mcp, ok := merged.Plugins[0].Config["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("config.mcp not replaced: %v", merged.Plugins[0].Config)
	}
	if mcp["server_name"] != "memory" {
		t.Errorf("server_name = %v", mcp["server_name"])
	}
}

func TestApplyPatchOverlaysAppendNew(t *testing.T) {
	base := &Config{
		Plugins: []PluginEntry{
			{Name: "existing", Type: "tool", Enabled: true},
		},
	}
	overlay := &PatchOverlay{
		Entries: []PluginEntry{
			{Name: "mcp-memory", Type: "dsc", Config: map[string]any{"mcp": map[string]any{}}},
		},
	}
	merged := ApplyPatchOverlays(base, []*PatchOverlay{overlay})
	if len(merged.Plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(merged.Plugins))
	}
	if merged.Plugins[1].Name != "mcp-memory" {
		t.Errorf("appended plugin name = %q", merged.Plugins[1].Name)
	}
	if !merged.Plugins[1].Enabled {
		t.Error("appended plugin should be enabled by default")
	}
}

func TestApplyPatchOverlaysMultiple(t *testing.T) {
	base := &Config{Plugins: []PluginEntry{}}
	overlays := []*PatchOverlay{
		{Entries: []PluginEntry{{Name: "a", Type: "dsc"}}},
		{Entries: []PluginEntry{{Name: "b", Type: "dsc"}}},
	}
	merged := ApplyPatchOverlays(base, overlays)
	if len(merged.Plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(merged.Plugins))
	}
}

func TestLoadPatchFilesEmpty(t *testing.T) {
	overlays, err := LoadPatchFiles(nil)
	if err != nil {
		t.Fatalf("LoadPatchFiles(nil): %v", err)
	}
	if overlays != nil {
		t.Fatalf("expected nil, got %v", overlays)
	}
}

// TestApplyPatchOverlaysPreservesUnlistedFields overlay 合并必须原样保留「没被 patch
// 碰到的字段」。
//
// 这里曾经是逐字段列举拷贝：新加配置项只要忘了加一行，用 -patch 启动时就被静默抹掉。
// mcp 开关与 history_shows 正是这样漏的——而 mcp 被抹掉意味着「用户明明开了 MCP，
// 加了 -patch 启动又变成关闭」，症状是工具莫名消失，极难归因。
func TestApplyPatchOverlaysPreservesUnlistedFields(t *testing.T) {
	base := &Config{
		WorkspaceRoot: "/tmp/ws",
		HistoryShows:  5,
		MCP:           &MCPConfig{Enabled: true},
		Plugins:       []PluginEntry{{Name: "a", Type: "tool", Enabled: true}},
	}
	overlays := []*PatchOverlay{{Entries: []PluginEntry{{Name: "b", Type: "tool"}}}}

	merged := ApplyPatchOverlays(base, overlays)
	if merged.MCP == nil || !merged.MCP.Enabled {
		t.Fatalf("mcp 开关被 patch 合并抹掉了：%+v", merged.MCP)
	}
	if merged.HistoryShows != 5 {
		t.Fatalf("history_shows 被抹掉了：%d", merged.HistoryShows)
	}
	if merged.WorkspaceRoot != "/tmp/ws" {
		t.Fatalf("workspace_root 变了：%q", merged.WorkspaceRoot)
	}
	if len(merged.Plugins) != 2 {
		t.Fatalf("插件应合并为 2 条，实际 %d", len(merged.Plugins))
	}
	// 不修改 base（切片深拷贝）
	if len(base.Plugins) != 1 {
		t.Fatalf("不应写回 base：%d", len(base.Plugins))
	}
}
