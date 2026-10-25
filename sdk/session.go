package dsc

import (
	"dsc/core"
	"dsc/session"
)

// 会话存储的统一入口。
//
// 全仓只有一处实现「项目路径 → 会话键」与「会话目录/文件后缀」的规则
// （dsc/session 包）。插件一律经这里调用，不得自行拼 <ExecDir>/sessions、
// 自行 replace 路径分隔符或自行清洗会话 id——多方实现一旦分歧，就会出现
// 「宿主读 A 文件、插件写 B 文件」以及不同项目撞进同一份存档的问题。

// 会话数据目录名与文件后缀（转发 dsc/session 的定义，避免插件另写一份字面量）。
const (
	// SessionDirName 会话数据目录名（相对 ExecDir）。
	SessionDirName = session.DirName
	// SessionFileSuffix 会话事件日志后缀 <id>.jsonl。
	SessionFileSuffix = session.FileSuffix
)

// SessionDir 返回会话数据目录：优先 DSC_SESSION_DIR（宿主注入，可与
// ExecDir 分离部署），缺省为 <ExecDir>/sessions。
//
// 不要退回相对路径 "sessions"：插件进程的 cwd 未必是 ExecDir，写出的
// 会话文件宿主读不到（插件写一份、宿主读另一份，会话恢复失败）。
func SessionDir() string {
	if dir := EnvStr("DSC_SESSION_DIR", ""); dir != "" {
		return dir
	}
	return session.Dir(ExecDir())
}

// SessionKeyForProject 把项目根路径转换为会话文件名（键）。
// 分隔符与转义规则见 dsc/session.SessionKeyForProject：名字里的 "-"/"+" 原样
// 保留，仅对会与分隔符混淆的位置转义。
func SessionKeyForProject(projectRoot string) string {
	return session.SessionKeyForProject(projectRoot)
}

// ProjectSessionKey 返回当前工作区对应的默认会话键。
// 未收到显式会话 id（事件透传）时，各插件用它取「同一项目同一份状态」。
func ProjectSessionKey() string {
	return session.SessionKeyForProject(WorkspaceRoot())
}

// ValidSessionID 校验会话 id 能否安全用作单组件文件名（拒绝路径分隔符与 ".."）。
// 供需要以会话 id 命名文件的插件（如压缩状态）复用同一套校验。
func ValidSessionID(id string) bool {
	return session.ValidID(id)
}

// TempDir 返回临时目录：优先 DSC_TEMP_DIR（宿主注入），缺省 <ExecDir>/temp。
// 用于工具产物（截图、spill、脚本临时档）：按进程 cwd 落盘会在不同启动方式下漂移。
func TempDir() string {
	// 单一实现放在 core：/tmp 虚拟根映射用的也是它，两边必须同源，否则模型写的
	// /tmp/a 与工具自己的临时文件会落进不同目录。
	return core.TempDir()
}

// SkillsDir 返回技能目录：优先 DSC_SKILLS_DIR（宿主注入，可与 ExecDir 分离），
// 缺省 <ExecDir>/skills。
//
// 不要退回相对路径 "./skills"：插件 cwd 未必是 ExecDir，宿主 TUI 列表读的是
// <ExecDir>/skills，两边一旦分歧就出现「TUI 看不到已装技能 / 装载不到」。
func SkillsDir() string {
	return core.SkillsDir(ExecDir())
}

// MemoryDir 返回记忆库目录：优先 DSC_MEMORY_DIR（宿主注入），缺省
// <ExecDir>/memory。插件不得按进程 cwd 定位（cwd 会随启动方式漂移，
// 同一份记忆被写到不同目录）。
func MemoryDir() string {
	if dir := EnvStr("DSC_MEMORY_DIR", ""); dir != "" {
		return dir
	}
	return PJoin(ExecDir(), core.MemoryDirName)
}
