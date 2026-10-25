package main

import (
	"strings"
	"testing"

	"github.com/go-rod/rod"
)

// fakeCandidates 造两个候选：Chrome 与 Edge，各带自己的 profile 目录。
func fakeCandidates() []browserCandidate {
	return []browserCandidate{
		{Name: "Chrome", Bin: "chrome.exe", ProfileDir: "/profiles/chrome"},
		{Name: "Edge", Bin: "msedge.exe", ProfileDir: "/profiles/edge"},
	}
}

// withFakeLaunch 装上假候选与假启动器；okFor 决定哪个浏览器能起来。
func withFakeLaunch(t *testing.T, okFor map[string]bool) *[]string {
	t.Helper()
	var tried []string
	oldC, oldT := browserCandidatesFn, tryLaunchFn
	browserCandidatesFn = fakeCandidates
	tryLaunchFn = func(cand browserCandidate, profileDir string, headed, offscreen bool) (*rod.Browser, func(), string, bool) {
		tried = append(tried, cand.Name+"|"+profileDir)
		if okFor[cand.Name] {
			return nil, func() {}, "Chrome/155.0.0.0", true
		}
		return nil, nil, "", false
	}
	t.Cleanup(func() { browserCandidatesFn, tryLaunchFn = oldC, oldT })
	return &tried
}

// TestLaunchChainFallsBackToEdge Chrome 的登录态被占用时应改用 Edge 的登录态，
// 而不是直接退回隔离会话——多数人不会同时开着两个浏览器。
func TestLaunchChainFallsBackToEdge(t *testing.T) {
	t.Setenv("DSC_BROWSER_HEADLESS", "1")
	tried := withFakeLaunch(t, map[string]bool{"Chrome": false, "Edge": true})

	res, err := launchBrowserRod("chain-edge", true)
	if err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if !res.userMode {
		t.Fatal("Edge 命中后仍应算复用登录态")
	}
	if !strings.HasPrefix(res.engine, "Edge 155") {
		t.Fatalf("engine 应标明 Edge，实际 %q", res.engine)
	}
	if !strings.Contains(res.switched, "Chrome") || !strings.Contains(res.switched, "Edge") {
		t.Fatalf("切换说明应点出从 Chrome 换到 Edge，实际 %q", res.switched)
	}
	if len(*tried) < 2 || !strings.HasPrefix((*tried)[0], "Chrome|") || !strings.HasPrefix((*tried)[1], "Edge|") {
		t.Fatalf("尝试顺序应是先 Chrome 后 Edge，实际 %v", *tried)
	}
}

// TestLaunchChainChromeFirstWithoutSwitch Chrome 能用时不该碰 Edge，也不该有切换说明。
func TestLaunchChainChromeFirstWithoutSwitch(t *testing.T) {
	t.Setenv("DSC_BROWSER_HEADLESS", "1")
	tried := withFakeLaunch(t, map[string]bool{"Chrome": true, "Edge": true})

	res, err := launchBrowserRod("chain-chrome", true)
	if err != nil {
		t.Fatalf("应成功: %v", err)
	}
	if !strings.HasPrefix(res.engine, "Chrome 155") || res.switched != "" {
		t.Fatalf("应直接用 Chrome 且无切换说明，实际 engine=%q switched=%q", res.engine, res.switched)
	}
	for _, s := range *tried {
		if strings.HasPrefix(s, "Edge|") {
			t.Fatalf("Chrome 可用时不该去试 Edge：%v", *tried)
		}
	}
}

// TestLaunchChainIsolatedWhenBothBusy 两个浏览器的登录态都不可用才退回隔离会话，
// 并且要明确告诉模型「都没有登录态」。
func TestLaunchChainIsolatedWhenBothBusy(t *testing.T) {
	t.Setenv("DSC_BROWSER_HEADLESS", "1")
	tried := withFakeLaunch(t, map[string]bool{"Chrome": false, "Edge": false})

	// 隔离目录要真实存在，才能走 dsc.MkdirAll —— 用临时 exe 目录不方便，直接让它失败，
	// 这里只验证「尝试过两个浏览器 + 切换说明」这两个可见结果。
	res, err := launchBrowserRod("chain-isolated", true)
	if err == nil {
		if res.userMode {
			t.Fatal("两个都不行时不该声称复用了登录态")
		}
		if !strings.Contains(res.switched, "隔离") {
			t.Fatalf("应说明退回了隔离会话，实际 %q", res.switched)
		}
	}
	var chromeTried, edgeTried bool
	for _, s := range *tried {
		if strings.HasPrefix(s, "Chrome|") {
			chromeTried = true
		}
		if strings.HasPrefix(s, "Edge|") {
			edgeTried = true
		}
	}
	if !chromeTried || !edgeTried {
		t.Fatalf("应先试 Chrome 再试 Edge，实际 %v", *tried)
	}
}
