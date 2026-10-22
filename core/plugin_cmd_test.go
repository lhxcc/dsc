package core

import (
	"os"
	"strings"
	"testing"

	plugin "github.com/hashicorp/go-plugin"
)

// 本文件守护「插件子进程 env 只在 pluginCmd / newPluginClientConfig 一处装配」的约定。
// 历史事故：终端里存在与 config.yaml 同名的 DSC_<P>_API_KEY 默认值，插件最终读到
// 终端旧 key 收到 401；另 4 个热重载拉起点干脆没设 cmd.Env，重载后配置注入全丢。

// TestPluginCmdUnifiedEnv 验证统一入口给出的 ClientConfig：
//   - SkipHostEnv=true（否则 fork 追加 os.Environ()，重复键后者胜出会覆盖配置值，
//     并把对非 LLM 插件滤除的凭据键重新带回子进程）；
//   - cmd.Env 恒为完整列表，且 config.yaml 的 entry.Env 胜过宿主同名变量；
//   - 非 LLM 插件不含凭据类键。
func TestPluginCmdUnifiedEnv(t *testing.T) {
	t.Setenv("DSC_ANTHROPIC_API_KEY", "sk-from-host")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-secret")
	t.Setenv("PLAIN_HOST_VAR", "host")

	m := NewManager(&ManagerConfig{})

	llmEntry := PluginEntry{Name: "llm-anthropic", Type: "llm", Env: map[string]string{
		"DSC_ANTHROPIC_API_KEY": "sk-from-config",
		"DSC_ANTHROPIC_MODEL":   "deepseek-flash",
	}}
	cfg := m.newPluginClientConfig(llmEntry, "llm-anthropic.exe", "",
		plugin.HandshakeConfig{}, map[string]plugin.Plugin{}, nil)

	if !cfg.SkipHostEnv {
		t.Fatal("ClientConfig 必须 SkipHostEnv=true：env 已由 pluginCmd 完整给出")
	}
	if cfg.Cmd == nil || len(cfg.Cmd.Env) == 0 {
		t.Fatal("cmd.Env 必须为完整 env 列表（nil 会丢部署配置，Windows 下 CreateProcess 亦会失败）")
	}
	env := envToMap(t, cfg.Cmd.Env)
	if got := env["DSC_ANTHROPIC_API_KEY"]; got != "sk-from-config" {
		t.Fatalf("config 注入的 API key 必须胜过宿主同名变量，got %q", got)
	}
	if env["DSC_ANTHROPIC_MODEL"] != "deepseek-flash" {
		t.Fatalf("config 注入的模型名必须生效，got %q", env["DSC_ANTHROPIC_MODEL"])
	}
	if env["PLAIN_HOST_VAR"] != "host" {
		t.Fatal("插件仍须继承宿主环境（非机密键）")
	}

	// 非 LLM 插件：完整宿主环境照给，但凭据类键必须缺席——SkipHostEnv=true 是
	// 这条过滤真正生效的前提（否则 fork 会把它们原样带回）。
	toolCfg := m.newPluginClientConfig(PluginEntry{Name: "tool-x", Type: "tool"}, "tool-x.exe", "",
		plugin.HandshakeConfig{}, map[string]plugin.Plugin{}, nil)
	toolEnv := envToMap(t, toolCfg.Cmd.Env)
	if _, ok := toolEnv["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatal("非 LLM 插件不得继承凭据类键")
	}
	if _, ok := toolEnv["DSC_ANTHROPIC_API_KEY"]; ok {
		t.Fatal("非 LLM 插件不得继承 DSC_*_API_KEY（LLM 凭据同样属于凭据）")
	}
	if toolEnv["PLAIN_HOST_VAR"] != "host" {
		t.Fatal("非 LLM 插件仍须继承非机密宿主变量")
	}
}

// TestPluginCmdNoDuplicateKeys 验证统一入口产出的 env 无重复键：宿主环境与
// entry.Env 已经过 map 合并去重，重复键交给 os/exec 去重属于不可依赖的隐式行为。
func TestPluginCmdNoDuplicateKeys(t *testing.T) {
	t.Setenv("DSC_ANTHROPIC_MODEL", "host-model")
	m := NewManager(&ManagerConfig{})
	entry := PluginEntry{Name: "llm-a", Type: "llm", Env: map[string]string{
		"DSC_ANTHROPIC_MODEL": "config-model",
	}}
	cmd := m.pluginCmd(entry, "llm-a.exe", "")
	seen := make(map[string]bool, len(cmd.Env))
	for _, kv := range cmd.Env {
		i := strings.Index(kv, "=")
		if i <= 0 {
			continue
		}
		k := kv[:i]
		if seen[k] {
			t.Fatalf("env 含重复键 %q：%v", k, cmd.Env)
		}
		seen[k] = true
	}
	if !seen["DSC_ANTHROPIC_MODEL"] {
		t.Fatal("entry.Env 的键必须出现在 env 中")
	}
}

// TestPluginCmdInfantProcessSeesConfigEnv 用真实子进程确认「子进程看到的就是
// pluginCmd 装配的那份 env」——尤其是 config 值不被宿主同名变量覆盖。
func TestPluginCmdInfantProcessSeesConfigEnv(t *testing.T) {
	const marker = "DSC_TEST_ENV_HELPER"
	if os.Getenv(marker) == "1" {
		// 子进程分支：输出关心的键后退出（父进程负责断言）。
		os.Stdout.WriteString("API_KEY=" + os.Getenv("DSC_ANTHROPIC_API_KEY") + "\n")
		os.Stdout.WriteString("MODEL=" + os.Getenv("DSC_ANTHROPIC_MODEL") + "\n")
		os.Stdout.WriteString("REASONING=" + os.Getenv("DSC_REASONING_EFFORT") + "\n")
		os.Exit(0)
	}

	t.Setenv("DSC_ANTHROPIC_API_KEY", "sk-from-host")
	t.Setenv("DSC_ANTHROPIC_MODEL", "host-model")

	m := NewManager(&ManagerConfig{})
	entry := PluginEntry{Name: "llm-x", Type: "llm", Env: map[string]string{
		marker:                  "1",
		"DSC_ANTHROPIC_API_KEY": "sk-from-config",
		"DSC_ANTHROPIC_MODEL":   "deepseek-flash",
		"DSC_REASONING_EFFORT":  "high",
	}}

	cmd := m.pluginCmd(entry, os.Args[0], "")
	cmd.Args = []string{os.Args[0], "-test.run=TestPluginCmdInfantProcessSeesConfigEnv", "--"}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper 子进程启动失败: %v (output=%s)", err, out)
	}
	got := string(out)
	for _, want := range []string{
		"API_KEY=sk-from-config", // 不是 sk-from-host
		"MODEL=deepseek-flash",   // 不是 host-model
		"REASONING=high",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("子进程环境不符合预期，缺少 %q；实际输出：\n%s", want, got)
		}
	}
	if strings.Contains(got, "sk-from-host") || strings.Contains(got, "MODEL=host-model") {
		t.Fatalf("宿主同名变量劫持了 config 注入值：\n%s", got)
	}
}

// TestReloadEntryReusesLoadedEnv 验证热重载复现首次加载的 entry.Env：热重载不
// 重新解析 config.yaml（孤儿插件甚至不在 config 内），只能靠 loadedEntries。
func TestReloadEntryReusesLoadedEnv(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	entry := PluginEntry{Name: "llm-a", Type: "llm", Env: map[string]string{
		"DSC_REASONING_EFFORT": "high",
	}}
	m.mu.Lock()
	m.loadedEntries[entry.Name] = entry
	m.mu.Unlock()

	got := m.reloadEntry("llm-a", "llm")
	if got.Env["DSC_REASONING_EFFORT"] != "high" {
		t.Fatalf("热重载应复用已登记条目的 env，got %#v", got.Env)
	}

	// 未登记（理论上仅未持久化的孤儿插件）：退化为最小条目，类型仍须正确，
	// 以保证 coreEnv 对 llm 放行凭据、对其它类型滤除。
	fb := m.reloadEntry("ghost", "tool")
	if fb.Type != "tool" || fb.Name != "ghost" || len(fb.Env) != 0 {
		t.Fatalf("未登记条目应退化为最小条目，got %#v", fb)
	}

	// dispose 后条目作废，避免同名插件换配置后残留旧 env。
	m.mu.Lock()
	m.markDisposedLocked(entry.Name)
	m.mu.Unlock()
	if got := m.reloadEntry("llm-a", "llm"); len(got.Env) != 0 {
		t.Fatalf("dispose 后条目应被清除，got %#v", got.Env)
	}
}

// TestAllSpawnSitesGoThroughUnifiedEntry 静态守护：manager.go 内不允许再出现
// 独立拼装 cmd.Env 的调用点（统一入口之外的 env 装配会重新引入环境漂移）。
func TestAllSpawnSitesGoThroughUnifiedEntry(t *testing.T) {
	data, err := os.ReadFile("manager.go")
	if err != nil {
		t.Fatalf("读取 manager.go 失败: %v", err)
	}
	src := string(data)

	// 8 个生产拉起点（首次加载 3 + 热重载 5）都必须经 newPluginClientConfig：
	// 全文件应只剩 pluginCmd 内的一处 exec.Command 与一处 cmd.Env 赋值。
	if got := strings.Count(src, "exec.Command("); got != 1 {
		t.Fatalf("exec.Command 调用点应为 1 处（仅 pluginCmd 内），实际 %d 处", got)
	}
	if got := strings.Count(src, "cmd.Env = "); got != 1 {
		t.Fatalf("cmd.Env 赋值应为 1 处（仅 pluginCmd 内），实际 %d 处", got)
	}
	if got := strings.Count(src, "m.newPluginClientConfig("); got != 8 {
		t.Fatalf("应有 8 个拉起点经统一入口 newPluginClientConfig，实际 %d 处", got)
	}
	if got := strings.Count(src, "plugin.NewClient("); got != 8 {
		t.Fatalf("plugin.NewClient 应恰为 8 处（与统一入口一一对应），实际 %d 处", got)
	}
	if !strings.Contains(src, "SkipHostEnv: true") {
		t.Fatal("newPluginClientConfig 必须置 SkipHostEnv=true")
	}
}

// TestSummarizeLLMEnvMasksCredentials 验证启动日志里的 env 摘要：只列 LLM 相关键、
// 凭据仅留后 4 位、空值显式标出、无关宿主变量不刷屏。
func TestSummarizeLLMEnvMasksCredentials(t *testing.T) {
	env := []string{
		"DSC_ANTHROPIC_API_KEY=sk-89f423e999534743b880614b54869797",
		"DSC_ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic",
		"DSC_ANTHROPIC_MODEL=deepseek-flash",
		"DSC_REASONING_EFFORT=high",
		"DSC_OLLAMA_API_KEY=",
		"PATH=/usr/bin:/bin",
		"HOME=/root",
		"ANTHROPIC_AUTH_TOKEN=terminal-token-abcd",
	}
	got := summarizeLLMEnv(env)
	want := []string{
		"ANTHROPIC_AUTH_TOKEN=****abcd",
		"DSC_ANTHROPIC_API_KEY=****9797",
		"DSC_ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic",
		"DSC_ANTHROPIC_MODEL=deepseek-flash",
		"DSC_OLLAMA_API_KEY=(empty)",
		"DSC_REASONING_EFFORT=high",
	}
	if len(got) != len(want) {
		t.Fatalf("摘要条目数不符：got %v / want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("摘要第 %d 项不符：got %q / want %q（完整 got=%v）", i, got[i], want[i], got)
		}
	}
	if maskCredential("abc") != "****" || maskCredential("") != "(empty)" {
		t.Fatal("短凭据/空凭据的脱敏不符合预期")
	}
}

// TestMirrorLLMEnvToLegacyNames 验证 DSC_ 组回写到无前缀旧名，并清除同组 auth token。
// 这是修复「终端 ANTHROPIC_AUTH_TOKEN 抢占 Bearer 认证」的关键一步。
func TestMirrorLLMEnvToLegacyNames(t *testing.T) {
	env := []string{
		"DSC_ANTHROPIC_API_KEY=sk-config",
		"DSC_ANTHROPIC_BASE_URL=https://api.deepseek.com/anthropic",
		"DSC_ANTHROPIC_MODEL=deepseek-flash",
		"ANTHROPIC_AUTH_TOKEN=sk-terminal-888888", // 终端 .profile 遗留
		"ANTHROPIC_BASE_URL=http://192.168.124.197:8008",
		"PATH=/usr/bin:/bin",
	}
	got := envToMap(t, mirrorLLMEnvToLegacyNames(env))

	if got["ANTHROPIC_API_KEY"] != "sk-config" {
		t.Fatalf("ANTHROPIC_API_KEY 应回写为配置值，got %q", got["ANTHROPIC_API_KEY"])
	}
	if got["ANTHROPIC_BASE_URL"] != "https://api.deepseek.com/anthropic" {
		t.Fatalf("ANTHROPIC_BASE_URL 应回写为配置值，got %q", got["ANTHROPIC_BASE_URL"])
	}
	if got["ANTHROPIC_MODEL"] != "deepseek-flash" {
		t.Fatalf("ANTHROPIC_MODEL 应回写为配置值，got %q", got["ANTHROPIC_MODEL"])
	}
	if _, ok := got["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatal("DSC_ 组存在时必须清除旧名 AUTH_TOKEN，否则 SDK 会发 Authorization: Bearer 抢占认证")
	}
	if got["DSC_ANTHROPIC_API_KEY"] != "sk-config" {
		t.Fatal("原始 DSC_ 键必须保留")
	}
	if got["PATH"] != "/usr/bin:/bin" {
		t.Fatal("无关变量不得被改动")
	}

	// 无 DSC_ 组时不介入：旧名整组配置（含 auth token）继续可用。
	legacy := envToMap(t, mirrorLLMEnvToLegacyNames([]string{
		"ANTHROPIC_AUTH_TOKEN=sk-legacy",
		"OPENAI_API_KEY=sk-openai",
	}))
	if legacy["ANTHROPIC_AUTH_TOKEN"] != "sk-legacy" || legacy["OPENAI_API_KEY"] != "sk-openai" {
		t.Fatalf("无 DSC_ 组时不得改写旧名配置，got %v", legacy)
	}

	// ollama 的 HOST 也回写（旧名 OLLAMA_HOST），且其 API_KEY 一并同步。
	oll := envToMap(t, mirrorLLMEnvToLegacyNames([]string{
		"DSC_OLLAMA_HOST=http://dsc-host:11434",
		"DSC_OLLAMA_MODEL=qwen2.5:14b",
	}))
	if oll["OLLAMA_HOST"] != "http://dsc-host:11434" || oll["OLLAMA_MODEL"] != "qwen2.5:14b" {
		t.Fatalf("ollama 旧名回写失败，got %v", oll)
	}
}

// TestLLMEnvMirrorsToLegacyNames 验证统一入口对 llm 类型自动应用回写（插件拿到
// 的 env 里既有 DSC_ 键也有旧名键），而对非 llm 类型不介入。
func TestLLMEnvMirrorsToLegacyNames(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-terminal-888888")

	env := envToMap(t, m.coreEnv(PluginEntry{
		Name: "llm-anthropic", Type: "llm",
		Env: map[string]string{
			"DSC_ANTHROPIC_API_KEY": "sk-config",
			"DSC_ANTHROPIC_MODEL":   "deepseek-flash",
		},
	}))
	if env["ANTHROPIC_API_KEY"] != "sk-config" {
		t.Fatalf("llm 插件 env 应含回写后的旧名 key，got %v", env)
	}
	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatal("llm 插件 env 不得残留旧名 AUTH_TOKEN")
	}

	// 非 llm 插件：凭据类键本就被滤除，也不做回写。
	toolEnv := envToMap(t, m.coreEnv(PluginEntry{Name: "tool-x", Type: "tool"}))
	if _, ok := toolEnv["DSC_ANTHROPIC_API_KEY"]; ok {
		t.Fatal("非 llm 插件不应得到 DSC_*_API_KEY")
	}
}

func envToMap(t *testing.T, env []string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(env))
	for _, kv := range env {
		i := strings.Index(kv, "=")
		if i <= 0 {
			continue
		}
		out[kv[:i]] = kv[i+1:]
	}
	return out
}
