package main

import (
	"os"
	"os/exec"
	"testing"

	dsc "dsc-sdk"
)

// newGitRepo 创建临时 git 仓库（含提交所需的最小配置）。
func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCmdT(t, dir, "init", "-q")
	gitCmdT(t, dir, "config", "user.email", "test@example.com")
	gitCmdT(t, dir, "config", "user.name", "test")
	return dir
}

// gitCmdT 执行 git 命令，失败即终止测试。
func gitCmdT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	dsc.ConfigureChildProcessAttrs(cmd)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeFileT 写入文件（相对 dir）。
func writeFileT(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(dsc.PJoin(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// setWorkspaceRootT 把插件解析的工作区根临时指向 dir。
func setWorkspaceRootT(t *testing.T, dir string) {
	t.Helper()
	old := workspaceRoot
	workspaceRoot = func() string { return dir }
	t.Cleanup(func() { workspaceRoot = old })
}
