package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"

	dsc "dsc-sdk"
)

// 浏览器进程的启动与回收。
//
// 为什么不用 rod 的 launcher.MustLaunch（两处都只在 Windows 上显现）：
//
//  1. 它给子进程只设 CREATE_NEW_PROCESS_GROUP，不设 HideWindow / CREATE_NO_WINDOW。
//     当 DSC 从「没有控制台」的上下文（快捷方式、GUI 宿主、某些终端的分屏）拉起时，
//     Windows 会给控制台子系统子进程分配一个新的控制台窗口——屏幕上一闪而过一个黑框。
//  2. 它默认开 leakless：把浏览器二进制复制到临时目录、经一个 Go 小进程转启，退出时
//     再调用 taskkill，这些都是控制台进程，进一步放大闪烁与拖慢关闭。
//
// 这两点都无法从 rod 外部关掉（osSetupCmd 是内部方法），所以自己起进程：参数仍由
// rod 的 FormatArgs 生成（flag 集合不变），exec 换成能挂统一防护网的那种——子进程属性
// 一律经 SDK 的 ConfigureChildProcessAttrs（宿主唯一 chokepoint，Windows 隐藏控制台、
// 其余平台 no-op），不在插件里另写一份 SysProcAttr。
// 附带好处：进程句柄归我们管，会话关闭时能真正结束浏览器，而不是留一堆僵尸 chrome。

// launchChrome 启动浏览器并返回 CDP 端点，以及一个结束进程的函数。
func launchChrome(l *launcher.Launcher) (controlURL, product string, kill func(), err error) {
	bin := l.Get(flags.Bin) // rod 把浏览器路径存在自定义 flag 里，FormatArgs 会过滤它
	if bin == "" {
		bin = browserBin()
	}
	if bin == "" {
		return "", "", nil, fmt.Errorf("找不到可用的浏览器（用 DSC_BROWSER_BIN 指定 chrome.exe 路径）")
	}

	port, err := freePort()
	if err != nil {
		return "", "", nil, err
	}
	l = l.Set(flags.RemoteDebuggingPort, strconv.Itoa(port))
	args := l.FormatArgs()

	cmd := exec.Command(bin, args...)
	dsc.ConfigureChildProcessAttrs(cmd) // ← 关键：子进程不弹窗（宿主统一防护网）
	cmd.Stdout = io.Discard             // Chrome 的日志没人看，别继承我们的控制台
	cmd.Stderr = io.Discard
	if env, ok := l.GetFlags(flags.Env); ok && len(env) > 0 {
		cmd.Env = env
	}
	if dir := l.Get(flags.WorkingDir); dir != "" {
		cmd.Dir = dir
	}
	if err := cmd.Start(); err != nil {
		return "", "", nil, fmt.Errorf("启动浏览器失败（%s）: %w", bin, err)
	}

	httpURL := "http://127.0.0.1:" + strconv.Itoa(port)
	wsURL, product, err := waitCDP(httpURL, 20*time.Second, cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		return "", "", nil, err
	}
	return wsURL, product, func() { killProcessTree(cmd) }, nil
}

// waitCDP 轮询 /json/version，直到调试端点就绪；子进程提前退出则立刻报错（不必等满超时）。
//
// 常见的「提前退出」原因：profile 被运行中的 Chrome 占用、参数不被这个版本接受。
// 之前用 rod 的 launcher 时这类失败会等到它自己的超时，慢且看不出原因。
// 返回 webSocketDebuggerUrl：rod 的 ControlURL 需要 ws 地址，直接给 http 地址会在
// websocket 握手时 404。
func waitCDP(baseURL string, timeout time.Duration, cmd *exec.Cmd) (wsURL, product string, err error) {
	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-exited:
			return "", "", fmt.Errorf("浏览器进程提前退出（profile 被占用或参数不被接受）")
		default:
		}

		resp, err := client.Get(baseURL + "/json/version")
		if err == nil {
			var v struct {
				WS      string `json:"webSocketDebuggerUrl"`
				Browser string `json:"Browser"`
			}
			if resp.StatusCode == http.StatusOK && json.NewDecoder(resp.Body).Decode(&v) == nil && v.WS != "" {
				_ = resp.Body.Close()
				return v.WS, v.Browser, nil
			}
			_ = resp.Body.Close()
		}

		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("等待浏览器调试端口超时（%s）", baseURL)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// freePort 让内核分配一个空闲端口并立刻释放，交给浏览器监听。
//
// 有极小的竞态窗口（释放到浏览器绑定之间可能被别人抢），实践上可以忽略；rod 的
// launcher 也用同样的做法（--remote-debugging-port=0 + 解析 stderr）。
func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("找不到空闲端口: %w", err)
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("找不到空闲端口: 非 TCP 地址 %s", ln.Addr())
	}
	return addr.Port, nil
}

// chromeRunningHint 把「profile 被占用」这类常见失败翻译成能照做的建议。
func chromeRunningHint(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "提前退出") {
		return "（如果正在用 Chrome，请先完全退出，或用 user_mode=false 走隔离会话）"
	}
	return ""
}
