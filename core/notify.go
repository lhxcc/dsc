package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"dsc/jobs"
	"dsc/proto"

	plugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
)

// 插件通知服务（互通机制 2，插件→宿主）：插件进程经 broker 连接本服务，
// 把事件（含后台任务完成通知）发布到宿主事件总线，供 TUI/其他插件订阅。
// 宿主在 broker 挂载并注入 serviceID（DSC_NOTIFY_SERVICE_ID）到插件进程 env。

// servePluginNotifyLocked 在 agent broker 上挂载 PluginNotifyService（需已持有 m.mu）。
func (m *Manager) servePluginNotifyLocked() (uint32, error) {
	id, err := m.servePluginNotifyOnBroker(m.broker)
	if err == nil {
		m.coreNotifyServiceID = id
	}
	return id, err
}

// servePluginNotifyOnBroker 在指定 broker 上挂载 PluginNotifyService；返回
// serviceID。互通机制 2 中，该服务须挂在本插件 client 的 broker 上（插件
// 进程经自身 broker.Dial 访问），而不仅是 agent broker。
func (m *Manager) servePluginNotifyOnBroker(broker *plugin.GRPCBroker) (uint32, error) {
	if broker == nil {
		return 0, fmt.Errorf("broker not available, cannot serve core notify service")
	}
	serviceID := broker.NextId()
	go broker.AcceptAndServe(serviceID, func(opts []grpc.ServerOption) *grpc.Server {
		s := grpc.NewServer(opts...)
		proto.RegisterPluginNotifyServiceServer(s, &coreNotifyServer{m: m})
		return s
	})
	return serviceID, nil
}

// coreNotifyServer PluginNotifyService 宿主侧实现。
type coreNotifyServer struct {
	proto.UnimplementedPluginNotifyServiceServer
	m *Manager
}

// Notify 把插件事件发布到宿主事件总线：
//   - name == job/done 时特化：data 为任务快照 JSON，宿主解析为 JobSnapshot 后
//     发 JobDoneEvent（复用 TUI 完成通知唤醒体系）；
//   - 其余 name 为插件自定义事件，data 尝试 JSON 解析（失败按原样字符串）。
func (s *coreNotifyServer) Notify(ctx context.Context, req *proto.NotifyRequest) (*proto.NotifyResponse, error) {
	if req.GetName() == string(JobDoneEvent) {
		var snap jobs.JobSnapshot
		if err := json.Unmarshal([]byte(req.GetData()), &snap); err != nil {
			return nil, fmt.Errorf("notify: invalid job/done data: %w", err)
		}
		s.m.events.Emit(JobDoneEvent, EventContext{Data: snap})
		return &proto.NotifyResponse{}, nil
	}
	data := any(req.GetData())
	if req.GetData() != "" {
		var v any
		if err := json.Unmarshal([]byte(req.GetData()), &v); err == nil {
			data = v
		}
	}
	s.m.events.Emit(EventName(req.GetName()), EventContext{Data: data})
	return &proto.NotifyResponse{}, nil
}

// hookClientsSnapshot 返回按加载顺序的插件钩子客户端快照（读锁保护，防热加载竞态）。
// 已标记不可用（进程退出/连接断开）的插件被跳过。
func (m *Manager) hookClientsSnapshot() []proto.PluginHookServiceClient {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]proto.PluginHookServiceClient, 0, len(m.toolHookOrder))
	for _, n := range m.toolHookOrder {
		if m.deadHookClients[n] {
			continue
		}
		if c := m.toolHookClients[n]; c != nil {
			out = append(out, c)
		}
	}
	return out
}

// markHookClientDead 把钩子客户端标记为不可用（插件进程退出/连接断开）。
// 只告警一次；同名插件重新注册时清除标记。原因见 Manager.deadHookClients。
func (m *Manager) markHookClientDead(name string, err error) {
	m.mu.Lock()
	if m.deadHookClients[name] {
		m.mu.Unlock()
		return
	}
	if m.deadHookClients == nil {
		m.deadHookClients = make(map[string]bool)
	}
	m.deadHookClients[name] = true
	m.mu.Unlock()
	m.logger.Warn("hook plugin unavailable; skipping until it is reloaded",
		"plugin", name, "error", err.Error())
}

// compactionBackendName 返回配置选定的压缩后端插件名（空 = 未选，走 agent 内联压缩）。
func (m *Manager) compactionBackendName() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.compactionBackend
}

// compactionBackendActive 报告选定的压缩后端是否已加载且声明了 compaction 能力。
//
// 只认 compaction 字段指定的那一个：其它插件即使同样声明 Provides compaction 也不
// 参与接管（否则 dsc-system.compaction-basic 与 dsc-billion-context 的 pre-step 改写
// 会叠加成双重压缩，且与配置意图不符）。
func (m *Manager) compactionBackendActive() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.compactionBackend == "" {
		return false
	}
	if m.deadHookClients[m.compactionBackend] {
		// 后端进程已退出：声明「无后端」，让 agent 退回内联压缩，
		// 而不是一直等一个再也不会改写载荷的死插件。
		return false
	}
	info := m.coreMetadata[m.compactionBackend]
	if info == nil {
		return false
	}
	v, ok := info.Capabilities["compaction"]
	return ok && v != "false"
}

// LoadLuaHooks 加载外部脚本钩子配置（-hooks 指定的 hooks.json；路径不存在为
// 无配置静默）。加载成功后，外部钩子在工具流水线 BeforeTool/AfterTool 阶段
// 参与裁定（veto/改写），与插件 gRPC 钩子串联（插件先行，外部脚本随后）。
func (m *Manager) LoadLuaHooks(path string) error {
	b := NewLuaHookBridge()
	if err := b.LoadConfig(path); err != nil {
		return err
	}
	m.mu.Lock()
	m.luaHooks = b
	m.mu.Unlock()
	m.logger.Info("lua hooks loaded", "path", path)
	return nil
}

// runPluginBeforeTool 调用所有插件 BeforeTool 钩子（按加载顺序）：任一 veto
// 阻止执行；参数可被改写（后续用新参数）。插件不可用（UNIMPLEMENTED 等）跳过。
// 插件 gRPC 钩子之后串联外部脚本钩子（LuaHookBridge：严格 LUA / 直接 exec），
// 每次调用结算留痕（-log 启用时的钩子诊断能力：谁被调、耗时、veto/改写、失败原因）。
func (m *Manager) runPluginBeforeTool(ctx context.Context, inv *ToolInvocation) error {
	for _, c := range m.hookClientsSnapshot() {
		if c == nil {
			continue
		}
		resp, err := c.BeforeTool(ctx, &proto.BeforeToolRequest{
			ToolName: inv.ToolName, ArgumentsJson: inv.ArgumentsJSON, CallId: inv.CallID,
		})
		if err != nil {
			continue // UNIMPLEMENTED/插件不可用容错
		}
		if resp.GetVeto() {
			if resp.GetError() != "" {
				return fmt.Errorf("core vetoed %s: %s", inv.ToolName, resp.GetError())
			}
			return fmt.Errorf("core vetoed %s", inv.ToolName)
		}
		if resp.GetArgumentsJson() != "" && resp.GetArgumentsJson() != inv.ArgumentsJSON {
			inv.ArgumentsJSON = resp.GetArgumentsJson()
		}
	}
	// 外部脚本钩子（严格 LUA / 直接 exec）：veto 语义与插件钩子一致，失败 contain。
	m.mu.RLock()
	hooks := m.luaHooks
	m.mu.RUnlock()
	if hooks != nil {
		start := time.Now()
		rewritten, err := hooks.RunBeforeTool(ctx, inv.ToolName, inv.ArgumentsJSON, inv.CallID)
		elapsed := time.Since(start).Milliseconds()
		if err != nil {
			// veto：模型可见的阻止原因（对齐 DSH「用模型可见消息阻止工具调用」）
			m.logger.Warn("lua hook vetoed tool", "tool", inv.ToolName,
				"duration_ms", elapsed, "error", err.Error())
			return err
		}
		if rewritten != inv.ArgumentsJSON {
			m.logger.Info("lua hook rewrote args", "tool", inv.ToolName,
				"duration_ms", elapsed, "args_len", len(rewritten))
			inv.ArgumentsJSON = rewritten
		} else {
			m.logger.Info("lua hook passed", "tool", inv.ToolName, "duration_ms", elapsed)
		}
	}
	return nil
}

// runPluginAfterTool 调用所有插件 AfterTool 钩子：可改写结果/错误。随后串联
// 外部脚本钩子（LuaHookBridge），调用结算留痕同 BeforeTool。
func (m *Manager) runPluginAfterTool(ctx context.Context, inv *ToolInvocation) {
	for _, c := range m.hookClientsSnapshot() {
		if c == nil {
			continue
		}
		resp, err := c.AfterTool(ctx, &proto.AfterToolRequest{
			ToolName: inv.ToolName, ArgumentsJson: inv.ArgumentsJSON, CallId: inv.CallID,
			Result: inv.Result, Error: errString(inv.Err),
		})
		if err != nil {
			continue
		}
		if resp.GetError() != "" {
			inv.Err = fmt.Errorf("%s", resp.GetError())
		} else if resp.GetResult() != "" {
			inv.Result = resp.GetResult()
			inv.Err = nil
		}
	}
	// 外部脚本钩子（严格 LUA / 直接 exec）：改写结果/置错，失败 contain。
	m.mu.RLock()
	hooks := m.luaHooks
	m.mu.RUnlock()
	if hooks != nil {
		start := time.Now()
		result, toolErr := hooks.RunAfterTool(ctx, inv.ToolName, inv.ArgumentsJSON,
			inv.Result, errString(inv.Err), inv.CallID)
		elapsed := time.Since(start).Milliseconds()
		switch {
		case toolErr != "" && (inv.Err == nil || toolErr != inv.Err.Error()):
			m.logger.Warn("lua hook set tool error", "tool", inv.ToolName,
				"duration_ms", elapsed, "error", toolErr)
			inv.Err = fmt.Errorf("%s", toolErr)
		case result != inv.Result:
			m.logger.Info("lua hook rewrote result", "tool", inv.ToolName,
				"duration_ms", elapsed, "result_chars", len(result))
			inv.Result = result
			inv.Err = nil
		default:
			m.logger.Info("lua hook passed", "tool", inv.ToolName, "duration_ms", elapsed, "phase", "after")
		}
	}
}

// errString 错误转字符串（nil → 空）。
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// dispatchEventToPlugins 把宿主事件同步分发到所有插件（互通机制 3 OnEvent）：
// 插件可经此订阅宿主及其他插件（notify 发布）的事件，无需协调发布方。
//
// 分发模式由事件名决定（见 eventDispatchMode 映射表，对齐 DSH Cordis ctx.on()）：
//   - emit（默认）：同步顺序调用所有插件，忽略返回值；插件内部可自行 go func 派发副作用
//   - waterfall：洋葱模型，每个插件可调 next 委托下游，返回非空 result_json 改写载荷
//     透传给下游；返回非空 error 短路链视为 veto
//   - serial/bail：error 非空使链停止（bail 短路返回，serial 错误聚合）
//
// 同步语义：宿主等待所有插件返回后才继续（避免异步竞态）。
// 通知型插件（如 notify）若需异步副作用（如播放音效），自行在 handler 内 go func()。
//
// waterfall 模式下返回 (result, err)：result 为最终改写后的载荷（any 类型，调用方
// 自行类型断言）；err 非 nil 表示链被 veto（含插件返回的 error 文本）。
// namedHookClient 带插件名的钩子客户端（派发时需知道来源才能决定是否采纳其改写）。
type namedHookClient struct {
	name   string
	client proto.PluginHookServiceClient
}

// hookClientsNamedSnapshot 按加载顺序返回（插件名, 客户端）快照，跳过空客户端与
// 已标记不可用（进程退出/连接断开）的插件。
func (m *Manager) hookClientsNamedSnapshot() []namedHookClient {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]namedHookClient, 0, len(m.toolHookOrder))
	for _, n := range m.toolHookOrder {
		if m.deadHookClients[n] {
			continue
		}
		if c := m.toolHookClients[n]; c != nil {
			out = append(out, namedHookClient{name: n, client: c})
		}
	}
	return out
}

// collectPreStepAppends 从插件返回的 result_json 中取出一次性插播消息。
// 约定形状：{"append":[{"role":"system","content":"…"}]}；无该字段返回 nil。
func collectPreStepAppends(resultJSON string) []any {
	var payload struct {
		Append []any `json:"append"`
	}
	if err := json.Unmarshal([]byte(resultJSON), &payload); err != nil {
		return nil
	}
	return payload.Append
}

func (m *Manager) dispatchEventToPlugins(name EventName, data any) (any, error) {
	clients := m.hookClientsNamedSnapshot()
	if len(clients) == 0 {
		return data, nil
	}
	dataJSON := ""
	if data != nil {
		if b, err := json.Marshal(data); err == nil {
			dataJSON = string(b)
		}
	}
	mode := eventDispatchMode[name]
	if mode == "" {
		mode = "emit"
	}

	// agent/pre-step 的「消息改写权」只归 compaction 字段选定的后端：多个插件可能都
	// 声明 Provides compaction（dsc-system 的 compaction-basic 与 dsc-billion-context），
	// 若全部采纳就会层层改写、叠加压缩，与配置选择的后端不符。
	// 注意仍向所有插件派发——pre-step 还承载副作用（dsc-system 重置重复调用链、
	// 通知型 listener 等），只是它们的 result_json 不参与载荷改写；未选后端时
	// 一律不采纳改写，由 agent 内联压缩兜底。
	backend := ""
	if name == EventAgentPreStep {
		backend = m.compactionBackendName()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch mode {
	case "waterfall":
		// 洋葱模型：从尾向前包装，每个插件可改写 result_json
		// 初始 result 为原始 dataJSON；每个插件收到的 dataJSON 是上游改写后的
		var appends []any // 所有插件请求的一次性插播消息（与改写权无关）
		next := func() (string, error) { return dataJSON, nil }
		for i := len(clients) - 1; i >= 0; i-- {
			nc := clients[i]
			adopt := backend != "" && nc.name == backend
			innerNext := next
			next = func() (string, error) {
				resp, err := nc.client.OnEvent(ctx, &proto.OnEventRequest{Name: string(name), DataJson: dataJSON})
				if err != nil {
					// 传输层失败（插件崩溃/连接断开）不是语义否决：跳过该插件继续，
					// 否则一个退出的观察者/压缩后端会让整轮请求永久失败。
					m.markHookClientDead(nc.name, err)
					return innerNext()
				}
				if resp.GetError() != "" {
					return "", fmt.Errorf("%s", resp.GetError())
				}
				if r := resp.GetResultJson(); r != "" {
					// 插播消息：任何插件都可请求（只影响消息尾部，不改写既有内容），
					// 与「载荷改写只归压缩后端」互不冲突。
					if name == EventAgentPreStep {
						appends = append(appends, collectPreStepAppends(r)...)
					}
					if adopt {
						dataJSON = r // 只有选定后端可改写载荷
					}
				}
				return innerNext()
			}
		}
		finalResult, err := next()
		if err != nil {
			return nil, err
		}
		// 反序列化最终 result 为 any（调用方自行类型断言）
		var result any
		if finalResult != "" {
			_ = json.Unmarshal([]byte(finalResult), &result)
		}
		if len(appends) > 0 {
			// 合并进返回载荷的 append 字段，交由调用方追加到消息末尾
			m, _ := result.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			merged, _ := m["append"].([]any)
			m["append"] = append(merged, appends...)
			result = m
		}
		return result, nil

	case "serial":
		// 顺序调用，遇 error 即停（传输层失败按不可用跳过，不视为语义 veto）
		for _, nc := range clients {
			resp, err := nc.client.OnEvent(ctx, &proto.OnEventRequest{Name: string(name), DataJson: dataJSON})
			if err != nil {
				m.markHookClientDead(nc.name, err)
				continue
			}
			if resp.GetError() != "" {
				return nil, fmt.Errorf("%s", resp.GetError())
			}
		}
		return data, nil

	case "bail":
		// 首个非空 result 或 error 短路返回
		for _, nc := range clients {
			resp, err := nc.client.OnEvent(ctx, &proto.OnEventRequest{Name: string(name), DataJson: dataJSON})
			if err != nil {
				m.markHookClientDead(nc.name, err)
				continue
			}
			if resp.GetError() != "" {
				return nil, fmt.Errorf("%s", resp.GetError())
			}
			if r := resp.GetResultJson(); r != "" {
				var result any
				_ = json.Unmarshal([]byte(r), &result)
				return result, nil
			}
		}
		return data, nil

	case "parallel":
		// 并发调用全部插件，等待全部返回后聚合错误（任一失败不影响其余完成）。
		var wg sync.WaitGroup
		errCh := make(chan error, len(clients))
		for _, nc := range clients {
			wg.Add(1)
			go func(c proto.PluginHookServiceClient, plugin string) {
				defer wg.Done()
				resp, err := c.OnEvent(ctx, &proto.OnEventRequest{Name: string(name), DataJson: dataJSON})
				if err != nil {
					// 传输层失败：标记不可用并跳过，不计入 veto 聚合。
					m.markHookClientDead(plugin, err)
					return
				}
				if resp.GetError() != "" {
					errCh <- fmt.Errorf("%s", resp.GetError())
				}
			}(nc.client, nc.name)
		}
		wg.Wait()
		close(errCh)
		var errs []error
		for err := range errCh {
			errs = append(errs, err)
		}
		if len(errs) > 0 {
			return nil, errors.Join(errs...)
		}
		return data, nil

	default: // "emit"
		// 同步顺序调用，忽略返回值（兼容现有通知型 listener）；传输层失败只
		// 标记不可用，不影响其余插件与调用方。
		for _, nc := range clients {
			if _, err := nc.client.OnEvent(ctx, &proto.OnEventRequest{Name: string(name), DataJson: dataJSON}); err != nil {
				m.markHookClientDead(nc.name, err)
			}
		}
		return data, nil
	}
}
