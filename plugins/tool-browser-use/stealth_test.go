package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/launcher"
)

// TestBrowserHeadedDefaults 默认无头（不弹窗）；HEADLESS / HEADED / OFFSCREEN 可覆盖，
// 优先级 HEADLESS > HEADED > OFFSCREEN。
func TestBrowserHeadedDefaults(t *testing.T) {
	clear := func() {
		t.Setenv("DSC_BROWSER_HEADLESS", "")
		t.Setenv("DSC_BROWSER_HEADED", "")
		t.Setenv("DSC_BROWSER_OFFSCREEN", "")
	}

	clear()
	if browserHeaded() {
		t.Fatal("默认应无头（实测无头与有头在常用指纹上一致，且不弹窗）")
	}

	clear()
	t.Setenv("DSC_BROWSER_OFFSCREEN", "1")
	if !browserHeaded() || !browserOffscreen() {
		t.Fatal("OFFSCREEN 应有头且离屏")
	}

	clear()
	t.Setenv("DSC_BROWSER_HEADED", "1")
	if !browserHeaded() || browserOffscreen() {
		t.Fatal("HEADED 应有头且可见（不离屏）")
	}

	clear()
	t.Setenv("DSC_BROWSER_HEADED", "1")
	t.Setenv("DSC_BROWSER_HEADLESS", "1")
	if browserHeaded() {
		t.Fatal("HEADLESS 优先级最高")
	}
}

// TestTruthy 开关解析要宽松（1/true/yes/on），别因为写成大写就不认。
func TestTruthy(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", " yes ", "on"} {
		if !truthy(v) {
			t.Fatalf("%q 应判为开", v)
		}
	}
	for _, v := range []string{"", "0", "false", "off", "no"} {
		if truthy(v) {
			t.Fatalf("%q 应判为关", v)
		}
	}
}

// TestCandidateBrowsersExplicitBinFirst DSC_BROWSER_BIN 指定的二进制必须排第一，
// 并按路径推断内核（msedge → Edge），profile 目录随之匹配。
func TestCandidateBrowsersExplicitBinFirst(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "msedge.exe")
	if err := os.WriteFile(bin, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DSC_BROWSER_BIN", bin)
	cands := browserCandidates()
	if len(cands) == 0 || cands[0].Bin != bin {
		t.Fatalf("DSC_BROWSER_BIN 应排第一：%+v", cands)
	}
	if cands[0].Name != "Edge" {
		t.Fatalf("按路径应识别为 Edge，实际 %q", cands[0].Name)
	}
}

// TestHardenLauncherFlags 两个开关缺一不可：
//   - 必须从引擎侧关掉 AutomationControlled（只删命令行开关无效，CDP 接管后
//     navigator.webdriver 仍会变成 true）；
//   - 不能留 rod 默认的 --enable-automation（它会挂「正受自动测试软件控制」提示条）。
//
// 无头时还要用新的 --headless=new，否则 UA 里带 HeadlessChrome。
func TestHardenLauncherFlags(t *testing.T) {
	l := hardenLauncher(launcher.New().Leakless(false), false, false)

	if got := l.Get("disable-blink-features"); !strings.Contains(got, "AutomationControlled") {
		t.Fatalf("必须设置 disable-blink-features=AutomationControlled，实际 %q", got)
	}
	if l.Has("enable-automation") {
		t.Fatal("不能保留 --enable-automation")
	}
	if !l.Has("headless") {
		t.Fatal("无头模式应设置 --headless=new")
	}

	// 有头：不应带 headless；离屏时窗口要挪出可视区
	h := hardenLauncher(launcher.New().Leakless(false), true, false)
	if h.Has("headless") {
		t.Fatal("有头不应带 --headless")
	}
	if h.Has("window-position") {
		t.Fatal("可见有头不该挪窗口")
	}
	off := hardenLauncher(launcher.New().Leakless(false), true, true)
	if pos := off.Get("window-position"); !strings.Contains(pos, "-32000") {
		t.Fatalf("离屏模式应把窗口挪到 -32000，实际 %q", pos)
	}
}

// TestSessionNote 只有启动阶段写了 Switched（换浏览器 / 退回隔离会话）才提示模型。
func TestSessionNote(t *testing.T) {
	if got := sessionNote(nil); got != "" {
		t.Fatalf("无会话时不该提示：%q", got)
	}
	if got := sessionNote(&BrowserSession{}); got != "" {
		t.Fatalf("正常复用登录态时不该提示：%q", got)
	}
	switched := &BrowserSession{Switched: "Chrome 的登录态被占用，已改用 Edge 的登录态"}
	if got := sessionNote(switched); got != switched.Switched {
		t.Fatalf("应转达启动阶段的说明，实际 %q", got)
	}
}

// TestEngineLabel 引擎标签要讲清楚「哪个内核 + 主版本 + 有没有登录态」。
func TestEngineLabel(t *testing.T) {
	cases := []struct {
		name, product string
		userMode      bool
		want          string
	}{
		{"Chrome", "Chrome/155.0.0.0", true, "Chrome 155（用户登录态）"},
		{"Edge", "Edg/141.0.3537.71", false, "Edge 141（隔离会话）"},
		{"Chrome", "", false, "Chrome（隔离会话）"},
	}
	for _, c := range cases {
		if got := engineLabel(c.name, c.product, c.userMode); got != c.want {
			t.Fatalf("engineLabel(%q,%q,%v) = %q, want %q", c.name, c.product, c.userMode, got, c.want)
		}
	}
}

// TestShortVersion 从 "Chrome/155.0.0.0" 取主版本；异常输入原样返回。
func TestShortVersion(t *testing.T) {
	for in, want := range map[string]string{
		"Chrome/155.0.0.0":  "155",
		"Edg/141.0.3537.71": "141",
		"155.0.0.0":         "155.0.0.0", // 没有斜杆：原样（避免猜）
		"":                  "",
	} {
		if got := shortVersion(in); got != want {
			t.Fatalf("shortVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBrowserCandidatesPairProfile 每个候选都带「与自身内核匹配」的 profile 目录，
// 且 Chrome 排在 Edge 前面——顺序即降级顺序：Chrome 被占用时先试 Edge，而不是直接隔离。
func TestBrowserCandidatesPairProfile(t *testing.T) {
	t.Setenv("DSC_BROWSER_BIN", "")
	cands := browserCandidates()
	if len(cands) == 0 {
		t.Skip("本机未安装 Chrome/Edge")
	}
	firstEdge, firstChrome := -1, -1
	for i, c := range cands {
		if c.Bin == "" {
			t.Fatalf("候选不该有空路径：%+v", c)
		}
		if c.ProfileDir != "" && c.Name == "Edge" && strings.Contains(strings.ToLower(c.ProfileDir), "google") {
			t.Fatalf("Edge 不能用 Chrome 的 profile：%+v", c)
		}
		if c.ProfileDir != "" && c.Name == "Chrome" && strings.Contains(strings.ToLower(c.ProfileDir), "edge") {
			t.Fatalf("Chrome 不能用 Edge 的 profile：%+v", c)
		}
		if c.Name == "Edge" && firstEdge < 0 {
			firstEdge = i
		}
		if c.Name == "Chrome" && firstChrome < 0 {
			firstChrome = i
		}
	}
	if firstChrome < 0 {
		t.Fatalf("候选里应有 Chrome：%+v", cands)
	}
	if firstEdge >= 0 && firstEdge < firstChrome {
		t.Fatalf("Chrome 应排在 Edge 之前：%+v", cands)
	}
}
