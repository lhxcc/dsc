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

// TestCopyAssetDirSkipsArchives 附带资源里的归档不随包，但字体与说明要随包。
//
// 真实形态：plugins/tool-pdf/fonts/ 同时有 NotoSansSC-Regular.ttf(10MB)、
// HarmonyOS-Sans.zip(52MB) 与 字体下载.txt。整目录照拷 = 每个平台白多 52MB，
// 而插件运行时只按文件名读 ttf 本体。
func TestCopyAssetDirSkipsArchives(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "fonts")
	write := func(name string) {
		if err := os.WriteFile(filepath.Join(src, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("NotoSansSC-Regular.ttf")
	write("HarmonyOS-Sans.zip")
	write("字体下载.txt")

	st := copyAssetDir(src, dst)
	if st.fonts != 1 {
		t.Fatalf("fonts = %d，应为 1", st.fonts)
	}
	if st.files != 2 {
		t.Fatalf("files = %d，应为 2（ttf + 说明，压缩包不入包）", st.files)
	}
	if _, err := os.Stat(filepath.Join(dst, "NotoSansSC-Regular.ttf")); err != nil {
		t.Fatalf("字体没被拷入: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "字体下载.txt")); err != nil {
		t.Fatalf("说明没被拷入: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "HarmonyOS-Sans.zip")); err == nil {
		t.Fatal("压缩包不应随包")
	}
}

// TestFontAssetsAreStaged 锁住「字体随包」这件事本身。
//
// 背景：tool-pdf 启动时硬校验 fonts 里至少一个 .ttf（有意设计，缺字形比装不上更糟），
// 所以漏打包 = 新装的包里那 13 个 pdf 工具永远不可见，而打包日志看起来一切正常。
// 这里同时检查两件事：白名单里有 fonts、真实仓库里的字体能被暂存进包。
func TestFontAssetsAreStaged(t *testing.T) {
	listed := false
	for _, d := range pluginAssetDirs {
		if d == fontAssetDir {
			listed = true
		}
	}
	if !listed {
		t.Fatalf("%s 不在 pluginAssetDirs 里：字体不会随包，插件会在新机器上启动即 panic", fontAssetDir)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := filepath.Dir(wd)
	src := filepath.Join(repoRoot, "plugins", "tool-pdf", "fonts")
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		t.Skip("仓库里没有 plugins/tool-pdf/fonts（本机未放字体），跳过")
	}

	dst := filepath.Join(t.TempDir(), "fonts")
	st := copyAssetDir(src, dst)
	if st.fonts == 0 {
		t.Fatalf("%s 里没有 .ttf：打出来的包在启动时会 panic（把 TrueType 字体放进去）", src)
	}
	entries, err := os.ReadDir(dst)
	if err != nil {
		t.Fatal(err)
	}
	ttf := 0
	for _, e := range entries {
		if strings.EqualFold(filepath.Ext(e.Name()), ".ttf") {
			ttf++
		}
	}
	if ttf == 0 {
		t.Fatal("暂存后包内没有 .ttf")
	}
}
