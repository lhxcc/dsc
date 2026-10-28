package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbletea/v2"
	"dsc/core"
)

// MCP 管理斜杆命令族（/mcp ...）。
//
// 三条约束，与插件管理同源：
//  1. 前端不复制业务实现：连接/列举/加载/卸载/断开一律走宿主工具（manager.ExecuteTool），
//     与模型调用同一份代码——校验、注册表增删、曝光集变更只有一处。
//  2. 用户手打命令本身即授权，注入 approval=never；工具内部的校验照旧。
//  3. 开关（on/off）**只由人**改：它是「要不要把这些工具发给模型」的部署决定，模型侧
//     没有任何工具能开关它（否则模型可以自行把一个会话的目录预算调上去）。切换会立即
//     写回 config.yaml 的 mcp.enabled，重启后保持；缺省（无 mcp 段）即未启用。
//
// 默认关闭是刻意的：日常会话用不到 MCP 时不该常態为它付目录成本。/mcp on 之后才注入
// MCP 全部管理工具（含入口 mcp_connect），关掉连入口一起不注入——目录成本恰为 0。
const mcpAdminTimeout = 90 * time.Second

// mcpAdminMsg MCP 管理命令的执行结果（tea.Cmd → 事件循环）。
type mcpAdminMsg struct {
	sub  string
	text string
	err  error
}

// mcpAdminSub 一条子命令。tool 为空表示前端直接处理（status / on / off）。
type mcpAdminSub struct {
	usage string
	tool  string
	build func(pos []string, opts map[string]string) (map[string]any, error)
}

var mcpAdminSubs = map[string]mcpAdminSub{
	"status": {usage: "status", tool: ""},
	"on":     {usage: "on", tool: ""},
	"off":    {usage: "off", tool: ""},
	"list": {
		usage: "list [server]",
		tool:  "mcp_list",
		build: func(pos []string, _ map[string]string) (map[string]any, error) {
			args := map[string]any{"limit": 200} // 人工查看时默认给全（模型侧默认 40 是为了省上下文）
			if len(pos) > 0 {
				args["server"] = pos[0]
			}
			return args, nil
		},
	},
	"connect": {
		usage: "connect <name> <endpoint>",
		tool:  "mcp_connect",
		build: func(pos []string, _ map[string]string) (map[string]any, error) {
			if len(pos) < 2 {
				return nil, fmt.Errorf("需要 <name> <endpoint>")
			}
			return map[string]any{"server_name": pos[0], "endpoint": pos[1]}, nil
		},
	},
	"disconnect": {
		usage: "disconnect [server]",
		tool:  "mcp_disconnect",
		build: func(pos []string, _ map[string]string) (map[string]any, error) {
			args := map[string]any{}
			if len(pos) > 0 {
				args["server_name"] = pos[0]
			}
			return args, nil
		},
	},
	"load": {
		usage: "load <tool...> | load --server <name>",
		tool:  "mcp_load",
		build: func(pos []string, opts map[string]string) (map[string]any, error) {
			if srv := opts["server"]; srv != "" {
				return map[string]any{"server": srv}, nil
			}
			if len(pos) == 0 {
				return nil, fmt.Errorf("需要工具名，或 --server <name>")
			}
			return map[string]any{"tools": pos}, nil
		},
	},
	"unload": {
		usage: "unload [tool...] | unload --server <name>",
		tool:  "mcp_unload",
		build: func(pos []string, opts map[string]string) (map[string]any, error) {
			if srv := opts["server"]; srv != "" {
				return map[string]any{"server": srv}, nil
			}
			if len(pos) == 0 {
				return map[string]any{}, nil // 无参数 = 全部卸载
			}
			return map[string]any{"tools": pos}, nil
		},
	},
}

// mcpAdminAliases 别名 → 规范子命令。
var mcpAdminAliases = map[string]string{
	"ls":  "list",
	"add": "connect",
	"rm":  "disconnect",
}

func normalizeMCPSub(sub string) string {
	sub = strings.ToLower(strings.TrimSpace(sub))
	if canon, ok := mcpAdminAliases[sub]; ok {
		return canon
	}
	if _, ok := mcpAdminSubs[sub]; ok {
		return sub
	}
	return ""
}

// mcpAdminUsage 用法清单。
func mcpAdminUsage() string {
	names := make([]string, 0, len(mcpAdminSubs))
	for n := range mcpAdminSubs {
		names = append(names, n)
	}
	sort.Strings(names)
	lines := []string{"MCP 管理（连接与工具的加载状态都由宿主维护，与模型同一套实现）："}
	for _, n := range names {
		lines = append(lines, "  /mcp "+mcpAdminSubs[n].usage)
	}
	return strings.Join(lines, "\n")
}

// parseMCPArgs 解析位置参数与 --server 旗标。
func parseMCPArgs(argv []string) (pos []string, opts map[string]string, err error) {
	opts = map[string]string{}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--server" || a == "-s":
			if i+1 >= len(argv) {
				return nil, nil, fmt.Errorf("--server 需要一个值")
			}
			opts["server"] = argv[i+1]
			i++
		case strings.HasPrefix(a, "--server="):
			opts["server"] = strings.TrimPrefix(a, "--server=")
		case strings.HasPrefix(a, "-") && a != "-":
			return nil, nil, fmt.Errorf("未知选项 %s", a)
		default:
			pos = append(pos, a)
		}
	}
	return pos, opts, nil
}

// runMCPAdmin 执行一条 /mcp 子命令（异步：连接可能要等远端握手）。
func (m *Model) runMCPAdmin(sub string, argv []string) tea.Cmd {
	sub = normalizeMCPSub(sub)
	if sub == "" {
		m.appendMessage(assistantNameSty.Render(assistantMark+" DSC · MCP") + "\n" + mcpAdminUsage())
		return nil
	}
	if m.manager == nil {
		m.appendMessage(errorSty.Render("錯誤: 宿主不可用"))
		return nil
	}

	// status / on / off 是宿主状态，不需要走工具流水线（也不需要审批）
	switch sub {
	case "status":
		m.appendMessage(assistantNameSty.Render(assistantMark+" DSC · MCP") + "\n" + mcpStatusText(m.manager.MCPStatus()))
		return nil
	case "on", "off":
		enable := sub == "on"
		// 先内存后落盘：写盘失败也照样生效于本次运行，但必须让人知道「重启后不保持」。
		perr := m.manager.SetMCPToolsEnabledPersist(enable)
		text := mcpStatusText(m.manager.MCPStatus())
		if perr != nil {
			text += "\n" + "写回配置失败（本次运行已生效，重启后不保持）：" + perr.Error()
		} else {
			text += "\n已写入配置：" + m.manager.MCPConfigPath()
		}
		m.appendMessage(assistantNameSty.Render(assistantMark+" DSC · MCP") + "\n" + text)
		return nil
	}

	// 连接/加载在开关关闭时没有意义（注入不了，模型也看不见）。不静默帮用户打开开关：
	// 开关是要持久化到配置文件里的部署决定，必须是人明确按下的那一次 /mcp on。
	if !m.manager.MCPToolsEnabled() && (sub == "connect" || sub == "load") {
		m.appendMessage(errorSty.Render("MCP 未啟用：先執行 /mcp on（開關會寫入 config.yaml，關閉時連 mcp_connect 都不注入）"))
		return nil
	}

	spec := mcpAdminSubs[sub]
	pos, opts, err := parseMCPArgs(argv)
	if err != nil {
		m.appendMessage(errorSty.Render(err.Error()) + "\n" + mcpAdminUsage())
		return nil
	}
	args, err := spec.build(pos, opts)
	if err != nil {
		m.appendMessage(errorSty.Render("用法: /mcp " + spec.usage))
		return nil
	}

	manager := m.manager
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), mcpAdminTimeout)
		defer cancel()
		// 用户手打命令即授权（WithUserGrant）；approval=never 另有用途——它满足
		// 「要求审批策略为 never」的工具能力闸门，两者不是同一件事。
		ctx = core.WithUserGrant(core.WithApprovalPolicy(ctx, "never"))
		raw, _ := json.Marshal(args)
		text, err := manager.ExecuteTool(ctx, spec.tool, raw)
		return mcpAdminMsg{sub: sub, text: text, err: err}
	}
}

// mcpStatusText 渲染 /mcp 状态块：开关、服务器、已加载工具与它们的目录成本。
//
// 成本必须显示：/mcp on 之后每个请求都要带上已加载的工具，用户得知道自己在付多少。
func mcpStatusText(st core.MCPStatus) string {
	var b strings.Builder
	if st.Enabled {
		b.WriteString("注入：已开启（MCP 工具会随请求发给模型）\n")
	} else {
		b.WriteString("注入：已关闭（模型看不到任何 MCP 工具，MCP 目录成本为 0）/mcp on 开启\n")
	}
	if len(st.Servers) == 0 {
		b.WriteString("服务器：未连接（/mcp connect <name> <endpoint>）")
		if len(st.Loaded) > 0 {
			fmt.Fprintf(&b, "\n已加载：%d 个（服务器已断开，加载记录仍在）", len(st.Loaded))
		}
		return b.String()
	}
	b.WriteString("服务器：\n")
	for _, srv := range st.Servers {
		fmt.Fprintf(&b, "  %s  %s  %d 个工具", srv.Name, srv.Endpoint, srv.Tools)
		if srv.Loaded > 0 {
			fmt.Fprintf(&b, "（已加载 %d）", srv.Loaded)
		}
		b.WriteString("\n")
	}
	if len(st.Loaded) == 0 {
		b.WriteString("已加载：无（/mcp list 查看，/mcp load <tool...> 加载）")
	} else {
		fmt.Fprintf(&b, "已加载：%d 个 / 约 %d tokens 每请求\n", len(st.Loaded), st.LoadedCost)
		for _, name := range st.Loaded {
			fmt.Fprintf(&b, "  %s\n", name)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}
