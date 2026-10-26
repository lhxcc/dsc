package core

import (
	"strings"
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
	m.toolHookClients["backend-b"] = other
	m.toolHookOrder = []string{"backend-b", "dsc-system"}
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
	m.toolHookClients["backend-b"] = h
	m.toolHookOrder = []string{"backend-b"}
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
// 未选中的提供者（同时启用了第二个压缩后端）不得让 agent 误判为「有后端」。
func TestCompactionBackendActiveRequiresConfiguredPlugin(t *testing.T) {
	m, _ := hookManager()
	m.mu.Lock()
	m.coreMetadata["backend-b"] = &metadata.PluginInfo{
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

// TestPreStepCollectsAppendsFromAnyPlugin 插播消息（append）与改写权无关：
// 任何插件都可请求，只有 compaction 后端能改写 messages。记忆插件正靠这条通道
// 在任务收尾时插入一条 system 消息，而不去改（会破坏缓存前缀的）system prompt。
func TestPreStepCollectsAppendsFromAnyPlugin(t *testing.T) {
	m, _ := hookManager()
	backend := &fakeHook{events: make(chan *proto.OnEventRequest, 4)}
	notifier := &fakeHook{events: make(chan *proto.OnEventRequest, 4)}
	notifier.eventResult = `{"append":[{"role":"system","content":"task done, write memory"}]}`

	m.mu.Lock()
	m.toolHookClients["dsc-system"] = backend
	m.toolHookClients["tool-memory-service"] = notifier
	m.toolHookOrder = []string{"tool-memory-service", "dsc-system"}
	m.compactionBackend = "dsc-system"
	m.mu.Unlock()

	out, err := m.dispatchEventToPlugins(EventAgentPreStep, map[string]string{"messages": "[]"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("返回载荷类型 %T", out)
	}
	appends, _ := payload["append"].([]any)
	if len(appends) != 1 {
		t.Fatalf("应收集 1 条插播，got %+v", payload["append"])
	}

	// 解析为消息，并核对角色/内容
	msgs := parsePreStepAppends(appends)
	if len(msgs) != 1 || msgs[0].GetRole() != "system" || !strings.Contains(msgs[0].GetContent(), "write memory") {
		t.Fatalf("插播消息解析不符: %+v", msgs)
	}
}

// TestParsePreStepAppendsFilters 只接受 system/user 且丢弃空内容：
// assistant/tool 需要配对的调用信息，插件无从构造。
func TestParsePreStepAppendsFilters(t *testing.T) {
	raw := []any{
		map[string]any{"role": "system", "content": "note"},
		map[string]any{"content": "default role"},
		map[string]any{"role": "user", "content": "u"},
		map[string]any{"role": "assistant", "content": "drop"},
		map[string]any{"role": "tool", "content": "drop"},
		map[string]any{"role": "system", "content": "   "},
	}
	msgs := parsePreStepAppends(raw)
	if len(msgs) != 3 {
		t.Fatalf("应保留 3 条，got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].GetRole() != "system" || msgs[1].GetRole() != "system" || msgs[2].GetRole() != "user" {
		t.Fatalf("角色映射不符: %+v", msgs)
	}
	if parsePreStepAppends(nil) != nil {
		t.Fatal("nil 应返回 nil")
	}
}

// TestDispatchRequestErrorBackendRetryAdopted 溢出紧急压缩的重试决定权只采纳选定
// 后端：后端返回 {"retry": true} 时必须被采纳，否则首次请求超窗没有安全阀（只会
// 收到 provider 400）。
func TestDispatchRequestErrorBackendRetryAdopted(t *testing.T) {
	m, _ := hookManager()
	backend := &fakeHook{events: make(chan *proto.OnEventRequest, 4), eventResult: `{"retry": true}`}
	other := &fakeHook{events: make(chan *proto.OnEventRequest, 4), eventResult: `{"retry": true}`}
	m.mu.Lock()
	m.toolHookClients["dsc-system"] = backend
	m.toolHookClients["backend-b"] = other
	m.toolHookOrder = []string{"backend-b", "dsc-system"}
	m.compactionBackend = "dsc-system"
	m.mu.Unlock()

	got, err := m.dispatchEventToPlugins(EventAgentRequestError, map[string]string{"code": "context_window_exceeded"})
	if err != nil {
		t.Fatalf("request-error 派发失败: %v", err)
	}
	payload, ok := got.(map[string]any)
	if !ok || payload["retry"] != true {
		t.Fatalf("应采纳选定后端的 retry=true, got %v", got)
	}
}

// TestDispatchRequestErrorRetryIgnoredWithoutBackend 未配置压缩后端时，任何插件的
// retry 都不采纳（避免非后端插件造成无意义的重试循环）。
func TestDispatchRequestErrorRetryIgnoredWithoutBackend(t *testing.T) {
	m, _ := hookManager()
	h := &fakeHook{events: make(chan *proto.OnEventRequest, 1), eventResult: `{"retry": true}`}
	m.mu.Lock()
	m.toolHookClients["backend-b"] = h
	m.toolHookOrder = []string{"backend-b"}
	m.compactionBackend = ""
	m.mu.Unlock()

	got, err := m.dispatchEventToPlugins(EventAgentRequestError, map[string]string{"code": "x"})
	if err != nil {
		t.Fatalf("request-error 派发失败: %v", err)
	}
	payload, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("返回值类型异常: %T", got)
	}
	if payload["retry"] == true {
		t.Fatalf("未选后端时不应采纳 retry, got %v", payload)
	}
}
