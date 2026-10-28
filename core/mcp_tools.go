package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"dsc/proto"
)

// 宿主内置的 MCP 管理工具族：把「连接一个 MCP 服务器」「看清它有哪些工具」
// 「把需要的工具放进模型目录」「用完撤出」做成模型可调用的工具，与 cron /
// 插件管理同族（都是宿主执行、模型只能经工具请求）。
//
// 为什么必须在宿主：**模型能看到哪些工具**由 AllToolsProto() 一处裁定，而它供
// 三类消费者使用——主 agent 的 tools 数组、子代理的 tools 数组、run_code 描述里的
// SDK 清单。把曝光集放在任何一侧的外围（比如 agent 插件里过滤）都只能照顾到其中
// 一路：子代理照样收到整份目录（实测一个服务器 229 个工具），run_code 的清单也
// 还是全量。连接表 mcpClients、工具注册表都在 Manager，曝光集跟着同一粒度才自洽。
//
// 为什么要有曝光集（而不是连上就全给）：MCP 服务器的工具数是「一装就几百」的量级，
// 实测 229 个工具 ≈ 6.97 万 tokens，而工具目录**每个请求都发**——1M 窗口占 7%，
// 128K 的本地模型占 54%，光目录就吃掉半个窗口。按 server 配白名单能降下来，但那
// 把「哪些工具该出现」固化成用户输入：改配置要重启、换个任务又对不上、条目越堆越乱。
// 这里改成「默认全不暴露，模型按需 load、用完 unload」，判断留在运行期。
//
// 代价说清楚：每次 load/unload 都会改变工具目录，而目录属于提示缓存前缀，改一次
// 就让这一跳的前缀失效。所以加载/卸载要**按需成批**，不是每步一抖；加载后一直留在
// 目录里，直到模型自己卸载、断开服务器或重启进程。
const (
	mcpConnectToolName    = "mcp_connect"
	mcpDisconnectToolName = "mcp_disconnect"
	mcpListToolName       = "mcp_list"
	mcpLoadToolName       = "mcp_load"
	mcpUnloadToolName     = "mcp_unload"

	// mcpToolNamePrefix 是 MCP 工具名的固定前缀：mcp__<server>__<raw>。
	// 宿主内置的管理工具用单下划线，不会被当成 MCP 工具。
	mcpToolNamePrefix = "mcp__"

	mcpConnectTimeout = 60 * time.Second

	mcpListDefaultLimit = 40
	mcpListMaxLimit     = 200
	mcpListDescRunes    = 110
)

// isMCPToolName 判断工具名是否来自 MCP 服务器。
func isMCPToolName(name string) bool {
	return strings.HasPrefix(name, mcpToolNamePrefix)
}

// mcpServerOf 从公开工具名里取出服务器名（mcp__<server>__<raw>）。
func mcpServerOf(publicName string) string {
	rest := strings.TrimPrefix(publicName, mcpToolNamePrefix)
	if i := strings.Index(rest, "__"); i > 0 {
		return rest[:i]
	}
	return rest
}

// ---- 曝光集 ----

// mcpExposure 已经加载（对模型可见）的 MCP 工具集合。
//
// 用自己的锁而不是 m.mu：AllToolsProto 会在不少路径上被调用（其中一些调用方可能
// 已经持有别的锁），共用一个锁容易埋下锁序反了的死锁。
type mcpExposure struct {
	mu   sync.RWMutex
	load map[string]bool
	// enabled 是 MCP 的**注入开关**，默认关。
	//
	// 为什么要有它：日常会话里绝大多数时间用不到 MCP，但管理工具的定义本身要占几百
	// tokens、每个请求都发。默认关掉即「零成本」，要用时由用户（/mcp on）或模型自己
	// （调 mcp_connect）打开——两者都是明确的意图表达，不是猜测。
	//
	// 关闭时连已加载的 MCP 工具一起不注入：off 的语义是「这个话题现在不占我任何目录
	// 预算」，而不是「只藏管理入口、留下的照付」。加载记录保留，重新 on 即恢复。
	enabled bool
}

func (m *Manager) ensureMCPExposure() *mcpExposure {
	m.mcpExposeOnce.Do(func() {
		m.mcpExpose = &mcpExposure{load: map[string]bool{}}
	})
	return m.mcpExpose
}

// mcpToolLoaded 报告某个 MCP 工具是否对模型可见。
func (m *Manager) mcpToolLoaded(name string) bool {
	e := m.ensureMCPExposure()
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.load[name]
}

// setMCPToolsLoaded 批量设置曝光状态，返回实际发生变化的名字。
func (m *Manager) setMCPToolsLoaded(names []string, loaded bool) []string {
	e := m.ensureMCPExposure()
	e.mu.Lock()
	defer e.mu.Unlock()
	var changed []string
	for _, name := range names {
		if loaded {
			if !e.load[name] {
				e.load[name] = true
				changed = append(changed, name)
			}
			continue
		}
		if e.load[name] {
			delete(e.load, name)
			changed = append(changed, name)
		}
	}
	return changed
}

// loadedMCPTools 返回当前全部已加载的工具名（排序，供成本核算与报告）。
func (m *Manager) loadedMCPTools() []string {
	e := m.ensureMCPExposure()
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]string, 0, len(e.load))
	for name := range e.load {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// forgetMCPTools 在服务器断开时清掉它的曝光记录：否则重新连上同名服务器时，同一批
// 工具会「凭空」直接可见，而模型上一轮明明卸载过。
func (m *Manager) forgetMCPTools(serverName string) {
	e := m.ensureMCPExposure()
	e.mu.Lock()
	defer e.mu.Unlock()
	for name := range e.load {
		if mcpServerOf(name) == serverName {
			delete(e.load, name)
		}
	}
}

// filterMCPCatalog 按开关与曝光集裁剪模型可见目录（AllToolsProto 的唯一出口）。
//
// 三层规则，从外到内：
//  1. 开关关闭（默认）：除 mcp_connect 外，所有 MCP 相关工具都不出现——包括已加载
//     的 mcp__* 与全部管理工具。日常会话因此付 0 tokens。加载记录留着，重新打开即恢复。
//  2. 开关打开但一台服务器都没连：三个运行期管理工具也不出现（它们一个也用不上，
//     却要占几百 tokens、每个请求都发）。
//  3. 都有：mcp__* 仅当被 mcp_load 加载过才出现（见 mcp_load/mcp_unload）。
//
// mcp_connect 始终可见（关闭时也留）：它是把「没有 MCP」变成「有」的唯一入口，而且
// 它的描述里写着「不要起另一个 dsc 进程去连 MCP」——那句话本身就是防错说明。
// 执行它会把开关打开，否则模型刚连上却拿不到 mcp_list，只能干瞪眼。
func (m *Manager) filterMCPCatalog(tools []*proto.Tool) []*proto.Tool {
	hasMCP := false
	for _, t := range tools {
		if isMCPToolName(t.GetName()) {
			hasMCP = true
			break
		}
	}
	e := m.ensureMCPExposure()
	e.mu.RLock()
	enabled := e.enabled
	loaded := make(map[string]bool, len(e.load))
	for name := range e.load {
		loaded[name] = true
	}
	e.mu.RUnlock()

	out := make([]*proto.Tool, 0, len(tools))
	for _, t := range tools {
		name := t.GetName()
		switch {
		case name == mcpConnectToolName:
			// 常驻：见函数注释
		case isMCPToolName(name):
			if !enabled || !loaded[name] {
				continue
			}
		case isMCPManagementTool(name):
			if !enabled || !hasMCP {
				continue
			}
		}
		out = append(out, t)
	}
	return out
}

// isMCPManagementTool 判断是否为运行期管理工具（开关关闭或没有服务器时都不值得占目录）。
// mcp_connect 不在其中：它是入口，始终保留。
func isMCPManagementTool(name string) bool {
	switch name {
	case mcpListToolName, mcpLoadToolName, mcpUnloadToolName, mcpDisconnectToolName:
		return true
	}
	return false
}

// MCPToolsEnabled 报告 MCP 注入开关的状态。
func (m *Manager) MCPToolsEnabled() bool {
	e := m.ensureMCPExposure()
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.enabled
}

// SetMCPToolsEnabled 打开/关闭 MCP 注入，返回是否发生了变化。
//
// 关闭只影响「注入」，不动连接与加载记录：服务器还连着、工具还加载着，只是这一个
// 进程不再把它们发给模型。重新打开即原样恢复——这让 /mcp off 成为一个可以随手按的
// 「先别花这个钱」开关，而不是一个会丢状态的破坏性操作。
func (m *Manager) SetMCPToolsEnabled(enabled bool) bool {
	e := m.ensureMCPExposure()
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.enabled == enabled {
		return false
	}
	e.enabled = enabled
	return true
}

// MCPStatus 是 MCP 当前状态的只读快照（供斜杆命令展示）。
type MCPStatus struct {
	Enabled    bool
	Servers    []MCPServerStatus
	Loaded     []string
	LoadedCost int
}

// MCPServerStatus 一台已连接服务器的概要。
type MCPServerStatus struct {
	Name     string
	Endpoint string
	Tools    int
	Loaded   int
}

// MCPStatus 取当前状态快照。
func (m *Manager) MCPStatus() MCPStatus {
	status := MCPStatus{Enabled: m.MCPToolsEnabled()}
	loaded := map[string]bool{}
	for _, name := range m.loadedMCPTools() {
		loaded[name] = true
	}
	for _, view := range m.mcpServers() {
		srv := MCPServerStatus{Name: view.name, Endpoint: view.endpoint, Tools: len(view.tools)}
		for _, tool := range view.tools {
			if loaded[tool.publicName] {
				srv.Loaded++
				status.Loaded = append(status.Loaded, tool.publicName)
			}
		}
		status.Servers = append(status.Servers, srv)
	}
	sort.Strings(status.Loaded)
	status.LoadedCost = m.loadedMCPCost()
	return status
}

// ---- 服务器快照 ----

// mcpServerView 一台已连接服务器的只读视图（工具指针在发现后不再变化，可直接共享）。
type mcpServerView struct {
	name     string
	endpoint string
	tools    []*MCPTool
}

// mcpServers 返回已连接服务器的快照（按名字排序，输出稳定）。
func (m *Manager) mcpServers() []mcpServerView {
	m.mu.Lock()
	m.ensureMCPClients()
	views := make([]mcpServerView, 0, len(m.mcpClients))
	for name, client := range m.mcpClients {
		views = append(views, mcpServerView{name: name, endpoint: client.endpoint, tools: client.Tools()})
	}
	m.mu.Unlock()
	sort.Slice(views, func(i, j int) bool { return views[i].name < views[j].name })
	return views
}

// findMCPTool 在已连接服务器里按名字找一个工具。
//
// full 是完整公开名（mcp__<server>__<raw>）；也接受唯一的裸工具名，因为模型常把
// 名字记短。返回匹配集合，由调用方处理「多个候选」。
func (m *Manager) findMCPTools(want string) (exact, suffix []*MCPTool) {
	want = strings.TrimSpace(want)
	if want == "" {
		return nil, nil
	}
	for _, view := range m.mcpServers() {
		for _, t := range view.tools {
			switch {
			case t.publicName == want:
				exact = append(exact, t)
			case strings.HasSuffix(t.publicName, "__"+want):
				suffix = append(suffix, t)
			}
		}
	}
	return exact, suffix
}

// mcpToolCost 一个 MCP 工具在工具目录里的估算开销。
func mcpToolCost(t *MCPTool) int {
	return EstimateToolTokens([]Tool{{
		Name:           t.publicName,
		Description:    t.Description,
		ParametersJSON: string(t.Schema),
	}})
}

// ============================================================
// mcp_connect
// ============================================================

type mcpConnectTool struct{ m *Manager }

func (t *mcpConnectTool) Name() string { return mcpConnectToolName }

func (t *mcpConnectTool) Description() string {
	return "Connect to an external MCP server over streamable HTTP and register its tools (endpoint like http://127.0.0.1:8765/mcp). " +
		"Use when the user asks to use an MCP server that is not connected yet. This is a host capability: do NOT start another `dsc` process to connect to MCP — that process has its own tool table and this session can never see it. " +
		"stdio transport is not supported; start the server with an HTTP endpoint first (e.g. `rea mcp --http --port 8765`). " +
		"Connecting turns MCP management on for this session (mcp_list/mcp_load become usable) — connecting means you intend to use it. " +
		"Registered tools stay hidden from your tool list to keep it small — use mcp_list to see them, then mcp_load for the ones you need, and mcp_unload/mcp_disconnect when done. Requires approval."
}

func (t *mcpConnectTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"server_name":{"type":"string","description":"Short name for this server, [A-Za-z0-9_-] only, max 32 chars. Becomes the tool name prefix: mcp__<server_name>__<tool>."},
"endpoint":{"type":"string","description":"MCP streamable-HTTP URL, e.g. http://127.0.0.1:8765/mcp."}
},
"required":["server_name","endpoint"],"additionalProperties":false}`)
}

// ApprovalReason 实现 ApprovalRequester：连接外部服务器等于把该服务器提供的任意
// 工具接进当前会话（执行权限等同本机用户），属能力扩张，须经用户审批。
func (t *mcpConnectTool) ApprovalReason(argumentsJSON string) string {
	var in struct {
		ServerName string `json:"server_name"`
		Endpoint   string `json:"endpoint"`
	}
	_ = json.Unmarshal([]byte(argumentsJSON), &in)
	if in.Endpoint == "" {
		return "Connects an external MCP server and makes its tools available to this session."
	}
	return fmt.Sprintf("Connects MCP server %q at %s; its tools will run with your local privileges once loaded.",
		in.ServerName, in.Endpoint)
}

func (t *mcpConnectTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ServerName string `json:"server_name"`
		Endpoint   string `json:"endpoint"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	name := strings.TrimSpace(in.ServerName)
	endpoint := strings.TrimSpace(in.Endpoint)
	if name == "" || endpoint == "" {
		return "", fmt.Errorf("server_name and endpoint are required")
	}
	if !isMCPServerNameValid(name) {
		return "", fmt.Errorf("invalid server_name %q: use [A-Za-z0-9_-] only, max 32 chars", name)
	}
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		return "", fmt.Errorf("endpoint must be an http(s) URL (streamable HTTP), got %q", endpoint)
	}

	// 幂等：重复连接是模型常见动作（上一轮连过、或它不确定），报「已连接」并指向
	// mcp_list 比抛冲突好用。
	if existing := t.m.GetMCPClient(name); existing != nil {
		t.m.SetMCPToolsEnabled(true)
		return fmt.Sprintf("MCP server %q is already connected (%d tools). Its tools are hidden from your tool list by default: "+
			"call mcp_list to see the names, then mcp_load for the ones you need.", name, existing.ToolCount()), nil
	}

	cctx, cancel := context.WithTimeout(ctx, mcpConnectTimeout)
	defer cancel()
	count, err := t.m.ConnectMCPServer(cctx, name, endpoint)
	if err != nil {
		return "", err
	}
	// 连上即视为「要用」：不打开开关的话，模型刚连上却拿不到 mcp_list（管理工具还没
	// 注入），只能干瞪眼，或者去重复 connect。
	t.m.SetMCPToolsEnabled(true)
	return fmt.Sprintf("Connected MCP server %q: %d tools registered (prefix mcp__%s__); MCP tools are now enabled for this session. "+
		"Registered is not visible: call mcp_list to see the names and mcp_load for the ones you need.", name, count, name), nil
}

// ============================================================
// mcp_list
// ============================================================

type mcpListTool struct{ m *Manager }

func (t *mcpListTool) Name() string { return mcpListToolName }

func (t *mcpListTool) Description() string {
	return "List the tools that connected MCP servers provide. MCP tools are hidden from your tool list by default (a server can expose hundreds, and the tool list is sent with every request), " +
		"so use this to find what is available: one line per tool, name + short description, no parameter schemas. " +
		"[loaded] marks the ones already in your tool list. mcp_load then puts a tool (with its schema) into the tool list. " +
		"Narrow with query on large servers."
}

func (t *mcpListTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"server":{"type":"string","description":"Only list tools of this MCP server."},
"query":{"type":"string","description":"Only list tools whose name or description contains these words (all must match)."},
"limit":{"type":"integer","description":"Maximum entries to return (default 40, max 200)."},
"offset":{"type":"integer","description":"Skip this many entries (paging on very large servers)."}
},"additionalProperties":false}`)
}

func (t *mcpListTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Server string `json:"server"`
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
	}
	if s := strings.TrimSpace(string(args)); s != "" && s != "null" {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("mcp_list: %v", err)
		}
	}

	servers := t.m.mcpServers()
	if len(servers) == 0 {
		return "No MCP server is connected. Use mcp_connect with the server's streamable-HTTP endpoint (e.g. http://127.0.0.1:8765/mcp) to connect one.", nil
	}

	terms := strings.Fields(strings.ToLower(in.Query))
	type entry struct {
		name, desc string
		loaded     bool
	}
	var matched []entry
	total, loadedCount := 0, 0
	for _, view := range servers {
		if in.Server != "" && view.name != in.Server {
			continue
		}
		for _, tool := range view.tools {
			total++
			if t.m.mcpToolLoaded(tool.publicName) {
				loadedCount++
			}
			hay := strings.ToLower(tool.publicName + " " + tool.Description)
			ok := true
			for _, term := range terms {
				if !strings.Contains(hay, term) {
					ok = false
					break
				}
			}
			if !ok {
				continue
			}
			matched = append(matched, entry{
				name:   tool.publicName,
				desc:   tool.Description,
				loaded: t.m.mcpToolLoaded(tool.publicName),
			})
		}
	}
	if len(matched) == 0 {
		return fmt.Sprintf("No MCP tool matches %q. %s.", in.Query, mcpServerSummary(servers)), nil
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].name < matched[j].name })

	limit := in.Limit
	if limit <= 0 {
		limit = mcpListDefaultLimit
	}
	if limit > mcpListMaxLimit {
		limit = mcpListMaxLimit
	}
	start := in.Offset
	if start < 0 || start > len(matched) {
		start = 0
	}
	end := start + limit
	if end > len(matched) {
		end = len(matched)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s — %d tool(s)", mcpServerSummary(servers), total)
	if loadedCount > 0 {
		fmt.Fprintf(&b, "; %d already loaded into your tool list", loadedCount)
	} else {
		b.WriteString("; none loaded (names only here — mcp_load adds the ones you need, and every loaded tool is sent with every request)")
	}
	b.WriteString("\n")
	for _, e := range matched[start:end] {
		mark := ""
		if e.loaded {
			mark = " [loaded]"
		}
		fmt.Fprintf(&b, "  %s%s\n      %s\n", e.name, mark, mcpOneLine(e.desc, mcpListDescRunes))
	}
	if len(matched) > end || start > 0 {
		fmt.Fprintf(&b, "(%d matching, showing %d-%d — page with offset, narrow with query, or scope with server)\n",
			len(matched), start+1, end)
	}
	return b.String(), nil
}

// ============================================================
// mcp_load
// ============================================================

type mcpLoadTool struct{ m *Manager }

func (t *mcpLoadTool) Name() string { return mcpLoadToolName }

func (t *mcpLoadTool) Description() string {
	return "Load MCP tools into your tool list so you can call them. Call mcp_list first to get exact names. " +
		"Load only what this task needs: every loaded tool is sent with every subsequent request, so loading a whole large server can cost tens of thousands of tokens per step. " +
		"Loaded tools stay until you mcp_unload them, their server disconnects, or the process restarts. Takes effect on your next step."
}

func (t *mcpLoadTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"tools":{"type":"array","items":{"type":"string"},"description":"Tool names from mcp_list (e.g. \"mcp__rea__list_jeb_units\"). A bare tool name works when it is unambiguous."},
"server":{"type":"string","description":"Load EVERY tool of this server (only for small servers; check the reported cost). Ignored when tools is given."}
},"additionalProperties":false}`)
}

func (t *mcpLoadTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Tools  []string `json:"tools"`
		Server string   `json:"server"`
	}
	if s := strings.TrimSpace(string(args)); s != "" && s != "null" {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("mcp_load: %v", err)
		}
	}

	var picked []*MCPTool
	if len(in.Tools) == 0 {
		server := strings.TrimSpace(in.Server)
		if server == "" {
			return "", fmt.Errorf("mcp_load needs tools (names from mcp_list) or server (to load every tool of one server)")
		}
		for _, view := range t.m.mcpServers() {
			if view.name == server {
				picked = append(picked, view.tools...)
			}
		}
		if len(picked) == 0 {
			return "", fmt.Errorf("no MCP tool found for server %q. %s", server, mcpServerSummary(t.m.mcpServers()))
		}
	} else {
		var problems []string
		for _, want := range in.Tools {
			tool, err := t.m.resolveMCPTool(want)
			if err != nil {
				problems = append(problems, err.Error())
				continue
			}
			picked = append(picked, tool)
		}
		if len(problems) > 0 {
			return "", fmt.Errorf("nothing loaded — %s", strings.Join(problems, "; "))
		}
	}

	names := make([]string, len(picked))
	for i, tool := range picked {
		names[i] = tool.publicName
	}
	changed := t.m.setMCPToolsLoaded(names, true)

	// 成本只算**这次真正新加进来的**：重复 load 时把已加载的也算一遍，会让模型
	// 以为目录又涨了一截，从而做多余的 unload。
	added := map[string]bool{}
	for _, name := range changed {
		added[name] = true
	}
	cost := 0
	for _, tool := range picked {
		if added[tool.publicName] {
			cost += mcpToolCost(tool)
		}
	}
	var b strings.Builder
	if len(changed) == 0 {
		fmt.Fprintf(&b, "All %d requested tool(s) were already loaded; your tool list is unchanged.\n", len(picked))
	} else {
		fmt.Fprintf(&b, "Loaded %d tool(s) — in your tool list from the next step on:\n", len(changed))
		sort.Strings(changed)
		for _, name := range changed {
			fmt.Fprintf(&b, "  %s\n", name)
		}
	}
	fmt.Fprintf(&b, "Cost: ~%d tokens per request for these, ~%d tokens for all %d loaded now. "+
		"They stay loaded until you mcp_unload them.\n", cost, t.m.loadedMCPCost(), len(t.m.loadedMCPTools()))
	return b.String(), nil
}

// resolveMCPTool 解析一个工具名；裸名在唯一时可用，多个候选时报清楚。
func (m *Manager) resolveMCPTool(want string) (*MCPTool, error) {
	exact, suffix := m.findMCPTools(want)
	switch {
	case len(exact) == 1:
		return exact[0], nil
	case len(exact) > 1:
		return nil, fmt.Errorf("%q names more than one tool; use the full name from mcp_list", want)
	case len(suffix) == 1:
		return suffix[0], nil
	case len(suffix) > 1:
		names := make([]string, len(suffix))
		for i, t := range suffix {
			names[i] = t.publicName
		}
		return nil, fmt.Errorf("%q is ambiguous (%s); use the full name from mcp_list", want, strings.Join(names, ", "))
	default:
		return nil, fmt.Errorf("%q is not a tool of any connected MCP server; mcp_list shows the available names", want)
	}
}

// loadedMCPCost 当前已加载工具的目录开销（每次请求都要发的那部分）。
func (m *Manager) loadedMCPCost() int {
	e := m.ensureMCPExposure()
	e.mu.RLock()
	names := make([]string, 0, len(e.load))
	for name := range e.load {
		names = append(names, name)
	}
	e.mu.RUnlock()
	if len(names) == 0 {
		return 0
	}
	want := map[string]bool{}
	for _, name := range names {
		want[name] = true
	}
	cost := 0
	for _, view := range m.mcpServers() {
		for _, tool := range view.tools {
			if want[tool.publicName] {
				cost += mcpToolCost(tool)
			}
		}
	}
	return cost
}

// ============================================================
// mcp_unload
// ============================================================

type mcpUnloadTool struct{ m *Manager }

func (t *mcpUnloadTool) Name() string { return mcpUnloadToolName }

func (t *mcpUnloadTool) Description() string {
	return "Remove MCP tools from your tool list when you are done with them, so they stop costing tokens on every request. " +
		"Called with no arguments it unloads everything. The server stays connected — mcp_load brings tools back without reconnecting. " +
		"Prefer unloading in the same turn you finish using them."
}

func (t *mcpUnloadTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"tools":{"type":"array","items":{"type":"string"},"description":"Tool names to unload. Omit to unload everything."},
"server":{"type":"string","description":"Unload every loaded tool of this server."}
},"additionalProperties":false}`)
}

func (t *mcpUnloadTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		Tools  []string `json:"tools"`
		Server string   `json:"server"`
	}
	if s := strings.TrimSpace(string(args)); s != "" && s != "null" {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("mcp_unload: %v", err)
		}
	}

	var target []string
	switch {
	case len(in.Tools) > 0:
		for _, want := range in.Tools {
			exact, suffix := t.m.findMCPTools(want)
			if len(exact) == 1 {
				target = append(target, exact[0].publicName)
			} else if len(exact) == 0 && len(suffix) == 1 {
				target = append(target, suffix[0].publicName)
			}
		}
	case strings.TrimSpace(in.Server) != "":
		for _, name := range t.m.loadedMCPTools() {
			if mcpServerOf(name) == in.Server {
				target = append(target, name)
			}
		}
	default: // 无参数 = 全卸载（「用完即弃」的默认动作）
		target = t.m.loadedMCPTools()
	}

	changed := t.m.setMCPToolsLoaded(target, false)
	if len(changed) == 0 {
		return "No loaded MCP tool matched; your tool list is unchanged. (mcp_list marks the loaded ones.)", nil
	}
	sort.Strings(changed)
	var b strings.Builder
	fmt.Fprintf(&b, "Unloaded %d tool(s); they stop counting toward the tool list from the next step on. "+
		"The server stays connected — mcp_load brings them back:\n", len(changed))
	for _, name := range changed {
		fmt.Fprintf(&b, "  %s\n", name)
	}
	fmt.Fprintf(&b, "Still loaded: ~%d tokens per request.\n", t.m.loadedMCPCost())
	return b.String(), nil
}

// ============================================================
// mcp_disconnect
// ============================================================

type mcpDisconnectTool struct{ m *Manager }

func (t *mcpDisconnectTool) Name() string { return mcpDisconnectToolName }

func (t *mcpDisconnectTool) Description() string {
	return "Disconnect an MCP server: unregister its tools and forget which ones were loaded. " +
		"Use it when the server is gone, was connected to the wrong endpoint, or the user is done with it. " +
		"Use mcp_unload instead when you only want the tools out of your tool list but still want the server connected. " +
		"An empty server_name disconnects every server. Requires approval."
}

func (t *mcpDisconnectTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
"server_name":{"type":"string","description":"Server to disconnect (as passed to mcp_connect). Omit or leave empty to disconnect every server."}
},"additionalProperties":false}`)
}

// ApprovalReason 实现 ApprovalRequester：断开会让正在进行的调用失败（工具随即从注册表
// 消失），属高影响变更。
func (t *mcpDisconnectTool) ApprovalReason(argumentsJSON string) string {
	var in struct {
		ServerName string `json:"server_name"`
	}
	_ = json.Unmarshal([]byte(argumentsJSON), &in)
	if strings.TrimSpace(in.ServerName) == "" {
		return "Disconnects every MCP server and unregisters all their tools."
	}
	return fmt.Sprintf("Disconnects MCP server %q and unregisters its tools.", in.ServerName)
}

func (t *mcpDisconnectTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	var in struct {
		ServerName string `json:"server_name"`
	}
	if s := strings.TrimSpace(string(args)); s != "" && s != "null" {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("mcp_disconnect: %v", err)
		}
	}
	name := strings.TrimSpace(in.ServerName)
	if name == "" {
		servers := t.m.mcpServers()
		if len(servers) == 0 {
			return "No MCP server is connected; nothing to disconnect.", nil
		}
		var removed int
		var names []string
		for _, view := range servers {
			n, err := t.m.DisconnectMCPServer(view.name)
			if err != nil {
				return "", err
			}
			removed += n
			names = append(names, view.name)
		}
		return fmt.Sprintf("Disconnected %d server(s) (%s); %d tool(s) unregistered. mcp_connect reconnects them.",
			len(names), strings.Join(names, ", "), removed), nil
	}

	if t.m.GetMCPClient(name) == nil {
		return "", fmt.Errorf("MCP server %q is not connected. Connected: %s", name, mcpServerSummary(t.m.mcpServers()))
	}
	removed, err := t.m.DisconnectMCPServer(name)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Disconnected MCP server %q; %d tool(s) unregistered and their loaded state forgotten. "+
		"Other servers are unaffected.", name, removed), nil
}

// ---- 展示辅助 ----

// mcpServerSummary 生成「服务器名 + 工具数」的一行摘要，如 "rea (229 tools), jeb (12 tools)"。
func mcpServerSummary(servers []mcpServerView) string {
	if len(servers) == 0 {
		return "no MCP server connected"
	}
	parts := make([]string, len(servers))
	for i, view := range servers {
		parts[i] = fmt.Sprintf("%s (%d tools)", view.name, len(view.tools))
	}
	if len(parts) == 1 {
		return "MCP server " + parts[0]
	}
	return "MCP servers " + strings.Join(parts, ", ")
}

// mcpOneLine 把工具说明压成一行并截断——列表里每条占一行，模型扫名字比读长描述快；
// 真需要细节时会 mcp_load 拿到完整 schema。
func mcpOneLine(s string, maxRunes int) string {
	flat := strings.Join(strings.Fields(s), " ")
	if flat == "" {
		return "(no description)"
	}
	r := []rune(flat)
	if len(r) <= maxRunes {
		return flat
	}
	return string(r[:maxRunes]) + "..."
}
