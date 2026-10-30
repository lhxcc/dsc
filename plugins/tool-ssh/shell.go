package main

// 持久 shell：在同一条 SSH 连接上跑一个常驻的 `/bin/sh`，命令之间共享 cwd / 环境变量
// / umask / 函数——这才是模型心里「持久会话」的样子。
//
// 为什么不是 PTY：PTY 是给**交互式程序**（top/vim/密码提示）用的，它会把输入回显、
// 提示符、ANSI 序列一起塞进流里，取「某条命令的输出」就得跟终端回声搏斗。这里要的是
// 状态（cd/env），普通管道完全够用，输出还干净：没有回显、没有提示符、没有 ESC。
// 需要交互式程序的场景走 `ssh_exec`（它本来就没有终端）。
//
// 协议：每条命令写成
//
//	{ <命令>
//	}; __dsc_rc=$?; printf '\n<marker>%d\n' "$__dsc_rc"
//
// 读到 marker 就切出输出与退出码。marker 带随机 nonce，因此不可能被命令自身输出撞上。

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dsc-sdk"

	"golang.org/x/crypto/ssh"
)

// shellBufferLimit 单条命令在内存里最多攒这么多输出。超出后只保留尾部，并在结果里
// 说明截断——不设上限的话一条 `yes` 就能把宿主内存吃光。
const shellBufferLimit = 4 << 20

// persistentShell 一个常驻的 sh 进程（无 PTY）。
type persistentShell struct {
	sess  *ssh.Session
	stdin io.WriteCloser

	mu     sync.Mutex
	buf    []byte
	trunc  bool
	closed bool
	why    error         // shell 退出的原因（EOF / 命令里 exit / 被关）
	wake   chan struct{} // 1 槽：有新字节就通知等待方
	// ctx 当前正在执行的那条命令的上下文：输出一到就用它上报活动与尾部快照
	// （宿主的空閒看门狗据此续命，后台任务的 job_output 据此显示「刚在刷什么」）。
	ctx context.Context
}

// startPersistentShell 在给定连接上起一个常驻 sh。
func startPersistentShell(c *ssh.Client) (*persistentShell, error) {
	sess, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		_ = sess.Close()
		return nil, err
	}
	sh := &persistentShell{sess: sess, stdin: stdin, wake: make(chan struct{}, 1)}
	// stdout / stderr 合并：模型要的是「这条命令说了什么」，分开反而要自己拼。
	sess.Stdout = sh
	sess.Stderr = sh
	// 不用 -i：交互模式会带来提示符与行编辑（有的 sh 还会自己回显）。非交互照样保留
	// 状态，因为整条命令历史都喂给同一个进程。
	if err := sess.Start("/bin/sh"); err != nil {
		_ = sess.Close()
		return nil, err
	}
	// 必须有人收 Wait：否则 sh 自己退出（命令里写了 exit、远端把通道关了）我们完全
	// 不知道，run 会一直等一个永远不会来的 marker。
	go func() {
		err := sess.Wait()
		sh.mu.Lock()
		sh.closed = true
		sh.why = err
		sh.ctx = nil
		sh.mu.Unlock()
		select {
		case sh.wake <- struct{}{}:
		default:
		}
	}()
	return sh, nil
}

// Write 收命令输出（stdout 与 stderr 可能并发调用）。
func (sh *persistentShell) Write(p []byte) (int, error) {
	sh.mu.Lock()
	if len(sh.buf)+len(p) > shellBufferLimit {
		// 只留尾部：旧字节丢掉，标记截断。
		keep := shellBufferLimit - len(p)
		if keep < 0 {
			keep = 0
		}
		if keep < len(sh.buf) {
			sh.buf = append(sh.buf[:0], sh.buf[len(sh.buf)-keep:]...)
		}
		sh.trunc = true
	}
	sh.buf = append(sh.buf, p...)
	ctx := sh.ctx
	sh.mu.Unlock()
	if ctx != nil {
		dsc.TouchActivity(ctx)
		dsc.AppendOutput(ctx, p)
	}
	select {
	case sh.wake <- struct{}{}:
	default:
	}
	return len(p), nil
}

// close 关掉这条 shell 通道（sh 与它的前台子进程随之结束）。
func (sh *persistentShell) close() {
	sh.mu.Lock()
	sh.closed = true
	sh.ctx = nil
	sh.mu.Unlock()
	_ = sh.sess.Close()
}

func (sh *persistentShell) isClosed() bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.closed
}

func (sh *persistentShell) closeReason() error {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.why
}

func (sh *persistentShell) setCtx(ctx context.Context) {
	sh.mu.Lock()
	sh.ctx = ctx
	sh.mu.Unlock()
}

// snapshot 取当前尚未被切走的原始输出（中断时用于保住「已经打印出来的那一段」）。
func (sh *persistentShell) snapshot() string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	raw := string(sh.buf)
	return strings.TrimSuffix(strings.TrimSuffix(raw, "\n"), "\r")
}

// run 在持久 shell 里执行一条命令，返回输出与退出码。
func (sh *persistentShell) run(ctx context.Context, command string) (string, int, error) {
	sh.mu.Lock()
	closed := sh.closed
	sh.mu.Unlock()
	if closed {
		return "", 0, fmt.Errorf("persistent shell is closed")
	}
	sh.setCtx(ctx)
	defer sh.setCtx(nil)

	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return "", 0, err
	}
	tag := hex.EncodeToString(nonce)
	marker := "__DSC_E_" + tag + "__:"
	delim := "__DSC_C_" + tag + "__"
	// heredoc + eval：命令文本原样到达（不经过任何引号层），eval 在当前 shell 里执行，
	// 所以 cd / export / 函数都保留。
	script := "__dsc_cmd=$(cat <<'" + delim + "'\n" + command + "\n" + delim +
		"\n); eval \"$__dsc_cmd\" </dev/null; __dsc_rc=$?; printf '\\n" + marker + "%d\\n' \"$__dsc_rc\"\n"
	if _, err := io.WriteString(sh.stdin, script); err != nil {
		return "", 0, err
	}

	for {
		text, code, ok, trunc := sh.extract(marker)
		if ok {
			if trunc {
				text = fmt.Sprintf("[output truncated: only the last %d bytes of this command are shown]\n%s",
					shellBufferLimit, text)
			}
			return text, code, nil
		}
		if sh.isClosed() {
			// sh 自己退了（命令里 exit、或远端把通道关了）：把已经打印的带回去。
			partial := sh.snapshot()
			reason := ""
			if err := sh.closeReason(); err != nil {
				reason = ": " + err.Error()
			}
			return partial, 0, fmt.Errorf("persistent shell exited before the command finished%s "+
				"(the next ssh_shell_exec starts a fresh shell; cwd/env are gone)", reason)
		}
		select {
		case <-ctx.Done():
			// 管道里没有控制字符这回事，Ctrl-C 对 sh 无效；唯一可靠的打断是关掉这条
			// shell 通道。代价是 cd/env 丢失，必须如实告诉模型（下一次调用会重开）。
			partial := sh.snapshot()
			sh.close()
			return partial, 0, fmt.Errorf("command interrupted (%s); the persistent shell was reset to stop it, "+
				"so cwd/env set earlier are gone (next ssh_shell_exec starts a fresh shell)", ctx.Err())
		case <-sh.wake:
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// extract 在缓冲里找 marker：找到就切出「marker 之前的输出 + 退出码」，并从缓冲里摘掉。
func (sh *persistentShell) extract(marker string) (text string, code int, ok bool, trunc bool) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	idx := bytes.Index(sh.buf, []byte(marker))
	if idx < 0 {
		return "", 0, false, sh.trunc
	}
	after := sh.buf[idx+len(marker):]
	nl := bytes.IndexByte(after, '\n')
	if nl < 0 {
		return "", 0, false, sh.trunc // marker 的退出码还没到齐
	}
	raw := string(sh.buf[:idx])
	codeText := strings.TrimSpace(string(after[:nl]))
	consumed := idx + len(marker) + nl + 1
	sh.buf = append(sh.buf[:0], sh.buf[consumed:]...)
	trunc = sh.trunc
	sh.trunc = false

	// 去掉尾部空行：printf 前多打的那个换行，加上命令自己末尾的换行，留着只会让
	// 每个结果都顶着一堆空行。\r 是给将来万一接 PTY 留的（管道不会产生）。
	raw = strings.TrimRight(raw, "\r\n")
	if !utf8.ValidString(raw) {
		raw = strings.ToValidUTF8(raw, "\uFFFD")
	}
	code = 0
	if codeText != "" {
		if _, err := fmt.Sscanf(codeText, "%d", &code); err != nil {
			code = -1
		}
	}
	return raw, code, true, trunc
}

// ---- manager 侧接线 ----

// shellFor 取当前连接上的持久 shell，没有就起一个（连接被替换后旧 shell 自动作废）。
//
// 注意：起 shell 的过程（可能含重连）**不能**在持 shellMu 时做——重连会经 setClient
// 调 dropShell，那里要拿同一把锁，持锁重连就是自锁。
func (s *sshSession) shellFor() (*persistentShell, error) {
	s.shellMu.Lock()
	sh := s.shell
	if sh != nil && !sh.isClosed() && s.shellClient == s.current() {
		s.shellMu.Unlock()
		return sh, nil
	}
	if sh != nil {
		s.shell.close()
	}
	s.shell, s.shellClient = nil, nil
	s.shellMu.Unlock()

	c := s.current()
	if c == nil {
		return nil, fmt.Errorf("session is closed")
	}
	sh, err := startPersistentShell(c)
	if err != nil {
		// 连接悄悄死了（保活还没跑到）：重连一次再起 shell，session id 不变。
		if rerr := s.reconnect(); rerr != nil {
			return nil, fmt.Errorf("start persistent shell: %w (reconnect also failed: %v)", err, rerr)
		}
		c = s.current()
		if c == nil {
			return nil, fmt.Errorf("session is closed")
		}
		if sh, err = startPersistentShell(c); err != nil {
			return nil, fmt.Errorf("start persistent shell: %w", err)
		}
	}
	s.shellMu.Lock()
	s.shell, s.shellClient = sh, c
	s.shellMu.Unlock()
	return sh, nil
}

// dropShell 关掉持久 shell（不影响连接本身）。
func (s *sshSession) dropShell() bool {
	s.shellMu.Lock()
	sh := s.shell
	s.shell, s.shellClient = nil, nil
	s.shellMu.Unlock()
	if sh == nil {
		return false
	}
	sh.close()
	return true
}

func (s *sshSession) hasShell() bool {
	s.shellMu.Lock()
	sh := s.shell
	s.shellMu.Unlock()
	return sh != nil && !sh.isClosed()
}

// execShell 在持久 shell 里跑命令（同一会话内串行：一个 sh 只有一个前台）。
func (m *SSHSessionManager) execShell(ctx context.Context, id, command string) (string, int, error) {
	s, ok := m.lookup(id)
	if !ok {
		return "", 0, fmt.Errorf("session %s not found (call ssh_connect first)", id)
	}
	// 一个 sh 只有一个前台：并发的第二个调用不能傻等（等的那条可能跑十分钟，
	// 宿主的看门狗到点会把整轮判死）。直接告诉模型「这条会话的 shell 正忙」，
	// 它可以用 ssh_exec 并行跑一次性命令。先拿闸门再取 shell，顺带避免两个调用
	// 同时去起 shell。
	if !s.shellRunMu.TryLock() {
		return "", 0, fmt.Errorf("persistent shell on session %s is busy: another ssh_shell_exec is still running; "+
			"wait for it, or use ssh_exec for a command that does not need the shell state", id)
	}
	defer s.shellRunMu.Unlock()
	sh, err := s.shellFor()
	if err != nil {
		return "", 0, err
	}
	return sh.run(ctx, command)
}

// closeShell 关闭某会话的持久 shell，返回是否真的关掉了一个。
func (m *SSHSessionManager) closeShell(id string) (bool, error) {
	s, ok := m.lookup(id)
	if !ok {
		return false, fmt.Errorf("session %s not found", id)
	}
	return s.dropShell(), nil
}
