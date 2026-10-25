package core

import (
	"os"
	"runtime"
	"strings"
)

// 虚拟根与设备名映射：模型书写的路径 → 真实路径。
//
// 为什么集中成表：这套映射原先散在四处（core 两个前缀、文件系统插件的 AST 重写、
// 插件的 res()、以及后来的 workdir），覆盖范围各不一致——`/workspace` 只在 core 与
// AST 里，`/tmp` 只在插件里，于是 `workdir: /tmp` 在 Windows 上落到当前盘根。
// 现在只有一张表、一个匹配函数：**新增一个虚拟根 = 表里加一行**，所有入口
// （命令字面量重写、内建命令参数、重定向目标、workdir）自动一致。

// virtualRoot 一条虚拟根规则。goos 为空表示全平台生效。
type virtualRoot struct {
	prefix string
	goos   string
	// resolve 把「前缀之后的剩余部分」解析为真实路径。rest 已保证为空或以 "/" 开头。
	// ok=false 表示这条规则实际不匹配（如 /mnt/<非盘符>），继续往下试。
	resolve func(rest, root, temp string) (string, bool)
}

// virtualRoots 虚拟根表，**按顺序匹配、命中即返回，不做二次映射**。
//
// 单次匹配是有意设计：旧实现是「先 /tmp 再 /workspace」两段串联，而 /tmp 映射不
// 幂等——当工作区根自身位于 /tmp/... 下（从 /tmp 启动、或 DSC_WORKSPACE_ROOT=/tmp/x）
// 时，先把 /workspace 映射成 /tmp/x，再被 /tmp 规则改写，路径就被误伤。表 + 首次
// 命中从结构上消灭了这种顺序依赖。
var virtualRoots = []virtualRoot{
	// Windows 上没有真实 /mnt，WSL 风格的 /mnt/c/... 指真实盘符。
	{prefix: "/mnt", goos: "windows", resolve: resolveDriveRoot},
	// Windows 上没有真实 /tmp（会被解析成当前盘根），映射到 DSC 自己的临时目录；
	// POSIX 上 /tmp 是真实挂载点，规则不生效（不列全平台）。
	{prefix: "/tmp", goos: "windows", resolve: resolveTempRoot},
	// 统一工作空间根，跨平台（模型不知道工作区的真实路径）。
	{prefix: "/workspace", resolve: resolveWorkspaceRoot},
	// 单字母盘符别名（Git Bash / MSYS 风格）/c/Users → C:/Users、/d → D:/。
	// 前缀为空 = 兜底规则，**必须排在最后**：它匹配一切输入，放在前面会把 /tmp 吃成
	// T:/mp、/workspace 吃成 W:/orkspace。表内首次命中，顺序即优先级；解析器对
	// 非盘符形状（/cdrom、/dev/null、/docs）返回 false，于是继续落到「原样返回」。
	{prefix: "", goos: "windows", resolve: resolveDriveLetterRoot},
}

// devicePath 一个设备名映射（只做精确匹配：设备名不是路径根，不做前缀展开）。
type devicePath struct {
	from string
	to   string
	goos string
}

// devicePaths 设备名映射表。目前只有 Windows 的 /dev/null → NUL（无此设备时
// mvdan/sh 会把它当普通文件在当前盘根创建）。其余 /dev/*（stdin/stdout/stderr）
// 语义上应落到解释器的标准流，不做文件系统映射，故不入表。
var devicePaths = []devicePath{
	{from: "/dev/null", to: "NUL", goos: "windows"},
}

// TempDir 返回临时目录：优先 DSC_TEMP_DIR（宿主注入），缺省 <ExecDir>/temp。
//
// 与 SDK 的 TempDir 同源（SDK 直接转发本函数）：/tmp 虚拟根必须与工具产物落在
// 同一个目录，否则模型写的 /tmp/a 与工具自己写的临时文件会分家。
func TempDir() string {
	if d := strings.TrimSpace(os.Getenv("DSC_TEMP_DIR")); d != "" {
		return PClean(d)
	}
	// 与 SDK 的 ExecDir 同义：宿主注入 DSC_EXEC_DIR，缺席回退进程 cwd（不是
	// os.Executable 所在目录——那会把插件自己的目录当成安装目录）。
	if d := strings.TrimSpace(os.Getenv("DSC_EXEC_DIR")); d != "" {
		return PJoin(PClean(d), TempDirName)
	}
	if cwd, err := os.Getwd(); err == nil {
		return PJoin(cwd, TempDirName)
	}
	return TempDirName
}

// MapDevicePath 按设备名表映射（Windows 上 /dev/null → NUL），无匹配原样返回。
func MapDevicePath(p string) string {
	for _, d := range devicePaths {
		if d.goos != "" && d.goos != runtime.GOOS {
			continue
		}
		if p == d.from {
			return d.to
		}
	}
	return p
}

// MapWorkspacePath 把模型书写的路径按虚拟根表映射为真实路径。
//
// 匹配规则：取表内首个「前缀命中且平台适用」的规则；未命中任何规则时原样返回
// （盘符路径、POSIX 真实根路径、相对路径都属于这一类，由调用方决定如何绝对化）。
func MapWorkspacePath(p string) string {
	return mapVirtualRootsFor(p, WorkspaceRoot, TempDir(), runtime.GOOS)
}

// MapWorkspacePathFor 是 MapWorkspacePath 的纯函数本体（root/temp/goos 显式注入）。
// 供跨平台测试与需要显式指定根的插件使用；生产路径一律用 MapWorkspacePath。
func MapWorkspacePathFor(p, root, temp, goos string) string {
	return mapVirtualRootsFor(p, root, temp, goos)
}

// MapModelPath 映射模型书写的完整路径：设备名表 → 虚拟根表。
//
// 插件在「文件参数/重定向目标/工作目录」这类入口统一调用它，不要自己再写一条
// /tmp 或 /dev/null 规则——那正是此前四处实现互相不一致的成因。
func MapModelPath(p string) string {
	return MapWorkspacePath(MapDevicePath(p))
}

// mapVirtualRootsFor 是虚拟根表的匹配实现（纯函数）。
func mapVirtualRootsFor(p, root, temp, goos string) string {
	if p == "" {
		return p
	}
	// 规则匹配统一在正斜杆副本上进行（模型可能传入 \workspace、\tmp 等 Windows 风格）。
	// 不用 filepath.ToSlash：它在非 Windows 宿主上是空操作（反斜杆在 POSIX 不是
	// 分隔符），纯函数必须显式替换才能宿主无关。
	slash := strings.ReplaceAll(p, `\\`, "/")
	slash = strings.ReplaceAll(slash, "\\", "/")

	for _, r := range virtualRoots {
		if r.goos != "" && r.goos != goos {
			continue
		}
		if !strings.HasPrefix(slash, r.prefix) {
			continue
		}
		rest := slash[len(r.prefix):]
		// 边界：前缀后必须是分隔符或结尾，否则 /workspacefoo、/tmp2 是独立路径，
		// 不能当成别名（否则沙箱与执行两端会对同一路径给出不同结论）。
		if rest != "" && !strings.HasPrefix(rest, "/") {
			continue
		}
		if out, ok := r.resolve(rest, root, temp); ok {
			return out
		}
	}
	return p
}

// resolveWorkspaceRoot /workspace[/…] → 工作空间根[/…]。
func resolveWorkspaceRoot(rest, root, _ string) (string, bool) {
	r := cleanSlashRoot(root)
	if r == "" {
		return "", false // 根未知：保持原样，交给下游按真实根语义处理
	}
	return joinSlashRoot(r, rest), true
}

// resolveTempRoot /tmp[/…] → 临时目录[/…]（仅 Windows 入表）。
func resolveTempRoot(rest, _, temp string) (string, bool) {
	t := cleanSlashRoot(temp)
	if t == "" {
		return "", false
	}
	return joinSlashRoot(t, rest), true
}

// resolveDriveRoot /mnt/<drive>[/…] → <DRIVE>:/…（仅 Windows 入表）。
func resolveDriveRoot(rest, _, _ string) (string, bool) {
	// rest 形如 "/c/foo" 或 "/c"（前缀 "/mnt" 已剥离）
	body := strings.TrimPrefix(rest, "/")
	if body == "" {
		return "", false
	}
	drive := body[:1]
	after := body[1:]
	if len(after) != 0 && after[0] != '/' {
		return "", false // /mnt/cdrom 不是盘符别名
	}
	return driveToRoot(drive, after)
}

// resolveDriveLetterRoot 单字母盘符别名：/c[/…] → C:/[…]（仅 Windows 入表）。
//
// rest 形如 "/c" 或 "/c/Users"。rest 后面不是分隔符的（如 /cdrom、/dev/null）不匹配，
// 交给后续处理——真实根语义下它们是当前盘根下的路径，不能当成盘符别名。
func resolveDriveLetterRoot(rest, _, _ string) (string, bool) {
	body := strings.TrimPrefix(rest, "/")
	if body == "" {
		return "", false
	}
	drive := body[:1]
	if !isDriveLetter(drive) {
		return "", false
	}
	after := body[1:]
	if after != "" && !strings.HasPrefix(after, "/") {
		return "", false
	}
	return driveToRoot(drive, after)
}

// driveToRoot 拼「盘符 + 剩余部分」：after 为空时给盘根 C:/，否则 C:/rest。
func driveToRoot(drive, after string) (string, bool) {
	if !isDriveLetter(drive) {
		return "", false
	}
	if after == "" {
		return strings.ToUpper(drive) + ":/", true
	}
	return strings.ToUpper(drive) + ":" + after, true
}

// isDriveLetter 报告 b 是否为单个 ASCII 字母（盘符）。
func isDriveLetter(b string) bool {
	if len(b) != 1 {
		return false
	}
	c := b[0]
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// cleanSlashRoot 归一化一个真实根目录：正斜杆、去掉尾随分隔符。
func cleanSlashRoot(root string) string {
	r := strings.ReplaceAll(root, `\\`, "/")
	r = strings.ReplaceAll(r, "\\", "/")
	return strings.TrimRight(r, "/")
}

// joinSlashRoot 拼接「真实根 + 剩余部分」；rest 为空时返回根本身。
func joinSlashRoot(root, rest string) string {
	sub := strings.TrimLeft(rest, "/")
	if sub == "" {
		return root
	}
	return root + "/" + sub
}
