package main

// 持久会话的行为测试：一个最小 SSH 服务器 + 真实客户端，验证「连上就一直能用」。
//
// 为什么要自己写服务器：这几条性质（连接复用、取消只断命令、大输出不丢尾、掉线后
// 同 id 重连）都是客户端与协议交互的产物，单测桩模拟不出来——而它们正是「持久会话」
// 的全部意义。

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeSSH 一个只做「开通道、按命令吐输出」的最小 SSH 服务器。
type fakeSSH struct {
	addr string
	ln   net.Listener

	mu       sync.Mutex
	conns    int // 已接受的 TCP 连接数（重连会 +1）
	channels int // 已开的 session 通道数（每条命令 +1）
	live     []net.Conn
}

func startFakeSSH(t *testing.T) *fakeSSH {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate host key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) { return nil, nil },
	}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &fakeSSH{addr: ln.Addr().String(), ln: ln}
	go srv.serve(cfg)
	t.Cleanup(func() {
		_ = ln.Close()
		srv.dropAll()
	})
	return srv
}

func (s *fakeSSH) serve(cfg *ssh.ServerConfig) {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns++
		s.live = append(s.live, c)
		s.mu.Unlock()
		go s.handleConn(c, cfg)
	}
}

func (s *fakeSSH) handleConn(c net.Conn, cfg *ssh.ServerConfig) {
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	// 全局请求（客户端用 keepalive@openssh.com 探活）：一律应答，否则客户端会卡住。
	go func() {
		for req := range reqs {
			if req.WantReply {
				_ = req.Reply(true, nil)
			}
		}
	}()
	for nc := range chans {
		if nc.ChannelType() != "session" {
			_ = nc.Reject(ssh.UnknownChannelType, "session only")
			continue
		}
		ch, chReqs, err := nc.Accept()
		if err != nil {
			continue
		}
		s.mu.Lock()
		s.channels++
		s.mu.Unlock()
		go s.handleSession(ch, chReqs)
	}
}

func (s *fakeSSH) handleSession(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer ch.Close()
	for req := range reqs {
		if req.Type != "exec" {
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
			continue
		}
		var payload struct{ Command string }
		_ = ssh.Unmarshal(req.Payload, &payload)
		_ = req.Reply(true, nil)
		go s.runCommand(ch, payload.Command)
	}
}

func (s *fakeSSH) runCommand(ch ssh.Channel, cmd string) {
	switch {
	case strings.HasPrefix(cmd, "big"):
		_, _ = ch.Write([]byte(strings.Repeat("x", 512*1024) + "\n"))
	case strings.HasPrefix(cmd, "slow"):
		for i := 0; i < 100; i++ {
			if _, err := ch.Write([]byte("tick\n")); err != nil {
				return // 客户端关掉通道了（取消），停止
			}
			time.Sleep(20 * time.Millisecond)
		}
	default:
		_, _ = ch.Write([]byte("ok:" + cmd + "\n"))
	}
	_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
	// 客户端要等通道关闭才认为命令结束（它靠 reqs 通道收尾），只发 exit-status 不够。
	_ = ch.Close()
}

// dropAll 掐掉所有已建立的 TCP 连接，模拟 NAT 超时/网络中断。
func (s *fakeSSH) dropAll() {
	s.mu.Lock()
	conns := s.live
	s.live = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *fakeSSH) counts() (conns, channels int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns, s.channels
}

// newTestSession 起服务器并连上一条会话。
func newTestSession(t *testing.T) (*fakeSSH, *SSHSessionManager, string) {
	t.Helper()
	srv := startFakeSSH(t)
	host, portStr, err := net.SplitHostPort(srv.addr)
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	m := &SSHSessionManager{sessions: make(map[string]*sshSession)}
	id, err := m.connect(host, port, "tester", "secret", "")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { m.close(id) })
	return srv, m, id
}

// TestSSHConnectionIsReusedAcrossExecs 连一次、跑多条命令：只应该有一条 TCP 连接。
func TestSSHConnectionIsReusedAcrossExecs(t *testing.T) {
	srv, m, id := newTestSession(t)

	for i := 0; i < 3; i++ {
		out, code, reconnected, err := m.exec(context.Background(), id, "echo hi")
		if err != nil {
			t.Fatalf("exec #%d: %v", i, err)
		}
		if code != 0 || !strings.Contains(out, "ok:echo hi") {
			t.Fatalf("exec #%d: out=%q code=%d", i, out, code)
		}
		if reconnected {
			t.Fatalf("exec #%d 不该重连（连接一直活着）", i)
		}
	}

	conns, channels := srv.counts()
	if conns != 1 {
		t.Fatalf("三条命令后 TCP 连接数 = %d，want 1（连接应被复用）", conns)
	}
	if channels != 3 {
		t.Fatalf("session 通道数 = %d，want 3（每条命令一条通道）", channels)
	}
}

// TestSSHExecKeepsLargeOutput 大输出必须完整返回。
//
// 这条钉住一个真实踩过的坑：用 StdoutPipe 时先 Wait 再读完，库会在命令退出时关掉
// 管道，尾部就丢了；改成 Stdout/Stderr Writer 由库自己等拷贝完成。
func TestSSHExecKeepsLargeOutput(t *testing.T) {
	_, m, id := newTestSession(t)

	out, _, _, err := m.exec(context.Background(), id, "big")
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if len(out) != 512*1024+1 {
		t.Fatalf("输出长度 = %d，want %d（尾部被截断）", len(out), 512*1024+1)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatal("输出尾部缺失")
	}
}

// TestSSHCancelKeepsConnectionUsable 取消只终止当前命令，连接与 session id 继续可用。
func TestSSHCancelKeepsConnectionUsable(t *testing.T) {
	srv, m, id := newTestSession(t)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(120 * time.Millisecond)
		cancel()
	}()
	if _, _, _, err := m.exec(ctx, id, "slow"); err == nil {
		t.Fatal("慢命令应被取消")
	} else if !strings.Contains(err.Error(), "still open") {
		t.Fatalf("错误里应说明连接仍然可用，实际 %v", err)
	}

	out, _, reconnected, err := m.exec(context.Background(), id, "echo after-cancel")
	if err != nil {
		t.Fatalf("取消后应仍可用：%v", err)
	}
	if !strings.Contains(out, "ok:echo after-cancel") {
		t.Fatalf("out = %q", out)
	}
	if reconnected {
		t.Fatal("取消不该触发重连（连接本身没坏）")
	}
	if conns, _ := srv.counts(); conns != 1 {
		t.Fatalf("取消把整条连接关掉了：conns = %d", conns)
	}
}

// TestSSHListReportsAlive ssh_list 要说清「这条连接现在还能不能用」。
func TestSSHListReportsAlive(t *testing.T) {
	srv, m, id := newTestSession(t)

	list := m.list()
	if len(list) != 1 || list[0].ID != id {
		t.Fatalf("list = %+v", list)
	}
	if !list[0].Alive {
		t.Fatalf("存活连接被判死：%+v", list[0])
	}
	if list[0].Host != srv.addr {
		t.Fatalf("host = %q, want %q", list[0].Host, srv.addr)
	}

	// 掐掉连接后应如实报 down（而不是仍然 up）
	srv.dropAll()
	time.Sleep(150 * time.Millisecond)
	if list := m.list(); len(list) != 1 || list[0].Alive {
		t.Fatalf("断线后仍报 alive：%+v", list)
	}
}

// TestSSHReconnectsAfterTransportDrop 掉线后用同一个 session id 重连，命令照跑。
func TestSSHReconnectsAfterTransportDrop(t *testing.T) {
	srv, m, id := newTestSession(t)

	srv.dropAll()
	time.Sleep(150 * time.Millisecond)

	out, _, reconnected, err := m.exec(context.Background(), id, "echo back")
	if err != nil {
		t.Fatalf("掉线后应能自动重连：%v", err)
	}
	if !reconnected {
		t.Fatal("结果应标记 reconnected=true，让模型知道远端进程状态可能已变")
	}
	if !strings.Contains(out, "ok:echo back") {
		t.Fatalf("out = %q", out)
	}
	if conns, _ := srv.counts(); conns != 2 {
		t.Fatalf("conns = %d，want 2（重连建了新连接）", conns)
	}
}
