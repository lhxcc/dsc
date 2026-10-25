package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fixedPolicy 返回固定策略的读取函数（测试简化用）。
func fixedPolicy(p SandboxPolicy) func() SandboxPolicy {
	return func() SandboxPolicy { return p }
}

// isFileSystemCaseInsensitive 探测 path 所在文件系统是否大小写不敏感：
// 在 path 同目录下创建一个含大写字母的临时子目录（如 CaseProbe-NNN），再尝试以全小写
// 形式 stat 它。能 stat 到 → FS 大小写不敏感；找不到（或创建失败）→ 视为大小写敏感。
//
// 这是文件系统的真实属性，而非「路径字符串是否含大写字母」。用于跳过仅在大小写
// 不敏感 FS 上才有意义的回归测试（如 Windows / macOS 默认 FS），避免在 Linux ext4
// 等大小写敏感 FS 上误失败。返回值仅供测试跳过判定，不进入生产路径。
func isFileSystemCaseInsensitive(path string) bool {
	dir := filepath.Dir(path)
	// 在 dir 下创建临时子目录；dir 可能不存在，迭代到最深的已存在祖先
	for {
		if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
	probe, err := os.MkdirTemp(dir, "CaseProbe-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(probe)
	lower := strings.ToLower(probe)
	// 全小写形式与原路径不同（即原路径含大写字母）且能 stat 到 → FS 大小写不敏感
	if lower == probe {
		return false // 原路径不含大写字母：探测无意义
	}
	if _, err := os.Stat(lower); err == nil {
		return true
	}
	return false
}

// jsonPath 将路径转义后嵌入 JSON 字面量：Windows 的 t.TempDir() 含反斜杆，
// 直接拼进 JSON 会是非法转义序列，导致沙箱把解析失败当写拦截（fail-closed）。
func jsonPath(p string) string {
	return strings.ReplaceAll(strings.ReplaceAll(p, `\`, `\\`), `"`, `\"`)
}

func TestSandboxReadOnlyBlocksWrite(t *testing.T) {
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxReadOnly)))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	_, err := m.ExecuteTool(context.Background(), "str_replace_editor",
		json.RawMessage(`{"command":"str_replace","path":"/tmp/x.txt","old_str":"a","new_str":"b"}`))
	if err == nil || !strings.Contains(err.Error(), "file access denied under read-only mode") {
		t.Fatalf("err = %v, want read-only block", err)
	}
}

func TestSandboxReadOnlyAllowsView(t *testing.T) {
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxReadOnly)))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor",
		json.RawMessage(`{"command":"view","path":"/tmp/x.txt"}`)); err != nil {
		t.Fatalf("view should pass read-only policy: %v", err)
	}
}

func TestSandboxReadOnlyBlocksShellExecutor(t *testing.T) {
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxReadOnly)))
	_ = m.toolRegistry.Register(&mockTool{name: "shell"})

	// shell 是解释器/执行器，read-only 下即使命令看似只读也须整体禁用（无法判定是否写文件）。
	if _, err := m.ExecuteTool(context.Background(), "shell",
		json.RawMessage(`{"command":"echo hello"}`)); err == nil || !strings.Contains(err.Error(), "file access denied under read-only mode") {
		t.Fatalf("shell should be blocked under read-only: %v", err)
	}
}

func TestSandboxWorkspaceAllowsShellExecutor(t *testing.T) {
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxWorkspaceWrite)))
	_ = m.toolRegistry.Register(&mockTool{name: "shell"})

	// workspace-write（缺省档）下 shell 放行：运行 shell 是其主要用途。
	if _, err := m.ExecuteTool(context.Background(), "shell",
		json.RawMessage(`{"command":"echo hi"}`)); err != nil {
		t.Fatalf("shell should pass workspace-write policy: %v", err)
	}
}

func TestSandboxWorkspaceAllowsInside(t *testing.T) {
	orig := WorkspaceRoot
	WorkspaceRoot = t.TempDir() + "/ws"
	defer func() { WorkspaceRoot = orig }()
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxWorkspaceWrite)))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor",
		json.RawMessage(`{"command":"str_replace","path":"`+jsonPath(WorkspaceRoot)+`/a.txt","old_str":"a","new_str":"b"}`)); err != nil {
		t.Fatalf("write inside workspace should pass: %v", err)
	}
}

func TestSandboxWorkspaceBlocksOutside(t *testing.T) {
	orig := WorkspaceRoot
	WorkspaceRoot = t.TempDir() + "/ws"
	defer func() { WorkspaceRoot = orig }()
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxWorkspaceWrite)))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	_, err := m.ExecuteTool(context.Background(), "str_replace_editor",
		json.RawMessage(`{"command":"insert","path":"/etc/hosts","new_str":"x","insert_line":1}`))
	if err == nil || !strings.Contains(err.Error(), "file access denied under workspace-write mode") {
		t.Fatalf("err = %v, want workspace-write block", err)
	}
}

func TestSandboxWorkspaceAcceptsVirtualPrefix(t *testing.T) {
	orig := WorkspaceRoot
	WorkspaceRoot = t.TempDir() + "/ws"
	defer func() { WorkspaceRoot = orig }()
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxWorkspaceWrite)))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	// 模型按工具描述传入 /workspace/<rel> 虚拟根前缀，应与真实 WorkspaceRoot 等价放行
	for _, p := range []string{"/workspace/a.txt", `\workspace\b.txt`, "/workspace"} {
		if _, err := m.ExecuteTool(context.Background(), "str_replace_editor",
			json.RawMessage(`{"command":"str_replace","path":"`+jsonPath(p)+`","old_str":"a","new_str":"b"}`)); err != nil {
			t.Fatalf("write under virtual /workspace prefix %q should pass: %v", p, err)
		}
	}
	// 别名边界仍须守住：前缀后未跟分隔符（/workspacefoo、/workspacex）是独立绝对
	// 路径，不得被当作工作区根别名而放行越界写。
	for _, p := range []string{"/workspacefoo/x", "/workspacex"} {
		_, err := m.ExecuteTool(context.Background(), "str_replace_editor",
			json.RawMessage(`{"command":"str_replace","path":"`+jsonPath(p)+`","old_str":"a","new_str":"b"}`))
		if err == nil || !strings.Contains(err.Error(), "file access denied under workspace-write mode") {
			t.Fatalf("path %q 不是 /workspace 别名，应被拒: err=%v", p, err)
		}
	}
}

// TestSandboxWorkspaceAcceptsWSLFormPath 沙箱与工具侧共用同一份虚拟根映射
// （core.MapWorkspacePath），不得各存一套：Windows 上模型的 WSL 习惯路径
// /mnt/<drive>/... 与工具侧同源映射为 <drive>:/...，故指向工作区内的 WSL 形态写法
// 必须与真实路径等价放行（沙箱若把它当宿主盘根下的 /mnt/... 目录，就会把工具实际
// 能写的工作区文件判成越界）。
//
// 该映射按设计仅在 Windows 生效（非 Windows 上 /mnt/c/... 是合法 POSIX 真实路径，
// 可能是真实挂载点），故断言按 GOOS 门控。
func TestSandboxWorkspaceAcceptsWSLFormPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("WSL 形态路径映射仅 Windows 启用（非 Windows 上 /mnt/<drive> 是真实 POSIX 路径）")
	}
	orig := WorkspaceRoot
	WorkspaceRoot = t.TempDir() + "/ws"
	defer func() { WorkspaceRoot = orig }()
	_ = os.MkdirAll(WorkspaceRoot, 0o755)

	// 由工作区真实路径构造 WSL 形态：C:/a/b → /mnt/c/a/b
	root := canonicalWorkspaceRoot()
	if len(root) < 2 || root[1] != ':' {
		t.Skipf("工作区不在盘符路径下（%q），无法构造 WSL 形态", root)
	}
	wsl := "/mnt/" + strings.ToLower(root[:1]) + root[2:]
	if !inWorkspace(root + "/inside.txt") {
		t.Fatalf("前置条件失败：真实路径 %q 应判为工作区内", root+"/inside.txt")
	}
	if !inWorkspace(wsl + "/inside.txt") {
		t.Fatalf("WSL 形态路径 %q 与真实路径同源映射，应判为工作区内", wsl+"/inside.txt")
	}
}

// TestSandboxWorkspaceAllowsRealPathCaseInsensitive 回归「换成真实路径被拦截」死循环：
// Windows 文件系统大小写不敏感，模型按 pwd 或自身推断传回的盘符/目录名大小写可能与
// WorkspaceRoot 不一致（如小写盘符 d:\agents\dsc\...）。修复前 inWorkspace 的词法前缀
// 比较区分大小写，导致真实路径在 workspace 内却被误拦，模型只好退回 /workspace 虚拟
// 前缀（该前缀在 shell 中又不存在），陷入死循环，最终被迫切换 full-access。
//
// 跳过策略：通过「同一目录名大小写变体是否解析到同一 inode」探测当前 FS 是否大小写
// 不敏感——是 FS 实际属性，而非路径是否恰好全小写（旧实现用 alt == root+suffix 跳过，
// 在 Linux 大小写敏感 FS 上对带大写字母的 /tmp/TestSandboxNNN 路径会误判为「有大小写
// 差异」继续跑，结果 EvalSymlinks 找不到小写目录而误失败）。
func TestSandboxWorkspaceAllowsRealPathCaseInsensitive(t *testing.T) {
	orig := WorkspaceRoot
	WorkspaceRoot = t.TempDir() + "/ws"
	defer func() { WorkspaceRoot = orig }()
	_ = os.MkdirAll(WorkspaceRoot, 0o755)

	// canonical root（解析符号链接后）
	root := canonicalWorkspaceRoot()

	// 探测当前文件系统是否大小写不敏感：在 WorkspaceRoot 同层创建一个含大写字母的
	// 子目录，再尝试以小写形式 stat。能 stat 到 → FS 大小写不敏感；否则敏感。
	// 用 root 作为探测基点（root 必存在）。
	if !isFileSystemCaseInsensitive(root) {
		t.Skip("当前文件系统大小写敏感，跳过 case-insensitive 回归测试")
	}

	// 以与 canonical 根大小写不同的真实路径写 workspace 内文件（模拟模型回传路径）
	alt := strings.ToLower(root) + "/inside.txt"
	// JSON 中反斜杆需转义（真实请求由模型产出合法 JSON，这里等价构造）
	escaped := strings.ReplaceAll(alt, `\`, `\\`)

	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxWorkspaceWrite)))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor",
		json.RawMessage(`{"command":"str_replace","path":"`+escaped+`","old_str":"a","new_str":"b"}`)); err != nil {
		t.Fatalf("write to real path with case variant should pass on case-insensitive FS: %v", err)
	}
}

// TestSandboxWorkspaceAllowsRelativePath 回归「相对路径写被误拒」：任务给模型的相对路径
// （如 bench-out/editor_flow/reply.txt）会被工具以工作区根为基解析（对齐 str_replace_editor
// 的 safePath 与 tool-filesystem 的 shell cwd），沙箱判定亦须以 WorkspaceRoot 为基——若按
// 宿主进程 cwd 解析，cwd 与工作区根不一致时会把本在工作区内的相对写误判为越界而误拒，
// 模型被迫改用 shell 绕过（曾致 StrReplaceEditor 无法在 workspace-write 下写文件）。
func TestSandboxWorkspaceAllowsRelativePath(t *testing.T) {
	orig := WorkspaceRoot
	WorkspaceRoot = t.TempDir() + "/ws"
	defer func() { WorkspaceRoot = orig }()
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxWorkspaceWrite)))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor",
		json.RawMessage(`{"command":"str_replace","path":"bench-out/editor_flow/reply.txt","old_str":"a","new_str":"b"}`)); err != nil {
		t.Fatalf("relative-path write should be resolved against WorkspaceRoot and pass: %v", err)
	}
}

func TestSandboxFullAllowsWrite(t *testing.T) {
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxFullAccess)))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor",
		json.RawMessage(`{"command":"str_replace","path":"/tmp/x.txt","old_str":"a","new_str":"b"}`)); err != nil {
		t.Fatalf("full access should allow write: %v", err)
	}
}

func TestSandboxIgnoresNonWriteTools(t *testing.T) {
	m := newRouterManager()
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(fixedPolicy(SandboxReadOnly)))
	_ = m.toolRegistry.Register(&mockTool{name: "plain-tool"})

	if _, err := m.ExecuteTool(context.Background(), "plain-tool", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("non-write tool should not be blocked by sandbox: %v", err)
	}
}

func TestSandboxDynamicSwitch(t *testing.T) {
	// /tmp 现在由虚拟根表映射到临时目录（Windows 上没有真实 /tmp）。本用例要表达的
	// 是「工作区外的写入被拦」，所以把临时目录钉在工作区之外；否则临时目录恰好落在
	// 工作区内（测试进程 cwd 就是仓库根）时，判定为「工作区内写」反而是正确的。
	t.Setenv("DSC_TEMP_DIR", t.TempDir())

	m := newRouterManager()
	m.sandboxPolicyVal.Store(int32(SandboxWorkspaceWrite))
	m.events.OnWaterfall(EventToolPreExecute, sandboxPolicy(m.GetSandboxPolicy))
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	write := json.RawMessage(`{"command":"str_replace","path":"/tmp/x.txt","old_str":"a","new_str":"b"}`)
	// 初始 workspace：/tmp 写被拦
	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor", write); err == nil {
		t.Fatal("workspace policy should block /tmp write")
	}
	// 切到 full：放行
	m.SetSandboxPolicy(SandboxFullAccess)
	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor", write); err != nil {
		t.Fatalf("after switch to full, write should pass: %v", err)
	}
	// 切到 readonly：拦截
	m.SetSandboxPolicy(SandboxReadOnly)
	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor", write); err == nil {
		t.Fatal("after switch to read-only, write should be blocked")
	}
}

// declaredWriteTool 测试用工具：模拟插件自声明写能力（实现 CapabilityTool 与
// Capabilities，供宿主枚举带参数的写能力 token）。
type declaredWriteTool struct {
	name string
	caps []string
}

func (t *declaredWriteTool) Name() string                      { return t.name }
func (t *declaredWriteTool) Description() string               { return "declared write tool" }
func (t *declaredWriteTool) ParametersSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (t *declaredWriteTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "ok", nil
}

func (t *declaredWriteTool) HasCapability(cap string) bool {
	for _, c := range t.caps {
		if c == cap {
			return true
		}
	}
	return false
}

func (t *declaredWriteTool) Capabilities() []string { return t.caps }

// TestSandboxToolDeclaredWriteCapabilities 校验沙箱按工具自声明的写能力裁决，
// 且未声明能力的旧工具仍走 legacy 按名回退。
func TestSandboxToolDeclaredWriteCapabilities(t *testing.T) {
	orig := WorkspaceRoot
	WorkspaceRoot = t.TempDir() + "/ws"
	defer func() { WorkspaceRoot = orig }()
	_ = os.MkdirAll(WorkspaceRoot, 0o755)

	inside := jsonPath(WorkspaceRoot) + "/a.txt"
	editorCaps := []string{"writes-path:path", "read-only-when:command=view"}

	cases := []struct {
		name string
		mode SandboxPolicy
		tool ToolDefinition
		args string
		deny string // 期望拒绝信息包含的片段；空表示应放行
	}{
		{
			name: "writes-path 工作区内写放行",
			mode: SandboxWorkspaceWrite,
			tool: &declaredWriteTool{name: "declared-editor", caps: editorCaps},
			args: `{"command":"str_replace","path":"` + inside + `"}`,
		},
		{
			name: "writes-path 工作区外写拒绝",
			mode: SandboxWorkspaceWrite,
			tool: &declaredWriteTool{name: "declared-editor", caps: editorCaps},
			args: `{"command":"str_replace","path":"/etc/hosts"}`,
			deny: "file access denied under workspace-write mode",
		},
		{
			name: "writes-path 只读策略拒绝",
			mode: SandboxReadOnly,
			tool: &declaredWriteTool{name: "declared-editor", caps: editorCaps},
			args: `{"command":"str_replace","path":"` + inside + `"}`,
			deny: "file access denied under read-only mode",
		},
		{
			name: "read-only-when 命中时只读放行",
			mode: SandboxReadOnly,
			tool: &declaredWriteTool{name: "declared-editor", caps: editorCaps},
			args: `{"command":"view","path":"` + inside + `"}`,
		},
		{
			name: "空值与未知 token 忽略后仍按 writes-path 裁决",
			mode: SandboxReadOnly,
			tool: &declaredWriteTool{name: "declared-editor", caps: []string{"", "bogus", "writes-path:", "read-only-when:command=", "writes-path:path", "read-only-when:command=view"}},
			args: `{"command":"view","path":"` + inside + `"}`,
		},
		{
			name: "writes-unknown 只读拒绝",
			mode: SandboxReadOnly,
			tool: &declaredWriteTool{name: "declared-runner", caps: []string{"writes-unknown"}},
			args: `{"command":"echo hi"}`,
			deny: "file access denied under read-only mode",
		},
		{
			name: "writes-unknown 工作区写放行",
			mode: SandboxWorkspaceWrite,
			tool: &declaredWriteTool{name: "declared-runner", caps: []string{"writes-unknown"}},
			args: `{"command":"echo hi"}`,
		},
		{
			name: "writes-path 参数缺失按写处理",
			mode: SandboxWorkspaceWrite,
			tool: &declaredWriteTool{name: "declared-editor", caps: editorCaps},
			args: `{"command":"create"}`,
			deny: "file access denied under workspace-write mode",
		},
		{
			name: "writes-path 参数无法解析按写处理",
			mode: SandboxWorkspaceWrite,
			tool: &declaredWriteTool{name: "declared-editor", caps: editorCaps},
			args: `{"command":`,
			deny: "file access denied under workspace-write mode",
		},
		{
			name: "未声明能力旧 shell 只读拒绝",
			mode: SandboxReadOnly,
			tool: &mockTool{name: "shell"},
			args: `{"command":"echo hi"}`,
			deny: "file access denied under read-only mode",
		},
		{
			name: "未声明能力旧 shell 工作区写放行",
			mode: SandboxWorkspaceWrite,
			tool: &mockTool{name: "shell"},
			args: `{"command":"echo hi"}`,
		},
		{
			name: "未声明能力旧编辑器工作区外拒绝",
			mode: SandboxWorkspaceWrite,
			tool: &mockTool{name: "str_replace_editor"},
			args: `{"command":"str_replace","path":"/etc/hosts"}`,
			deny: "file access denied under workspace-write mode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newRouterManager()
			m.events.OnWaterfall(EventToolPreExecute, m.sandboxPolicy(fixedPolicy(tc.mode)))
			if err := m.toolRegistry.Register(tc.tool); err != nil {
				t.Fatalf("register tool: %v", err)
			}
			_, err := m.ExecuteTool(context.Background(), tc.tool.Name(), json.RawMessage(tc.args))
			if tc.deny == "" {
				if err != nil {
					t.Fatalf("应放行，却返回错误: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.deny) {
				t.Fatalf("err = %v, want 包含 %q", err, tc.deny)
			}
		})
	}
}

func TestParseSandboxPolicy(t *testing.T) {
	cases := map[string]SandboxPolicy{
		"full":      SandboxFullAccess,
		"readonly":  SandboxReadOnly,
		"workspace": SandboxWorkspaceWrite,
		"":          SandboxWorkspaceWrite,
		"bogus":     SandboxWorkspaceWrite,
	}
	for in, want := range cases {
		if got := ParseSandboxPolicy(in); got != want {
			t.Errorf("ParseSandboxPolicy(%q) = %v, want %v", in, got, want)
		}
	}
}
