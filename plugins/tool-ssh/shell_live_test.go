package main

// 真机集成测试：验证持久 shell 的状态是否真的跨调用保留。
//
// 需要一台可达的 SSH 服务器，因此默认跳过，只在显式给环境变量时跑：
//
//	DSC_SSH_TEST_ADDR=192.168.124.165:22 DSC_SSH_TEST_USER=root \
//	DSC_SSH_TEST_PASS=rootpass go test -run TestLivePersistentShell -v
//
// 用真机而不是桩：cd/env 是否保留取决于服务端真的跑着一个 sh，桩模拟不出这件事。

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func liveManager(t *testing.T) (*SSHSessionManager, string) {
	t.Helper()
	addr := os.Getenv("DSC_SSH_TEST_ADDR")
	if addr == "" {
		t.Skip("set DSC_SSH_TEST_ADDR to run the live SSH test")
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	m := &SSHSessionManager{sessions: make(map[string]*sshSession)}
	id, err := m.connect(host, port, os.Getenv("DSC_SSH_TEST_USER"), os.Getenv("DSC_SSH_TEST_PASS"), "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { m.close(id) })
	return m, id
}

// mustShell 跑一条命令并要求成功。
func mustShell(t *testing.T, m *SSHSessionManager, id, cmd string) string {
	t.Helper()
	out, code, err := m.execShell(context.Background(), id, cmd)
	if err != nil {
		t.Fatalf("shell %q: %v", cmd, err)
	}
	if code != 0 {
		t.Fatalf("shell %q: rc=%d out=%q", cmd, code, out)
	}
	return out
}

// TestLivePersistentShell 同一个会话里的 cd / export 必须跨调用保留，退出码要准。
func TestLivePersistentShell(t *testing.T) {
	m, id := liveManager(t)

	wd := strings.TrimSpace(mustShell(t, m, id, "pwd"))
	if wd == "" {
		t.Fatal("pwd 无输出")
	}
	mustShell(t, m, id, "cd /tmp")
	if got := strings.TrimSpace(mustShell(t, m, id, "pwd")); got != "/tmp" {
		t.Fatalf("cd 未跨调用保留：pwd = %q, want /tmp（上一次在 %s）", got, wd)
	}
	mustShell(t, m, id, "export DSC_MARK=persisted")
	if got := strings.TrimSpace(mustShell(t, m, id, "echo $DSC_MARK")); got != "persisted" {
		t.Fatalf("export 未跨调用保留：%q", got)
	}
	// 函数也能保留（真 shell 状态，不只是 cwd）
	mustShell(t, m, id, "dsc_fn(){ echo fn-ok; }")
	if got := strings.TrimSpace(mustShell(t, m, id, "dsc_fn")); got != "fn-ok" {
		t.Fatalf("shell 函数未保留：%q", got)
	}

	// 退出码：非 0 必须如实回传
	out, code, err := m.execShell(context.Background(), id, "false")
	if err != nil {
		t.Fatalf("false: %v", err)
	}
	if code != 1 {
		t.Fatalf("false 的退出码 = %d, want 1（out=%q）", code, out)
	}
	// 多行输出保持原样
	if got := mustShell(t, m, id, "printf 'a\\nb\\n'"); got != "a\nb" {
		t.Fatalf("多行输出 = %q", got)
	}
	// 命令文本里有裸 `}`：`{ ... }` 分组会被打断（marker 永远不到），heredoc 不会
	if got := mustShell(t, m, id, "echo }"); got != "}" {
		t.Fatalf("裸花括号命令 = %q", got)
	}
	// 命令自带 here-doc：原样到达 eval，不该被外层包装破坏
	script := "cat <<'X'\nline-a\nline-b\nX"
	if got := mustShell(t, m, id, script); got != "line-a\nline-b" {
		t.Fatalf("嵌套 here-doc = %q", got)
	}
	// 读 stdin 的命令（cat）必须立刻 EOF，不能把后续命令从管道里吃掉
	if got := mustShell(t, m, id, "cat"); got != "" {
		t.Fatalf("cat 应读到 EOF（空输出），实际 %q", got)
	}
	if got := strings.TrimSpace(mustShell(t, m, id, "echo stream-intact")); got != "stream-intact" {
		t.Fatalf("stdin 被污染，后续命令丢失：%q", got)
	}

	// ssh_list 应标出 shell 已打开
	list := m.list()
	if len(list) != 1 || !list[0].Shell {
		t.Fatalf("ssh_list 未标出持久 shell：%+v", list)
	}

	// ssh_exec 是另一条路：它的 cd 不该影响持久 shell，也不该被影响
	if _, _, _, err := m.exec(context.Background(), id, "cd /usr"); err != nil {
		t.Fatalf("exec cd: %v", err)
	}
	if got := strings.TrimSpace(mustShell(t, m, id, "pwd")); got != "/tmp" {
		t.Fatalf("ssh_exec 污染了持久 shell 的 cwd：%q", got)
	}

	// 关闭 shell 后重开：状态应重置
	closed, err := m.closeShell(id)
	if err != nil || !closed {
		t.Fatalf("closeShell = %v, %v", closed, err)
	}
	if got := strings.TrimSpace(mustShell(t, m, id, "pwd")); got == "/tmp" {
		t.Fatalf("关掉 shell 后重开，cwd 不该还是 /tmp（%q）", got)
	}
}

// TestLiveShellCancelResets 取消一条长命令：进程要停、要说明 shell 被重置，之后还能用。
func TestLiveShellCancelResets(t *testing.T) {
	m, id := liveManager(t)

	mustShell(t, m, id, "cd /tmp")
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	out, _, err := m.execShell(ctx, id, "sleep 30; echo never")
	if err == nil {
		t.Fatal("超时应中断命令")
	}
	if !strings.Contains(err.Error(), "shell was reset") {
		t.Fatalf("错误里应说明 shell 被重置：%v", err)
	}
	if strings.Contains(out, "never") {
		t.Fatalf("中断后不该有后续输出：%q", out)
	}

	// 之后仍然可用（新 shell），且 cwd 已回到初始位置（状态丢了是明说的代价）
	start := time.Now()
	if got := strings.TrimSpace(mustShell(t, m, id, "pwd")); got == "" {
		t.Fatal("中断后 shell 不可用")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("中断后重开 shell 太慢：%v", elapsed)
	}
}

// TestLiveShellBusyDoesNotBlock 同一会话上的第二个并发调用要立刻报忙，而不是傻等
// （等的那条可能跑十分钟，宿主看门狗会把整轮判死）。
func TestLiveShellBusyDoesNotBlock(t *testing.T) {
	m, id := liveManager(t)
	mustShell(t, m, id, "true") // 先把 shell 起起来

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _, _ = m.execShell(context.Background(), id, "sleep 2; echo slow-done")
	}()
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	_, _, err := m.execShell(context.Background(), id, "echo nope")
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("并发调用应报 busy，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("报忙不该阻塞：等了 %v", elapsed)
	}
	<-done

	// 前一条结束后仍可用（状态还在）
	if got := strings.TrimSpace(mustShell(t, m, id, "echo after")); got != "after" {
		t.Fatalf("after = %q", got)
	}
}

// TestLiveShellRestartsAfterExit 命令里 exit 会结束 shell：必须报错而不是挂住，
// 下一次调用自动重开（cwd/env 重置，且明说）。
func TestLiveShellRestartsAfterExit(t *testing.T) {
	m, id := liveManager(t)
	mustShell(t, m, id, "cd /tmp")

	done := make(chan error, 1)
	go func() {
		_, _, err := m.execShell(context.Background(), id, "exit 0")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "exited") {
			t.Fatalf("exit 应让 shell 结束并报错，实际 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("shell 退出后调用没有返回（应报错而不是挂住）")
	}

	if got := strings.TrimSpace(mustShell(t, m, id, "pwd")); got == "/tmp" {
		t.Fatalf("重开的 shell 不该保留 /tmp（%q）", got)
	}
}
