package core

import (
	"encoding/json"
	"strings"
	"testing"

	"dsc/proto"
	"dsc/proto/metadata"
	plugin "github.com/hashicorp/go-plugin"
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
	m.coreMetadata["dsc-system"] = declaresCompaction()
	m.coreMetadata["backend-b"] = declaresCompaction()
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

// TestDispatchPreStepIgnoresRewritesWithoutBackend 岗位空缺（没有任何合格候选）时
// 改写一律不采纳，但事件仍派发以保留其它插件的副作用。
//
// 注意：正常运行不会出现这个状态——agent 对压缩是强依赖，空缺时宿主启动即失败
// （见 main 的启动闸门）。这里守的是「派发层不因为缺岗位而丢副作用」。
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
	m.coreMetadata["dsc-system"] = declaresCompaction()
	m.toolHookClients["dsc-system"] = &fakeHook{}
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

	// 未点名但存在唯一已加载候选 → 自动当值（启用即生效）；没有任何合格候选时才是空缺。
	m.mu.Lock()
	m.compactionBackend = ""
	m.coreMetadata["dsc-system"] = declaresCompaction()
	m.mu.Unlock()
	if !m.compactionBackendActive() {
		t.Fatal("未点名但唯一候选应自动当值")
	}

	m.mu.Lock()
	delete(m.coreMetadata, "dsc-system")
	delete(m.toolHookClients, "dsc-system")
	m.mu.Unlock()
	if m.compactionBackendActive() {
		t.Fatal("没有任何候选时应为空缺")
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
	m.coreMetadata["dsc-system"] = declaresCompaction()
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
	m.coreMetadata["dsc-system"] = declaresCompaction()
	m.coreMetadata["backend-b"] = declaresCompaction()
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

// declaresCompaction 构造一个声明了 compaction 能力的插件元数据。
func declaresCompaction() *metadata.PluginInfo {
	return &metadata.PluginInfo{Capabilities: map[string]string{"compaction": "true"}}
}

// TestCompactionRoleResolvesConfiguredHolder 岗位 = 配置点名 + 已加载 + 声明能力。
// 有能力的插件可以有多个，但当值的只有一个；其余同为候选人（可解释、可切换）。
func TestCompactionRoleResolvesConfiguredHolder(t *testing.T) {
	m, _ := hookManager()
	m.mu.Lock()
	m.toolHookClients["backend-a"] = &fakeHook{}
	m.toolHookClients["backend-b"] = &fakeHook{}
	m.coreMetadata["backend-a"] = declaresCompaction()
	m.coreMetadata["backend-b"] = declaresCompaction()
	m.compactionBackend = "backend-b"
	m.mu.Unlock()

	role := m.ResolveCompactionRole()
	if role.Holder != "backend-b" {
		t.Fatalf("岗位应归配置点名的 backend-b，got %q", role.Holder)
	}
	if len(role.Candidates) != 2 || role.Candidates[0] != "backend-a" || role.Candidates[1] != "backend-b" {
		t.Fatalf("候选人应排序为 [backend-a backend-b]，got %v", role.Candidates)
	}
	if role.Vacancy != "" {
		t.Fatalf("有人当值时不应有 vacancy 说明，got %q", role.Vacancy)
	}
	// 诊断口径与岗位一致
	if configured, active := m.CompactionBackendState(); configured != "backend-b" || !active {
		t.Fatalf("state = %q/%v", configured, active)
	}
}

// TestCompactionRoleVacancyReasons 岗位解析的四种形态，且空缺时必须说明缺的是哪个。
//
// 空缺不再是「可选形态」：压缩是 agent 的强依赖，空缺 = 启动失败。所以这里的每条
// vacancy 文案都是给用户看的「怎么补」的依据，不能含糊。
func TestCompactionRoleVacancyReasons(t *testing.T) {
	cases := []struct {
		name       string
		configured string
		loaded     bool
		declare    bool
		wantHolder bool
		contains   string
	}{
		{"未点名但唯一候选（自动当值）", "", true, true, true, ""},
		{"配置了但没加载", "backend-a", false, true, false, "is not loaded"},
		{"加载了但没声明能力", "backend-a", true, false, false, "does not declare"},
		{"三条件齐备", "backend-a", true, true, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _ := hookManager()
			m.mu.Lock()
			if c.loaded {
				m.toolHookClients["backend-a"] = &fakeHook{}
			}
			if c.declare {
				m.coreMetadata["backend-a"] = declaresCompaction()
			}
			m.compactionBackend = c.configured
			m.mu.Unlock()

			role := m.ResolveCompactionRole()
			if (role.Holder != "") != c.wantHolder {
				t.Fatalf("holder = %q, want holder=%v", role.Holder, c.wantHolder)
			}
			if !strings.Contains(role.Vacancy, c.contains) {
				t.Fatalf("vacancy = %q, want contains %q", role.Vacancy, c.contains)
			}
			// 自动当值只发生在「没点名 + 唯一候选」这一种形态
			if wantAuto := c.configured == "" && c.wantHolder; role.AutoSelected != wantAuto {
				t.Fatalf("auto_selected = %v, want %v", role.AutoSelected, wantAuto)
			}
		})
	}
}

// TestCompactionRoleMultipleCandidatesNeedName 多候选未点名 → 空缺并要求点名：
// 两个后端同时改写请求面会层层叠加，结果既不是 A 也不是 B。
func TestCompactionRoleMultipleCandidatesNeedName(t *testing.T) {
	m, _ := hookManager()
	m.mu.Lock()
	m.toolHookClients["backend-a"] = &fakeHook{}
	m.toolHookClients["backend-b"] = &fakeHook{}
	m.coreMetadata["backend-a"] = declaresCompaction()
	m.coreMetadata["backend-b"] = declaresCompaction()
	m.mu.Unlock()

	role := m.ResolveCompactionRole()
	if role.Holder != "" {
		t.Fatalf("多候选未点名时不应自动选一个，got %q", role.Holder)
	}
	if !strings.Contains(role.Vacancy, "multiple compaction candidates") {
		t.Fatalf("vacancy 应说明多候选未点名，got %q", role.Vacancy)
	}
	if len(role.Candidates) != 2 {
		t.Fatalf("候选应全部列出，got %v", role.Candidates)
	}

	// 点名后立即当值（候选仍然完整，便于用户切换）
	m.mu.Lock()
	m.compactionBackend = "backend-b"
	m.mu.Unlock()
	if role := m.ResolveCompactionRole(); role.Holder != "backend-b" {
		t.Fatalf("点名后应归 backend-b，got %q", role.Holder)
	}
}

// TestPreStepPayloadOnlyToRoleHolder pre-step 的完整消息列表只发给岗位持有者。
//
// 岗位之外没人能改写请求面，把几 MB 的消息复制过去是纯白工——而实测非岗位的压缩
// 后端还会真的调模型做摘要，结果被宿主丢弃。副作用（重复链重置、记忆落盘）只需要
// session / user_input，轻载荷里都有。
func TestPreStepPayloadOnlyToRoleHolder(t *testing.T) {
	m, _ := hookManager()
	holder := &fakeHook{events: make(chan *proto.OnEventRequest, 1)}
	other := &fakeHook{events: make(chan *proto.OnEventRequest, 1), eventResult: `{"messages":"[other]"}`}
	m.mu.Lock()
	m.toolHookClients["backend-a"] = holder
	m.toolHookClients["backend-b"] = other
	m.toolHookOrder = []string{"backend-b", "backend-a"}
	m.coreMetadata["backend-a"] = declaresCompaction()
	m.coreMetadata["backend-b"] = declaresCompaction()
	m.compactionBackend = "backend-a"
	m.mu.Unlock()

	ev := AgentPreStepEvent{
		Agent: "agent", Session: "s1", Turn: 3, Step: 1,
		MessagesJSON: `[{"role":"user","content":"hi"}]`,
		TokenCount:   1234, ToolTokens: 100, ProjectedTokens: 1300,
		UserInput: true,
	}
	if _, err := m.dispatchEventToPlugins(EventAgentPreStep, ev); err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	rolePayload := decodePreStep(t, <-holder.events)
	if rolePayload.MessagesJSON == "" {
		t.Fatal("岗位持有者必须收到完整消息列表")
	}
	otherPayload := decodePreStep(t, <-other.events)
	if otherPayload.MessagesJSON != "" {
		t.Fatalf("非岗位插件不应收到消息列表（白传几 MB），got %.80s", otherPayload.MessagesJSON)
	}
	// 轻载荷必须保留副作用需要的一切
	if otherPayload.Session != "s1" || !otherPayload.UserInput || otherPayload.TokenCount != 1234 {
		t.Fatalf("轻载荷丢了副作用需要的字段: %+v", otherPayload)
	}
}

// TestPreStepObserverCapabilityKeepsFullPayload 声明了 pre-step-messages 的观察型插件
// 仍收完整列表：默认省流量，但确实要看内容的插件得有办法声明，而不是被猜。
func TestPreStepObserverCapabilityKeepsFullPayload(t *testing.T) {
	m, _ := hookManager()
	holder := &fakeHook{events: make(chan *proto.OnEventRequest, 1)}
	observer := &fakeHook{events: make(chan *proto.OnEventRequest, 1)}
	m.mu.Lock()
	m.toolHookClients["backend-a"] = holder
	m.toolHookClients["memory"] = observer
	m.toolHookOrder = []string{"memory", "backend-a"}
	m.coreMetadata["backend-a"] = declaresCompaction()
	m.coreMetadata["memory"] = &metadata.PluginInfo{
		Capabilities: map[string]string{capPreStepMessages: "true"},
	}
	m.compactionBackend = "backend-a"
	m.mu.Unlock()

	ev := AgentPreStepEvent{Session: "s1", MessagesJSON: `[{"role":"user","content":"hi"}]`}
	if _, err := m.dispatchEventToPlugins(EventAgentPreStep, ev); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got := decodePreStep(t, <-observer.events); got.MessagesJSON == "" {
		t.Fatal("声明了 pre-step-messages 的插件应收到完整消息列表")
	}
}

// TestRequestErrorOnlyToRoleHolder request-error 只发给岗位持有者：只有它会做紧急压缩。
// 广播给其他声明了 compaction 的插件，它们会真的再压一遍（调模型、写状态），结果被丢弃。
func TestRequestErrorOnlyToRoleHolder(t *testing.T) {
	m, _ := hookManager()
	holder := &fakeHook{events: make(chan *proto.OnEventRequest, 2), eventResult: `{"retry": true}`}
	other := &fakeHook{events: make(chan *proto.OnEventRequest, 2), eventResult: `{"retry": true}`}
	m.mu.Lock()
	m.toolHookClients["backend-a"] = holder
	m.toolHookClients["backend-b"] = other
	m.toolHookOrder = []string{"backend-b", "backend-a"}
	m.coreMetadata["backend-a"] = declaresCompaction()
	m.coreMetadata["backend-b"] = declaresCompaction()
	m.compactionBackend = "backend-a"
	m.mu.Unlock()

	got, err := m.dispatchEventToPlugins(EventAgentRequestError, map[string]string{"code": "context_window_exceeded"})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if payload, ok := got.(map[string]any); !ok || payload["retry"] != true {
		t.Fatalf("应采纳岗位持有者的 retry=true, got %v", got)
	}
	select {
	case <-holder.events:
	default:
		t.Fatal("岗位持有者应收到 request-error")
	}
	select {
	case <-other.events:
		t.Fatal("非岗位插件不应收到 request-error（它会白压一遍）")
	default:
	}

	// 岗位空缺：没人该动，也不该有人重试
	m.mu.Lock()
	m.compactionBackend = ""
	m.mu.Unlock()
	got, err = m.dispatchEventToPlugins(EventAgentRequestError, map[string]string{"code": "context_window_exceeded"})
	if err != nil {
		t.Fatalf("vacant dispatch: %v", err)
	}
	if payload, ok := got.(map[string]any); !ok || payload["retry"] == true {
		t.Fatalf("岗位空缺时不应采纳 retry, got %v", got)
	}
}

// decodePreStep 解析 fakeHook 收到的 pre-step 载荷。
func decodePreStep(t *testing.T, req *proto.OnEventRequest) AgentPreStepEvent {
	t.Helper()
	var ev AgentPreStepEvent
	if err := json.Unmarshal([]byte(req.GetDataJson()), &ev); err != nil {
		t.Fatalf("解析 pre-step 载荷失败: %v（原文 %.120s）", err, req.GetDataJson())
	}
	return ev
}

// declaresTransform 构造一个声明了「可组合请求面变换」能力的插件元数据。
func declaresTransform() *metadata.PluginInfo {
	return &metadata.PluginInfo{Capabilities: map[string]string{capPreStepTransform: "true"}}
}

// TestPreStepProjectionComposesAfterCompaction 第二阶段必须作用在**压缩改写后的视图**上，
// 且结果被采纳——投影是独立工序，不是压缩的一部分。
//
// 现场动因：图像预算卸载原先寄生在压缩插件内部（插件内串链），于是压缩换成另一个后端
// 时，投影跟着一起消失。分开之后：谁当值压缩归岗位，投影按能力由宿主依次调用。
func TestPreStepProjectionComposesAfterCompaction(t *testing.T) {
	m, _ := hookManager()
	holder := &fakeHook{events: make(chan *proto.OnEventRequest, 4),
		eventResult: `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"compacted"}]}`}
	projector := &fakeHook{events: make(chan *proto.OnEventRequest, 4),
		eventResult: `{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"projected"}]}`}
	m.mu.Lock()
	m.toolHookClients["compactor"] = holder
	m.toolHookClients["projector"] = projector
	m.toolHookOrder = []string{"projector", "compactor"}
	m.coreMetadata["compactor"] = declaresCompaction()
	m.coreMetadata["projector"] = declaresTransform()
	m.compactionBackend = "compactor"
	m.mu.Unlock()

	ev := AgentPreStepEvent{Session: "s1", MessagesJSON: `[{"role":"user","content":"original"}]`, TokenCount: 100}
	out, err := m.dispatchEventToPlugins(EventAgentPreStep, ev)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	// 阶段一：投影者只拿到轻载荷（它不当值压缩）
	first := decodePreStep(t, <-projector.events)
	if first.Stage != PreStepStageCompaction || first.MessagesJSON != "" {
		t.Fatalf("阶段一应发轻载荷(compaction/无消息)，got stage=%q msgs=%.40s", first.Stage, first.MessagesJSON)
	}
	// 阶段二：拿到压缩后的视图，阶段标记为 projection
	second := decodePreStep(t, <-projector.events)
	if second.Stage != PreStepStageProjection {
		t.Fatalf("阶段二 stage = %q", second.Stage)
	}
	if !strings.Contains(second.MessagesJSON, "compacted") {
		t.Fatalf("投影必须作用在压缩改写后的视图上，got %.80s", second.MessagesJSON)
	}

	// 结果取投影的输出
	payload, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("返回载荷类型 %T", out)
	}
	raw, _ := json.Marshal(payload["messages"])
	if !strings.Contains(string(raw), "projected") {
		t.Fatalf("最终视图应为投影结果，got %s", raw)
	}
}

// TestPreStepProjectionIndependentOfRole 岗位空缺（没配压缩后端）时投影照跑：
// 它的触发条件是预算，不是岗位。压缩与投影是两件事的另一个证明。
func TestPreStepProjectionIndependentOfRole(t *testing.T) {
	m, _ := hookManager()
	projector := &fakeHook{events: make(chan *proto.OnEventRequest, 4),
		eventResult: `{"messages":[{"role":"user","content":"projected"}]}`}
	m.mu.Lock()
	m.toolHookClients["projector"] = projector
	m.toolHookOrder = []string{"projector"}
	m.coreMetadata["projector"] = declaresTransform()
	m.compactionBackend = ""
	m.mu.Unlock()

	ev := AgentPreStepEvent{Session: "s1", MessagesJSON: `[{"role":"user","content":"original"}]`}
	out, err := m.dispatchEventToPlugins(EventAgentPreStep, ev)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	payload, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("返回载荷类型 %T", out)
	}
	raw, _ := json.Marshal(payload["messages"])
	if !strings.Contains(string(raw), "projected") {
		t.Fatalf("岗位空缺时投影仍应生效，got %s", raw)
	}
}

// TestPreStepProjectionFailureKeepsPreviousView 投影失败不能让整轮请求失败：
// 它省的是字节（图像/长输出），不是把请求压进窗口的那一步。压缩的失败才必须 veto。
func TestPreStepProjectionFailureKeepsPreviousView(t *testing.T) {
	m, _ := hookManager()
	holder := &fakeHook{events: make(chan *proto.OnEventRequest, 2),
		eventResult: `{"messages":[{"role":"user","content":"compacted"}]}`}
	bad := &fakeHook{events: make(chan *proto.OnEventRequest, 2)}
	bad.eventResult = ""
	bad.eventError = "projection exploded"
	bad.eventErrStage = PreStepStageProjection
	m.mu.Lock()
	m.toolHookClients["compactor"] = holder
	m.toolHookClients["projector"] = bad
	m.toolHookOrder = []string{"compactor", "projector"}
	m.coreMetadata["compactor"] = declaresCompaction()
	m.coreMetadata["projector"] = declaresTransform()
	m.compactionBackend = "compactor"
	m.mu.Unlock()

	ev := AgentPreStepEvent{Session: "s1", MessagesJSON: `[{"role":"user","content":"original"}]`}
	out, err := m.dispatchEventToPlugins(EventAgentPreStep, ev)
	if err != nil {
		t.Fatalf("投影失败不应让派发失败: %v", err)
	}
	payload, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("返回载荷类型 %T", out)
	}
	raw, _ := json.Marshal(payload["messages"])
	if !strings.Contains(string(raw), "compacted") {
		t.Fatalf("应保留上一步（压缩）结果，got %s", raw)
	}
}

// TestCompactionRoleProblemIsStartupGate 启动闸门的输入：岗位空缺时必须给出一句能直接
// 贴到错误信息里的说明（缺的是哪一环），有人当值时为空。
func TestCompactionRoleProblemIsStartupGate(t *testing.T) {
	m, _ := hookManager()
	if got := m.CompactionRoleProblem(); got == "" || !strings.Contains(got, "no compaction backend on duty") {
		t.Fatalf("岗位空缺时应给出原因，got %q", got)
	}
	m.mu.Lock()
	m.coreMetadata["backend-a"] = declaresCompaction()
	m.toolHookClients["backend-a"] = &fakeHook{}
	m.mu.Unlock()
	if got := m.CompactionRoleProblem(); got != "" {
		t.Fatalf("有人当值时不应报问题，got %q", got)
	}
}

// TestUnloadRefusesCompactionRoleHolder 当值压缩后端不可卸载：agent 依赖它，卸掉之后
// 没有任何东西能改写请求面。换后端必须先启用并点名另一个。
func TestUnloadRefusesCompactionRoleHolder(t *testing.T) {
	m, _ := hookManager()
	m.mu.Lock()
	m.coreMetadata["backend-a"] = declaresCompaction()
	m.toolHookClients["backend-a"] = &fakeHook{}
	m.clients["backend-a"] = &plugin.Client{} // 占位：只要"存在"就会走到守卫
	m.mu.Unlock()

	err := m.UnloadPlugin("backend-a")
	if err == nil {
		t.Fatal("卸载当值压缩后端应被拒绝")
	}
	if !strings.Contains(err.Error(), "compaction role holder") {
		t.Fatalf("错误信息应说明它是压缩岗位持有者，got %v", err)
	}
}
