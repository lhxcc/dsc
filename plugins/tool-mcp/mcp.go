package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	dscmcp "dsc-sdk/mcp"
)

// serverSpec 一台 MCP 服务器的配置（配置文件的形状，也是 mcp_connect 的入参）。
type serverSpec struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"`
	Endpoint  string            `json:"endpoint"`
	Command   string            `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	CWD       string            `json:"cwd"`
}

// serverConn 一台已连接的服务器。
type serverConn struct {
	spec   serverSpec
	client *dscmcp.Client
}

// app 插件运行时状态。
type app struct {
	mu sync.Mutex
	// clients 已连接的服务器（按名字索引）。
	clients map[string]*serverConn
	// specs 启动时要连的服务器（来自配置）。
	specs []serverSpec
	// loaded 已加载（对模型可见）的工具：公开名集合。
	//
	// 只存内存、不落盘：与旧宿主一致——重连/重启后模型需要重新 mcp_load。落盘会让
	// 「上次加载过」变成隐式的常驻目录成本，而用户已经不记得自己付着这笔钱。
	loaded map[string]bool
	// enabled 总闸（默认关，落盘）。
	enabled bool
	// statePath 状态文件路径（enabled 持久化）。
	statePath string
}

// mcpState 落盘的状态（只存「人做过的决定」，不存运行期缓存）。
type mcpState struct {
	Enabled bool `json:"enabled"`
}

const (
	publicPrefix   = "mcp__"
	connectTimeout = 60 * time.Second
	listDefault    = 40
	listMax        = 200
)

func newApp() *app {
	a := &app{
		clients:   map[string]*serverConn{},
		loaded:    map[string]bool{},
		statePath: statePathFromEnv(),
	}
	a.specs = specsFromEnv()
	a.enabled = loadEnabled(a.statePath)
	return a
}

// statePathFromEnv 状态文件位置：DSC_MCP_STATE 优先，否则工作区根下 .dsc/mcp.state.json。
//
// 放在工作区（而不是系统临时目录）：/mcp on 是人的部署决定，换一次工具调用就丢掉的
// 位置等于没记住。
func statePathFromEnv() string {
	if p := strings.TrimSpace(os.Getenv("DSC_MCP_STATE")); p != "" {
		return p
	}
	root := strings.TrimSpace(os.Getenv("DSC_WORKSPACE_ROOT"))
	if root == "" {
		root, _ = os.Getwd()
	}
	if root == "" {
		return ""
	}
	return strings.TrimRight(strings.ReplaceAll(root, "\\", "/"), "/") + "/.dsc/mcp.state.json"
}

func loadEnabled(path string) bool {
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var st mcpState
	if json.Unmarshal(data, &st) != nil {
		return false
	}
	return st.Enabled
}

// persistEnabled 原子写状态（先写临时文件再改名）：写一半的 JSON 会在下次启动被丢弃，
// 用户的「开/关」看起来像随机失效。
func (a *app) persistEnabled() error {
	path := a.statePath
	if path == "" {
		return fmt.Errorf("no state path")
	}
	a.mu.Lock()
	enabled := a.enabled
	a.mu.Unlock()

	if dir := dirOf(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.Marshal(mcpState{Enabled: enabled})
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func dirOf(path string) string {
	if i := strings.LastIndexAny(path, "/\\"); i > 0 {
		return path[:i]
	}
	return ""
}

// specsFromEnv 读服务器配置。
//
// 两个来源，后者兼容旧部署：
//   - DSC_MCP_SERVERS：JSON 数组，插件自己的配置形状；
//   - DSC_PLUGIN_CONFIG：宿主把插件条目的 config: 块整块以 JSON 传下来（通用机制），
//     取其中的 "mcp"：支持 {servers:[...]} 与单个 {server_name|name, ...} 两种写法。
//
// 两种都不写「MCP 这个词属于宿主」——宿主只是转发配置，解释权在插件。
func specsFromEnv() []serverSpec {
	if raw := strings.TrimSpace(os.Getenv("DSC_MCP_SERVERS")); raw != "" {
		var specs []serverSpec
		if json.Unmarshal([]byte(raw), &specs) == nil {
			return specs
		}
		fmt.Fprintf(os.Stderr, "tool-mcp: DSC_MCP_SERVERS is not a JSON array of servers\n")
	}
	raw := strings.TrimSpace(os.Getenv("DSC_PLUGIN_CONFIG"))
	if raw == "" {
		return nil
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &all); err != nil {
		return nil
	}
	block, ok := all["mcp"]
	if !ok {
		return nil
	}
	var multi struct {
		Servers []serverSpec `json:"servers"`
	}
	if json.Unmarshal(block, &multi) == nil && len(multi.Servers) > 0 {
		return multi.Servers
	}
	// 旧形状：一条插件条目一个服务器（名字可能叫 server_name）。
	var single map[string]any
	if json.Unmarshal(block, &single) != nil {
		return nil
	}
	if v, ok := single["server_name"].(string); ok && single["name"] == nil {
		single["name"] = v
	}
	data, err := json.Marshal(single)
	if err != nil {
		return nil
	}
	var spec serverSpec
	if json.Unmarshal(data, &spec) != nil {
		return nil
	}
	if spec.Name == "" && spec.Endpoint == "" && spec.Command == "" {
		return nil
	}
	return []serverSpec{spec}
}

// onCatalogChange 服务器推送目录变更后的清理与留痕。
//
// 只做两件事：把已消失工具从 loaded 里摘掉（否则「已加载」会指向不存在的名字，
// /mcp status 与目录成本都跟着虚高），以及写一行日志。工具本身的增删由宿主的
// 工具同步机制（每 ~800ms 重新 ListTools）体现在模型目录里，插件不必推。
func (a *app) onCatalogChange(server string, kind dscmcp.CatalogChange) {
	a.mu.Lock()
	conn, ok := a.clients[server]
	if !ok {
		a.mu.Unlock()
		return // 已经断开：这次通知作废
	}
	alive := map[string]bool{}
	for _, t := range conn.client.Tools() {
		alive[publicName(server, t.Name)] = true
	}
	var dropped []string
	for name := range a.loaded {
		if serverOf(name) != server {
			continue
		}
		if !alive[name] {
			delete(a.loaded, name)
			dropped = append(dropped, name)
		}
	}
	tools, resources, prompts := conn.client.ToolCount(), conn.client.ResourceCount(), conn.client.PromptCount()
	a.mu.Unlock()

	fmt.Fprintf(os.Stderr, "tool-mcp: %s: server reported %s changed → tools=%d resources=%d prompts=%d\n",
		server, kind, tools, resources, prompts)
	if len(dropped) > 0 {
		sort.Strings(dropped)
		fmt.Fprintf(os.Stderr, "tool-mcp: %s: unloaded tools that no longer exist: %s\n",
			server, strings.Join(dropped, ", "))
	}
}

// connect 建立（或重建）一台服务器的连接并登记它的工具。
func (a *app) connect(spec serverSpec) error {
	spec.Name = strings.TrimSpace(spec.Name)
	if !validServerName(spec.Name) {
		return fmt.Errorf("invalid server name %q: use [A-Za-z0-9_-] only, max 32 chars", spec.Name)
	}

	var client *dscmcp.Client
	var err error
	switch strings.ToLower(strings.TrimSpace(spec.Transport)) {
	case "", "http", "streamable-http", "streamable_http":
		if strings.TrimSpace(spec.Endpoint) == "" {
			return fmt.Errorf("server %q: endpoint is required for http transport", spec.Name)
		}
		client = dscmcp.NewHTTPClient(spec.Name, strings.TrimSpace(spec.Endpoint), nil)
	case "stdio":
		if strings.TrimSpace(spec.Command) == "" {
			return fmt.Errorf("server %q: command is required for stdio transport", spec.Name)
		}
		client, err = dscmcp.NewStdioClient(spec.Name, dscmcp.StdioSpec{
			Command: strings.TrimSpace(spec.Command),
			Args:    spec.Args,
			Env:     spec.Env,
			CWD:     strings.TrimSpace(spec.CWD),
		}, nil)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("server %q: unsupported transport %q (http or stdio)", spec.Name, spec.Transport)
	}

	// 服务器说「目录变了」时：清掉已不存在的加载记录，让人在日志里看得见。
	// 不需要通知宿主——它每次取工具目录都会重新问本插件（ListTools 每 ~800ms 同步一次），
	// 插件这边缓存一更新，新工具自然出现在下一次请求里。
	client.SetCatalogChangeHandler(func(kind dscmcp.CatalogChange) {
		a.onCatalogChange(spec.Name, kind)
	})

	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		client.Disconnect() // 连不上就别留下子进程
		return err
	}

	a.mu.Lock()
	old := a.clients[spec.Name]
	a.clients[spec.Name] = &serverConn{spec: spec, client: client}
	// 重连后旧的加载记录作废：同一台服务器的工具集可能变了，留着会指向不存在的工具。
	a.forgetServerLocked(spec.Name)
	a.mu.Unlock()

	if old != nil {
		old.client.Disconnect()
	}
	return nil
}

// disconnect 断开一台服务器（不传名字则全部）。
func (a *app) disconnect(name string) (int, error) {
	a.mu.Lock()
	if name == "" {
		conns := make([]*serverConn, 0, len(a.clients))
		for _, c := range a.clients {
			conns = append(conns, c)
		}
		a.clients = map[string]*serverConn{}
		a.loaded = map[string]bool{}
		a.mu.Unlock()
		for _, c := range conns {
			c.client.Disconnect()
		}
		return len(conns), nil
	}
	conn, ok := a.clients[name]
	if !ok {
		a.mu.Unlock()
		return 0, fmt.Errorf("MCP server %q is not connected", name)
	}
	delete(a.clients, name)
	a.forgetServerLocked(name)
	a.mu.Unlock()
	conn.client.Disconnect()
	return 1, nil
}

// forgetServerLocked 清掉某台服务器的加载记录（须持锁）。
func (a *app) forgetServerLocked(server string) {
	for name := range a.loaded {
		if serverOf(name) == server {
			delete(a.loaded, name)
		}
	}
}

func (a *app) setEnabled(on bool) {
	a.mu.Lock()
	a.enabled = on
	a.mu.Unlock()
}

func (a *app) isEnabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.enabled
}

// servers 已连接服务器的名字（排序，输出稳定）。
func (a *app) servers() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.clients))
	for name := range a.clients {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// validServerName 服务器名会进工具名与日志，限制在安全字符集内。
func validServerName(name string) bool {
	if len(name) == 0 || len(name) > 32 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// publicName 公开工具名：mcp__<server>__<raw>，非安全字符替换为 _，截断到 64。
//
// 带服务器前缀是硬要求：两台服务器各有一个 search 时，裸名会互相覆盖。
func publicName(server, raw string) string {
	name := publicPrefix + server + "__" + normalizeName(raw)
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

func normalizeName(s string) string {
	b := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			b[i] = c
		} else {
			b[i] = '_'
		}
	}
	return string(b)
}

// serverOf 从公开名取服务器名。
func serverOf(public string) string {
	rest := strings.TrimPrefix(public, publicPrefix)
	if i := strings.Index(rest, "__"); i > 0 {
		return rest[:i]
	}
	return rest
}
