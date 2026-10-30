package core

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	plugin "github.com/hashicorp/go-plugin"
)

// 本文件覆盖「策略 advisory 上下文（notices）」与「文件观察策略裁决」两条宿主
// 聚合链（AGENTS 字段保真规则）：真实 dsc-system 插件进程（通用类型 + services
// 声明 "policy"）经生产同款 registerDscCoreLocked 桥接工具流水线后，宿主聚合工具
// 调用须把策略裁决的 notice 机械收集并透传到调用方（ExecuteToolWithView 第 4
// 返回值 → 聚合 Tool 服务的 ExecuteToolResponse.notices），成功与失败调用同样
// 携带；文件观察策略的放行/拒绝同样须经宿主这一跳原样生效。任何一层只返回子集
// 都会让提醒或裁决静默丢失——此处用真实插件进程端到端锁死。

// TestHostDscPolicyChainPropagatesNotices 真实 dsc-system 进程 → 宿主 policy 桥
// → 工具流水线 → notices 透传的端到端断言。
func TestHostDscPolicyChainPropagatesNotices(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "dsc-system.exe")
	buildToolBin(t, filepath.Join("..", "plugins", "dsc-system"), exe)

	// 1. spawn 真实插件进程（阈值 2 便于断言；排除表清空避免宿主 env 泄漏干扰）
	cmd := exec.Command(exe)
	ConfigureChildProcessAttrs(cmd)
	cmd.Env = append(os.Environ(), "DSC_REPEAT_THRESHOLDS=2", "DSC_REPEAT_EXCLUDE=")
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          map[string]plugin.Plugin{"dsc_core": &DSCPluginGRPC{}},
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Cmd:              cmd,
	})
	defer client.Kill()
	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	grpcClient, ok := rpcClient.(*plugin.GRPCClient)
	if !ok {
		t.Fatalf("client %T not *GRPCClient", rpcClient)
	}
	info, err := GetPluginInfo(grpcClient.Conn)
	if err != nil || info.Type != "dsc" {
		t.Fatalf("GetPluginInfo = %+v, err %v（期望 dsc 通用类型）", info, err)
	}

	// 2. 生产同款登记：case "dsc" 按 PluginInfo.services 的 "policy" 声明桥接流水线
	m := NewManager(&ManagerConfig{ExecDir: dir})
	m.mu.Lock()
	m.registerDscCoreLocked("dsc-system", info, client, grpcClient, nil, interconnectRefs{})
	m.mu.Unlock()
	t.Cleanup(func() { m.Shutdown() })
	if _, bridged := m.policyOff["dsc-system"]; !bridged {
		t.Fatalf("services 声明 policy 的 dsc 插件应被桥接到工具流水线")
	}

	// 3. 注册宿主侧被观察工具并连续调用同一参数
	if err := m.toolRegistry.Register(&mockTool{name: "plain-tool"}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	exec := func() (string, []string, error) {
		ctx := WithCaller(context.Background(), "sess-1")
		result, _, _, notices, err := m.ExecuteToolWithView(ctx, "plain-tool", json.RawMessage(`{"k":1}`))
		return result, notices, err
	}
	if _, notices, err := exec(); err != nil || len(notices) != 0 {
		t.Fatalf("call 1: err=%v notices=%v（首调未达阈值应无提醒）", err, notices)
	}
	_, notices, err := exec()
	if err != nil {
		t.Fatalf("call 2: %v", err)
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "repeating the exact same tool call") {
		t.Fatalf("call 2 应携带重复提醒 notice，got %v", notices)
	}

	// 4. 用户插话重置：pre-step 事件（waterfall）清链后重新计数
	m.dispatchEventToPlugins(EventAgentPreStep, AgentPreStepEvent{
		Agent: "agent-react-loop", Session: "sess-1", UserInput: true,
	})
	if _, notices, err := exec(); err != nil || len(notices) != 0 {
		t.Fatalf("after user input reset: err=%v notices=%v（链应已重置）", err, notices)
	}
	_, notices, err = exec()
	if err != nil || len(notices) != 1 {
		t.Fatalf("reset 后第 2 调应再次提醒: err=%v notices=%v", err, notices)
	}
}

// TestHostDscPolicyChainHonorsWorkspacePathForms 文件观察策略的宿主聚合链回归
// （报告场景）：模型以 /workspace 虚拟根别名或工作区相对路径书写时，真实
// dsc-system 进程在 view 成功后不得把紧随的编辑误判为「文件不存在」。
//
// 报告：在 C:\Users\einpc\Desktop\steelfront 启动 dsc，模型 view
// /workspace/assets/game.html 成功，紧接着 str_replace 同路径被拒
// （cannot modify ...: file does not exist — create it with the 'create' command
// first）——策略的存在性探针误拿模型书写的原始路径去 os.Stat，与观察键
// （keyFor 归并后的真实路径）分叉：/workspace 落到宿主盘根、相对路径落到插件
// 进程 cwd，两者都被记成「不存在」。
//
// 本用例覆盖「宿主聚合层（会话号 + 参数 JSON 转发）→ 真实插件裁决」整跳：只测
// 插件侧会漏掉宿主转发这一跳，只测宿主侧（假策略）会漏掉真实路径语义。
func TestHostDscPolicyChainHonorsWorkspacePathForms(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "dsc-system.exe")
	buildToolBin(t, filepath.Join("..", "plugins", "dsc-system"), exe)

	// 工作区根经 DSC_WORKSPACE_ROOT 注入（对齐宿主 injectRuntimeEnv），文件真实存在
	ws := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(filepath.Join(ws, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	game := filepath.Join(ws, "assets", "game.html")
	if err := os.WriteFile(game, []byte("<html></html>\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(exe)
	ConfigureChildProcessAttrs(cmd)
	cmd.Env = append(os.Environ(), "DSC_WORKSPACE_ROOT="+filepath.ToSlash(ws))
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  Handshake,
		Plugins:          map[string]plugin.Plugin{"dsc_core": &DSCPluginGRPC{}},
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Cmd:              cmd,
	})
	defer client.Kill()
	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	grpcClient, ok := rpcClient.(*plugin.GRPCClient)
	if !ok {
		t.Fatalf("client %T not *GRPCClient", rpcClient)
	}
	info, err := GetPluginInfo(grpcClient.Conn)
	if err != nil || info.Type != "dsc" {
		t.Fatalf("GetPluginInfo = %+v, err %v（期望 dsc 通用类型）", info, err)
	}

	m := NewManager(&ManagerConfig{ExecDir: dir})
	m.mu.Lock()
	m.registerDscCoreLocked("dsc-system", info, client, grpcClient, nil, interconnectRefs{})
	m.mu.Unlock()
	t.Cleanup(func() { m.Shutdown() })

	// 同名工具接入注册表：本用例只关心策略裁决，工具本体是桩（观察记录由
	// post-execute 事件写入，不依赖工具真实落盘）
	if err := m.toolRegistry.Register(&mockTool{name: "str_replace_editor"}); err != nil {
		t.Fatalf("register tool: %v", err)
	}
	call := func(command, path string) error {
		ctx := WithCaller(context.Background(), "sess-alias")
		args, err := json.Marshal(map[string]string{"command": command, "path": path})
		if err != nil {
			t.Fatalf("marshal args: %v", err)
		}
		_, _, _, _, execErr := m.ExecuteToolWithView(ctx, "str_replace_editor", args)
		return execErr
	}
	for _, p := range []string{"/workspace/assets/game.html", "assets/game.html"} {
		if err := call("view", p); err != nil {
			t.Fatalf("view(%s) 应放行: %v", p, err)
		}
		if err := call("str_replace", p); err != nil {
			t.Fatalf("view 成功后同路径改写(%s)不得被拒: %v", p, err)
		}
	}
}
