package main

import (
	"testing"

	core "dsc/core"
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

// TestAssembleDoesNotInjectMaxOutputTokens 装配阶段不碰 max_tokens：装配只看配置本身，
// 预算发生在别处（启动期探一次服务端上限 → Manager；每帧由 core.outputBudgetFor 按
// 「窗口 − 本帧表面 − 余量」算出来，随 ChatStream 的 maxTokens 参数下发）。
//
// 语义：用户显式写了就按用户的（>0 用值、-1 不携带），宿主一次都不填；配置里的显式值
// 必须在装配后原样保留，不能被任何默认值改写。
func TestAssembleDoesNotInjectMaxOutputTokens(t *testing.T) {
	mainCfg := &core.Config{Plugins: []core.PluginEntry{
		{Name: "llm-anthropic", Type: "llm", Enabled: true, BinaryPath: "./plugins/llm-anthropic/a.exe"},
		{Name: "llm-openai", Type: "llm", Enabled: true, BinaryPath: "./plugins/llm-openai/o.exe",
			Env: map[string]string{"DSC_MAX_OUTPUT_TOKENS": "8192"}},
	}}
	merged := assembleMerged(extractLLMs(mainCfg), extractAgent(mainCfg), mainCfg, nil, 262144, false, "")
	for _, p := range merged.Plugins {
		if p.Type != "llm" {
			continue
		}
		got, ok := p.Env["DSC_MAX_OUTPUT_TOKENS"]
		if p.Name == "llm-anthropic" && ok {
			t.Fatalf("装配阶段不得凭空注入 max_tokens，got %q", got)
		}
		if p.Name == "llm-openai" && got != "8192" {
			t.Fatalf("显式配置必须原样保留，got %q", got)
		}
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
	mainCfg := &core.Config{Compaction: "backend-x"}
	presetCfg := &core.Config{Compaction: "dsc-system"}
	if got := assembleMerged(nil, nil, mainCfg, presetCfg, 128000, false, "").Compaction; got != "dsc-system" {
		t.Fatalf("preset 声明时应取 preset 值, got %q", got)
	}
	if got := assembleMerged(nil, nil, mainCfg, nil, 128000, false, "").Compaction; got != "backend-x" {
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

// TestHistoryInjectionCountPassThrough config 与 agent 共用 DSC 三值语义，
// 原值透传、不翻译（负值=不注入恢复历史、0=未定义即默认不限制、N 条）。
func TestHistoryInjectionCountPassThrough(t *testing.T) {
	if _, ok := historyInjectionCount(nil); ok {
		t.Fatal("nil config 不应下发")
	}
	for _, want := range []int{-1, 0, 1, 50} {
		got, ok := historyInjectionCount(&core.Config{HistoryInjection: want})
		if !ok || got != want {
			t.Fatalf("config %d → (%d,%v), want (%d,true)", want, got, ok, want)
		}
	}
}

// TestInjectModelNameAndResolve 模型名要注入所有插件（agent 写 request/header 的信封
// 字段时读 DSC_MODEL），且解析只有一个入口（TUI 展示与信封共用）。
func TestInjectModelNameAndResolve(t *testing.T) {
	llmEntries := []core.PluginEntry{
		{Name: "llm-openai", Type: "llm", Env: map[string]string{"DSC_OPENAI_MODEL": "deepseek-v4"}},
	}
	if got := resolveActiveModelName(llmEntries, "llm-openai"); got != "deepseek-v4" {
		t.Fatalf("应从 provider 条目解析模型名，got %q", got)
	}
	// 未知 provider / 无 env → 兜底名（不能返回空串，否则信封里模型字段恒空）
	if got := resolveActiveModelName(nil, "llm-none"); got == "" {
		t.Fatal("兜底不应为空串")
	}

	merged := &core.Config{Plugins: []core.PluginEntry{
		{Name: "agent-x", Type: "agent"},
		{Name: "tool-x", Type: "tool", Env: map[string]string{"DSC_MODEL": "explicit"}},
	}}
	injectModelName(merged, "deepseek-v4")
	if got := merged.Plugins[0].Env["DSC_MODEL"]; got != "deepseek-v4" {
		t.Fatalf("未配置的插件应获得模型名，got %q", got)
	}
	if got := merged.Plugins[1].Env["DSC_MODEL"]; got != "explicit" {
		t.Fatalf("显式配置不得被覆盖，got %q", got)
	}
}
