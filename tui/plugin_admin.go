package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"dsc/core"
)

// 插件管理斜杆命令族：把模型已有的六个插件管理工具原样暴露给用户（/plugin ...）。
//
// 三条设计约束：
//  1. 前端**不写任何插件名**（项目硬约束：核心/SDK/前端不得出现插件名），只写宿主
//     工具名与参数形状；可用插件一律由 list_dsc_plugins 动态枚举（它合并 config +
//     运行态 + 磁盘三方），与模型看到的是同一份事实，不维护第二份清单。
//  2. 执行走宿主工具流水线（manager.ExecuteTool），与模型调用**同一实现**——校验、
//     备份 config、热加载、审批语义都不在前端复制一份。
//  3. 用户手打命令本身即授权，故注入 approval=never；工具内部的校验与回滚照旧。

// pluginAdminTimeout 插件管理命令的超时：安装/升级要拷贝目录并热加载进程，
// 比普通工具慢，但不能无限等（界面不能卡死）。
const pluginAdminTimeout = 120 * time.Second

// pluginAdminMsg 插件管理命令的执行结果（tea.Cmd → 事件循环）。
type pluginAdminMsg struct {
	sub  string
	text string
	err  error
}

// pluginAdminSub 一条子命令：用法、宿主工具名、参数构造。
type pluginAdminSub struct {
	usage string
	tool  string
	build func(pos []string, opts map[string]string) (map[string]any, error)
}

// pluginAdminSubs 子命令表（含常用别名）。键是用户输入，值是宿主工具。
var pluginAdminSubs = map[string]pluginAdminSub{
	"list": {
		usage: "list",
		tool:  "list_dsc_plugins",
		build: func([]string, map[string]string) (map[string]any, error) { return map[string]any{}, nil },
	},
	"install": {
		usage: "install <name> <source> [--type tool]",
		tool:  "install_dsc_plugin",
		build: func(pos []string, opts map[string]string) (map[string]any, error) {
			if len(pos) < 2 {
				return nil, fmt.Errorf("需要 <name> <source>")
			}
			typ := opts["type"]
			if typ == "" {
				typ = "tool" // 安装时宿主会按插件元数据校验，类型不符会整体回滚
			}
			return map[string]any{"name": pos[0], "source": pos[1], "type": typ}, nil
		},
	},
	"remove": {
		usage: "remove <name>",
		tool:  "uninstall_dsc_plugin",
		build: func(pos []string, _ map[string]string) (map[string]any, error) {
			if len(pos) < 1 {
				return nil, fmt.Errorf("需要 <name>")
			}
			return map[string]any{"name": pos[0]}, nil
		},
	},
	"upgrade": {
		usage: "upgrade <name> <version> <source>",
		tool:  "upgrade_dsc_plugin",
		build: func(pos []string, _ map[string]string) (map[string]any, error) {
			if len(pos) < 3 {
				return nil, fmt.Errorf("需要 <name> <version> <source>")
			}
			return map[string]any{"name": pos[0], "version": pos[1], "source": pos[2]}, nil
		},
	},
	"load": {
		usage: "load <name> [--type tool] [--persist]",
		tool:  "load_dsc_plugin",
		build: func(pos []string, opts map[string]string) (map[string]any, error) {
			if len(pos) < 1 {
				return nil, fmt.Errorf("需要 <name>")
			}
			a := map[string]any{"name": pos[0], "persist": opts["persist"] == "true"}
			if typ := opts["type"]; typ != "" {
				a["type"] = typ
			}
			return a, nil
		},
	},
	"unload": {
		usage: "unload <name> [--persist]",
		tool:  "unload_dsc_plugin",
		build: func(pos []string, opts map[string]string) (map[string]any, error) {
			if len(pos) < 1 {
				return nil, fmt.Errorf("需要 <name>")
			}
			return map[string]any{"name": pos[0], "persist": opts["persist"] == "true"}, nil
		},
	},
}

// pluginAdminAliases 别名 → 规范子命令。
var pluginAdminAliases = map[string]string{
	"ls":        "list",
	"rm":        "remove",
	"uninstall": "remove",
	"delete":    "remove",
	"up":        "upgrade",
	"help":      "help",
}

// normalizePluginSub 归一子命令（别名折叠）；未知返回空串。
func normalizePluginSub(sub string) string {
	sub = strings.ToLower(strings.TrimSpace(sub))
	if canon, ok := pluginAdminAliases[sub]; ok {
		return canon
	}
	if _, ok := pluginAdminSubs[sub]; ok {
		return sub
	}
	return ""
}

// pluginAdminUsage 用法清单（按子命令名排序，便于阅读）。
func pluginAdminUsage() string {
	names := make([]string, 0, len(pluginAdminSubs))
	for n := range pluginAdminSubs {
		names = append(names, n)
	}
	sort.Strings(names)
	lines := []string{"插件管理（与模型同一套工具，动态枚举，无硬编码插件名）："}
	for _, n := range names {
		lines = append(lines, "  /plugin "+pluginAdminSubs[n].usage)
	}
	lines = append(lines, "  /plugins  = /plugin list")
	return strings.Join(lines, "\n")
}

// parsePluginArgs 解析位置参数与旗标（--type X / --type=X / --persist）。
func parsePluginArgs(argv []string) (pos []string, opts map[string]string, err error) {
	opts = map[string]string{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--persist" || a == "-p":
			opts["persist"] = "true"
		case a == "--type" || a == "-t":
			if i+1 >= len(argv) {
				return nil, nil, fmt.Errorf("--type 需要一个值")
			}
			opts["type"] = argv[i+1]
			i++
		case strings.HasPrefix(a, "--type="):
			opts["type"] = strings.TrimPrefix(a, "--type=")
		case strings.HasPrefix(a, "-"):
			return nil, nil, fmt.Errorf("未知选项 %s", a)
		default:
			pos = append(pos, a)
		}
	}
	return pos, opts, nil
}

// runPluginAdmin 执行一条插件管理子命令（异步，避免安装/热加载阻塞界面）。
// 参数/用法错误直接回显，返回 nil 表示无需执行命令。
func (m *Model) runPluginAdmin(sub string, argv []string) tea.Cmd {
	sub = normalizePluginSub(sub)
	if sub == "help" || sub == "" {
		m.appendMessage(assistantNameSty.Render(assistantMark+" DSC · 插件") + "\n" + pluginAdminUsage())
		return nil
	}
	spec := pluginAdminSubs[sub]
	pos, opts, err := parsePluginArgs(argv)
	if err != nil {
		m.appendMessage(errorSty.Render(err.Error()) + "\n" + pluginAdminUsage())
		return nil
	}
	args, err := spec.build(pos, opts)
	if err != nil {
		m.appendMessage(errorSty.Render("用法: /plugin " + spec.usage))
		return nil
	}
	if m.manager == nil {
		m.appendMessage(errorSty.Render("錯誤: 插件管理器不可用"))
		return nil
	}

	manager := m.manager
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pluginAdminTimeout)
		defer cancel()
		// 用户手打命令即授权：审批门不必再问一次（工具内部校验与回滚不变）。
		ctx = core.WithApprovalPolicy(ctx, "never")
		raw, _ := json.Marshal(args)
		text, err := manager.ExecuteTool(ctx, spec.tool, raw)
		return pluginAdminMsg{sub: sub, text: text, err: err}
	}
}

// renderPluginAdminResult 把工具结果整理成可读输出：list 渲染成表，其余显示 note
// 与精简 JSON（工具返回的是小对象，原样 pretty-print 反而更好核对）。
func renderPluginAdminResult(text string) string {
	if strings.TrimSpace(text) == "" {
		return "（无输出）"
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(text), &probe); err != nil {
		return text
	}
	if rows, ok := probe["plugins"].([]any); ok {
		return renderPluginRows(rows, probe)
	}
	if note, ok := probe["note"].(string); ok && note != "" {
		out, _ := json.MarshalIndent(probe, "", "  ")
		return note + "\n" + string(out)
	}
	out, err := json.MarshalIndent(probe, "", "  ")
	if err != nil {
		return text
	}
	return string(out)
}

// renderPluginRows 渲染插件列表（列宽按显示宽度算，CJK 名字不会错位）。
func renderPluginRows(rows []any, all map[string]any) string {
	type rec struct{ name, typ, state, enabled string }
	recs := make([]rec, 0, len(rows))
	for _, r := range rows {
		obj, ok := r.(map[string]any)
		if !ok {
			continue
		}
		get := func(k string) string {
			if v, ok := obj[k]; ok {
				return fmt.Sprint(v)
			}
			return ""
		}
		recs = append(recs, rec{name: get("name"), typ: get("type"), state: get("state"), enabled: get("enabled")})
	}
	if len(recs) == 0 {
		return "（没有插件）"
	}
	wName, wType, wState := lipgloss.Width("名称"), lipgloss.Width("类型"), lipgloss.Width("状态")
	for _, r := range recs {
		wName = max(wName, lipgloss.Width(r.name))
		wType = max(wType, lipgloss.Width(r.typ))
		wState = max(wState, lipgloss.Width(r.state))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%-*s  %-*s  %-*s  %s\n", wName, "名称", wType, "类型", wState, "状态", "启用")
	for _, r := range recs {
		fmt.Fprintf(&b, "%-*s  %-*s  %-*s  %s\n", wName, r.name, wType, r.typ, wState, r.state, r.enabled)
	}
	if n, ok := all["count"]; ok {
		fmt.Fprintf(&b, "\n共 %v 个（state: loaded=已加载; configured=配置未加载; orphan=磁盘未配置）", n)
	}
	return strings.TrimRight(b.String(), "\n")
}
