package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"dsc-sdk"
)

// dropSession 清理测试建出来的会话，避免污染全局池（池是进程级的）。
func dropSession(t *testing.T, key string) {
	t.Helper()
	t.Cleanup(func() {
		globalSessionManager.mu.Lock()
		delete(globalSessionManager.sessions, key)
		globalSessionManager.mu.Unlock()
	})
}

// TestSessionKeyPersistsByDefault 默认就是持久：不传 session_id 时绑定到调用方会话，
// 两次调用必须落到同一个键（否则等于每条命令一个 shell，cd 白写）。
func TestSessionKeyPersistsByDefault(t *testing.T) {
	ctx := dsc.WithCallInfo(context.Background(), dsc.CallInfo{SessionID: "conv-1"})
	k1 := sessionKeyFor(ctx, "")
	k2 := sessionKeyFor(ctx, "")
	if k1 != k2 || k1 != "conv:conv-1" {
		t.Fatalf("默认键应稳定绑定调用方会话，got %q / %q", k1, k2)
	}

	// 不同会话互不干扰
	other := sessionKeyFor(dsc.WithCallInfo(context.Background(), dsc.CallInfo{SessionID: "conv-2"}), "")
	if other == k1 {
		t.Fatalf("不同调用方会话应得到不同键，got %q", other)
	}

	// 宿主没给身份：退化成进程内单例，仍然稳定
	if got := sessionKeyFor(context.Background(), ""); got != defaultSessionKey {
		t.Fatalf("无调用方身份时应退回 %q，got %q", defaultSessionKey, got)
	}

	// 显式 "new"：每次都是新键
	a := sessionKeyFor(ctx, "new")
	b := sessionKeyFor(ctx, "new")
	if a == b || !strings.HasPrefix(a, "shell-") {
		t.Fatalf("\"new\" 应每次生成新键，got %q / %q", a, b)
	}

	// 显式名字：按模型给的名字分组，不受调用方会话影响
	if got := sessionKeyFor(ctx, "task-a"); got != "task-a" {
		t.Fatalf("显式 session_id 应原样使用，got %q", got)
	}
}

// TestShellStatePersistsAcrossCalls 真实行为验证：cd 在同一会话内保持。
func TestShellStatePersistsAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := dsc.WithCallInfo(context.Background(), dsc.CallInfo{SessionID: "persist-1"})
	key := sessionKeyFor(ctx, "")
	dropSession(t, key)

	sess, err := getOrCreateSession(key, dir)
	if err != nil {
		t.Fatalf("getOrCreateSession: %v", err)
	}
	if _, _, err := execSessionCommand(ctx, sess, "cd sub && export DSCTEST=1"); err != nil {
		t.Fatalf("cd: %v", err)
	}
	out, _, err := execSessionCommand(ctx, sess, "pwd; echo $DSCTEST")
	if err != nil {
		t.Fatalf("pwd: %v", err)
	}
	if !strings.Contains(out, "sub") || !strings.Contains(out, "1") {
		t.Fatalf("会话应保持 cwd 与环境变量，got %q", out)
	}
}

// TestSessionNewStartsClean 显式 "new" 拿到干净 shell（cwd 与变量都不继承）。
func TestSessionNewStartsClean(t *testing.T) {
	dir := t.TempDir()
	ctx := dsc.WithCallInfo(context.Background(), dsc.CallInfo{SessionID: "clean-1"})
	baseKey := sessionKeyFor(ctx, "")
	dropSession(t, baseKey)

	sess, err := getOrCreateSession(baseKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := execSessionCommand(ctx, sess, "cd / && export DSCCLEAN=1"); err != nil {
		t.Fatal(err)
	}

	newKey := sessionKeyFor(ctx, "new")
	dropSession(t, newKey)
	fresh, err := getOrCreateSession(newKey, dir)
	if err != nil {
		t.Fatal(err)
	}
	out, _, _ := execSessionCommand(ctx, fresh, "pwd; echo \"[$DSCCLEAN]\"")
	if strings.Contains(out, "[1]") {
		t.Fatalf("新会话不应继承变量，got %q", out)
	}
	if strings.Contains(strings.ToLower(out), strings.ToLower("/\n")) && !strings.Contains(out, filepath.Base(dir)) {
		t.Fatalf("新会话不应继承 cwd，got %q", out)
	}
}

// TestWorkdirAppliesToReusedSession 复用会话时 workdir 必须生效——此前是直接 return，
// 模型按描述传 workdir 会被静默忽略，命令照旧跑在旧目录。
func TestWorkdirAppliesToReusedSession(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	ctx := context.Background()
	key := "workdir-reuse"
	dropSession(t, key)

	s1, err := getOrCreateSession(key, dirA)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := getOrCreateSession(key, dirB) // 同一键、换目录
	if err != nil {
		t.Fatal(err)
	}
	if s1 != s2 {
		t.Fatal("同一 session_id 应复用同一个 runner")
	}
	out, _, err := execSessionCommand(ctx, s2, "pwd")
	if err != nil {
		t.Fatal(err)
	}
	if !samePath(strings.TrimSpace(out), dirB) {
		t.Fatalf("workdir 应切到 %q，实际 pwd=%q", dirB, out)
	}

	// 不传 workdir 的调用不得把目录改回去
	out, _, _ = execSessionCommand(ctx, s2, "pwd")
	if !samePath(strings.TrimSpace(out), dirB) {
		t.Fatalf("不传 workdir 时应保持当前目录，got %q", out)
	}
}

// samePath 比较两个路径是否指向同一位置（Windows 上大小写/斜杆方向差异不算不同）。
func samePath(a, b string) bool {
	if strings.EqualFold(filepath.Clean(a), filepath.Clean(b)) {
		return true
	}
	ra, err1 := filepath.EvalSymlinks(a)
	rb, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && strings.EqualFold(filepath.Clean(ra), filepath.Clean(rb))
}

// TestSessionEvictionIsLRU 满池淘汰最久未用的，而不是 map 里随便一个：否则正在用的
// 会话会被踢掉，模型下一轮拿到全新 shell，cwd「莫名失忆」。
func TestSessionEvictionIsLRU(t *testing.T) {
	globalSessionManager.mu.Lock()
	saved := globalSessionManager.sessions
	globalSessionManager.sessions = make(map[string]*Session)
	globalSessionManager.mu.Unlock()
	t.Cleanup(func() {
		globalSessionManager.mu.Lock()
		globalSessionManager.sessions = saved
		globalSessionManager.mu.Unlock()
	})

	now := time.Now()
	// 填满池子：0 号最旧，63 号最新
	for i := 0; i < maxSessions; i++ {
		globalSessionManager.sessions["old-"+itoa(i)] = &Session{lastUsed: now.Add(time.Duration(i-maxSessions) * time.Minute)}
	}
	// 再创建一个新会话 → 应淘汰 old-0（最旧）
	if _, err := getOrCreateSession("newcomer", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	globalSessionManager.mu.RLock()
	_, hasOldest := globalSessionManager.sessions["old-0"]
	_, hasNewest := globalSessionManager.sessions["old-"+itoa(maxSessions-1)]
	_, hasNew := globalSessionManager.sessions["newcomer"]
	globalSessionManager.mu.RUnlock()
	if hasOldest {
		t.Fatal("最久未用的会话应被淘汰")
	}
	if !hasNewest || !hasNew {
		t.Fatal("最近用过的会话和新会话都应保留")
	}
	if len(globalSessionManager.sessions) != maxSessions {
		t.Fatalf("池大小应保持上限 %d，got %d", maxSessions, len(globalSessionManager.sessions))
	}
}

// itoa 避免为整数引入额外依赖（包内测试辅助）。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestWorkdirUsesVirtualRootTable workdir 与命令里的路径走同一张表：
// Windows 上 /tmp 映射到临时目录（此前 workdir 走的是只认 /workspace 的旧入口，
// /tmp 直接落到当前盘根）。
func TestWorkdirUsesVirtualRootTable(t *testing.T) {
	temp := t.TempDir()
	t.Setenv("DSC_TEMP_DIR", temp)

	got := resolveSessionWorkdir("/tmp")
	if runtime.GOOS == "windows" {
		if !samePath(got, temp) {
			t.Fatalf("windows workdir=/tmp 应落到临时目录 %q，got %q", temp, got)
		}
	} else if got != "/tmp" {
		t.Fatalf("POSIX 上 /tmp 是真实挂载点，应原样，got %q", got)
	}

	// /workspace 仍然映射到工作区根
	setRoot := dsc.WorkspaceRoot()
	ws := t.TempDir()
	dsc.SetWorkspaceRoot(ws)
	t.Cleanup(func() { dsc.SetWorkspaceRoot(setRoot) })
	if got := resolveSessionWorkdir("/workspace/sub"); !samePath(got, filepath.Join(ws, "sub")) {
		t.Fatalf("/workspace/sub 应映射到工作区根下，got %q", got)
	}
}
