package main

import (
	"os"
	"path/filepath"
	"testing"

	core "dsc/core"
	"gopkg.in/yaml.v3"
)

// TestAssembleMergedConfigToolsDedup 验证 config.yaml 中启用的 tool/dsc 条目
// 并入合并集（模型安装的插件可跨重启生效），并与 preset 按名去重——同名冲突取 preset。
// TestInjectContextWindow 上下文窗口须下发到所有插件（不只 agent）：压缩后端按它算
// 触发阈值，缺省会退回 128K，云端 1M 窗口时几步就压缩。显式配置不得被覆盖。
func TestInjectContextWindow(t *testing.T) {
	merged := &core.Config{Plugins: []core.PluginEntry{
		{Name: "agent-x", Type: "agent", Env: map[string]string{"DSC_CONTEXT_WINDOW": "1048576"}},
		{Name: "dsc-system", Type: "dsc"},
		{Name: "tool-x", Type: "tool", Env: map[string]string{"DSC_CONTEXT_WINDOW": "65536"}},
	}}
	injectContextWindow(merged, 1048576)

	if got := merged.Plugins[1].Env["DSC_CONTEXT_WINDOW"]; got != "1048576" {
		t.Fatalf("未配置的插件应获得有效窗口值，got %q", got)
	}
	if got := merged.Plugins[2].Env["DSC_CONTEXT_WINDOW"]; got != "65536" {
		t.Fatalf("显式配置不得被覆盖，got %q", got)
	}
	if got := merged.Plugins[0].Env["DSC_CONTEXT_WINDOW"]; got != "1048576" {
		t.Fatalf("agent 已带同值，应保持不变，got %q", got)
	}

	// window<=0 → 不注入
	none := &core.Config{Plugins: []core.PluginEntry{{Name: "x", Type: "tool"}}}
	injectContextWindow(none, 0)
	if _, ok := none.Plugins[0].Env["DSC_CONTEXT_WINDOW"]; ok {
		t.Fatal("窗口未知时不应注入")
	}
}

// TestInjectMaxOutputTokensRespectsExplicit 验证注入不覆盖插件 env 里显式写的
// DSC_MAX_OUTPUT_TOKENS（含 -1=不携带）——否则示例配置里的 -1 会被静默改成窗口值。
func TestInjectMaxOutputTokensRespectsExplicit(t *testing.T) {
	merged := &core.Config{Plugins: []core.PluginEntry{
		{Name: "llm-anthropic", Type: "llm", Env: map[string]string{"DSC_MAX_OUTPUT_TOKENS": "-1"}},
		{Name: "llm-openai", Type: "llm"},
		{Name: "tool-x", Type: "tool"},
	}}
	injectMaxOutputTokens(merged, 1048576)

	if got := merged.Plugins[0].Env["DSC_MAX_OUTPUT_TOKENS"]; got != "-1" {
		t.Fatalf("显式 -1 不得被覆盖，got %q", got)
	}
	if got := merged.Plugins[1].Env["DSC_MAX_OUTPUT_TOKENS"]; got != "1048576" {
		t.Fatalf("未显式配置时应注入窗口值，got %q", got)
	}
	if _, ok := merged.Plugins[2].Env["DSC_MAX_OUTPUT_TOKENS"]; ok {
		t.Fatal("非 LLM 插件不应被注入 max_tokens")
	}
}

func TestAssembleMergedConfigToolsDedup(t *testing.T) {
	mainCfg := &core.Config{Plugins: []core.PluginEntry{
		{Name: "agent-react-loop", Type: "agent", Enabled: true, BinaryPath: "./plugins/agent-react-loop/a.exe"},
		{Name: "llm-openai", Type: "llm", Enabled: true, BinaryPath: "./plugins/llm-openai/l.exe"},
		// config.yaml 里安装的（preset 没有的）工具——install_dsc_plugin 写入的位置
		{Name: "tool-installed", Type: "tool", Enabled: true, BinaryPath: "./plugins/tool-installed/t.exe"},
		// config.yaml 里与 preset 同名的（duplicate）——preset 优先，保留 preset 的 binary_path
		{Name: "tool-filesystem", Type: "tool", Enabled: true, BinaryPath: "./plugins/config/tool-filesystem/cf.exe"},
		// config.yaml 里对 preset 同名插件做 env 补充——不生效，preset 条目主导
		{Name: "tool-lua-host", Type: "tool", Enabled: true, BinaryPath: "./plugins/config/tool-lua-host/cf.exe"},
	}}
	presetCfg := &core.Config{Plugins: []core.PluginEntry{
		{Name: "tool-filesystem", Type: "tool", Enabled: true, BinaryPath: "./plugins/tool-filesystem/pf.exe"},
		{Name: "tool-lua-host", Type: "tool", Enabled: true, BinaryPath: "./plugins/tool-lua-host/pl.exe"},
	}}
	merged := assembleMerged(extractLLMs(mainCfg), extractAgent(mainCfg), mainCfg, presetCfg, 128000, false, "")

	names := map[string]bool{}
	binary := map[string]string{}
	for _, p := range merged.Plugins {
		names[p.Name] = true
		binary[p.Name] = p.BinaryPath
	}
	// config-only 安装的工具在
	if !names["tool-installed"] {
		t.Fatalf("config.yaml 安装的工具未被并入, got %v", keySet(names))
	}
	// 与 preset 同名的只保留一份（去重）
	if !names["tool-filesystem"] || !names["tool-lua-host"] {
		t.Fatalf("应保留 tool-filesystem/tool-lua-host")
	}
	if n := countName(merged, "tool-filesystem"); n != 1 {
		t.Fatalf("tool-filesystem 应去重为 1 份, got %d", n)
	}
	// 同名冲突：preset 优先
	if binary["tool-filesystem"] != "./plugins/tool-filesystem/pf.exe" {
		t.Fatalf("tool-filesystem 应取 preset 的 binary_path, got %q", binary["tool-filesystem"])
	}
	if binary["tool-lua-host"] != "./plugins/tool-lua-host/pl.exe" {
		t.Fatalf("tool-lua-host 应取 preset 的 binary_path, got %q", binary["tool-lua-host"])
	}
}

// TestAssembleMergedCompactionPropagation 验证压缩后端选择（配置层）：
// preset 显式声明的 compaction 优先（preset 主导行为，与插件条目同规）；
// config.yaml 仅在 preset 未声明时兜底；两侧均未声明为空（agent 内联兜底）。
func TestAssembleMergedCompactionPropagation(t *testing.T) {
	mainCfg := &core.Config{Compaction: "dsc-billion-context"}
	presetCfg := &core.Config{Compaction: "dsc-system"}
	if got := assembleMerged(nil, nil, mainCfg, presetCfg, 128000, false, "").Compaction; got != "dsc-system" {
		t.Fatalf("preset 声明时应取 preset 值, got %q", got)
	}
	if got := assembleMerged(nil, nil, mainCfg, nil, 128000, false, "").Compaction; got != "dsc-billion-context" {
		t.Fatalf("preset 缺失时应兜底 config.yaml 值, got %q", got)
	}
	if got := assembleMerged(nil, nil, nil, nil, 128000, false, "").Compaction; got != "" {
		t.Fatalf("两侧未声明时应为空, got %q", got)
	}
}

func extractLLMs(cfg *core.Config) []core.PluginEntry {
	var out []core.PluginEntry
	for _, e := range cfg.Plugins {
		if e.Enabled && e.Type == "llm" {
			out = append(out, e)
		}
	}
	return out
}

func extractAgent(cfg *core.Config) *core.PluginEntry {
	for i := range cfg.Plugins {
		if cfg.Plugins[i].Enabled && cfg.Plugins[i].Type == "agent" {
			return &cfg.Plugins[i]
		}
	}
	return nil
}

func countName(merged *core.Config, name string) int {
	n := 0
	for _, p := range merged.Plugins {
		if p.Name == name {
			n++
		}
	}
	return n
}

func keySet(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestHistoryInjectionCountPassesValueThrough 历史注入条数原值透传，不做转码。
//
// 回归：config 侧曾用 -1=禁止、0=未定义，agent 侧用 -1=不限制、0=不注入，两侧
// 语义相反。预设里写 -1 的本意是「不限制」，却被译成「不注入跨轮历史」——
// 表现就是第二轮开始模型完全不记得上一轮（实测 178K 上下文骤降到 9K）。
func TestHistoryInjectionCountPassesValueThrough(t *testing.T) {
	ptr := func(v int) *int { return &v }

	if _, ok := historyInjectionCount(nil); ok {
		t.Fatal("nil config 不应下发")
	}
	if _, ok := historyInjectionCount(&core.Config{}); ok {
		t.Fatal("未设置时不应下发（agent 默认不限制）")
	}
	for _, c := range []struct {
		in   int
		want int
	}{
		{-1, -1}, // 不限制：必须原样下发 -1，绝不能被译成 0
		{0, 0},   // 显式关闭跨轮历史
		{5, 5},
	} {
		got, ok := historyInjectionCount(&core.Config{HistoryInjection: ptr(c.in)})
		if !ok || got != c.want {
			t.Fatalf("config %d 应原值下发 %d，got %d(ok=%v)", c.in, c.want, got, ok)
		}
	}
}

// TestDeepSeekPresetKeepsHistoryUnlimited 预设文件不得默认关闭跨轮历史：
// 这是把「模型失忆」写进出厂配置的坑。
func TestDeepSeekPresetKeepsHistoryUnlimited(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("config", "config.DeepSeek.yaml"))
	if err != nil {
		t.Skip(err)
	}
	var cfg core.Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("解析预设: %v", err)
	}
	if cfg.HistoryInjection == nil {
		return // 未设置 = 默认不限制
	}
	if *cfg.HistoryInjection == 0 {
		t.Fatal("预设把 history_injection 设为 0：新装的用户第二轮就会丢上下文")
	}
}
