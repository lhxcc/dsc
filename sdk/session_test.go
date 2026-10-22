package dsc

import (
	"strings"
	"testing"
)

// TestSessionDirPrefersEnvAndFallsBackToExecDir 会话目录只能有一个解析规则：
// DSC_SESSION_DIR 优先，缺省 <ExecDir>/sessions；不得回退相对路径 "sessions"
// （插件 cwd 未必是 ExecDir，宿主会读不到插件写的会话）。
func TestSessionDirPrefersEnvAndFallsBackToExecDir(t *testing.T) {
	t.Setenv("DSC_EXEC_DIR", "/opt/dsc")
	t.Setenv("DSC_SESSION_DIR", "/var/dsc-sessions")
	if got := SessionDir(); got != "/var/dsc-sessions" {
		t.Fatalf("应优先 DSC_SESSION_DIR，got %q", got)
	}

	t.Setenv("DSC_SESSION_DIR", "")
	if got := SessionDir(); got != "/opt/dsc/"+SessionDirName {
		t.Fatalf("缺省应为 <ExecDir>/%s，got %q", SessionDirName, got)
	}
}

// TestSkillsAndMemoryDirFollowExecDir 技能/记忆目录同样以 ExecDir 为基准，
// 不用进程 cwd（否则插件与 TUI 各看一个目录）。
func TestSkillsAndMemoryDirFollowExecDir(t *testing.T) {
	t.Setenv("DSC_EXEC_DIR", "/opt/dsc")
	t.Setenv("DSC_SKILLS_DIR", "")
	t.Setenv("DSC_MEMORY_DIR", "")
	if got := SkillsDir(); got != "/opt/dsc/skills" {
		t.Fatalf("SkillsDir = %q", got)
	}
	if got := MemoryDir(); got != "/opt/dsc/memory" {
		t.Fatalf("MemoryDir = %q", got)
	}

	t.Setenv("DSC_SKILLS_DIR", "/shared/skills")
	t.Setenv("DSC_MEMORY_DIR", "/shared/memory")
	if got := SkillsDir(); got != "/shared/skills" {
		t.Fatalf("应优先 DSC_SKILLS_DIR，got %q", got)
	}
	if got := MemoryDir(); got != "/shared/memory" {
		t.Fatalf("应优先 DSC_MEMORY_DIR，got %q", got)
	}
}

// TestSessionKeyAndValidation 插件侧的键/校验必须与宿主同一实现：
// 路径规则正常透传，会话 id 校验拒绝路径穿越。
func TestSessionKeyAndValidation(t *testing.T) {
	if got := SessionKeyForProject(`/w/my-project`); !strings.HasPrefix(got, "w--my-project-") {
		t.Fatalf("SessionKeyForProject = %q，应以编码后的路径前缀开头", got)
	}
	for _, ok := range []string{"session-1", "O=--Agents--dsc", "a~db"} {
		if !ValidSessionID(ok) {
			t.Errorf("%q 应视为合法会话 id", ok)
		}
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`, "../x"} {
		if ValidSessionID(bad) {
			t.Errorf("%q 应被拒绝", bad)
		}
	}
	if SessionFileSuffix != ".jsonl" {
		t.Fatalf("会话日志后缀变更需同步各插件: %q", SessionFileSuffix)
	}
}
