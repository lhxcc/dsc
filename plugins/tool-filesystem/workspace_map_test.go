package main

import (
	dsc "dsc-sdk"
	"runtime"
	"strings"
	"testing"

	"mvdan.cc/sh/v3/syntax"
)

// setRoot 在测试期内直接设置统一根变量（dsc.WorkspaceRoot 在进程 init 时从
// DSC_WORKSPACE_ROOT/cwd 解析后冻结，t.Setenv 改不了它），测试结束后恢复。
func setRoot(t *testing.T, root string) {
	old := dsc.WorkspaceRoot()
	dsc.SetWorkspaceRoot(root)
	t.Cleanup(func() { dsc.SetWorkspaceRoot(old) })
}

func TestMapWorkspacePath(t *testing.T) {
	setRoot(t, "/tmp/myws")

	cases := []struct{ in, want string }{
		{"/workspace", "/tmp/myws"},
		{"/workspace/", "/tmp/myws"},
		{"/workspace/a/b.txt", "/tmp/myws/a/b.txt"},
		{"/workspace/x", "/tmp/myws/x"},
		// 边界：/workspacefoo 不作为别名
		{"/workspacefoo/x", "/workspacefoo/x"},
		{"/workspacex", "/workspacex"},
	}

	for _, c := range cases {
		if got := mapWorkspacePath(c.in); got != c.want {
			t.Fatalf("mapWorkspacePath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMapWorkspacePathNoRoot(t *testing.T) {
	// 生产中 WorkspaceRoot 经 init 回退链恒非空；空串仅在防御分支可达，
	// 此处显式构造验证「无根不改写」语义保留。
	setRoot(t, "")
	if got := mapWorkspacePath("/workspace/x"); got != "/workspace/x" {
		t.Fatalf("未注入根时不应改写: got %q", got)
	}
}

func TestMapWorkspacePathInvalidPrefix(t *testing.T) {
	setRoot(t, "/tmp/myws")
	if got := mapWorkspacePath("workspacerel"); got != "workspacerel" {
		t.Fatalf("相对路径应原样: got %q", got)
	}
	if got := mapWorkspacePath("/my/workspace/x"); got != "/my/workspace/x" {
		t.Fatalf("非前缀路径应原样: got %q", got)
	}
}

// TestMapWorkspacePathWSLGating 验证 WSL/GIT BASH 风格路径 /mnt/<drive>/... 的映射严格按
// 宿主 GOOS 分支：
//   - Windows：/mnt/c/Users/... → C:/Users/...（mvdan POSIX 解释器非 WSL/GIT BASH，无法访问 /mnt/c/）
//   - Linux/macOS：原样返回，/mnt/c/... 是合法 POSIX 路径（可能是真实挂载点），不得改写
//
// 这是 DSC 跨平台根本约束的具体落地：路径映射绝不可在能合法访问 /mnt/c/ 的系统上破坏真实路径。
func TestMapWorkspacePathWSLGating(t *testing.T) {
	setRoot(t, "/tmp/myws")
	cases := []struct{ in, want string }{
		{"/mnt/c/Users/foo", "/mnt/c/Users/foo"},
		{"/mnt/d/projects/x", "/mnt/d/projects/x"},
		{"/mnt/c", "/mnt/c"},
		{"/mnt/z/path/to/file", "/mnt/z/path/to/file"},
	}
	if runtime.GOOS == "windows" {
		// Windows: WSL 习惯路径映射到 Windows 盘符
		cases = []struct{ in, want string }{
			{"/mnt/c/Users/foo", "C:/Users/foo"},
			{"/mnt/d/projects/x", "D:/projects/x"},
			{"/mnt/c", "C:/"},
			{"/mnt/z/path/to/file", "Z:/path/to/file"},
		}
	}
	for _, c := range cases {
		if got := mapWorkspacePath(c.in); got != c.want {
			t.Fatalf("mapWorkspacePath(%q) = %q, want %q (GOOS=%s)", c.in, got, c.want, runtime.GOOS)
		}
	}
}

// TestMapWorkspaceAST 校验 AST 层重写：裸词、单/双引号里的 /workspace 被映射，
// 变量展开/命令替换等复杂词不改；边界 /workspacefoo 不改。
func TestMapWorkspaceAST(t *testing.T) {
	setRoot(t, "/tmp/myws")
	cases := []struct {
		in, want string
	}{
		{`cd /workspace`, `cd /tmp/myws`},
		{`ls -la "/workspace/a b"`, `ls -la "/tmp/myws/a b"`},
		{`cat '/workspace/x.txt'`, `cat '/tmp/myws/x.txt'`},
		{`cd /workspace/sub && pwd`, `cd /tmp/myws/sub && pwd`},
		{`touch /workspacefoo/x`, `touch /workspacefoo/x`}, // 边界不改
		{`echo $HOME /workspace`, `echo $HOME /tmp/myws`},  // 变量展开不改，裸词映射
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			parser := syntax.NewParser()
			file, err := parser.Parse(strings.NewReader(c.in+"\n"), "")
			if err != nil {
				t.Fatal(err)
			}
			mapWorkspacePaths(file)
			var sb strings.Builder
			if err := syntax.NewPrinter().Print(&sb, file); err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(sb.String())
			if got != c.want {
				t.Fatalf("mapWorkspacePaths(%q)\n got: %q\nwant: %q", c.in, got, c.want)
			}
		})
	}
}

// TestMapWorkspacePathBarePosixRoot 裸 POSIX 绝对路径保持原样（所有平台，真实根语义）：
// Windows 上不再把 `/` 与 `/x` 锚定工作区根，由下游 filepath.Abs 解析为当前盘根
// （与 Linux 把 /x 解析到真实根行为一致）；/workspace 与 /mnt/<drive> 是显式别名；
// /dev/null（mvdan DefaultOpenHandler 特判重定向到 NUL）与 // UNC 前缀保持原样。
func TestMapWorkspacePathBarePosixRoot(t *testing.T) {
	setRoot(t, "/tmp/myws")

	cases := []struct{ in, want string }{
		{"/", "/"},
		{"/foo/bar.txt", "/foo/bar.txt"},
		{"/dev/null", "/dev/null"},
		{"//server/share", "//server/share"},
		{"/workspace/x", "/tmp/myws/x"},
	}
	if runtime.GOOS == "windows" {
		cases = []struct{ in, want string }{
			{"/", "/"},
			{"/foo/bar.txt", "/foo/bar.txt"},
			{"/Agents", "/Agents"},
			{"/dev/null", "/dev/null"},
			{"//server/share", "//server/share"},
			{"/mnt/d/x", "D:/x"},
			{"/workspace/x", "/tmp/myws/x"},
			{"/workspacefoo/x", "/workspacefoo/x"},
		}
	}
	for _, c := range cases {
		if got := mapWorkspacePath(c.in); got != c.want {
			t.Fatalf("mapWorkspacePath(%q) = %q, want %q (GOOS=%s)", c.in, got, c.want, runtime.GOOS)
		}
	}
}

// TestMapWorkspaceASTBareRoot 校验 AST 层：裸 `/` 与裸 POSIX 路径保持原样（不再改写
// 为工作区根），/dev/null 重定向保持原样。
func TestMapWorkspaceASTBareRoot(t *testing.T) {
	setRoot(t, "/tmp/myws")

	cases := []struct{ in, want string }{
		{`find / -maxdepth 2`, `find / -maxdepth 2`},
		{`cd / && pwd`, `cd / && pwd`},
		{`echo hi 2>/dev/null`, `echo hi 2>/dev/null`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			parser := syntax.NewParser()
			file, err := parser.Parse(strings.NewReader(c.in+"\n"), "")
			if err != nil {
				t.Fatal(err)
			}
			mapWorkspacePaths(file)
			var sb strings.Builder
			if err := syntax.NewPrinter().Print(&sb, file); err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(sb.String())
			if got != c.want {
				t.Fatalf("mapWorkspacePaths(%q)\n got: %q\nwant: %q", c.in, got, c.want)
			}
		})
	}
}

// TestMapWorkspaceASTKeepsCommandFlags 回归：shell AST 重写不得把 Windows 命令的
// 单字母开关误当盘符别名。taskkill /F、cmd /c 中的 /F、/c 是开关，必须原样；
// 多字母开关（/IM）本就不命中盘符规则，也一并确认。盘符引用（/c/Users）仍正常映射。
// 仅 Windows 生效（盘符别名规则只在 Windows 入表）。
func TestMapWorkspaceASTKeepsCommandFlags(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("盘符别名规则仅 Windows 入表，跳过")
	}
	cases := []struct{ in, want string }{
		{`taskkill /F /IM strada-serve.exe`, `taskkill /F /IM strada-serve.exe`},
		{`cd /workspace && cmd /c "taskkill /F /IM strada-serve.exe"`, `cd /tmp/myws && cmd /c "taskkill /F /IM strada-serve.exe"`},
		{`xcopy /S /E src dst`, `xcopy /S /E src dst`},
		{`robocopy /E a b`, `robocopy /E a b`},
		// 盘符引用仍映射（带路径成分）
		{`ls /c/Users/foo`, `ls C:/Users/foo`},
		{`cat /c/x.txt`, `cat C:/x.txt`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			setRoot(t, "/tmp/myws")
			parser := syntax.NewParser()
			file, err := parser.Parse(strings.NewReader(c.in+"\n"), "")
			if err != nil {
				t.Fatal(err)
			}
			mapWorkspacePaths(file)
			var sb strings.Builder
			if err := syntax.NewPrinter().Print(&sb, file); err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(sb.String())
			if got != c.want {
				t.Fatalf("mapWorkspacePaths(%q)\n got: %q\nwant: %q", c.in, got, c.want)
			}
		})
	}
}

// TestTempRootRuleViaSharedTable /tmp 虚拟根现在由 core 的虚拟根表决定（本插件不再
// 自带规则）。这里直接验证共享表本体：Windows 映射到临时目录，Unix 原样，边界不误判。
func TestTempRootRuleViaSharedTable(t *testing.T) {
	cases := []struct {
		in, tempDir, goos, want string
	}{
		{"/tmp", "/tmp/mytemp", "windows", "/tmp/mytemp"},
		{"/tmp/", "/tmp/mytemp", "windows", "/tmp/mytemp"},
		{"/tmp/a/b.txt", "/tmp/mytemp", "windows", "/tmp/mytemp/a/b.txt"},
		// Unix：/tmp 是真实挂载点，原样
		{"/tmp", "/tmp/mytemp", "linux", "/tmp"},
		{"/tmp/a", "/tmp/mytemp", "darwin", "/tmp/a"},
		// 边界：/tmpfoo、/tmpx 不是别名
		{"/tmpfoo/x", "/tmp/mytemp", "windows", "/tmpfoo/x"},
		{"/tmpx", "/tmp/mytemp", "windows", "/tmpx"},
	}
	for _, c := range cases {
		if got := dsc.MapWorkspacePathFor(c.in, "/tmp/myws", c.tempDir, c.goos); got != c.want {
			t.Fatalf("MapWorkspacePathFor(%q, temp=%q, %q) = %q, want %q",
				c.in, c.tempDir, c.goos, got, c.want)
		}
	}

	// 同一次映射里 /workspace 与 /tmp 互不干扰（表内按前缀各自命中，首次命中即返回）
	both := []struct{ in, want string }{
		{"/workspace/x", "/tmp/myws/x"},
		{"/tmp/x", "/tmp/mytemp/x"},
	}
	for _, c := range both {
		if got := dsc.MapWorkspacePathFor(c.in, "/tmp/myws", "/tmp/mytemp", "windows"); got != c.want {
			t.Fatalf("%q = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestMapTempAST 校验 AST 层重写同时覆盖 /workspace 与 /tmp：Windows 上 /tmp 字面量
// 被改写成临时目录（修复 cd /tmp 在 Windows 上落到当前盘根而失败），Unix 上保持原样；
// /workspace 映射不受 /tmp 处理影响（两前缀互不相交）。
func TestMapTempAST(t *testing.T) {
	setRoot(t, "/tmp/myws")
	t.Setenv("DSC_TEMP_DIR", "/tmp/mytemp")

	cases := []struct {
		in, wantWindows, wantUnix string
	}{
		{`cd /workspace`, `cd /tmp/myws`, `cd /tmp/myws`},
		{`cd /tmp`, `cd /tmp/mytemp`, `cd /tmp`},
		{`ls -la /tmp/sub`, `ls -la /tmp/mytemp/sub`, `ls -la /tmp/sub`},
		{`cat "/tmp/x.txt"`, `cat "/tmp/mytemp/x.txt"`, `cat "/tmp/x.txt"`},
		{`cd /tmp && echo hi`, `cd /tmp/mytemp && echo hi`, `cd /tmp && echo hi`},
		// 边界：/tmpfoo 不改
		{`touch /tmpfoo/x`, `touch /tmpfoo/x`, `touch /tmpfoo/x`},
		// 共存：一个命令里既有 /workspace 又有 /tmp
		{`cp /workspace/a /tmp/b`, `cp /tmp/myws/a /tmp/mytemp/b`, `cp /tmp/myws/a /tmp/b`},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			goos := runtime.GOOS
			want := c.wantUnix
			if goos == "windows" {
				want = c.wantWindows
			}
			parser := syntax.NewParser()
			file, err := parser.Parse(strings.NewReader(c.in+"\n"), "")
			if err != nil {
				t.Fatal(err)
			}
			mapWorkspacePathsFor(file, goos)
			var sb strings.Builder
			if err := syntax.NewPrinter().Print(&sb, file); err != nil {
				t.Fatal(err)
			}
			got := strings.TrimSpace(sb.String())
			if got != want {
				t.Fatalf("mapWorkspacePathsFor(%q, %q)\n got: %q\nwant: %q", c.in, goos, got, want)
			}
		})
	}
}
