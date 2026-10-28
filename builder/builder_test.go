package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestFindToolFindsVersionedDir 版本化目录里的工具必须能找到。
//
// 真实形态：E:\winkit\share\zig-x86_64-windows-0.17.0\zig.exe——目录名带版本号，
// 可执行文件名不带。只按 <root>/<name> 精确匹配会漏掉它，于是得出「未安装 zig」的
// 错误结论，把定位工作推回给人。
func TestFindToolFindsVersionedDir(t *testing.T) {
	if _, err := exec.LookPath("zig"); err == nil {
		t.Skip("PATH 里已有 zig，本用例测的是「不在 PATH」时的目录探测")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "zig-x86_64-windows-0.17.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := "zig"
	if runtime.GOOS == "windows" {
		exe = "zig.exe"
	}
	want := filepath.Join(dir, exe)
	if err := os.WriteFile(want, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, looked := findTool("zig", []string{root})
	if got != want {
		t.Fatalf("findTool = %q, want %q（已查 %v）", got, want, looked)
	}
}

// TestFindToolFindsBinSubdir 工具放在 <versioned>/bin/ 下同样要能找到。
func TestFindToolFindsBinSubdir(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "protoc_dir", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := "protoc"
	if runtime.GOOS == "windows" {
		exe = "protoc.exe"
	}
	want := filepath.Join(dir, exe)
	if err := os.WriteFile(want, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := findTool("protoc", []string{root}); got != want {
		t.Fatalf("findTool = %q, want %q", got, want)
	}
}

// TestFindToolNotFoundReportsLooked 找不到时必须回报「查过哪些地方」，
// 否则错误信息就是一句无用的「未找到」，等同于让人从零开始排查。
func TestFindToolNotFoundReportsLooked(t *testing.T) {
	root := t.TempDir()
	got, looked := findTool("definitely-not-installed-xyz", []string{root})
	if got != "" {
		t.Fatalf("不应找到，got %q", got)
	}
	if len(looked) < 2 { // 至少含 "PATH" 与传入的根目录
		t.Fatalf("应回报查过的位置，got %v", looked)
	}
}

// TestCCForZigNeedsCcSubcommand zig 作为 CC 时必须带 " cc" 子命令：
// 直接写 zig.exe 会把 go build 的参数当成 zig 自己的参数，报一堆看不懂的错。
func TestCCForZigNeedsCcSubcommand(t *testing.T) {
	p := filepath.Join(t.TempDir(), "zig.exe")
	if got := ccFor(p); !strings.HasSuffix(got, " cc") || !strings.Contains(got, "zig") {
		t.Fatalf("ccFor(zig) = %q，应以 \" cc\" 结尾", got)
	}
	gcc := filepath.Join(t.TempDir(), "gcc.exe")
	if got := ccFor(gcc); strings.HasSuffix(got, " cc") {
		t.Fatalf("ccFor(gcc) = %q，不应带子命令", got)
	}
}

// TestToolSearchRootsIncludesRepoRoot 仓库自带工具链优先于「往系统里找」：
// 本项目把 protoc 放在 protoc_dir/bin/，测试工具链同理。根目录在搜索列表里，
// 才不会出现「明明仓库里有、却说没装」。
func TestToolSearchRootsIncludesRepoRoot(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Dir(wd)
	roots := toolSearchRoots(repoRoot)
	if len(roots) == 0 || roots[0] != repoRoot {
		// DSC_TOOL_ROOTS 未设时第一个应为仓库根
		found := false
		for _, r := range roots {
			if r == repoRoot {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("搜索根应包含仓库根 %q，got %v", repoRoot, roots)
		}
	}
	// 仓库里确有 protoc 时，应当能被找到（没有则跳过，不制造环境依赖）
	if p, _ := findTool("protoc", roots); p == "" {
		t.Log("跳过：本机仓库未附带 protoc")
		return
	} else if !strings.Contains(p, "protoc") {
		t.Fatalf("findTool(protoc) = %q，路径可疑", p)
	}
}
