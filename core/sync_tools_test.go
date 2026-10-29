package core

import (
	"testing"
)

// TestApplyPluginToolsSync 校验宿主在插件工具变化时把最新工具集同步进共享 registry：
// 新增的工具被注册、移除的工具被注销、其他插件/内置工具不受影响。
func TestApplyPluginToolsSync(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	// 预置插件 tool-x 的既有工具集 t1,t2（模拟插件加载时注册）
	m.mu.Lock()
	m.toolServiceIDs["tool-x"] = 42
	m.coreToolNames["tool-x"] = []string{"t1", "t2"}
	m.mu.Unlock()
	for _, n := range []string{"t1", "t2"} {
		if err := m.toolRegistry.Register(&RemoteTool{name: n}); err != nil {
			t.Fatalf("register %s: %v", n, err)
		}
	}
	// 另一来源（内置/其他插件）的工具，不应因 tool-x 同步而被移除
	if err := m.toolRegistry.Register(&RemoteTool{name: "other-tool"}); err != nil {
		t.Fatal(err)
	}

	// tool-x 热加载后工具集变为 t1,t3（新增 t3、移除 t2）
	m.applyPluginTools("tool-x", []ToolDefinition{
		&RemoteTool{name: "t1"},
		&RemoteTool{name: "t3"},
	}, []string{"t1", "t3"})

	for _, want := range []string{"t1", "t3", "other-tool"} {
		if _, ok := m.toolRegistry.Get(want); !ok {
			t.Fatalf("registry 应含 %s", want)
		}
	}
	if _, ok := m.toolRegistry.Get("t2"); ok {
		t.Fatal("t2 应已被注销（插件不再提供）")
	}
	// 同步后该插件工具名表更新
	m.mu.RLock()
	got := m.coreToolNames["tool-x"]
	m.mu.RUnlock()
	if len(got) != 2 || got[0] != "t1" || got[1] != "t3" {
		t.Fatalf("coreToolNames[tool-x] = %v, want [t1 t3]", got)
	}
}

// TestApplyPluginToolsPicksUpDefinitionChange 校验「名字集合不变、定义变了」也会同步。
//
// 这是 MCP 的主路径：mcp_load 不改工具名，只把 hidden 从 true 翻成 false。按名字集合
// 早退的实现会让加载过的工具永远进不了模型目录（插件说可见、宿主还拿着旧定义）。
func TestApplyPluginToolsPicksUpDefinitionChange(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	m.mu.Lock()
	m.toolServiceIDs["tool-x"] = 7
	m.coreToolNames["tool-x"] = []string{"m"}
	m.mu.Unlock()
	if err := m.toolRegistry.Register(&RemoteTool{name: "m", hidden: true, description: "old"}); err != nil {
		t.Fatal(err)
	}
	if hasVisible(m.toolRegistry, "m") {
		t.Fatal("隐藏工具不应进目录")
	}

	// 同名同集合：只有 hidden 与 description 变了
	m.applyPluginTools("tool-x", []ToolDefinition{
		&RemoteTool{name: "m", hidden: false, description: "new"},
	}, []string{"m"})

	if !hasVisible(m.toolRegistry, "m") {
		t.Fatal("hidden 翻转必须被采纳（否则 mcp_load 永远无效）")
	}
	if t0, _ := m.toolRegistry.Get("m"); t0.Description() != "new" {
		t.Fatalf("description = %q, want new（定义变化必须被替换）", t0.Description())
	}

	// 反向：再翻回去也必须生效（unload 路径）
	m.applyPluginTools("tool-x", []ToolDefinition{
		&RemoteTool{name: "m", hidden: true, description: "new"},
	}, []string{"m"})
	if hasVisible(m.toolRegistry, "m") {
		t.Fatal("unload 后不应可见")
	}
}

// hasVisible 报告某个名字是否在模型可见目录里（NewManager 自带内置工具，不能比总数）。
func hasVisible(r *ToolRegistry, name string) bool {
	for _, t := range r.VisibleTools() {
		if t.Name() == name {
			return true
		}
	}
	return false
}
