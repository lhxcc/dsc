package core

import "testing"

// TestShutdownClearsRuntimeState 回归 Shutdown 清理完整性：工具注册表、钩子客户端、
// 服务 ID 映射与已加载二进制记录必须归零，避免复用同一 Manager 时残留工具/悬空 ID。
func TestShutdownClearsRuntimeState(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	if err := m.toolRegistry.Register(&mockTool{name: "leftover"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	m.mu.Lock()
	m.toolHookClients["p"] = &hookMockClient{content: "ctx"}
	m.toolHookOrder = []string{"p"}
	m.toolClients["p"] = &lctxMockClient{}
	m.loadedBinaries["p"] = "/plugins/tool-p/tool-p"
	m.llmServiceIDs["p"] = 7
	m.toolServiceIDs["p"] = 8
	m.coreToolNames["p"] = []string{"leftover"}
	m.mu.Unlock()

	m.Shutdown()

	if _, ok := m.toolRegistry.Get("leftover"); ok {
		t.Fatal("tool registry should be cleared after shutdown")
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.toolHookClients) != 0 || len(m.toolHookOrder) != 0 || len(m.toolClients) != 0 {
		t.Fatalf("hook/tool clients should be cleared: %d/%d/%d",
			len(m.toolHookClients), len(m.toolHookOrder), len(m.toolClients))
	}
	if len(m.loadedBinaries) != 0 || len(m.llmServiceIDs) != 0 || len(m.toolServiceIDs) != 0 || len(m.coreToolNames) != 0 {
		t.Fatal("service id / binary maps should be cleared after shutdown")
	}
}
