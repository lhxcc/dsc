package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	dsc "dsc-sdk"
)

// 管理面：本插件声明的操作都归这一组。宿主据此把它们汇进管理面目录（TUI 斜杆命令、
// admin REST 都从那里取），插件不认识任何界面。
const managementGroup = "mcp"

// currentTools 插件对宿主的工具视图（每次 ListTools/ExecuteTool 都会求值）。
//
// 三条规则，与旧宿主实现保持一致：
//   - 总闸关闭时：管理操作**仍然上报**但标 Hidden——否则呈现层连「怎么打开」都显示不出来；
//   - 已连接的服务器工具一律上报，未加载的标 Hidden——「隐藏 ≠ 不可用」；
//   - 默认不曝光 MCP 工具：一台服务器几百个工具，全塞进目录等于每个请求都为它付钱。
func (a *app) currentTools() []dsc.Tool {
	a.mu.Lock()
	defer a.mu.Unlock()

	enabled := a.enabled
	hasServers := len(a.clients) > 0
	tools := a.managementToolsLocked(enabled, hasServers)

	// 服务器顺序固定，工具顺序才稳定（目录字节稳定 → 前缀缓存可命中）。
	names := make([]string, 0, len(a.clients))
	for name := range a.clients {
		names = append(names, name)
	}
	sort.Strings(names)

	hasResources, hasPrompts := false, false
	for _, server := range names {
		caps := a.clients[server].client.Capabilities()
		hasResources = hasResources || caps.Resources
		hasPrompts = hasPrompts || caps.Prompts
	}
	// 资源/提示词是**按能力**出现的插件自有工具，不是服务器工具：一台只有 tools 的
	// 服务器（绝大多数）根本不注册它们，目录成本为 0。它们也不走 mcp_load 那一套——
	// 「几百个工具里挑几个」的曝光策略对这两三条访问器没有意义，能力在就摆出来。
	tools = append(tools, a.resourceToolsLocked(enabled, hasResources)...)
	tools = append(tools, a.promptToolsLocked(enabled, hasPrompts)...)

	for _, server := range names {
		conn := a.clients[server]
		for _, t := range conn.client.Tools() {
			public := publicName(server, t.Name)
			tools = append(tools, dsc.Tool{
				Name:        public,
				Description: t.Description,
				Schema:      t.Schema,
				Hidden:      !enabled || !a.loaded[public],
				Handler:     a.callHandler(server, t.Name),
			})
		}
	}
	return tools
}

// callHandler 一条 MCP 工具的调用闭包。
//
// server/raw 以参数传入（而不是闭包捕获循环变量）：Go 1.22 之前捕获会串到最后一次
// 迭代，那类 bug 表现为「调 A 服务器的工具，结果打到 B」。
func (a *app) callHandler(server, raw string) func(context.Context, json.RawMessage) (string, error) {
	return func(ctx context.Context, args json.RawMessage) (string, error) {
		a.mu.Lock()
		conn, ok := a.clients[server]
		enabled := a.enabled
		a.mu.Unlock()
		if !ok {
			return "", fmt.Errorf("MCP server %q is not connected", server)
		}
		if !enabled {
			return "", fmt.Errorf("MCP is off (/mcp on to enable)")
		}
		return conn.client.CallTool(ctx, raw, args)
	}
}

// managementToolsLocked 五条管理操作（须持锁读取状态）。
func (a *app) managementToolsLocked(enabled, hasServers bool) []dsc.Tool {
	// 关闭时藏入口，但没有服务器时只留入口：其余四条是空操作，藏起来少占目录。
	entryHidden := !enabled
	restHidden := !enabled || !hasServers

	return []dsc.Tool{
		{
			Name:        "mcp_connect",
			Description: connectDescription,
			Schema:      json.RawMessage(connectSchema),
			Hidden:      entryHidden,
			Management:  managementGroup,
			// 审批是能力的权限属性：连接/拉起服务器会碰这两个范围。
			// 一律两个都声明（即使这次只给 endpoint）——声明是「可能做什么」，
			// 按参数少声明就等于留了一条「先连 HTTP、再把同一工具改成 stdio」的绕道。
			Scopes: []string{scopeProcessSpawn, scopeNetEgress},
			ApprovalFn: func(_ context.Context, args json.RawMessage) string {
				return connectApprovalDetail(args)
			},
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execConnect(payload)
			},
		},
		{
			Name:        "mcp_list",
			Description: "List the tools that connected MCP servers provide. MCP tools are hidden from your tool list by default (a server can expose hundreds, and the catalog is sent with every request): use this to see their names, then mcp_load the ones you need.",
			Schema:      json.RawMessage(listSchema),
			Hidden:      restHidden,
			Management:  managementGroup,
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execList(payload)
			},
		},
		{
			Name:        "mcp_load",
			Description: "Load MCP tools into your tool list so you can call them. Call mcp_list first to get exact names. Load sparingly: every loaded tool is sent with every request.",
			Schema:      json.RawMessage(loadSchema),
			Hidden:      restHidden,
			Management:  managementGroup,
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execLoad(payload)
			},
		},
		{
			Name:        "mcp_unload",
			Description: "Remove MCP tools from your tool list when you are done with them, so they stop costing catalog tokens on every request. The connection stays: mcp_load brings them back without reconnecting.",
			Schema:      json.RawMessage(unloadSchema),
			Hidden:      restHidden,
			Management:  managementGroup,
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execUnload(payload)
			},
		},
		{
			Name:        "mcp_disconnect",
			Description: "Disconnect an MCP server: unregister its tools and forget which ones were loaded (omit the name to disconnect all).",
			Schema:      json.RawMessage(disconnectSchema),
			Hidden:      restHidden,
			Management:  managementGroup,
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execDisconnect(payload)
			},
		},
	}
}

// resourceToolsLocked 资源访问器：只在**有服务器声明了 resources 能力**时出现。
//
// 资源是内容（可读的文档/数据），不是动作；模型只能通过这两个工具读它。它们不参与
// mcp_load 那套曝光策略——一台服务器通常只有几条到几十条资源，而「先加载才能用」
// 会让模型在需要时才去猜插件支持什么，得不偿失。
func (a *app) resourceToolsLocked(enabled, has bool) []dsc.Tool {
	if !has {
		return nil
	}
	return []dsc.Tool{
		{
			Name:        "mcp_list_resources",
			Description: listResourcesDescription,
			Schema:      json.RawMessage(listResourcesSchema),
			Hidden:      !enabled,
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execListResources(payload)
			},
		},
		{
			Name:        "mcp_read_resource",
			Description: readResourceDescription,
			Schema:      json.RawMessage(readResourceSchema),
			Hidden:      !enabled,
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execReadResource(ctx, payload)
			},
		},
	}
}

// promptToolsLocked 提示词访问器：只在有服务器声明了 prompts 能力时出现。
func (a *app) promptToolsLocked(enabled, has bool) []dsc.Tool {
	if !has {
		return nil
	}
	return []dsc.Tool{
		{
			Name:        "mcp_list_prompts",
			Description: listPromptsDescription,
			Schema:      json.RawMessage(listPromptsSchema),
			Hidden:      !enabled,
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execListPrompts(payload)
			},
		},
		{
			Name:        "mcp_get_prompt",
			Description: getPromptDescription,
			Schema:      json.RawMessage(getPromptSchema),
			Hidden:      !enabled,
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				payload, err := marshalArgs(args)
				if err != nil {
					return "", err
				}
				return a.execGetPrompt(ctx, payload)
			},
		},
	}
}

// execConnect 实现 mcp_connect。
func (a *app) execConnect(payload map[string]any) (string, error) {
	spec, err := specFromPayload(payload)
	if err != nil {
		return "", err
	}
	if !a.isEnabled() {
		return "", fmt.Errorf("MCP is off: run /mcp on first (the switch is a person's decision, not a tool's)")
	}

	a.mu.Lock()
	_, already := a.clients[spec.Name]
	a.mu.Unlock()
	if already {
		return fmt.Sprintf("MCP server %q is already connected (%d tools). Its tools are hidden by default: "+
			"call mcp_list to see names, then mcp_load for the ones you need.", spec.Name, a.toolCount(spec.Name)), nil
	}

	if err := a.connect(spec); err != nil {
		return "", err
	}
	how := spec.Endpoint
	if strings.EqualFold(spec.Transport, "stdio") {
		how = "stdio: " + strings.Join(append([]string{spec.Command}, spec.Args...), " ")
	}
	extra := ""
	if n := a.resourceCount(spec.Name); n > 0 {
		extra += fmt.Sprintf(", %d resources (mcp_list_resources / mcp_read_resource)", n)
	}
	if n := a.promptCount(spec.Name); n > 0 {
		extra += fmt.Sprintf(", %d prompts (mcp_list_prompts / mcp_get_prompt)", n)
	}
	return fmt.Sprintf("Connected MCP server %q (%s): %d tools registered (prefix mcp__%s__)%s. "+
		"Registered is not visible: mcp_list shows the names, mcp_load makes the ones you need callable.",
		spec.Name, how, a.toolCount(spec.Name), spec.Name, extra), nil
}

// specFromPayload 把工具入参（或配置块）解析成 serverSpec。
func specFromPayload(payload map[string]any) (serverSpec, error) {
	str := func(key string) string {
		v, _ := payload[key].(string)
		return strings.TrimSpace(v)
	}
	spec := serverSpec{
		Name:      str("server_name"),
		Transport: str("transport"),
		Endpoint:  str("endpoint"),
		Command:   str("command"),
		CWD:       str("cwd"),
	}
	if spec.Name == "" {
		spec.Name = str("name")
	}
	if spec.Name == "" {
		return spec, fmt.Errorf("server_name is required")
	}
	if v, ok := payload["args"]; ok && v != nil {
		items, ok := v.([]any)
		if !ok {
			// 命令入口传的是 []string，JSON 入口是 []any：两种都要接受，
			// 否则「工具能连、命令不能连」这种分叉极难发现。
			if ss, ok2 := v.([]string); ok2 {
				spec.Args = ss
			} else {
				return spec, fmt.Errorf("args must be an array of strings")
			}
		} else {
			for _, item := range items {
				s, ok := item.(string)
				if !ok {
					return spec, fmt.Errorf("args must be an array of strings")
				}
				spec.Args = append(spec.Args, s)
			}
		}
	}
	if v, ok := payload["env"]; ok && v != nil {
		if m, ok := v.(map[string]any); ok {
			spec.Env = make(map[string]string, len(m))
			for k, val := range m {
				s, ok := val.(string)
				if !ok {
					return spec, fmt.Errorf("env[%s] must be a string", k)
				}
				spec.Env[k] = s
			}
		} else if m, ok := v.(map[string]string); ok {
			spec.Env = m
		}
	}
	if spec.Transport == "" {
		if spec.Endpoint != "" {
			spec.Transport = "http"
		} else if spec.Command != "" {
			spec.Transport = "stdio"
		}
	}
	return spec, nil
}

// connectApprovalDetail 审批理由：说清「这一次」具体要发生什么。
//
// 静态的 scopes 只能说明「可能碰什么」；用户要点头时需要知道的是具体命令或地址。
func connectApprovalDetail(args json.RawMessage) string {
	payload, err := marshalArgs(args)
	if err != nil {
		return "Connects an external MCP server and makes its tools available to this session."
	}
	spec, err := specFromPayload(payload)
	if err != nil {
		return "Connects an external MCP server and makes its tools available to this session."
	}
	if strings.EqualFold(spec.Transport, "stdio") {
		return fmt.Sprintf("Spawns `%s` as a local process for MCP server %q; its tools will run with your local privileges once loaded.",
			strings.Join(append([]string{spec.Command}, spec.Args...), " "), spec.Name)
	}
	return fmt.Sprintf("Connects MCP server %q at %s; its tools will run with your local privileges once loaded.",
		spec.Name, spec.Endpoint)
}

// resourceCount / promptCount 某台服务器已发现的资源、提示词数。
func (a *app) resourceCount(server string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[server]; ok {
		return c.client.ResourceCount()
	}
	return 0
}

func (a *app) promptCount(server string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[server]; ok {
		return c.client.PromptCount()
	}
	return 0
}

// toolCount 某台服务器已发现的工具数。
func (a *app) toolCount(server string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c, ok := a.clients[server]; ok {
		return c.client.ToolCount()
	}
	return 0
}

// execList 实现 mcp_list：列出工具（默认不给出 schema，省上下文）。
func (a *app) execList(payload map[string]any) (string, error) {
	want, _ := payload["server"].(string)
	limit := listDefault
	if v, ok := payload["limit"].(float64); ok && int(v) > 0 {
		limit = int(v)
	}
	if limit > listMax {
		limit = listMax
	}

	a.mu.Lock()
	servers := make([]string, 0, len(a.clients))
	for name := range a.clients {
		if want == "" || name == want {
			servers = append(servers, name)
		}
	}
	sort.Strings(servers)
	type row struct {
		name, desc string
		loaded     bool
	}
	var rows []row
	for _, server := range servers {
		for _, t := range a.clients[server].client.Tools() {
			public := publicName(server, t.Name)
			rows = append(rows, row{name: public, desc: firstLine(t.Description), loaded: a.loaded[public]})
		}
	}
	a.mu.Unlock()

	if len(servers) == 0 {
		if want != "" {
			return "", fmt.Errorf("MCP server %q is not connected", want)
		}
		return "No MCP server is connected. Use mcp_connect with either a streamable-HTTP endpoint or a stdio command to connect one.", nil
	}
	if len(rows) == 0 {
		return fmt.Sprintf("Connected servers (%s) expose no tools.", strings.Join(servers, ", ")), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d tools from %d server(s); showing up to %d.\n", len(rows), len(servers), limit)
	fmt.Fprintf(&b, "Prefix mcp__<server>__. Marked %s are already in your tool list; others need mcp_load.\n", "[loaded]")
	shown := 0
	for _, r := range rows {
		if shown >= limit {
			fmt.Fprintf(&b, "… %d more (raise limit or pass server=)\n", len(rows)-shown)
			break
		}
		mark := ""
		if r.loaded {
			mark = " [loaded]"
		}
		fmt.Fprintf(&b, "  %s%s — %s\n", r.name, mark, r.desc)
		shown++
	}
	return b.String(), nil
}

// serversSupporting 返回声明了某能力的已连接服务器（按名排序）。
func (a *app) serversSupporting(capability string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	names := make([]string, 0, len(a.clients))
	for name, conn := range a.clients {
		caps := conn.client.Capabilities()
		if (capability == "resources" && caps.Resources) || (capability == "prompts" && caps.Prompts) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// pickServers 解析入参里的 server：空 = 全部支持该能力的服务器。
//
// 三种错法要分清（提示不一样）：没连过 / 连了但这台不支持该能力 / 名字写错。
func (a *app) pickServers(want, capability string) ([]string, error) {
	supported := a.serversSupporting(capability)
	want = strings.TrimSpace(want)
	if want == "" {
		return supported, nil
	}
	a.mu.Lock()
	_, connected := a.clients[want]
	a.mu.Unlock()
	if !connected {
		return nil, fmt.Errorf("MCP server %q is not connected", want)
	}
	for _, name := range supported {
		if name == want {
			return []string{want}, nil
		}
	}
	return nil, fmt.Errorf("MCP server %q does not expose %s", want, capability)
}

// execListResources 实现 mcp_list_resources。
func (a *app) execListResources(payload map[string]any) (string, error) {
	want, _ := payload["server"].(string)
	limit := listDefault
	if v, ok := payload["limit"].(float64); ok && int(v) > 0 {
		limit = int(v)
	}
	if limit > listMax {
		limit = listMax
	}

	servers, err := a.pickServers(want, "resources")
	if err != nil {
		return "", err
	}
	if len(servers) == 0 {
		return "No connected MCP server exposes resources.", nil
	}

	type row struct{ server, uri, name, mime, desc string }
	var rows []row
	a.mu.Lock()
	for _, server := range servers {
		for _, r := range a.clients[server].client.Resources() {
			rows = append(rows, row{server: server, uri: r.URI, name: r.Name, mime: r.MimeType, desc: r.Description})
		}
	}
	a.mu.Unlock()

	if len(rows) == 0 {
		return fmt.Sprintf("Connected server(s) %s declare resources but list none.", strings.Join(servers, ", ")), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d resource(s) from %d server(s); showing up to %d. Read one with mcp_read_resource(uri=...).\n",
		len(rows), len(servers), limit)
	shown := 0
	for _, r := range rows {
		if shown >= limit {
			fmt.Fprintf(&b, "… %d more (raise limit or pass server=)\n", len(rows)-shown)
			break
		}
		fmt.Fprintf(&b, "  [%s] %s", r.server, r.uri)
		if r.name != "" && r.name != r.uri {
			fmt.Fprintf(&b, " — %s", r.name)
		}
		if r.mime != "" {
			fmt.Fprintf(&b, " (%s)", r.mime)
		}
		if r.desc != "" {
			fmt.Fprintf(&b, ": %s", firstLine(r.desc))
		}
		b.WriteString("\n")
		shown++
	}
	return b.String(), nil
}

// execReadResource 实现 mcp_read_resource。
func (a *app) execReadResource(ctx context.Context, payload map[string]any) (string, error) {
	uri := strings.TrimSpace(stringField(payload, "uri"))
	if uri == "" {
		return "", fmt.Errorf("uri is required")
	}
	want := strings.TrimSpace(stringField(payload, "server"))
	servers, err := a.pickServers(want, "resources")
	if err != nil {
		return "", err
	}

	// 没点名服务器就按 URI 找：唯一命中才读，多个命中列出来让人挑——猜错就是读错内容。
	if want == "" {
		var hits []string
		a.mu.Lock()
		for _, server := range servers {
			for _, r := range a.clients[server].client.Resources() {
				if r.URI == uri {
					hits = append(hits, server)
					break
				}
			}
		}
		a.mu.Unlock()
		switch len(hits) {
		case 0:
			return "", fmt.Errorf("no connected server exposes resource %q (call mcp_list_resources for exact URIs)", uri)
		case 1:
			want = hits[0]
		default:
			return "", fmt.Errorf("resource %q exists on several servers (%s); pass server= to pick one", uri, strings.Join(hits, ", "))
		}
	}

	a.mu.Lock()
	conn, ok := a.clients[want]
	a.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("MCP server %q is not connected", want)
	}
	return conn.client.ReadResource(ctx, uri)
}

// execListPrompts 实现 mcp_list_prompts。
func (a *app) execListPrompts(payload map[string]any) (string, error) {
	want, _ := payload["server"].(string)
	limit := listDefault
	if v, ok := payload["limit"].(float64); ok && int(v) > 0 {
		limit = int(v)
	}
	if limit > listMax {
		limit = listMax
	}

	servers, err := a.pickServers(want, "prompts")
	if err != nil {
		return "", err
	}
	if len(servers) == 0 {
		return "No connected MCP server exposes prompts.", nil
	}

	type row struct {
		server, name, desc string
		args               []string
	}
	var rows []row
	a.mu.Lock()
	for _, server := range servers {
		for _, p := range a.clients[server].client.Prompts() {
			args := make([]string, 0, len(p.Arguments))
			for _, arg := range p.Arguments {
				label := arg.Name
				if arg.Required {
					label += "*"
				}
				args = append(args, label)
			}
			rows = append(rows, row{server: server, name: p.Name, desc: p.Description, args: args})
		}
	}
	a.mu.Unlock()

	if len(rows) == 0 {
		return fmt.Sprintf("Connected server(s) %s declare prompts but list none.", strings.Join(servers, ", ")), nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d prompt(s) from %d server(s); showing up to %d. Render one with mcp_get_prompt(name=...).\n",
		len(rows), len(servers), limit)
	fmt.Fprintf(&b, "Arguments marked * are required.\n")
	shown := 0
	for _, r := range rows {
		if shown >= limit {
			fmt.Fprintf(&b, "… %d more (raise limit or pass server=)\n", len(rows)-shown)
			break
		}
		fmt.Fprintf(&b, "  [%s] %s", r.server, r.name)
		if len(r.args) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(r.args, ", "))
		}
		if r.desc != "" {
			fmt.Fprintf(&b, " — %s", firstLine(r.desc))
		}
		b.WriteString("\n")
		shown++
	}
	return b.String(), nil
}

// execGetPrompt 实现 mcp_get_prompt。
func (a *app) execGetPrompt(ctx context.Context, payload map[string]any) (string, error) {
	name := strings.TrimSpace(stringField(payload, "name"))
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	want := strings.TrimSpace(stringField(payload, "server"))
	servers, err := a.pickServers(want, "prompts")
	if err != nil {
		return "", err
	}

	if want == "" {
		var hits []string
		a.mu.Lock()
		for _, server := range servers {
			for _, p := range a.clients[server].client.Prompts() {
				if p.Name == name {
					hits = append(hits, server)
					break
				}
			}
		}
		a.mu.Unlock()
		switch len(hits) {
		case 0:
			return "", fmt.Errorf("no connected server exposes prompt %q (call mcp_list_prompts)", name)
		case 1:
			want = hits[0]
		default:
			return "", fmt.Errorf("prompt %q exists on several servers (%s); pass server= to pick one", name, strings.Join(hits, ", "))
		}
	}

	args := map[string]string{}
	if raw, ok := payload["arguments"].(map[string]any); ok {
		for k, v := range raw {
			if s, ok := v.(string); ok {
				args[k] = s
			} else {
				args[k] = fmt.Sprint(v)
			}
		}
	}
	a.mu.Lock()
	conn, ok := a.clients[want]
	a.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("MCP server %q is not connected", want)
	}
	return conn.client.GetPrompt(ctx, name, args)
}

// stringField 取字符串入参（缺失/类型不符一律空串）。
func stringField(payload map[string]any, key string) string {
	v, _ := payload[key].(string)
	return v
}

// resolveTools 解析工具名：完整公开名优先，其次唯一的裸名（模型常把名字记短）。
func (a *app) resolveTools(want []string) (resolved []string, unknown []string, ambiguous map[string][]string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	ambiguous = map[string][]string{}
	all := make([]string, 0, len(a.loaded))
	for server, conn := range a.clients {
		for _, t := range conn.client.Tools() {
			all = append(all, publicName(server, t.Name))
		}
	}
	for _, name := range want {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		exact := false
		for _, full := range all {
			if full == name {
				resolved = append(resolved, full)
				exact = true
				break
			}
		}
		if exact {
			continue
		}
		var matches []string
		for _, full := range all {
			if strings.HasSuffix(full, "__"+name) || strings.HasSuffix(full, "__"+normalizeName(name)) {
				matches = append(matches, full)
			}
		}
		switch len(matches) {
		case 0:
			unknown = append(unknown, name)
		case 1:
			resolved = append(resolved, matches[0])
		default:
			ambiguous[name] = matches
		}
	}
	return resolved, unknown, ambiguous
}

// execLoad 实现 mcp_load：把工具放进模型目录（只改曝光，不重新连接）。
func (a *app) execLoad(payload map[string]any) (string, error) {
	if !a.isEnabled() {
		return "", fmt.Errorf("MCP is off: run /mcp on first")
	}
	want, err := stringSlice(payload["tools"])
	if err != nil {
		return "", err
	}
	// server= 批量加载：先列出该服务器的全部工具。
	if server, _ := payload["server"].(string); server != "" {
		a.mu.Lock()
		conn, ok := a.clients[server]
		if !ok {
			a.mu.Unlock()
			return "", fmt.Errorf("MCP server %q is not connected", server)
		}
		for _, t := range conn.client.Tools() {
			want = append(want, publicName(server, t.Name))
		}
		a.mu.Unlock()
	}
	if len(want) == 0 {
		return "", fmt.Errorf("need tool names, or server=<name>")
	}

	resolved, unknown, ambiguous := a.resolveTools(want)
	if len(ambiguous) > 0 {
		var b strings.Builder
		b.WriteString("ambiguous tool names (use the full name):\n")
		for name, matches := range ambiguous {
			fmt.Fprintf(&b, "  %s -> %s\n", name, strings.Join(matches, ", "))
		}
		return "", fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
	}
	if len(unknown) > 0 {
		return "", fmt.Errorf("unknown tool(s): %s (call mcp_list for exact names)", strings.Join(unknown, ", "))
	}

	a.mu.Lock()
	for _, name := range resolved {
		a.loaded[name] = true
	}
	a.mu.Unlock()
	return fmt.Sprintf("Loaded %d tool(s) into the tool list: %s. They take effect on the next step.",
		len(resolved), strings.Join(resolved, ", ")), nil
}

// execUnload 实现 mcp_unload：移出目录（连接保留）。
func (a *app) execUnload(payload map[string]any) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if server, _ := payload["server"].(string); server != "" {
		removed := 0
		for name := range a.loaded {
			if serverOf(name) == server {
				delete(a.loaded, name)
				removed++
			}
		}
		return fmt.Sprintf("Unloaded %d tool(s) from server %q (connection kept).", removed, server), nil
	}
	want, err := stringSlice(payload["tools"])
	if err != nil {
		return "", err
	}
	if len(want) == 0 {
		n := len(a.loaded)
		a.loaded = map[string]bool{}
		return fmt.Sprintf("Unloaded all %d tool(s) (connections kept).", n), nil
	}

	a.mu.Unlock()
	resolved, _, _ := a.resolveTools(want)
	a.mu.Lock()
	removed := 0
	for _, name := range resolved {
		if a.loaded[name] {
			delete(a.loaded, name)
			removed++
		}
	}
	return fmt.Sprintf("Unloaded %d tool(s).", removed), nil
}

// execDisconnect 实现 mcp_disconnect。
func (a *app) execDisconnect(payload map[string]any) (string, error) {
	name, _ := payload["server_name"].(string)
	n, err := a.disconnect(strings.TrimSpace(name))
	if err != nil {
		return "", err
	}
	if name == "" {
		return fmt.Sprintf("Disconnected all %d MCP server(s); their tools are unregistered.", n), nil
	}
	return fmt.Sprintf("Disconnected MCP server %q; its tools are unregistered.", name), nil
}

// statusText /mcp 与 status 的文本（人看的，不发模型）。
func (a *app) statusText() string {
	a.mu.Lock()
	defer a.mu.Unlock()

	var b strings.Builder
	if a.enabled {
		b.WriteString("注入：已开启（已加载的 MCP 工具会随请求发给模型）\n")
	} else {
		b.WriteString("注入：已关闭（模型看不到任何 MCP 工具，目录成本为 0）；/mcp on 开启\n")
	}
	if len(a.clients) == 0 {
		b.WriteString("服务器：未连接（/mcp connect <name> <endpoint|command>）")
		if len(a.loaded) > 0 {
			fmt.Fprintf(&b, "\n已加载：%d 个（服务器已断开，加载记录仍在）", len(a.loaded))
		}
		return b.String()
	}
	b.WriteString("服务器：\n")
	names := make([]string, 0, len(a.clients))
	for name := range a.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		conn := a.clients[name]
		fmt.Fprintf(&b, "  %s  [%s] %s  %d 个工具", name, conn.client.Kind(), describe(conn), conn.client.ToolCount())
		// 资源/提示词只在服务器真的有的时候才提，免得每行都挂两个 0。
		if n := conn.client.ResourceCount(); n > 0 {
			fmt.Fprintf(&b, "、%d 个资源", n)
		}
		if n := conn.client.PromptCount(); n > 0 {
			fmt.Fprintf(&b, "、%d 个提示词", n)
		}
		b.WriteString("\n")
	}
	if len(a.loaded) > 0 {
		fmt.Fprintf(&b, "已加载：%d 个（每个请求都带，约 %d tokens）", len(a.loaded), a.loadedCostLocked())
	} else {
		b.WriteString("已加载：0 个（MCP 工具默认不进目录，用 /mcp load 或 mcp_load）")
	}
	return b.String()
}

func describe(conn *serverConn) string {
	if conn.client.Kind() == "stdio" {
		return "stdio: " + strings.Join(append([]string{conn.spec.Command}, conn.spec.Args...), " ")
	}
	return conn.spec.Endpoint
}

// loadedCostLocked 已加载工具的目录成本估算（须持锁）。
//
// 用 SDK 的统一估算口径（dsc.EstimateTextTokens），不自己写 len/4：口径分叉过一次，
// 界面上的数字与压缩判定对不上，排查了很久。
func (a *app) loadedCostLocked() int {
	total := 0
	for server, conn := range a.clients {
		for _, t := range conn.client.Tools() {
			if !a.loaded[publicName(server, t.Name)] {
				continue
			}
			total += dsc.EstimateTextTokens(t.Name) + dsc.EstimateTextTokens(t.Description) +
				dsc.EstimateTextTokens(string(t.Schema))
		}
	}
	return total
}

// firstLine 描述的首行（列表里只给一句）。
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) > 110 {
		return string(r[:110]) + "…"
	}
	return string(r)
}

// stringSlice 把 JSON 数组（[]any）或内部调用传的 []string 统一成 []string。
func stringSlice(v any) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	if ss, ok := v.([]string); ok {
		return ss, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("tools must be an array of strings")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("tools must be an array of strings")
		}
		out = append(out, s)
	}
	return out, nil
}
