package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Sandbox 进程效应策略层（对齐 DSH sandbox 的 fail-closed 语义，Windows
// 兼容的工具级实现）：工具流水线 pre-execute 阶段按策略拦截文件写操作。
//
//      ReadOnly       只允许读，任何文件写操作一律拒绝
//      WorkspaceWrite 允许 workspace 内写，拒绝 workspace 外写（缺省）
//      FullAccess     整个文件系统皆可写（workspace 保护等既有约束仍生效）
//
// 策略未显式配置时缺省 WorkspaceWrite；写操作判定失败时拒绝（fail-closed）。

// SandboxPolicy 沙箱策略档位。
type SandboxPolicy int

const (
	// SandboxFullAccess 整个文件系统皆可写。
	SandboxFullAccess SandboxPolicy = iota
	// SandboxWorkspaceWrite 仅允许 workspace 内写（缺省）。
	SandboxWorkspaceWrite
	// SandboxReadOnly 拒绝一切文件写操作。
	SandboxReadOnly
)

// ParseSandboxPolicy 解析策略名：full / workspace / readonly；空或未知回退 workspace。
func ParseSandboxPolicy(s string) SandboxPolicy {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "full", "full-access":
		return SandboxFullAccess
	case "readonly", "read-only":
		return SandboxReadOnly
	default:
		return SandboxWorkspaceWrite
	}
}

// 工具写能力 token（wire 语义：随 proto.Tool.capabilities 由工具自声明，宿主只做
// 统一裁决——新增同类工具无需改宿主；对齐 DSH 按能力而非名称识别工具族的做法）：
//
//	CapabilityWritesUnknown               工具可能写任意路径且无法从参数定位（如 shell）。
//	                                      read-only 下整体拒绝；workspace-write 下放行。
//	"writes-path:<argName>"               参数 <argName> 是该次调用的写入目标路径；
//	                                      read-only 一律拒绝，workspace-write 仅放行
//	                                      workspace 内路径（路径缺失/参数无法解析按写处理，
//	                                      fail-closed）。
//	"read-only-when:<argName>=<value>"    当参数 <argName> 等于 <value> 时本次调用只读
//	                                      （如 command=view），read-only 下放行。
//	CapabilityReadOnly                    工具任何调用都不写：显式声明后本次调用恒为只读。
//	                                      与 writes-* 互斥声明时以 writes-* 为准（保守）。
//
// 一个工具可同时声明多条；空 token、缺 "="、缺参数名/值、未知格式一律忽略。工具未声明
// 任何上述 token 时回退到按工具名的 legacy 判定（见 legacyWriteCallInfo）。
const (
	// CapabilityWritesUnknown 工具可能写任意路径、无法从参数定位（如代码解释器/shell）。
	CapabilityWritesUnknown = "writes-unknown"

	// CapabilityReadOnly 工具的任何调用都不产生写入：既是沙箱放行依据，也是 agent
	// 判定「同批调用可并行」的唯一依据（未声明者一律按不可并行处理，fail-closed）。
	CapabilityReadOnly = "read-only"

	writesPathTokenPrefix   = "writes-path:"
	readOnlyWhenTokenPrefix = "read-only-when:"
)

// writeCapabilityDecl 从工具能力标签解析出的写语义声明。
type writeCapabilityDecl struct {
	writesUnknown bool              // 可能写任意路径
	readOnly      bool              // 任何调用都不写
	pathArgs      []string          // 写入目标路径所在的参数名
	readOnlyWhen  map[string]string // 参数名 -> 该值下本次调用只读
}

// parseWriteCapabilityDecl 解析写能力 token；declared 表示是否声明了任何可识别的写能力
// （未知 token 不算，以免把「声明了无关能力」误判为已迁移）。
func parseWriteCapabilityDecl(caps []string) (decl writeCapabilityDecl, declared bool) {
	for _, raw := range caps {
		token := strings.TrimSpace(raw)
		switch {
		case token == CapabilityWritesUnknown:
			decl.writesUnknown = true
			declared = true
		case token == CapabilityReadOnly:
			decl.readOnly = true
			declared = true
		case strings.HasPrefix(token, writesPathTokenPrefix):
			arg := strings.TrimSpace(strings.TrimPrefix(token, writesPathTokenPrefix))
			if arg == "" {
				continue
			}
			decl.pathArgs = append(decl.pathArgs, arg)
			declared = true
		case strings.HasPrefix(token, readOnlyWhenTokenPrefix):
			name, value, ok := strings.Cut(strings.TrimPrefix(token, readOnlyWhenTokenPrefix), "=")
			name, value = strings.TrimSpace(name), strings.TrimSpace(value)
			if !ok || name == "" || value == "" {
				continue
			}
			if decl.readOnlyWhen == nil {
				decl.readOnlyWhen = map[string]string{}
			}
			decl.readOnlyWhen[name] = value
			declared = true
		}
	}
	return decl, declared
}

// writeCallDecision 是一次调用的写入语义裁决输入。
type writeCallDecision struct {
	path         string // 可定位的写入目标路径
	write        bool   // 本次调用是写操作（路径可定位）
	unknownWrite bool   // 可能写任意路径、无法定位（如 shell）
}

// sandboxWriteResolver 按工具自声明的能力解析一次调用的写入语义。
type sandboxWriteResolver func(toolName, argsJSON string) writeCallDecision

// capabilityEnumerator 可选接口：报告工具声明的完整能力标签列表。写能力 token 携带
// 参数（writes-path:<argName>），CapabilityTool 的精确查询无法枚举，故写能力解析需要
// 本接口。仅实现 Capabilities 即可（不必同时实现 HasCapability）。
type capabilityEnumerator interface {
	Capabilities() []string
}

// declaredWriteCapabilityDecl 读取工具自声明的写能力。未实现 capabilityEnumerator 或
// 未声明任何可识别的写能力时 declared=false，调用方回退 legacy 判定。
func declaredWriteCapabilityDecl(tool ToolDefinition) (writeCapabilityDecl, bool) {
	if tool == nil {
		return writeCapabilityDecl{}, false
	}
	if l, ok := tool.(capabilityEnumerator); ok {
		return parseWriteCapabilityDecl(l.Capabilities())
	}
	// 仅实现 CapabilityTool 的工具无法枚举 token：按无参 token 精确查询兜底。
	if ct, ok := tool.(CapabilityTool); ok && ct.HasCapability(CapabilityWritesUnknown) {
		return writeCapabilityDecl{writesUnknown: true}, true
	}
	return writeCapabilityDecl{}, false
}

// resolveWriteCall 优先按工具自声明的写能力解析本次调用；未声明写能力的工具（旧插件）
// 回退 legacy 按名判定。
func resolveWriteCall(tool ToolDefinition, toolName, argsJSON string) writeCallDecision {
	decl, declared := declaredWriteCapabilityDecl(tool)
	if !declared {
		path, write := legacyWriteCallInfo(toolName, argsJSON)
		return writeCallDecision{path: path, write: write, unknownWrite: legacyWriteCapableExecutor(toolName)}
	}
	return resolveWriteCallFromDecl(decl, argsJSON)
}

// resolveWriteCallFromDecl 按已解析的能力声明裁决一次调用的写入语义。
// read-only 声明恒为只读；writes-* 声明优先于 read-only（保守）。
func resolveWriteCallFromDecl(decl writeCapabilityDecl, argsJSON string) writeCallDecision {
	if decl.writesUnknown {
		// 无法定位写路径：read-only 由 unknownWrite 整体拒绝，workspace-write 放行。
		return writeCallDecision{unknownWrite: true}
	}
	if decl.readOnly {
		return writeCallDecision{}
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		// 解析失败且声明了 writes-path：无法判定写意图，fail-closed 按写处理。
		return writeCallDecision{write: len(decl.pathArgs) > 0}
	}
	// read-only-when 先判：命中即本次调用只读，即便工具同时声明了 writes-path。
	for name, value := range decl.readOnlyWhen {
		if v, ok := args[name].(string); ok && v == value {
			return writeCallDecision{}
		}
	}
	if len(decl.pathArgs) == 0 {
		return writeCallDecision{}
	}
	for _, name := range decl.pathArgs {
		if p, ok := args[name].(string); ok {
			return writeCallDecision{path: p, write: true}
		}
	}
	// 声明了 writes-path 但参数缺失：按写处理（无路径 → workspace-write 拒绝）。
	return writeCallDecision{write: true}
}

// ToolCallReadOnly 按工具声明的能力标签判定一次调用是否只读——agent 侧并行的唯一
// 准入条件（agent 与宿主分属不同进程，只能依赖 ListTools 随 proto.Tool 下发的
// capabilities，不能访问宿主的工具注册表）。
//
// 未声明任何写能力的工具一律返回 false：能力未知即不可并行（fail-closed）。
// 与宿主沙箱裁决（resolveWriteCall）共用同一解析逻辑，避免两处口径漂移。
func ToolCallReadOnly(capabilities []string, argsJSON string) bool {
	decl, declared := parseWriteCapabilityDecl(capabilities)
	if !declared {
		return false
	}
	d := resolveWriteCallFromDecl(decl, argsJSON)
	return !d.write && !d.unknownWrite
}

// legacyWriteCapableExecutors 是无法从参数路径判断写入目标、却可对文件系统任意写/执行
// 命令的解释器/执行器工具（如文件系统插件的 shell，内部为 mvdan/sh）。
// read-only 下必须整体禁用——因为无法从命令文本判定某条命令是否只读，放行即等于
// 允许「echo x > /anywhere」绕开只读沙箱。workspace-write 下放行（缺省档，运行 shell
// 是其主要用途；其工作区写入边界由工具/OS 层约束）。
//
// 仅为未声明写能力的旧插件保留的 legacy fallback；新工具应声明 CapabilityWritesUnknown。
var legacyWriteCapableExecutors = map[string]bool{
	"shell": true,
}

// legacyWriteCapableExecutor 判断工具是否为不可定位写入路径的解释器/执行器（legacy）。
func legacyWriteCapableExecutor(name string) bool {
	return legacyWriteCapableExecutors[name]
}

// sandboxCheck 判定一次工具调用是否被当前策略允许：resolve 非 nil 时按其（工具自声明
// 能力）解析写入语义，nil 时回退 legacy 按名判定。返回允许时的 nil，或拒绝原因。
func sandboxCheck(policy SandboxPolicy, toolName, argsJSON string, resolve sandboxWriteResolver) error {
	if policy == SandboxFullAccess {
		return nil
	}
	var d writeCallDecision
	if resolve != nil {
		d = resolve(toolName, argsJSON)
	} else {
		path, write := legacyWriteCallInfo(toolName, argsJSON)
		d = writeCallDecision{path: path, write: write, unknownWrite: legacyWriteCapableExecutor(toolName)}
	}
	switch policy {
	case SandboxReadOnly:
		// read-only：任何可写操作一律拒绝；无法定位写路径的执行器整体禁用。
		if d.write || d.unknownWrite {
			return fmt.Errorf("write via %s is blocked: this session is read-only", toolName)
		}
	case SandboxWorkspaceWrite:
		if d.write && (d.path == "" || !inWorkspace(d.path)) {
			// 必须带上**具体路径与工作区根**：模型看到的只有这一条错误，它得靠这句话
			// 判断「是我路径写错了」还是「该申请升级」。只说「被拒」等于逼它猜——
			// 现场就是猜错后改走 shell、用相对路径写进了工作区里的同名相对目录。
			if d.path == "" {
				return fmt.Errorf("cannot determine the write target of %s; it is refused under workspace-write", toolName)
			}
			return fmt.Errorf("write to %q is outside the workspace root %q", d.path, WorkspaceRoot)
		}
		// 不可定位写路径的执行器在 workspace-write 下放行（缺省档，见 legacy 注释）。
	}
	return nil
}

// legacyWriteCallInfo 是旧插件（未声明写能力）的兼容回退：提取目标路径与写操作判定。
// 仅对已知写语义的工具生效：str_replace_editor 的 command != view 视为写。
// 其余工具视为非写（不做额外拦截，避免误伤读操作）。
// 参数无法解析为合法 JSON 时按「写」处理（fail-closed），避免解析失败被当成
// 非写放行而与沙箱 fail-closed 声称矛盾。
func legacyWriteCallInfo(toolName, argsJSON string) (path string, write bool) {
	switch toolName {
	case "str_replace_editor":
		var m map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
			// 无法判定写意图：fail-closed 视作写，sandboxCheck 会据此拒绝。
			return "", true
		}
		path, _ = m["path"].(string)
		cmd, _ := m["command"].(string)
		return path, cmd != "view"
	default:
		return "", false
	}
}

// canonicalWorkspaceRoot 解析 WorkspaceRoot 的真实路径：绝对化后解析符号链接
// （失败时回退绝对化结果）。Windows 下盘符/路径大小写不敏感且可能存在 8.3 短名、
// 符号链接等别名，统一解析可避免真实路径与根因大小写/别名差异被误判为越界。
func canonicalWorkspaceRoot() string {
	abs, err := PAbs(WorkspaceRoot)
	if err != nil {
		return WorkspaceRoot
	}
	if real, err := PEvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// inWorkspace 判断路径是否位于 WorkspaceRoot 之下。
//
// 路径先经 core.MapWorkspacePath 归并——沙箱与各工具插件（str_replace_editor、
// 文件系统插件的 shell AST 字面量重写）必须用同一份「虚拟根 = 工作空间根」
// 映射，故此处不得另立一套实现：/workspace（含 \workspace 形态）别名锚定工作区根、
// Windows 上 WSL 习惯路径 /mnt/<drive>/... 映射为盘符路径；前缀后必须跟分隔符或
// 结尾（/workspacefoo 之类是独立绝对路径，不认作别名）。否则沙箱会把工具实际能写
// 的路径判成越界（或反之），重演「判定与执行各说各话」的历史缺陷。
//
// 归并后：先经 CanonicalPath 解析到真实路径（Windows 上用
// GetFinalPathNameByHandle 穿透 junction/symlink，Unix 用 EvalSymlinks），
// 再做包含判定——从而避免 workspace 内指向外部的 junction/symlink 令词法前缀
// 比较误判为「在根内」而写穿沙箱（P0-3）。
//
// 相对路径一律以 WorkspaceRoot 为基解析（对齐工具插件语义：str_replace_editor 的
// safePath 与文件系统插件的 shell 均把相对路径落在工作区根下）——若按宿主进程
// cwd 解析，cwd 与工作区根不一致时会把本在工作区内的相对写误判为越界而误拒。
// 例外：Windows 上盘符无关的根路径（/xxx 或 \xxx，filepath.IsAbs 判为假）不是
// 工作区相对路径，若并入 WorkspaceRoot 会把 /etc/hosts 之类误判为工作区内而放行
// 越界写，故维持既有语义（按当前盘根解析，本就落于工作区外）。
func inWorkspace(path string) bool {
	p := MapWorkspacePath(path)
	driveLessRoot := runtime.GOOS == "windows" && !filepath.IsAbs(p) &&
		(strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`))
	if !filepath.IsAbs(p) && !driveLessRoot {
		p = PJoin(WorkspaceRoot, p)
	}
	abs, err := PAbs(p)
	if err != nil {
		return false
	}
	root, rerr := PAbs(WorkspaceRoot)
	if rerr != nil {
		root = WorkspaceRoot
	}
	realRoot, cerr := CanonicalPath(root)
	if cerr != nil {
		realRoot = root
	}
	realTarget, terr := CanonicalPath(abs)
	if terr != nil {
		return false
	}
	if containsPath(realRoot, realTarget) {
		return true
	}
	// 词法/规范路径比较的兜底：大小写不敏感文件系统（macOS 默认 APFS）上，
	// EvalSymlinks 不还原磁盘真实大小写，大小写变体路径会被误判为越界。
	// 用文件身份（dev/ino）判定「目标最深已存在祖先确实位于根之下」——
	// 与大小写、8.3 短名、目录联接等路径别名无关，且不会放行真正越界的路径。
	return withinByFileIdentity(realRoot, realTarget)
}

// withinByFileIdentity 判断 target 的最深已存在祖先是否位于 root 之下
// （按文件身份比较，而非路径字符串）。根或目标不可访问时返回 false（fail-closed）。
func withinByFileIdentity(root, target string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	cur := target
	for {
		if info, serr := os.Stat(cur); serr == nil && os.SameFile(info, rootInfo) {
			return true
		}
		parent := PDir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// SetSandboxPolicy 运行时切换沙箱策略（TUI /sandbox 命令用；线程安全）。
func (m *Manager) SetSandboxPolicy(p SandboxPolicy) {
	m.sandboxPolicyVal.Store(int32(p))
}

// GetSandboxPolicy 读取当前沙箱策略。
func (m *Manager) GetSandboxPolicy() SandboxPolicy {
	return SandboxPolicy(m.sandboxPolicyVal.Load())
}

// resolveSandboxWrite 从 ToolRegistry 取被调工具，按其自声明的写能力解析本次调用的
// 写入语义；工具未注册或未声明写能力时回退 legacy 按名判定（旧插件兼容）。
func (m *Manager) resolveSandboxWrite(toolName, argsJSON string) writeCallDecision {
	var tool ToolDefinition
	if m.toolRegistry != nil {
		tool, _ = m.toolRegistry.Get(toolName)
	}
	return resolveWriteCall(tool, toolName, argsJSON)
}

// sandboxPolicy 工具流水线 pre-execute 瀑布策略：fail-closed 拦截写操作。
// 策略经 get 动态读取，支持运行时切换（TUI /sandbox 命令）。
// 审批门（approvalEscalation）已 approved 的升级调用以更宽档 EscalatedMode 复审。
// 被拒时返回对齐 DSH 的拒绝标记 + 升级提示，模型据此携 sandbox_permissions 升级重试。
func (m *Manager) sandboxPolicy(get func() SandboxPolicy) WaterfallListener {
	return sandboxPolicyWith(get, m.resolveSandboxWrite)
}

// sandboxPolicy 是无 Manager 的包级包装（legacy 判定，不解析工具能力），供既有测试与
// 独立装配使用；宿主运行时用 m.sandboxPolicy 以按工具自声明的能力裁决。
func sandboxPolicy(get func() SandboxPolicy) WaterfallListener {
	return sandboxPolicyWith(get, nil)
}

// sandboxPolicyWith 是上述两者的共用实现。
func sandboxPolicyWith(get func() SandboxPolicy, resolve sandboxWriteResolver) WaterfallListener {
	return func(ctx EventContext, next func(EventContext) error) error {
		inv, _ := ctx.Data.(*ToolInvocation)
		if inv == nil {
			return next(ctx)
		}
		mode := get()
		if inv.Escalated {
			mode = inv.EscalatedMode // 本调用已升级获批：以更宽档复审
		}
		if err := sandboxCheck(mode, inv.ToolName, inv.ArgumentsJSON, resolve); err != nil {
			// 具体原因（写到哪个路径、工作区根在哪）不能丢：它决定了模型下一步是改路径
			// 还是申请升级。此前这里只输出通用标记，等于把已经算出来的信息扔掉。
			return fmt.Errorf("%s %s\n%s",
				sandboxDenialMarker(sandboxModeString(mode)), err.Error(),
				escalationHintMarker(escalationSubject(inv.ToolName), mode))
		}
		return next(ctx)
	}
}
