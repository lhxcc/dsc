package core

import (
	"os"
	"sort"
	"strings"
	"sync"
)

// 权限范围（scope）：能力可能触碰的**效果边界**。
//
// 为什么是范围而不是「这个工具要不要审批」：同一类效果会由很多工具产生（拉起进程、
// 写文件、对外发请求），按工具逐个问，用户看到的是几十条互不相干的弹窗；按范围问，
// 他看到的是「这个程序能不能在本机起进程」——一个能回答的问题，也是一次能记住的决定。
//
// 命名约定 <资源>.<动作>，只收录**宿主能真正执行或验证**的范围：
//   - process.spawn：宿主知道插件确实拉起了子进程（stdio、浏览器、computer-use）；
//   - fs.write：沙箱按路径策略强制（writeCallInfo / isWriteCapableExecutor）；
//   - net.egress：仅对经宿主转发的请求可验证，插件内部直连只能算自我声明。
//
// 无法执行的范围不要收：那只是一句「这工具危险」，与文案没有区别，却有声明式的外观。
const (
	ScopeProcessSpawn = "process.spawn"
	ScopeNetEgress    = "net.egress"
	ScopeFSWrite      = "fs.write"
)

// ApprovalGranularity 审批粒度：同一个需求的两种诉求，不是替代关系。
//
//   - 工具级（默认）：每次调用都问，理由按本次参数给。适合「一次也不许自作主张」的场景。
//   - 范围级：同一范围问过一次就记住，之后同类调用不再打断。适合长期使用、范围一旦
//     同意就稳定的场景（连过的 MCP 服务器、允许写的工作目录）。
//
// 粒度是**策略**（由人配置），范围是**声明**（由工具给）。两者分开之后，同一批工具在
// 两种粒度下都能工作，不需要工具改一行。
type ApprovalGranularity string

const (
	// GranularityPerTool 每次调用都问（默认，最严）。
	GranularityPerTool ApprovalGranularity = "tool"
	// GranularityPerScope 每个范围问一次，之后记住。
	GranularityPerScope ApprovalGranularity = "scope"
)

// ParseApprovalGranularity 解析粒度配置；未知值按工具级处理（安全的一侧）。
func ParseApprovalGranularity(s string) ApprovalGranularity {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "scope", "scopes":
		return GranularityPerScope
	default:
		return GranularityPerTool
	}
}

// ApprovalGranularity 当前生效的审批粒度（env DSC_APPROVAL_GRANULARITY，默认 tool）。
//
// 每次现读而不是缓存：这是人的决定，改了应当下一次就生效；读取成本只是一次 getenv。
func (m *Manager) ApprovalGranularity() ApprovalGranularity {
	return ParseApprovalGranularity(os.Getenv("DSC_APPROVAL_GRANULARITY"))
}

// ScopeDeclarer 可选接口：工具声明自己可能触碰的权限范围。
//
// 进程内工具与插件工具都适用（插件经 proto.Tool.scopes 带上来，RemoteTool 实现本接口）。
// 声明是静态的（这个工具可能做什么），**具体这一次**做了什么由审批理由回答——两者缺一
// 都说不清：只有静态声明时用户不知道要发生什么，只有理由时宿主不知道要不要记账。
type ScopeDeclarer interface {
	Scopes() []string
}

// scopeLedger 授权账本：用户已经同意过的范围。
//
// 只有范围级粒度会用它（工具级每次都问，不需要记账）。账本记的是「人同意过这个范围」，
// 与审批策略是两件事：策略 never 表示**不能问**（fail-closed 拒绝），账本里有没有这条
// 都不因此放行——这两个概念混在一起是安全漏洞的经典来源。
type scopeLedger struct {
	mu      sync.RWMutex
	granted map[string]bool
	// asked 记录已问过的范围（无论同意还是拒绝），用于诊断与日志：范围级下「为什么
	// 这次没问」应当能答出「上次问过并同意了」。
	asked map[string]bool
}

func (m *Manager) authorizationLedger() *scopeLedger {
	m.scopeLedgerOnce.Do(func() {
		m.scopeLedger = &scopeLedger{granted: map[string]bool{}, asked: map[string]bool{}}
	})
	return m.scopeLedger
}

// SeedScopes 预授予范围（来自配置：人先写好「这个部署允许起进程」，启动即生效）。
func (m *Manager) SeedScopes(scopes []string) {
	l := m.authorizationLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range scopes {
		if s = strings.TrimSpace(s); s != "" {
			l.granted[s] = true
		}
	}
}

// GrantScopes 记录用户同意的范围（范围级粒度在审批通过后调用）。
func (m *Manager) GrantScopes(scopes []string) {
	m.SeedScopes(scopes)
}

// GrantedScopes 返回已授权范围（排序，供状态展示与诊断）。
func (m *Manager) GrantedScopes() []string {
	l := m.authorizationLedger()
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]string, 0, len(l.granted))
	for s := range l.granted {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ScopeGranted 报告某个范围是否已授权。
func (m *Manager) ScopeGranted(scope string) bool {
	if m == nil {
		return false
	}
	l := m.authorizationLedger()
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.granted[scope]
}

// missingScopes 返回尚未授权的范围（保持声明顺序、去重）。
func (m *Manager) missingScopes(scopes []string) []string {
	seen := make(map[string]bool, len(scopes))
	var out []string
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		if !m.ScopeGranted(s) {
			out = append(out, s)
		}
	}
	return out
}

// markScopesAsked 记录「这些范围已经问过人」，仅供诊断。
func (m *Manager) markScopesAsked(scopes []string) {
	l := m.authorizationLedger()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range scopes {
		l.asked[s] = true
	}
}
