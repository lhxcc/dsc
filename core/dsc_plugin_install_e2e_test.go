package core

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"dsc/proto"
)

// TestInstallUninstallDscPluginE2E 「模型自助安装/卸载插件」的端到端路径。
//
// 这条路径与 load/unload 的区别正是它容易出错的地方：install 要先把来源拷贝到
// plugins/<name>/（且执行文件名必须等于 <name><ext>，否则插件起来也找不到入口），
// 再经 LoadPlugin 完成「写 config + 热加载」，失败还要回滚目录。任何一步漏了，
// 现象都只是"装完重启就没了"或"目录在、插件不在"，事后极难排查。
//
// 因此这里用**真实插件二进制**跑完整链路，并逐项断言中间产物：目录、执行文件、
// config 条目、工具可用性；卸载后再逐项断言这些都被清掉。
func TestInstallUninstallDscPluginE2E(t *testing.T) {
	tmpDir := t.TempDir()
	repoRoot := ".." // core 包目录之上即仓库根

	// 真实 agent 提供 broker（热加载插件需要它）
	agentExe := PJoin(tmpDir, "agent-react-loop"+binExt())
	buildAgentBin(t, PJoin(repoRoot, "plugins", "agent-react-loop"), agentExe)

	// 待安装的来源：目录布局 plugins/<name>/，执行文件必须叫 <name><ext>
	const name = "tool-lisp-demo"
	srcDir := PJoin(t.TempDir(), name)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	buildToolBin(t, PJoin(repoRoot, "plugins", "tool-lisp-eval"), PJoin(srcDir, name+binExt()))

	cfgPath := PJoin(tmpDir, "config", "config.yaml")
	if err := os.MkdirAll(PDir(cfgPath), 0o755); err != nil {
		t.Fatalf("mkdir config: %v", err)
	}
	if err := os.WriteFile(cfgPath, []byte("default_llm: mock-llm\nplugins: []\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	m := NewManager(&ManagerConfig{ExecDir: tmpDir})
	m.SetConfigPath(cfgPath)
	loadTestAgent(t, m, "agent-react-loop", agentExe, tmpDir)
	t.Cleanup(func() { m.Shutdown() })

	install := &installDscPluginTool{m: m}
	uninstall := &uninstallDscPluginTool{m: m}
	agg := NewToolGRPCServer(m)

	installedExe := PJoin(tmpDir, "plugins", name, name+binExt())
	configHas := func() bool {
		b, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("read config: %v", err)
		}
		return strings.Contains(string(b), name)
	}
	// runLisp 经宿主聚合服务执行插件工具——这正是 agent 真实走的链路。
	runLisp := func(wantErr bool) {
		t.Helper()
		resp, err := agg.ExecuteTool(context.Background(), &proto.ExecuteToolRequest{
			ToolName:      "lisp_eval",
			ArgumentsJson: `{"expression":"(+ 1 2)"}`,
		})
		if err != nil {
			t.Fatalf("ExecuteTool(lisp_eval) rpc: %v", err)
		}
		if wantErr {
			if resp.Error == "" {
				t.Fatalf("卸载后工具应不可用，却成功返回 %q", resp.Content)
			}
			return
		}
		if resp.Error != "" || !strings.Contains(resp.Content, "3") {
			t.Fatalf("安装后工具应可用: err=%q content=%q", resp.Error, resp.Content)
		}
	}

	// ---- 安装前：工具不存在、目录不存在 ----
	if _, err := os.Stat(installedExe); !os.IsNotExist(err) {
		t.Fatalf("安装前不应存在 %s", installedExe)
	}
	runLisp(true)

	// ---- 安装 ----
	args, _ := json.Marshal(map[string]any{"type": "tool", "name": name, "source": srcDir})
	if _, err := install.Execute(context.Background(), args); err != nil {
		t.Fatalf("install: %v", err)
	}
	if _, err := os.Stat(installedExe); err != nil {
		t.Fatalf("安装后应存在 %s: %v", installedExe, err)
	}
	if !configHas() {
		t.Fatalf("安装应把 %s 写进 config（否则重启即丢）:\n%s", name, mustRead(t, cfgPath))
	}
	runLisp(false)

	// ---- 安装同名插件必须失败，且不得破坏已装的那一份 ----
	if _, err := install.Execute(context.Background(), args); err == nil {
		t.Fatal("重复安装同名插件应报错")
	}
	if _, err := os.Stat(installedExe); err != nil {
		t.Fatalf("重复安装失败后原有安装不得被破坏: %v", err)
	}

	// ---- 卸载（含删目录）----
	uargs, _ := json.Marshal(map[string]any{"name": name, "delete_dir": true})
	if _, err := uninstall.Execute(context.Background(), uargs); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(PJoin(tmpDir, "plugins", name)); !os.IsNotExist(err) {
		t.Fatalf("delete_dir=true 应删除插件目录")
	}
	if configHas() {
		t.Fatalf("卸载应把 %s 从 config 移除:\n%s", name, mustRead(t, cfgPath))
	}
	runLisp(true)
}

// mustRead 读文件内容（失败即 Fatal），只用于断言失败时的上下文展示。
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "(read failed: " + err.Error() + ")"
	}
	return string(b)
}
