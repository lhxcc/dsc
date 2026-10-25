package dsc

import (
	"context"
	"errors"
	"strings"

	"dsc/proto/metadata"
)

// ErrCommandNotSupported 插件未实现命令钩子（未声明命令或未接 RunCommand）。
var ErrCommandNotSupported = errors.New("plugin does not implement RunCommand")

// Command 一条斜杆命令的声明。
//
// **命令是插件声明出来的能力，不是宿主内置的**：核心/前端只认「声明清单 + 转发
// 通道」，不认任何插件名，也不认任何具体命令名。因此：
//   - 声明写在这里（Config.Commands），随 metadata 上报给宿主
//   - 执行写在 Hook.RunCommand，由宿主按名字转发过来
//   - 界面上的补全菜单由宿主动态汇总，卸载插件即消失
type Command struct {
	// Name 命令名，不含前导 "/"（如 "context"）。
	Name string
	// Summary 补全菜单里的一行说明。
	Summary string
	// Usage 用法提示（可选，如 "/context [--json]"）；空则用 "/"+Name。
	Usage string
}

// commandsToProto 把声明转成 metadata 结构；非法条目（空名或以 "/" 开头，
// 或含空白）直接丢弃——这类声明只会让用户敲出一个永远匹配不上的命令。
func commandsToProto(cmds []Command) []*metadata.PluginCommand {
	if len(cmds) == 0 {
		return nil
	}
	out := make([]*metadata.PluginCommand, 0, len(cmds))
	seen := make(map[string]bool, len(cmds))
	for _, c := range cmds {
		name := strings.TrimSpace(strings.TrimPrefix(c.Name, "/"))
		if name == "" || strings.ContainsAny(name, " \t/") || seen[name] {
			continue
		}
		seen[name] = true
		usage := c.Usage
		if usage == "" {
			usage = "/" + name
		}
		out = append(out, &metadata.PluginCommand{Name: name, Summary: c.Summary, Usage: usage})
	}
	return out
}

// RunCommandText 是插件侧的自测便利：直接调用 Hook.RunCommand 而不经 gRPC。
// 宿主侧不走这里（它经 PluginHookService.RunCommand 转发）。
func (h *Hook) RunCommandText(ctx context.Context, name, args string) (string, error) {
	if h == nil || h.RunCommand == nil {
		return "", ErrCommandNotSupported
	}
	return h.RunCommand(ctx, name, args)
}
