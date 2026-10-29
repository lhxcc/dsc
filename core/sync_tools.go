package core

import (
	"context"
	"strings"
	"time"

	"dsc/proto"
)

// syncMinInterval 聚合工具目录同步的最小间隔（节流）。模型每回合频繁取工具目录，
// 且 LUA 宿主插件的脚本热加载轮询为 2s，故 800ms 节流足够即时也不至于高频拉全量插件。
const syncMinInterval = 800 * time.Millisecond

// syncPluginTools 节流地重新拉取各 tool 插件的最新工具并同步到共享 registry。
// LUA 宿主插件 等脚本插件在运行中热加载新增/删除脚本工具后，宿主聚合工具目录
// （模型可直接调用的工具）随本同步更新，使新脚本工具无需重启即可被模型直接调用。
// 幂等：对工具集无变化的插件不产生任何 registry 变动。
func (m *Manager) syncPluginTools() {
	// 节流：取快照（不持锁做 gRPC），命中节流窗则跳过。
	m.mu.Lock()
	if time.Since(m.lastToolSync) < syncMinInterval {
		m.mu.Unlock()
		return
	}
	m.lastToolSync = time.Now()
	clients := make(map[string]proto.ToolServiceClient, len(m.toolClients))
	for name, c := range m.toolClients {
		clients[name] = c
	}
	m.mu.Unlock()

	for name, client := range clients {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		resp, err := client.ListTools(ctx, &proto.ListToolsRequest{})
		cancel()
		if err != nil {
			m.logger.Warn("sync plugin tools failed", "plugin", name, "error", err)
			continue
		}
		defs := make([]ToolDefinition, 0, len(resp.Tools))
		names := make([]string, 0, len(resp.Tools))
		for _, t := range resp.Tools {
			defs = append(defs, remoteToolFromProto(client, t))
			names = append(names, t.Name)
		}
		m.applyPluginTools(name, defs, names)
	}
}

// applyPluginTools 将某插件的最新工具集与共享 registry 对齐（新增/更新/删除）。
//
// 「更新」是这里最容易被漏掉的一半：工具名不变、但定义变了（插件把 hidden 从 true
// 翻成 false、改了 description/schema/scopes）是最常见的一种变化——MCP 的
// mcp_load 就是只翻 hidden，名字集合一个不差。以前这里按「名字集合有变化才动手」早退，
// 于是加载过的 MCP 工具永远进不了目录，而插件那边明明已经把它标成可见了。
//
// 所以 diff 的粒度必须是**定义**而不是名字：同名但定义不同就替换（ToolRegistry.Replace），
// 定义相同才跳过。
func (m *Manager) applyPluginTools(name string, defs []ToolDefinition, names []string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	old, _ := m.coreToolNames[name]
	oldSet := make(map[string]bool, len(old))
	for _, n := range old {
		oldSet[n] = true
	}
	newSet := make(map[string]bool, len(names))
	for _, n := range names {
		newSet[n] = true
	}

	nameByDef := make(map[string]ToolDefinition, len(defs))
	for _, d := range defs {
		nameByDef[d.Name()] = d
	}

	svcID := m.toolServiceIDs[name]
	// 新增 / 更新
	for _, n := range names {
		d, ok := nameByDef[n]
		if !ok {
			continue
		}
		if !oldSet[n] {
			if err := m.toolRegistry.Register(d); err != nil {
				m.logger.Warn("hot-sync register tool", "tool", n, "plugin", name, "error", err)
				continue
			}
			m.toolNameToServiceID[n] = svcID
			m.logger.Info("hot-synced new tool", "tool", n, "plugin", name)
			continue
		}
		cur, ok := m.toolRegistry.Get(n)
		if ok && toolFingerprint(cur) == toolFingerprint(d) {
			continue // 定义没变：不碰 registry，省掉无谓的目录抖动
		}
		m.toolRegistry.Replace(d)
		m.toolNameToServiceID[n] = svcID
		m.logger.Info("hot-synced updated tool", "tool", n, "plugin", name)
	}
	// 删除
	for _, n := range old {
		if newSet[n] {
			continue
		}
		m.toolRegistry.Unregister(n)
		delete(m.toolNameToServiceID, n)
		m.logger.Info("hot-synced removed tool", "tool", n, "plugin", name)
	}

	m.coreToolNames[name] = names
}

// toolFingerprint 是一条工具定义里「会影响宿主行为或模型所见」的全部字段的指纹。
//
// 只比名字不够（同名工具的 hidden 翻转是最常见的变化），比结构体指针更不够（每次
// ListTools 都是新对象）。可选能力逐项取：没实现的按「无声明」算，与实现者的零值一致。
func toolFingerprint(t ToolDefinition) string {
	var b strings.Builder
	b.WriteString(t.Name())
	b.WriteByte(0)
	b.WriteString(t.Description())
	b.WriteByte(0)
	b.Write(t.ParametersSchema())
	b.WriteByte(0)
	if h, ok := t.(HiddenTool); ok && h.IsHidden() {
		b.WriteString("hidden")
	}
	b.WriteByte(0)
	if s, ok := t.(ScopeDeclarer); ok {
		b.WriteString(strings.Join(s.Scopes(), ","))
	}
	b.WriteByte(0)
	if d, ok := t.(ManagementDeclarer); ok {
		b.WriteString(d.ManagementGroup())
	}
	return b.String()
}
