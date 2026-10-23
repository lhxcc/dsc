package core

import (
	"os"
	"strings"
)

// 可执行目录下的数据目录约定（宿主与 TUI 共用一份实现）。
//
// 会话目录的规则在 session 包（session.Dir），此处只放宿主侧其余的目录解析；
// 插件侧经 SDK（dsc.SessionDir / dsc.SkillsDir / dsc.MemoryDir）读取同一套环境
// 变量与同名子目录，不得各自拼字符串——多方实现一旦分歧，就会出现「宿主读 A
// 目录、插件写 B 目录」的静默错位。

const (
	// SkillsDirName 技能目录名（相对 ExecDir）。
	SkillsDirName = "skills"
	// MemoryDirName 记忆库目录名（相对 ExecDir）。
	MemoryDirName = "memory"
)

// SkillsDir 返回技能目录：DSC_SKILLS_DIR 优先，缺省 <exeDir>/skills。
// exeDir 传宿主可执行文件所在目录（即 ExecDir），由调用方提供，避免本函数
// 依赖进程自身可执行文件位置（同一进程内可能被测试或嵌入场景改变）。
func SkillsDir(exeDir string) string {
	if d := strings.TrimSpace(os.Getenv("DSC_SKILLS_DIR")); d != "" {
		return d
	}
	if exeDir == "" {
		exeDir = "."
	}
	return PJoin(exeDir, SkillsDirName)
}
