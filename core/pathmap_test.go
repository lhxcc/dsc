package core

import (
	"runtime"
	"strings"
	"testing"
)

// TestVirtualRootTable 虚拟根表：/workspace、/tmp（仅 Windows）、/mnt/<drive>（仅 Windows）
// 全部由同一张表决定，边界（/tmp2、/workspacefoo）不被误判为别名。
func TestVirtualRootTable(t *testing.T) {
	const root, temp = "G:/ws", "G:/tmp/dsc"

	win := []struct{ in, want string }{
		{"/workspace", root},
		{"/workspace/", root},
		{"/workspace/a.md", root + "/a.md"},
		// /tmp 在 Windows 上没有真实挂载点，落到 DSC 临时目录
		{"/tmp", temp},
		{"/tmp/x.txt", temp + "/x.txt"},
		{`\tmp\x`, temp + "/x"}, // 反斜杆形态同样识别
		// 边界：不是别名
		{"/tmp2/x", "/tmp2/x"},
		{"/workspacefoo", "/workspacefoo"},
		// 真实盘符与相对路径不参与映射
		{"C:/out/x", "C:/out/x"},
		{"rel/x", "rel/x"},
	}
	for _, c := range win {
		if got := MapWorkspacePathFor(c.in, root, temp, "windows"); got != c.want {
			t.Errorf("windows %q = %q, want %q", c.in, got, c.want)
		}
	}

	// POSIX 上 /tmp 是真实挂载点：原样，绝不能改写
	for _, goos := range []string{"linux", "darwin"} {
		for _, c := range []struct{ in, want string }{
			{"/tmp/x.txt", "/tmp/x.txt"},
			{"/tmp", "/tmp"},
			{"/workspace/a.md", root + "/a.md"}, // 跨平台规则仍生效
		} {
			if got := MapWorkspacePathFor(c.in, root, temp, goos); got != c.want {
				t.Errorf("%s %q = %q, want %q", goos, c.in, got, c.want)
			}
		}
	}
}

// TestVirtualRootSinglePass 工作区根落在 /tmp 下时不得二次映射——这正是旧实现
// 「先 /tmp 再 /workspace」两段串联会误伤的场景（/workspace/x 被改成 /tmp/x 后
// 又被 /tmp 规则改成临时目录）。表驱动 + 首次命中从结构上消除了它。
func TestVirtualRootSinglePass(t *testing.T) {
	const temp = "G:/tmp/dsc"
	got := MapWorkspacePathFor("/workspace/proj/a.go", "G:/tmp/myws", temp, "windows")
	if got != "G:/tmp/myws/proj/a.go" {
		t.Fatalf("工作区根在 /tmp 下时不应被 /tmp 规则二次改写，got %q", got)
	}
	if strings.Contains(got, temp) {
		t.Fatalf("结果不应落到临时目录: %q", got)
	}
}

// TestMapDevicePath 设备名表：Windows 的 /dev/null → NUL，其它平台与其它 /dev/* 原样。
func TestMapDevicePath(t *testing.T) {
	// MapDevicePath 读 runtime.GOOS，这里只验证「当前平台」的行为：
	// 非 Windows 上必须原样（否则会把 POSIX 的真实 /dev/null 改坏）。
	got := MapDevicePath("/dev/null")
	if runtime.GOOS == "windows" {
		if got != "NUL" {
			t.Fatalf("windows /dev/null 应映射为 NUL，got %q", got)
		}
	} else if got != "/dev/null" {
		t.Fatalf("非 windows 上 /dev/null 应原样，got %q", got)
	}
	if got := MapDevicePath("/dev/stdin"); got != "/dev/stdin" {
		t.Fatalf("stdin 不应做文件系统映射，got %q", got)
	}
	if got := MapDevicePath("/dev/null/x"); got != "/dev/null/x" {
		t.Fatalf("设备名只做精确匹配，got %q", got)
	}
}

// TestMapModelPathComposition 组合入口 = 设备名表 → 虚拟根表，插件只该调这一个。
func TestMapModelPathComposition(t *testing.T) {
	root := strings.ReplaceAll(t.TempDir(), `\`, "/") // 映射结果一律正斜杆
	setWorkspaceRoot(t, root)

	// /workspace 走虚拟根
	if got := MapModelPath("/workspace/a.md"); got != root+"/a.md" {
		t.Fatalf("/workspace 映射 = %q", got)
	}
	// /dev/null 走设备表（是否映射取决于平台）
	gotNull := MapModelPath("/dev/null")
	if runtime.GOOS == "windows" && gotNull != "NUL" {
		t.Fatalf("windows /dev/null = %q", gotNull)
	}
	if runtime.GOOS != "windows" && gotNull != "/dev/null" {
		t.Fatalf("posix /dev/null = %q", gotNull)
	}
	// 普通相对路径原样
	if got := MapModelPath("rel/a.txt"); got != "rel/a.txt" {
		t.Fatalf("相对路径应原样，got %q", got)
	}
}

// TestDriveLetterAlias 单字母盘符别名（仅 Windows）：/c/Users → C:/Users、/c/ → C:/。
// 关键约束是**表内顺序**：这条前缀只有一个 "/"，若排在 /tmp、/workspace 之前，
// 会把 /tmp 吃成 T:/mp、/workspace 吃成 W:/orkspace。
//
// 另：**裸 /<letter> 不当盘符别名**——它是 Windows 命令的单字母开关（taskkill /F、
// cmd /c、xcopy /S），若别名化会被 shell AST 重写改成盘符路径而令命令失效。下例
// 的 /F、/c、/S 必须原样返回。
func TestDriveLetterAlias(t *testing.T) {
	const root, temp = "G:/ws", "G:/tmp/dsc"
	cases := []struct{ in, want string }{
		{"/c/", "C:/"},
		{"/c/Users/foo", "C:/Users/foo"},
		{"/z/data", "Z:/data"},
		{`\e\proj`, "E:/proj"},
		// 裸 /<letter> 是命令开关，不是盘符别名（必须原样）
		{"/c", "/c"},
		{"/d", "/d"},
		{"/F", "/F"},
		{"/S", "/S"},
		// 不是盘符别名：前缀后不是分隔符
		{"/cdrom/x", "/cdrom/x"},
		{"/dev/null", "/dev/null"},
		// 已有更具体前缀的必须优先命中，不能被 /t、/w 抢走
		{"/tmp/x", temp + "/x"},
		{"/workspace/a", root + "/a"},
		{"/mnt/c/x", "C:/x"},
	}
	for _, c := range cases {
		if got := MapWorkspacePathFor(c.in, root, temp, "windows"); got != c.want {
			t.Errorf("windows %q = %q, want %q", c.in, got, c.want)
		}
	}
	// POSIX 上单字母路径是真实根下的目录，绝不改写
	for _, goos := range []string{"linux", "darwin"} {
		if got := MapWorkspacePathFor("/c/Users", root, temp, goos); got != "/c/Users" {
			t.Errorf("%s 不应把 /c 当盘符: %q", goos, got)
		}
	}
}
