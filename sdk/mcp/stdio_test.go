package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// stdio 传输的测试用「把测试二进制自己当服务器」的经典做法：
// 同一个 test 可执行文件带 GO_WANT_MCP_STDIO_HELPER=1 启动时进入 helper 分支，
// 从 stdin 读 JSON-RPC、往 stdout 写响应。好处是不用额外编译产物、不需要网络，
// 而且测的正是真实进程管道（fork/exec、stdin/stdout/stderr 三个管道、进程退出）。
const (
	helperEnv  = "GO_WANT_MCP_STDIO_HELPER"
	helperArg  = "-test.run=^TestStdioHelperServer$"
	largeBytes = 2 << 20 // 2 MiB：验证大结果不被行读取截断
)

// TestStdioHelperServer 是 helper 本体；正常测试运行时它会跳过。
func TestStdioHelperServer(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper process only (spawned by the stdio transport tests)")
	}
	runStdioHelper(os.Stdin, os.Stdout, os.Stderr)
	os.Exit(0) // 不回到测试框架：它会往 stdout 打印结果，污染 JSON-RPC 流
}

// runStdioHelper 是一个最小 MCP 服务器（只实现测试用到的部分）。
//
// 它刻意制造三种「客户端必须处理对」的情形：
//   - 初始化后主动向客户端发一个采样请求（客户端应回 -32601，而不是静默挂起）；
//   - stderr 灌一大段噪音（客户端若不消费 stderr 管道，进程会卡在写操作上）；
//   - 提供会退出进程的工具（客户端必须报错而不是一直等）。
func runStdioHelper(stdin *os.File, stdout, stderr *os.File) {
	in := bufio.NewReaderSize(stdin, 64*1024)
	out := bufio.NewWriterSize(stdout, 64*1024)
	write := func(v any) {
		b, err := json.Marshal(v)
		if err != nil {
			return
		}
		_, _ = out.Write(append(b, '\n'))
		_ = out.Flush()
	}
	result := func(id any, r any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": id, "result": r}
	}

	// stderr 噪音：超过管道缓冲的常规大小，用来证明客户端在持续消费它。
	fmt.Fprintf(stderr, "%s\n", strings.Repeat("stderr-noise ", 64*1024))
	_ = stderr.Sync()
	replySeen := false

	for {
		line, err := in.ReadBytes('\n')
		if s := strings.TrimSpace(string(line)); s != "" {
			var msg map[string]any
			if json.Unmarshal([]byte(s), &msg) == nil {
				method, _ := msg["method"].(string)
				id := msg["id"]
				switch method {
				case "initialize":
					write(result(id, map[string]any{
						"protocolVersion": "2024-11-05",
						"capabilities":    map[string]any{"tools": map[string]any{}},
						"serverInfo":      map[string]any{"name": "stdio-helper", "version": "1.0.0"},
					}))
					// 主动向客户端发请求：它必须回 -32601，我们据此判定客户端没有静默挂起
					write(map[string]any{
						"jsonrpc": "2.0", "id": "srv-1", "method": "sampling/createMessage",
						"params": map[string]any{},
					})
				case "notifications/initialized":
					// 通知无响应
				case "tools/list":
					write(result(id, map[string]any{"tools": []any{
						map[string]any{"name": "echo", "description": "echo args", "inputSchema": map[string]any{"type": "object"}},
						map[string]any{"name": "big", "description": "large result", "inputSchema": map[string]any{"type": "object"}},
						map[string]any{"name": "die", "description": "exit the server", "inputSchema": map[string]any{"type": "object"}},
						map[string]any{"name": "reply_seen", "description": "did client answer our request", "inputSchema": map[string]any{"type": "object"}},
					}}))
				case "tools/call":
					params, _ := msg["params"].(map[string]any)
					name, _ := params["name"].(string)
					switch name {
					case "echo":
						write(result(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": "pong"}}}))
					case "big":
						write(result(id, map[string]any{"content": []any{map[string]any{
							"type": "text", "text": strings.Repeat("x", largeBytes),
						}}}))
					case "die":
						os.Exit(3)
					case "reply_seen":
						write(result(id, map[string]any{"content": []any{map[string]any{
							"type": "text", "text": fmt.Sprintf("replySeen=%v", replySeen),
						}}}))
					default:
						write(result(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": "unknown tool"}}}))
					}
				}
			}
		}
		if err != nil {
			return
		}
		// 任何带 id 且非本次请求的响应 = 客户端对我们 srv-1 请求的答复
		if strings.Contains(strings.TrimSpace(string(line)), `"srv-1"`) {
			replySeen = true
		}
	}
}

// stdioHelperSpec 构造指向 helper 的启动规格。
func stdioHelperSpec(t *testing.T) StdioSpec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	return StdioSpec{Command: exe, Args: []string{helperArg}, Env: map[string]string{helperEnv: "1"}}
}

// TestStdioConnectAndCall 端到端：stdio 连接 → 发现工具 → 调用工具 → 关闭进程。
func TestStdioConnectAndCall(t *testing.T) {
	c, err := NewStdioClient("helper", stdioHelperSpec(t), nil)
	if err != nil {
		t.Fatalf("new stdio client: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer c.Disconnect()

	if kind := c.Kind(); kind != "stdio" {
		t.Fatalf("TransportKind = %q, want stdio", kind)
	}
	tools := c.Tools()
	if len(tools) != 4 {
		t.Fatalf("工具数 = %d, want 4: %+v", len(tools), tools)
	}
	if tools[0].Name != "big" && tools[0].Name != "die" &&
		tools[0].Name != "echo" && tools[0].Name != "reply_seen" {
		t.Fatalf("工具命名不符: %+v", tools)
	}

	out, err := c.CallTool(ctx, "echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool echo: %v", err)
	}
	if out != "pong" {
		t.Fatalf("echo 返回 %q, want pong", out)
	}
}

// TestStdioLargeResultNotTruncated 工具结果可能上兆：行读取不能有 token 上限。
// 这条防的是「把 bufio.Scanner 换回来」——默认 64K token 上限会把大结果截断成
// invalid JSON，现场表现是「某个工具一调就报解析失败」。
func TestStdioLargeResultNotTruncated(t *testing.T) {
	c, err := NewStdioClient("helper", stdioHelperSpec(t), nil)
	if err != nil {
		t.Fatalf("new stdio client: %v", err)
	}
	defer c.Disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	out, err := c.CallTool(ctx, "big", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool big: %v", err)
	}
	if len(out) != largeBytes {
		t.Fatalf("大结果长度 = %d, want %d（说明被截断了）", len(out), largeBytes)
	}
}

// TestStdioAnswersServerRequests 服务器反向发请求时必须回 -32601：
// 不声明 sampling / roots 能力的客户端若不回，服务器可能一直等，表现为「卡住」。
func TestStdioAnswersServerRequests(t *testing.T) {
	c, err := NewStdioClient("helper", stdioHelperSpec(t), nil)
	if err != nil {
		t.Fatalf("new stdio client: %v", err)
	}
	defer c.Disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	out, err := c.CallTool(ctx, "reply_seen", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool reply_seen: %v", err)
	}
	if out != "replySeen=true" {
		t.Fatalf("服务器没收到我们对它请求的答复: %q", out)
	}
}

// TestStdioProcessExitSurfacesError 子进程死了要立刻报错（带原因），不能挂到超时：
// 否则一次崩溃会把整轮对话卡在「工具调用中」。
func TestStdioProcessExitSurfacesError(t *testing.T) {
	c, err := NewStdioClient("helper", stdioHelperSpec(t), nil)
	if err != nil {
		t.Fatalf("new stdio client: %v", err)
	}
	defer c.Disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	start := time.Now()
	_, err = c.CallTool(ctx, "die", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("进程退出后调用应报错")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Fatalf("错误应说明进程退出，got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("应立刻失败，实际等了 %v", elapsed)
	}
}

// TestStdioEnvAllowlist 子进程环境白名单：宿主的密钥不能顺手继承，
// 需要什么必须在 spec.Env 里显式写。
func TestStdioEnvAllowlist(t *testing.T) {
	t.Setenv("DSC_MCP_TEST_SECRET", "leak-me-not")
	t.Setenv("MCP_TEST_NON_WHITELISTED", "also-not-inherited")

	env := stdioEnv(map[string]string{"MY_TOKEN": "tok"})
	joined := strings.Join(env, "\n")
	lower := strings.ToLower(joined)

	for _, banned := range []string{"dsc_mcp_test_secret", "mcp_test_non_whitelisted"} {
		if strings.Contains(lower, banned) {
			t.Fatalf("非白名单变量 %q 被继承给 MCP 子进程:\n%s", banned, joined)
		}
	}
	if !strings.Contains(joined, "MY_TOKEN=tok") {
		t.Fatalf("显式 env 必须传给子进程:\n%s", joined)
	}
	if !strings.Contains(lower, "path=") {
		t.Fatalf("基线必须包含 PATH（否则命令都找不到）:\n%s", joined)
	}
}
