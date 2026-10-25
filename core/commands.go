package core

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dsc/proto"
	"dsc/proto/metadata"
)

// 插件命令分发（声明式，不认插件名）。
//
// 分层约定：
//   - 插件在 metadata.commands 里**声明**命令名与说明（SDK: Config.Commands）
//   - 宿主把声明汇总成一张表（本文件），并负责把 /<name> 转发到声明它的插件
//     （PluginHookService.RunCommand）
//   - 前端只展示表、只转发输入，不认识任何插件，也不认识任何具体命令
//
// 为什么必须这样：命令是插件的能力，不是核心的内置功能。核心一旦写死某个命令名，
// 就等于把某个插件内置进来了——换插件要改核心，卸载插件命令还留着，两头都不对。

// PluginCommandInfo 一条插件声明的命令（前端用于补全菜单与帮助文本）。
type PluginCommandInfo struct {
	// Name 命令名，不含前导 "/"。
	Name string
	// Summary 一行说明（补全菜单用）。
	Summary string
	// Usage 用法提示（如 "/context [--json]"）。
	Usage string
	// Plugin 声明它的插件名。**仅供诊断**：前端不应据此做分支判断，
	// 更不应把它写进代码——展示与分发都只按命令名。
	Plugin string
}

// Commands 返回所有插件声明的命令，按命令名排序。
//
// 同名命令以**加载顺序靠前**的插件为准：两个插件抢同一个名字属于配置问题，
// 但必须给出确定的赢家，否则同一输入两次运行结果不同。冲突会打一条 warn。
func (m *Manager) Commands() []PluginCommandInfo {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	metas := make(map[string]*metadata.PluginInfo, len(m.coreMetadata))
	names := make([]string, 0, len(m.coreMetadata))
	for name, info := range m.coreMetadata {
		metas[name] = info
		names = append(names, name)
	}
	clients := make(map[string]bool, len(m.toolHookClients))
	for name := range m.toolHookClients {
		clients[name] = true
	}
	m.mu.RUnlock()

	// 排序保证结果稳定（map 迭代顺序随机）。
	sort.Strings(names)

	byName := make(map[string]PluginCommandInfo)
	for _, plugin := range names {
		meta := metas[plugin]
		if meta == nil {
			continue
		}
		// 没有 hook 服务的插件无法执行命令：声明了也跳过，避免菜单里出现
		// 一个点了必然报错的条目。
		if !clients[plugin] {
			continue
		}
		for _, c := range meta.GetCommands() {
			name := normalizeCommandName(c.GetName())
			if name == "" {
				continue
			}
			if prev, dup := byName[name]; dup {
				m.logger.Warn("duplicate plugin command declaration ignored",
					"command", name, "kept", prev.Plugin, "ignored", plugin)
				continue
			}
			usage := c.GetUsage()
			if usage == "" {
				usage = "/" + name
			}
			byName[name] = PluginCommandInfo{Name: name, Summary: c.GetSummary(), Usage: usage, Plugin: plugin}
		}
	}

	out := make([]PluginCommandInfo, 0, len(byName))
	for _, c := range byName {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// HasCommand 报告是否有插件声明了该命令（name 可带或不带前导 "/"）。
func (m *Manager) HasCommand(name string) bool {
	_, ok := m.commandOwner(name)
	return ok
}

// RunCommand 把命令转发给声明它的插件，返回插件给出的展示文本。
//
// name 可带前导 "/"；args 为命令名之后的原文。未声明该命令时返回错误——
// 调用方据此决定是否交给别处处理（例如当作普通消息）。
func (m *Manager) RunCommand(ctx context.Context, name, args string) (string, error) {
	owner, ok := m.commandOwner(name)
	if !ok {
		return "", fmt.Errorf("no plugin declares the command %q", normalizeCommandName(name))
	}
	client := m.pluginHookClient(owner)
	if client == nil {
		return "", fmt.Errorf("command %q is declared by %q but it provides no hook service", normalizeCommandName(name), owner)
	}
	resp, err := client.RunCommand(ctx, &proto.RunCommandRequest{
		Name: normalizeCommandName(name),
		Args: args,
	})
	if err != nil {
		return "", fmt.Errorf("command %q failed: %w", normalizeCommandName(name), err)
	}
	if resp.GetError() != "" {
		return resp.GetText(), fmt.Errorf("%s", resp.GetError())
	}
	return resp.GetText(), nil
}

// commandOwner 返回声明该命令的插件名。
func (m *Manager) commandOwner(name string) (string, bool) {
	target := normalizeCommandName(name)
	if target == "" {
		return "", false
	}
	for _, c := range m.Commands() {
		if c.Name == target {
			return c.Plugin, true
		}
	}
	return "", false
}

// pluginHookClient 取插件的 hook 客户端快照。
func (m *Manager) pluginHookClient(plugin string) proto.PluginHookServiceClient {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.toolHookClients[plugin]
}

// normalizeCommandName 归一命令名：去空白与前导 "/"，并拒绝含空白的名字
// （这类声明不可能被 /xxx 的输入匹配到）。
func normalizeCommandName(name string) string {
	n := strings.TrimSpace(name)
	n = strings.TrimPrefix(n, "/")
	n = strings.TrimSpace(n)
	if strings.ContainsAny(n, " \t\r\n") {
		return ""
	}
	return n
}
