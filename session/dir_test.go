package session

import "testing"

// TestDirIsSingleSource 会话目录名只有一处定义：Dir(execDir) 与 DirName 一致，
// 宿主与插件经它取路径，不再各自拼 "<ExecDir>/sessions"。
func TestDirIsSingleSource(t *testing.T) {
	if got, want := Dir("/opt/dsc"), "/opt/dsc/"+DirName; got != want {
		t.Fatalf("Dir = %q, want %q", got, want)
	}
	if FileSuffix != ".jsonl" || ACPSuffix != ".acp.json" {
		t.Fatalf("后缀约定变更需同步宿主清理逻辑: %q %q", FileSuffix, ACPSuffix)
	}
}
