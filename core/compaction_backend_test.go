package core

import (
	"testing"

	"dsc/proto"
	"dsc/proto/metadata"
)

// TestDispatchPreStepOnlyBackendRewriteAdopted 压缩改写权只归 compaction 字段选定的
// 后端：其它插件仍收到 pre-step（保留重置链等副作用），但它们的 result_json 不被采纳。
func TestDispatchPreStepOnlyBackendRewriteAdopted(t *testing.T) {
	m, _ := hookManager()
	backend := &fakeHook{events: make(chan *proto.OnEventRequest, 4), eventResult: `{"messages":"[backend]"}`}
	other := &fakeHook{events: make(chan *proto.OnEventRequest, 4), eventResult: `{"messages":"[other]"}`}

	m.mu.Lock()
	m.toolHookClients["dsc-system"] = backend
	m.toolHookClients["dsc-billion-context"] = other
	m.toolHookOrder = []string{"dsc-billion-context", "dsc-system"}
	m.compactionBackend = "dsc-system"
	m.mu.Unlock()

	got, err := m.dispatchEventToPlugins(EventAgentPreStep, map[string]string{"messages": "[orig]"})
	if err != nil {
		t.Fatalf("pre-step 派发失败: %v", err)
	}
	payload, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("返回值类型异常: %T", got)
	}
	if payload["messages"] != "[backend]" {
		t.Fatalf("应只采纳选定后端的改写，got %v", payload["messages"])
	}

	// 两个插件都必须收到事件：非后端的副作用（重置重复调用链）不能丢。
	for name, h := range map[string]*fakeHook{"backend": backend, "other": other} {
		select {
		case <-h.events:
		default:
			t.Fatalf("%s 应收到 pre-step（副作用不可丢）", name)
		}
	}
}

// TestDispatchPreStepIgnoresRewritesWithoutBackend 未配置后端时改写一律不采纳
// （由 agent 内联压缩接管），但事件仍派发以保留其它插件的副作用。
func TestDispatchPreStepIgnoresRewritesWithoutBackend(t *testing.T) {
	m, _ := hookManager()
	h := &fakeHook{events: make(chan *proto.OnEventRequest, 1), eventResult: `{"messages":"[rewritten]"}`}
	m.mu.Lock()
	m.toolHookClients["dsc-billion-context"] = h
	m.toolHookOrder = []string{"dsc-billion-context"}
	m.compactionBackend = ""
	m.mu.Unlock()

	got, err := m.dispatchEventToPlugins(EventAgentPreStep, map[string]string{"messages": "[orig]"})
	if err != nil {
		t.Fatalf("空后端时不应报错: %v", err)
	}
	payload, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("返回值类型异常: %T", got)
	}
	if payload["messages"] != "[orig]" {
		t.Fatalf("未选后端时改写不应生效，got %v", payload["messages"])
	}
	select {
	case <-h.events:
	default:
		t.Fatal("事件仍应派发（副作用不可丢）")
	}
}

// TestCompactionBackendActiveRequiresConfiguredPlugin 标记判定必须落在选定后头上：
// 未选中的提供者（同时启用的 dsc-billion-context）不得让 agent 误判为「有后端」。
func TestCompactionBackendActiveRequiresConfiguredPlugin(t *testing.T) {
	m, _ := hookManager()
	m.mu.Lock()
	m.coreMetadata["dsc-billion-context"] = &metadata.PluginInfo{
		Capabilities: map[string]string{"compaction": "true"},
	}
	m.compactionBackend = "dsc-system" // 选定但尚未加载
	m.mu.Unlock()

	if m.compactionBackendActive() {
		t.Fatal("未选中的提供者不应让后端视为 active")
	}

	m.mu.Lock()
	m.coreMetadata["dsc-system"] = &metadata.PluginInfo{
		Capabilities: map[string]string{"compaction": "true"},
	}
	m.mu.Unlock()
	if !m.compactionBackendActive() {
		t.Fatal("选定后端声明能力后应视为 active")
	}

	m.mu.Lock()
	m.coreMetadata["dsc-system"] = &metadata.PluginInfo{
		Capabilities: map[string]string{"compaction": "false"},
	}
	m.mu.Unlock()
	if m.compactionBackendActive() {
		t.Fatal("能力显式声明 false 时不应 active")
	}

	// 未配置后端 → 恒 false，即使有插件提供能力。
	m.mu.Lock()
	m.compactionBackend = ""
	m.mu.Unlock()
	if m.compactionBackendActive() {
		t.Fatal("未配置后端时应为 false")
	}
}

// TestCompactionBackendActiveFalseWhenPluginDead 后端进程退出后必须视为「无后端」，
// 让 agent 退回内联压缩，而不是一直等一个再也不会改写载荷的死插件。
func TestCompactionBackendActiveFalseWhenPluginDead(t *testing.T) {
	m, _ := hookManager()
	m.mu.Lock()
	m.compactionBackend = "dsc-system"
	m.coreMetadata["dsc-system"] = &metadata.PluginInfo{
		Capabilities: map[string]string{"compaction": "true"},
	}
	m.deadHookClients = map[string]bool{"dsc-system": true}
	m.mu.Unlock()

	if m.compactionBackendActive() {
		t.Fatal("压缩后端进程已退出时应视为无后端")
	}
}
