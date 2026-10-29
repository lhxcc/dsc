// tool-mcp：把 MCP 服务器接进 DSC 的工具插件。
//
// 这个插件是「MCP 属于插件」这件事的落点：客户端在 dsc-sdk/mcp（不依赖宿主），
// 这里只做装配——读配置、管连接、把服务器工具变成 DSC 工具、把管理操作挂上管理面。
//
// 三件与宿主约定的事：
//   - 工具是**动态**的（sdk.ToolProvider）：连上服务器就出现，断开就消失；未加载的
//     用 Hidden 标记（注册但不对模型曝光），而不是干脆不报——「隐藏 ≠ 不可用」。
//   - 管理操作声明 Management: "mcp"：宿主把它们汇进管理面目录，TUI 与 admin REST
//     都从那里取，插件不认识任何界面。
//   - 斜杆命令 /mcp 由本插件声明（sdk.Config.Commands + Hook.RunCommand）：命令是插件
//     的能力，不是前端内置的。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	dsc "dsc-sdk"
)

func main() {
	app := newApp()

	sdk := dsc.New(dsc.Config{
		Name:    "tool-mcp",
		Version: "1.0.0",
		Type:    dsc.TypeTool,
		// 命令声明：补全菜单由宿主动态汇总，卸载插件即消失。
		Commands: []dsc.Command{{
			Name:    "mcp",
			Summary: "MCP 服务器与工具（status / on / off / list / connect / load / unload / disconnect / resources / prompts）",
			Usage:   "/mcp [status|on|off|list|connect|load|unload|disconnect|resources|read|prompts|prompt]",
		}},
	})
	// 动态工具集：管理操作 + 已连接服务器的工具（每次 ListTools/ExecuteTool 求值）。
	sdk.ToolProvider(app.currentTools)
	sdk.Hook(dsc.Hook{RunCommand: app.runCommand})
	sdk.OnStart(app.start)
	sdk.OnStop(func() error { app.stop(); return nil })

	sdk.Serve()
}

// start 读配置、连服务器。
//
// 连接失败**不**阻塞插件启动：一台服务器连不上，其余服务器与「用 mcp_connect 现场
// 再试」都还可用；反复失败在日志里可见即可（与旧宿主行为一致）。
func (a *app) start(_ context.Context) error {
	a.mu.Lock()
	specs := a.specs
	a.mu.Unlock()

	for _, spec := range specs {
		if err := a.connect(spec); err != nil {
			fmt.Fprintf(os.Stderr, "tool-mcp: %s: %v\n", spec.Name, err)
		}
	}
	return nil
}

// stop 断开全部服务器（回收 stdio 子进程）。
func (a *app) stop() {
	a.mu.Lock()
	clients := make([]*serverConn, 0, len(a.clients))
	for _, c := range a.clients {
		clients = append(clients, c)
	}
	a.clients = map[string]*serverConn{}
	a.mu.Unlock()

	for _, c := range clients {
		c.client.Disconnect()
	}
}

// runCommand 实现 /mcp：参数解析在这里，执行复用与工具相同的实现。
//
// 命令与工具共用一套实现是刻意的：`/mcp list` 与 `mcp_list` 若各写一份，行为迟早
// 分叉（一边修了分页/报错措辞，另一边没修）。
func (a *app) runCommand(_ context.Context, name, args string) (string, error) {
	if name != "mcp" {
		return "", fmt.Errorf("unknown command %q", name)
	}
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return a.statusText(), nil
	}
	sub, rest := fields[0], fields[1:]

	switch sub {
	case "status":
		return a.statusText(), nil
	case "on", "off":
		on := sub == "on"
		a.setEnabled(on)
		note := ""
		if err := a.persistEnabled(); err != nil {
			// 写盘失败也照样生效于本次运行，但必须让人知道「重启后不保持」。
			note = "\n（写回状态失败，本次运行已生效、重启后不保持：" + err.Error() + "）"
		}
		return a.statusText() + note, nil
	case "list":
		payload := map[string]any{"limit": 200}
		if len(rest) > 0 {
			payload["server"] = rest[0]
		}
		return a.execList(payload)
	case "connect":
		return a.execConnectFields(rest)
	case "load":
		return a.execLoad(map[string]any{"tools": rest})
	case "unload":
		if len(rest) == 0 {
			return a.execUnload(map[string]any{})
		}
		return a.execUnload(map[string]any{"tools": rest})
	case "disconnect":
		payload := map[string]any{}
		if len(rest) > 0 {
			payload["server_name"] = rest[0]
		}
		return a.execDisconnect(payload)
	case "resources":
		payload := map[string]any{"limit": 200}
		if len(rest) > 0 {
			payload["server"] = rest[0]
		}
		return a.execListResources(payload)
	case "read":
		if len(rest) == 0 {
			return "", fmt.Errorf("用法：/mcp read <uri> [server]")
		}
		payload := map[string]any{"uri": rest[0]}
		if len(rest) > 1 {
			payload["server"] = rest[1]
		}
		return a.execReadResource(context.Background(), payload)
	case "prompts":
		payload := map[string]any{"limit": 200}
		if len(rest) > 0 {
			payload["server"] = rest[0]
		}
		return a.execListPrompts(payload)
	case "prompt":
		if len(rest) == 0 {
			return "", fmt.Errorf("用法：/mcp prompt <name> [key=value ...]")
		}
		payload := map[string]any{"name": rest[0]}
		args := map[string]any{}
		for _, kv := range rest[1:] {
			if k, v, ok := strings.Cut(kv, "="); ok {
				args[k] = v
			}
		}
		if len(args) > 0 {
			payload["arguments"] = args
		}
		return a.execGetPrompt(context.Background(), payload)
	}
	return "", fmt.Errorf("unknown /mcp subcommand %q（%s）", sub, mcpUsage)
}

// execConnectFields 解析 /mcp connect 的位置参数：<name> <endpoint|command> [args...]。
//
// 目标形态决定传输（与宿主原来的 /mcp connect 一致）：http(s):// 是 endpoint，其余是
// 要拉起的命令。命令带 - 开头的参数时用裸写法即可——这里已经把位置参数切好了。
func (a *app) execConnectFields(rest []string) (string, error) {
	if len(rest) < 2 {
		return "", fmt.Errorf("用法：/mcp connect <name> <endpoint|command> [args...]")
	}
	payload := map[string]any{"server_name": rest[0]}
	target := rest[1]
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		payload["endpoint"] = target
		if len(rest) > 2 {
			return "", fmt.Errorf("HTTP 只接一个 endpoint，多余参数：%v", rest[2:])
		}
	} else {
		payload["transport"] = "stdio"
		payload["command"] = target
		if len(rest) > 2 {
			payload["args"] = rest[2:]
		}
	}
	return a.execConnect(payload)
}

const mcpUsage = "status | on | off | list [server] | connect <name> <endpoint|command> [args...] | " +
	"load <tool...> | unload [tool...] | disconnect [server] | " +
	"resources [server] | read <uri> [server] | prompts [server] | prompt <name> [key=value ...]"

// marshalArgs 把工具入参解码成 map（工具与命令两条入口共用）。
func marshalArgs(args json.RawMessage) (map[string]any, error) {
	if len(strings.TrimSpace(string(args))) == 0 {
		return map[string]any{}, nil
	}
	var out map[string]any
	if err := json.Unmarshal(args, &out); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return out, nil
}
