// tool-ssh：SSH 远程命令执行终端插件。提供 ssh_connect / ssh_exec / ssh_list /
// ssh_close 四类持久会话能力（参考 GhostClaw 的 SSH 工具集）。登录支持密码或
// 私钥；会话按 id 缓存复用，方便模型连续在远程主机上执行命令。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"dsc-sdk"

	"golang.org/x/crypto/ssh"
)

// SSHSessionManager 管理持久化 SSH 连接。
//
// 连接一旦建立就常驻：只有 ssh_close 才断开。掉线（NAT 超时、网络切换、对端重启）
// 时用同一份连接参数在原 session id 下重连，模型不必重新 ssh_connect、也不必换 id。
type SSHSessionManager struct {
	mu       sync.Mutex
	sessions map[string]*sshSession
}

// connectOptions 建立连接所需的全部参数；重连要用同一份，所以随连接一起保存。
type connectOptions struct {
	Host           string
	Port           int
	Username       string
	Password       string
	PrivateKeyPath string
}

// sshSession 一条常驻连接。client 在掉线重连时被整体替换，session id 不变。
type sshSession struct {
	opts connectOptions
	addr string

	mu     sync.Mutex
	client *ssh.Client
	stopKA chan struct{}
}

// sshKeepaliveEvery 保活间隔。
//
// 常驻连接最怕「静默死亡」：中间 NAT/防火墙把空闲 TCP 丢掉，我们这边毫无察觉，
// 下一次 ssh_exec 要么卡在 TCP 重传上几十秒、要么回一句看不懂的 transport 错误。
// 30 秒一次 keepalive@openssh.com（OpenSSH 认这个请求，会回 success）既能让中间设备
// 看到流量，也能在连接真的断了时**立刻**发现并收掉，让下一次调用走重连。
const sshKeepaliveEvery = 30 * time.Second

var globalSSH = &SSHSessionManager{sessions: make(map[string]*sshSession)}

// hasAuthMethod 判断是否提供了任一可用认证方法（密码或私钥路径）。
func hasAuthMethod(password, privateKeyPath string) bool {
	return password != "" || privateKeyPath != ""
}

// resolveSSHPort 解析 SSH 端口：未指定（<=0）回退默认 22。
func resolveSSHPort(port int) int {
	if port <= 0 {
		return 22
	}
	return port
}

// dialSSH 按连接参数拨号（密码或私钥认证）。
func dialSSH(opts connectOptions) (*ssh.Client, error) {
	if !hasAuthMethod(opts.Password, opts.PrivateKeyPath) {
		return nil, fmt.Errorf("no auth method (password or private_key_path required)")
	}
	var auth []ssh.AuthMethod
	if opts.Password != "" {
		auth = append(auth, ssh.Password(opts.Password))
	}
	if opts.PrivateKeyPath != "" {
		pem, err := dsc.ReadFile(opts.PrivateKeyPath)
		if err != nil {
			return nil, fmt.Errorf("read private key: %w", err)
		}
		signer, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			return nil, fmt.Errorf("parse private key: %w", err)
		}
		auth = append(auth, ssh.PublicKeys(signer))
	}
	cfg := &ssh.ClientConfig{
		User:            opts.Username,
		Auth:            auth,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // 演示便利；生产建议换为 known_hosts 校验
		Timeout:         10 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", opts.Host, opts.Port)
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return client, nil
}

// keepaliveLoop 定期发保活请求；连接一断就立刻 Close，让后续调用快速失败并重连。
func keepaliveLoop(c *ssh.Client, stop <-chan struct{}) {
	t := time.NewTicker(sshKeepaliveEvery)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if _, _, err := c.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				_ = c.Close()
				return
			}
		}
	}
}

// setClient 安装（或替换）底层连接，并重启保活循环。旧连接随后关闭。
func (s *sshSession) setClient(c *ssh.Client) {
	s.mu.Lock()
	old := s.client
	stop := s.stopKA
	s.client = c
	kaStop := make(chan struct{})
	s.stopKA = kaStop
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if old != nil && old != c {
		_ = old.Close()
	}
	go keepaliveLoop(c, kaStop)
}

func (s *sshSession) current() *ssh.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// reconnect 用原连接参数重连，session id 不变。
func (s *sshSession) reconnect() error {
	c, err := dialSSH(s.opts)
	if err != nil {
		return err
	}
	s.setClient(c)
	return nil
}

// alive 探测连接是否还能用：发一次保活请求，超时即判死。
//
// 用探测而不是缓存状态：keepalive 循环可能还没跑到第一拍，缓存会谎报存活。
func (s *sshSession) alive(timeout time.Duration) bool {
	c := s.current()
	if c == nil {
		return false
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := c.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return err == nil
	case <-time.After(timeout):
		return false
	}
}

// connect 建立并缓存一个 SSH 连接，返回会话 id。支持密码或私钥认证。
func (m *SSHSessionManager) connect(host string, port int, username, password, privateKeyPath string) (string, error) {
	opts := connectOptions{
		Host:           host,
		Port:           resolveSSHPort(port),
		Username:       username,
		Password:       password,
		PrivateKeyPath: privateKeyPath,
	}
	client, err := dialSSH(opts)
	if err != nil {
		return "", err
	}
	s := &sshSession{opts: opts, addr: fmt.Sprintf("%s:%d", opts.Host, opts.Port)}
	s.setClient(client)

	id := fmt.Sprintf("ssh-%d", time.Now().UnixNano())
	m.mu.Lock()
	m.sessions[id] = s
	m.mu.Unlock()
	return id, nil
}

// outputSink 收命令输出：写内存缓冲（给最终结果），同时上报活动与尾部快照。
//
// stdout / stderr 由 ssh 库用两个 goroutine 并发写入，所以缓冲必须加锁。
// 上报走 SDK 的跨进程活动表：宿主的看门狗据此续命，后台任务的 job_output 据此
// 显示「刚刚在刷什么」。没有这一步，远端跑 20 分钟的构建在本地就是 20 分钟零活动。
type outputSink struct {
	ctx context.Context
	mu  sync.Mutex
	buf strings.Builder
}

func (w *outputSink) Write(p []byte) (int, error) {
	w.mu.Lock()
	n, err := w.buf.Write(p)
	w.mu.Unlock()
	if n > 0 {
		dsc.TouchActivity(w.ctx)
		dsc.AppendOutput(w.ctx, p[:n])
	}
	return n, err
}

func (w *outputSink) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// isConnectionLoss 判断错误是否代表「传输层没了」，而不是命令自己失败。
func isConnectionLoss(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := err.Error()
	for _, s := range []string{"connection reset", "broken pipe", "client is closed", "use of closed", "connection lost", "unexpected EOF"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// runCommand 在一条 session 通道上跑命令：输出边到边读、边上报。
func runCommand(ctx context.Context, sess *ssh.Session, command string) (string, int, error) {
	sink := &outputSink{ctx: ctx}
	// 直接用 Writer，而不是 StdoutPipe：后者与 Wait 的先后顺序有约定（Wait 会在命令
	// 退出时关闭管道），先 Wait 再读完可能丢尾部；Writer 形式由库自己等拷贝完成。
	sess.Stdout = sink
	sess.Stderr = sink
	// 远端读 stdin 的命令（cat / read / 交互式脚本）必须收到 EOF，否则一直等下去；
	// 命令本身不喂输入。
	sess.Stdin = strings.NewReader("")

	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			// 只关这一条 session 通道（= 这条命令）。SSH 连接本身不动，
			// 后续 ssh_exec 照用同一条连接、同一个 session id。
			_ = sess.Close()
		case <-finished:
		}
	}()

	err := sess.Run(command)
	code := 0
	if err != nil {
		var ee *ssh.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitStatus()
		} else {
			code = 1
		}
		if ctx.Err() != nil {
			// 中断（看门狗/用户取消）与「命令自己失败」要分开：前者不是远端的错，
			// 而且连接还活着，模型可以继续用同一个 session。
			err = fmt.Errorf("command interrupted (%s); the SSH connection itself is still open: %w", ctx.Err(), err)
		}
	}
	return sink.String(), code, err
}

// exec 在指定会话上执行命令，返回合并输出、退出码与「本次是否重连过」。
//
// 每个命令开一条新 session 通道（SSH 协议本来如此，连接是复用的），所以 cd/env
// 不会跨命令保留——需要连续目录请写成 `cd /path && cmd`。
func (m *SSHSessionManager) exec(ctx context.Context, id, command string) (string, int, bool, error) {
	s, ok := m.lookup(id)
	if !ok {
		return "", 0, false, fmt.Errorf("session %s not found (call ssh_connect first; ssh_list shows live sessions)", id)
	}

	c := s.current()
	if c == nil {
		return "", 0, false, fmt.Errorf("session %s is closed", id)
	}
	sess, err := c.NewSession()
	reconnected := false
	if err != nil {
		// 通道都没开起来 ⇒ 命令一个字都没执行，重连后重试是安全的（不会重复执行）。
		if rerr := s.reconnect(); rerr != nil {
			return "", 0, false, fmt.Errorf("connection to %s is gone and reconnect failed: %v (original: %v)", s.addr, rerr, err)
		}
		reconnected = true
		nc := s.current()
		if nc == nil {
			return "", 0, true, fmt.Errorf("session %s is closed", id)
		}
		sess, err = nc.NewSession()
		if err != nil {
			return "", 0, true, err
		}
	}
	defer sess.Close()

	out, code, err := runCommand(ctx, sess, command)
	// ctx 已取消时不要走「掉线」分支：那是我们主动关掉这条通道，连接本身没坏。
	if err != nil && ctx.Err() == nil && isConnectionLoss(err) {
		// 命令可能已经跑了一半（甚至已经生效）：绝不自动重试，只把连接恢复好，
		// 并如实告诉模型「结果未知」——重复执行 rm/make/部署的代价比多问一句大得多。
		_ = s.reconnect()
		return out, code, true, fmt.Errorf("connection to %s dropped while the command was running: its outcome is unknown; the session is re-established, re-run the command only if repeating it is safe: %w", s.addr, err)
	}
	return out, code, reconnected, err
}

func (m *SSHSessionManager) lookup(id string) (*sshSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// list 列出会话及其连通状态（探测带 2 秒上限，避免挂住工具调用）。
func (m *SSHSessionManager) list() []SSHSessionInfo {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	sort.Strings(ids)

	out := make([]SSHSessionInfo, 0, len(ids))
	for _, id := range ids {
		s, ok := m.lookup(id)
		if !ok {
			continue
		}
		out = append(out, SSHSessionInfo{ID: id, Host: s.addr, Alive: s.alive(2 * time.Second)})
	}
	return out
}

func (m *SSHSessionManager) close(id string) bool {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if ok {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
	if !ok {
		return false
	}
	// 先停保活，再关连接：否则旧循环可能在关闭后又发一次请求。
	s.mu.Lock()
	stop := s.stopKA
	s.stopKA = nil
	c := s.client
	s.client = nil
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
	if c != nil {
		_ = c.Close()
	}
	return true
}

// SSHConnectResult ssh_connect 的结果。
type SSHConnectResult struct {
	Success   bool   `json:"success"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// SSHExecResult ssh_exec 的结果。
type SSHExecResult struct {
	Success  bool   `json:"success"`
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
	// Reconnected 本次调用前发现连接已断，已用同一 session id 重连（命令是否执行过
	// 见输出说明）。模型据此知道「远端进程状态可能已不同」，但不必重新 ssh_connect。
	Reconnected bool `json:"reconnected,omitempty"`
}

// SSHSessionInfo 一条会话的状态。
//
// 只给 id 不够：常驻连接会静默死掉（NAT 超时、对端重启），模型看到 id 还在就以为
// 能用。Alive 是现场探测的结果（带 2 秒上限）。
type SSHSessionInfo struct {
	ID    string `json:"id"`
	Host  string `json:"host"`
	Alive bool   `json:"alive"`
}

// SSHListResult ssh_list 的结果。
type SSHListResult struct {
	Success  bool             `json:"success"`
	Sessions []SSHSessionInfo `json:"sessions"`
}

// SSHCloseResult ssh_close 的结果。
type SSHCloseResult struct {
	Success bool   `json:"success"`
	Closed  bool   `json:"closed"`
	Error   string `json:"error,omitempty"`
}

// sshConnectView 构造 ssh_connect 结果的结构化视图（卡片）。
func sshConnectView(result string) (json.RawMessage, error) {
	var r SSHConnectResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	badge := &dsc.ViewBadge{Text: "connected", Tone: "green"}
	if !r.Success {
		badge = &dsc.ViewBadge{Text: "failed", Tone: "red"}
	}
	fields := []dsc.ViewField{{Key: "session_id", Value: r.SessionID}}
	if r.Error != "" {
		fields = append(fields, dsc.ViewField{Key: "error", Value: r.Error, Tone: "red"})
	}
	return dsc.CardView("SSH", badge, fields), nil
}

// sshExecView 构造 ssh_exec 结果的结构化视图（纯文本 + 退出码徽标）。
func sshExecView(result string) (json.RawMessage, error) {
	var r SSHExecResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	badge := &dsc.ViewBadge{Text: fmt.Sprintf("exit %d", r.ExitCode), Tone: "gray"}
	if r.Reconnected {
		badge = &dsc.ViewBadge{Text: "reconnected", Tone: "yellow"}
	}
	if !r.Success {
		badge = &dsc.ViewBadge{Text: "error", Tone: "red"}
	}
	body := r.Output
	if !r.Success && r.Error != "" {
		body = r.Error
	}
	return dsc.PlainView("SSH", badge, body), nil
}

// sshListView 构造 ssh_list 结果的结构化视图（表格）。
func sshListView(result string) (json.RawMessage, error) {
	var r SSHListResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	live := 0
	rows := make([]dsc.ViewRow, 0, len(r.Sessions))
	for _, s := range r.Sessions {
		state := "down"
		if s.Alive {
			state = "up"
			live++
		}
		rows = append(rows, dsc.ViewRow{"id": s.ID, "host": s.Host, "state": state})
	}
	return dsc.TableView("SSH Sessions", &dsc.ViewBadge{Text: fmt.Sprintf("%d", live), Tone: "teal"},
		[]dsc.ViewColumn{{Key: "id", Title: "id"}, {Key: "host", Title: "host"}, {Key: "state", Title: "state"}}, rows), nil
}

// sshCloseView 构造 ssh_close 结果的结构化视图（卡片），session_id 取自调用参数。
func sshCloseView(args json.RawMessage, result string) (json.RawMessage, error) {
	var r SSHCloseResult
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return nil, nil
	}
	var p struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(args, &p)
	badge := &dsc.ViewBadge{Text: "not found", Tone: "gray"}
	if r.Closed {
		badge = &dsc.ViewBadge{Text: "closed", Tone: "green"}
	}
	return dsc.CardView("SSH", badge, []dsc.ViewField{{Key: "session_id", Value: p.SessionID}}), nil
}

func main() {
	// ssh_connect
	connectSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "host": {"type": "string", "description": "SSH host (IP or hostname)"},
                        "port": {"type": "integer", "description": "SSH port, default 22"},
                        "username": {"type": "string", "description": "SSH username"},
                        "password": {"type": "string", "description": "Password for password auth (optional)"},
                        "private_key_path": {"type": "string", "description": "Path to a PEM private key for key auth (optional)"}
                },
                "required": ["host", "username"]
        }`)
	connectHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var p struct {
			Host           string `json:"host"`
			Port           int    `json:"port"`
			Username       string `json:"username"`
			Password       string `json:"password"`
			PrivateKeyPath string `json:"private_key_path"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if p.Host == "" || p.Username == "" {
			return "", fmt.Errorf("host and username are required")
		}
		p.Port = resolveSSHPort(p.Port)
		id, err := globalSSH.connect(p.Host, p.Port, p.Username, p.Password, p.PrivateKeyPath)
		res := SSHConnectResult{Success: err == nil, SessionID: id}
		if err != nil {
			res.Error = err.Error()
		}
		b, _ := json.Marshal(res)
		return string(b), nil
	}

	// ssh_exec
	execSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "session_id": {"type": "string", "description": "SSH session ID from ssh_connect"},
                        "command": {"type": "string", "description": "Command to run on the remote host"}
                },
                "required": ["session_id", "command"]
        }`)
	execHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var p struct {
			SessionID string `json:"session_id"`
			Command   string `json:"command"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		if p.SessionID == "" || p.Command == "" {
			return "", fmt.Errorf("session_id and command are required")
		}
		out, code, reconnected, err := globalSSH.exec(ctx, p.SessionID, p.Command)
		res := SSHExecResult{Success: err == nil, Output: out, ExitCode: code, Reconnected: reconnected}
		if err != nil {
			res.Error = err.Error()
		}
		b, _ := json.Marshal(res)
		return string(b), nil
	}

	// ssh_list
	listSchema := json.RawMessage(`{
                "type": "object",
                "properties": {}
        }`)
	listHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		b, _ := json.Marshal(SSHListResult{Success: true, Sessions: globalSSH.list()})
		return string(b), nil
	}

	// ssh_close
	closeSchema := json.RawMessage(`{
                "type": "object",
                "properties": {
                        "session_id": {"type": "string", "description": "SSH session ID to close"}
                },
                "required": ["session_id"]
        }`)
	closeHandler := func(ctx context.Context, args json.RawMessage) (string, error) {
		var p struct {
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(args, &p); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
		res := SSHCloseResult{Success: true, Closed: globalSSH.close(p.SessionID)}
		b, _ := json.Marshal(res)
		return string(b), nil
	}

	sdk := dsc.New(dsc.Config{
		Name:    "ssh",
		Version: "1.0.0",
		Type:    dsc.TypeTool,
		Provides: map[string]string{
			// 提供 "ssh" 能力：含 ssh_connect/ssh_exec/ssh_list/ssh_close 远程会话工具
			"ssh": "true",
		},
	})
	sdk.Tool(dsc.Tool{Name: "ssh_connect", Description: "Connect to a remote host over SSH and return a session ID that stays valid for the rest of the conversation. The connection is kept open with a keepalive until you call ssh_close; if the transport drops, the next ssh_exec re-establishes it under the same ID (the result then says reconnected:true).", Schema: connectSchema, Handler: connectHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return sshConnectView(result)
		},
	})
	sdk.Tool(dsc.Tool{Name: "ssh_exec", Description: "Run a command on an existing SSH session and return its combined output and exit code. Each call opens a fresh channel on the persistent connection, so working directory and environment do NOT carry over from the previous call — combine steps in one command (\"cd /path && make\"). Output is streamed back as it arrives (long commands report activity instead of looking stuck), and cancelling a call stops only that command: the connection and the session ID stay usable.", Schema: execSchema, Handler: execHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return sshExecView(result)
		},
	})
	sdk.Tool(dsc.Tool{Name: "ssh_list", Description: "List SSH sessions with host and live state. A session reported alive:false has lost its transport; the next ssh_exec reconnects it under the same ID, so you do not need to connect again.", Schema: listSchema, Handler: listHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return sshListView(result)
		},
	})
	sdk.Tool(dsc.Tool{Name: "ssh_close", Description: "Close an SSH session by ID. This terminates any command still running on that session and drops the connection.", Schema: closeSchema, Handler: closeHandler,
		ViewFn: func(ctx context.Context, args json.RawMessage, result string) (json.RawMessage, error) {
			return sshCloseView(args, result)
		},
	})
	sdk.Serve()
}
