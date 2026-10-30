package core

import (
	"encoding/json"
	"fmt"
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

// writeCapableExecutors 是无法从参数路径判断写入目标、却可对文件系统任意写/执行
// 命令的解释器/执行器工具（如文件系统插件的 shell，内部为 mvdan/sh）。
// read-only 下必须整体禁用——因为无法从命令文本判定某条命令是否只读，放行即等于
// 允许「echo x > /anywhere」绕开只读沙箱（P0-1）。workspace-write 下放行（缺省档，
// 运行 shell 是其主要用途；其工作区写入边界由工具/OS 层约束）。
//
// 长期正解：由工具插件自声明「可执行的写能力」（自声明式写入语义），替换此处的
// 名称枚举。暂以此作为 read-only 缺口的最小封堵。
var writeCapableExecutors = map[string]bool{
	"shell": true,
}

// isWriteCapableExecutor 判断工具是否为不可定位写入路径的解释器/执行器。
func isWriteCapableExecutor(name string) bool {
	return writeCapableExecutors[name]
}

// sandboxCheck 判定一次工具调用是否被当前策略允许。返回允许时的 nil，或拒绝原因。
func sandboxCheck(policy SandboxPolicy, toolName, argsJSON string) error {
	if policy == SandboxFullAccess {
		return nil
	}
	path, write := writeCallInfo(toolName, argsJSON)
	switch policy {
	case SandboxReadOnly:
		// read-only：任何可写操作一律拒绝；无法定位写路径的解释器/执行器整体禁用。
		if write || isWriteCapableExecutor(toolName) {
			return fmt.Errorf("write via %s is blocked: this session is read-only", toolName)
		}
	case SandboxWorkspaceWrite:
		if write {
			if path == "" || !inWorkspace(path) {
				// 必须带上**具体路径与工作区根**：模型看到的只有这一条错误，它得靠这句话
				// 判断「是我路径写错了」还是「该申请升级」。只说「被拒」等于逼它猜——
				// 现场就是猜错后改走 shell、用相对路径写进了工作区里的同名相对目录。
				if path == "" {
					return fmt.Errorf("cannot determine the write target of %s; it is refused under workspace-write", toolName)
				}
				return fmt.Errorf("write to %q is outside the workspace root %q", path, WorkspaceRoot)
			}
		}
		// 不可定位写路径的执行器在 workspace-write 下放行（缺省档，见 writeCapableExecutors 注释）。
	}
	return nil
}

// writeCallInfo 提取工具调用的目标路径与写操作判定。
// 仅对已知写语义的工具生效：str_replace_editor 的 command != view 视为写。
// 其余工具视为非写（不做额外拦截，避免误伤读操作）。
// 参数无法解析为合法 JSON 时按「写」处理（fail-closed），避免解析失败被当成
// 非写放行而与沙箱 fail-closed 声称矛盾。
func writeCallInfo(toolName, argsJSON string) (path string, write bool) {
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
	return containsPath(realRoot, realTarget)
}

// SetSandboxPolicy 运行时切换沙箱策略（TUI /sandbox 命令用；线程安全）。
func (m *Manager) SetSandboxPolicy(p SandboxPolicy) {
	m.sandboxPolicyVal.Store(int32(p))
}

// GetSandboxPolicy 读取当前沙箱策略。
func (m *Manager) GetSandboxPolicy() SandboxPolicy {
	return SandboxPolicy(m.sandboxPolicyVal.Load())
}

// sandboxPolicy 工具流水线 pre-execute 瀑布策略：fail-closed 拦截写操作。
// 策略经 get 动态读取，支持运行时切换（TUI /sandbox 命令）。
// 审批门（approvalEscalation）已 approved 的升级调用以更宽档 EscalatedMode 复审。
// 被拒时返回对齐 DSH 的拒绝标记 + 升级提示，模型据此携 sandbox_permissions 升级重试。
func sandboxPolicy(get func() SandboxPolicy) WaterfallListener {
	return func(ctx EventContext, next func(EventContext) error) error {
		inv, _ := ctx.Data.(*ToolInvocation)
		if inv == nil {
			return next(ctx)
		}
		mode := get()
		if inv.Escalated {
			mode = inv.EscalatedMode // 本调用已升级获批：以更宽档复审
		}
		if err := sandboxCheck(mode, inv.ToolName, inv.ArgumentsJSON); err != nil {
			// 具体原因（写到哪个路径、工作区根在哪）不能丢：它决定了模型下一步是改路径
			// 还是申请升级。此前这里只输出通用标记，等于把已经算出来的信息扔掉。
			return fmt.Errorf("%s %s\n%s",
				sandboxDenialMarker(sandboxModeString(mode)), err.Error(),
				escalationHintMarker(escalationSubject(inv.ToolName), mode))
		}
		return next(ctx)
	}
}
