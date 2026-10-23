package core

import "testing"

// TestSkillsDirResolution 技能目录解析集中在 core：DSC_SKILLS_DIR 优先，
// 缺省 <exeDir>/skills。此前 dsc-system 回退 "./skills"（cwd 相对）、TUI 回退
// <exeDir>/skills，两者在 cwd≠ExecDir 时会看到不同目录。
func TestSkillsDirResolution(t *testing.T) {
	t.Setenv("DSC_SKILLS_DIR", "")
	if got, want := SkillsDir("/opt/dsc"), "/opt/dsc/skills"; got != want {
		t.Fatalf("SkillsDir = %q, want %q", got, want)
	}
	t.Setenv("DSC_SKILLS_DIR", "/shared/skills")
	if got, want := SkillsDir("/opt/dsc"), "/shared/skills"; got != want {
		t.Fatalf("应优先 DSC_SKILLS_DIR，got %q want %q", got, want)
	}
}
