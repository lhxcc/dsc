package main

import (
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"

	core "dsc/core"
	"github.com/hashicorp/go-hclog"
)

// discoverFirstLLMPlugin 扫描 plugins/ 目录，按名升序返回首个 llm-* 目录的
// provider 名（如 "anthropic"）。无任何 llm-* 目录返回空串。
// 与 setup.go discoverLLMProviders 同款发现机制，但此处仅返回首个供兜底。
// 对齐 AGENTS.md §8：宿主核心不得硬编码插件名——发现机制是目录名驱动的，
// 不预设具体 provider。
func discoverFirstLLMPlugin() string {
	pluginsDir := "./plugins"
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "llm-") {
			continue
		}
		names = append(names, strings.TrimPrefix(name, "llm-"))
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}

// discoverFirstAgentPlugin 扫描 plugins/ 目录，按名升序返回首个 agent-* 目录的
// agent 名（如 "react-loop"）。无任何 agent-* 目录返回空串。
// 与 discoverFirstLLMPlugin 同款发现机制，对齐 AGENTS.md §8。
func discoverFirstAgentPlugin() string {
	pluginsDir := "./plugins"
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return ""
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "agent-") {
			continue
		}
		names = append(names, strings.TrimPrefix(name, "agent-"))
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}

// assembleMerged 把 config.yaml（llm/agent）+ preset（tool/policy/dsc）合并成
// 交给 Manager 声明式加载的插件集，并做两件关键事：
//  1. config.yaml 里启用的 tool/policy/dsc（含 install_dsc_plugin 安装的）也并入，
//     与 preset 按名去重、preset 优先（同名冲突取 preset，config 仅补 preset 没有的）——
//     使模型安装的插件也能跨重启生效；
//  2. 为主路径与「失败回退续启」共用，避免重复的合并逻辑。
func assembleMerged(llmEntries []core.PluginEntry, agentEntry *core.PluginEntry, mainCfg, presetCfg *core.Config, contextWindow int, headless bool, inputText string) *core.Config {
	merged := &core.Config{}
	merged.Plugins = append(merged.Plugins, llmEntries...)

	ag := agentEntry
	if ag == nil {
		ext := modelExt()
		// 发现机制：扫描 plugins/ 目录取首个 agent-* 目录（按名升序），
		// 不再硬编码具体的 agent 插件名（对齐 AGENTS.md §8）。
		agentName := discoverFirstAgentPlugin()
		if agentName == "" {
			// 与 main.go LLM 兜底同款 fail-loud：log.Fatal 退出。
			logger := hclog.New(&hclog.LoggerOptions{Output: os.Stderr})
			logger.Error("no agent plugin configured: declare an agent entry in config.yaml or install a plugin under plugins/agent-*/")
			os.Exit(1)
		}
		ag = &core.PluginEntry{Name: agentName, Type: "agent", Enabled: true,
			BinaryPath: core.PJoin("./plugins", "agent-"+agentName, "agent-"+agentName+ext)}
	}
	agentEnv := map[string]string{"DSC_CONTEXT_WINDOW": strconv.Itoa(contextWindow)}
	if presetCfg != nil && presetCfg.Persona != "" {
		agentEnv["DSC_PRESET_PERSONA"] = presetCfg.Persona
	}
	if presetCfg != nil && presetCfg.PlanSection != "" {
		agentEnv["DSC_PLAN_SECTION"] = presetCfg.PlanSection
	}
	if (inputText != "" && !stdinIsRedirected()) || headless {
		agentEnv["DSC_SINGLE_TURN"] = "1"
	}
	for k, v := range ag.Env {
		agentEnv[k] = v
	}
	ag.Env = agentEnv
	merged.Plugins = append(merged.Plugins, *ag)

	// tool/policy/dsc：preset 是具体的预设、优先；config.yaml 只是补充——同名冲突取 preset，
	// config 仅并入 preset 没有的（install_dsc_plugin 写入 config.yaml 的插件因此仍能跨重启生效）。
	seen := map[string]bool{}
	for _, p := range merged.Plugins {
		seen[p.Name] = true
	}
	if presetCfg != nil {
		for _, e := range presetCfg.Plugins {
			if !e.Enabled || (e.Type != "tool" && e.Type != "policy" && e.Type != "dsc") {
				continue
			}
			if seen[e.Name] {
				continue
			}
			seen[e.Name] = true
			merged.Plugins = append(merged.Plugins, e)
		}
	}
	if mainCfg != nil {
		for _, e := range mainCfg.Plugins {
			if !e.Enabled || (e.Type != "tool" && e.Type != "policy" && e.Type != "dsc") {
				continue
			}
			if seen[e.Name] {
				continue
			}
			seen[e.Name] = true
			merged.Plugins = append(merged.Plugins, e)
		}
	}
	// 压缩后端选择（配置层）：preset 显式声明的 compaction 优先（preset 是具体
	// 预设、主导行为，与插件条目同规），config.yaml 仅在 preset 未声明时兜底。
	// preset 缺省启用某个压缩后端——「无配置时按能力依赖启动不因压缩
	// 后端缺席而阻塞」；merged 经 LoadFromConfig 交 Manager，m.compactionBackend
	// 据此验证 Provides compaction 能力声明（registerDscCoreLocked）。
	if presetCfg != nil && presetCfg.Compaction != "" {
		merged.Compaction = presetCfg.Compaction
	} else if mainCfg != nil {
		merged.Compaction = mainCfg.Compaction
	}
	// default_llm：Manager 在多 LLM provider 并存时用它做显式 primary 选择
	// （findProviderByCapabilityLocked → m.agentLLMName）。它只存在于 config.yaml，
	// 不随 preset 合并——漏拷会让 LLM 选择退化为「按名升序取首个」，与用户配置不符。
	if mainCfg != nil {
		merged.DefaultLLM = mainCfg.DefaultLLM
	}
	return merged
}

// assemblePluginSet 重试专用：从 config.yaml + preset 重新推导 llm/agent 条目并合并。
// 用于启动失败回退后，在主路径合并逻辑之外独立重建插件集（复用 assembleMerged）。
func assemblePluginSet(mainCfg, presetCfg *core.Config, contextWindow int, headless bool, inputText string) *core.Config {
	var llmEntries []core.PluginEntry
	if mainCfg != nil {
		for _, e := range mainCfg.Plugins {
			if e.Enabled && e.Type == "llm" {
				llmEntries = append(llmEntries, e)
			}
		}
	}
	if len(llmEntries) == 0 {
		llmName := os.Getenv("LLM_PROVIDER")
		if llmName == "" && mainCfg != nil {
			llmName = mainCfg.DefaultLLM
		}
		if llmName == "" {
			// 未显式配置 LLM provider 时，扫描 plugins/ 目录发现首个 llm-*
			// 目录（按名升序），与 setup.go discoverLLMProviders 一致——
			// 发现机制而非硬编码 "openai"（对齐 AGENTS.md §8：宿主核心不得
			// 硬编码插件名）。无任何 llm-* 目录时返回空，下方 fail-loud 拦截。
			llmName = discoverFirstLLMPlugin()
		}
		if llmName == "" {
			logger := hclog.New(&hclog.LoggerOptions{Output: os.Stderr})
			logger.Error("no LLM provider configured: set default_llm in config.yaml, LLM_PROVIDER env, or install a plugin under plugins/llm-*/")
			os.Exit(1)
		}
		llmEntries = []core.PluginEntry{{Name: llmName, Type: "llm", Enabled: true,
			BinaryPath: core.PJoin("./plugins", "llm-"+llmName, "llm-"+llmName+modelExt())}}
	}
	var agentEntry *core.PluginEntry
	if mainCfg != nil {
		for i := range mainCfg.Plugins {
			e := mainCfg.Plugins[i]
			if e.Enabled && e.Type == "agent" {
				agentEntry = &e
				break
			}
		}
	}
	return assembleMerged(llmEntries, agentEntry, mainCfg, presetCfg, contextWindow, headless, inputText)
}

func modelExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// injectRuntimeEnv 向所有插件进程注入当前模式/工作根/沙箱档/审批策略（主路径与「失败重试」共用）。
// injectContextWindow 向所有插件注入 DSC_CONTEXT_WINDOW（有效上下文窗口值）。
//
// 为什么不能只给 agent：压缩后端（基础压缩与上下文管理插件）
// 在各自进程里按窗口算压力阈值，此前拿不到该值只能退回 128K 默认——云端接 1M 窗口时
// 仍按 128K 判压力，几步就触发压缩（用户实测 TUI 标题已显示 1.0M）。
// 显式配置优先：插件条目 env 里写了该键（或宿主环境已有）则不覆盖。
func injectContextWindow(merged *core.Config, contextWindow int) {
	if contextWindow <= 0 {
		return
	}
	for i := range merged.Plugins {
		e := &merged.Plugins[i]
		if e.Env == nil {
			e.Env = map[string]string{}
		}
		if _, ok := e.Env["DSC_CONTEXT_WINDOW"]; ok {
			continue
		}
		e.Env["DSC_CONTEXT_WINDOW"] = strconv.Itoa(contextWindow)
	}
}

func injectRuntimeEnv(merged *core.Config, mode, workspaceRoot, sandboxPolicy string) {
	// 审批策略 env（供 agent 渲染 approval:policy 上下文）：显式 DSC_APPROVAL 尊重之；
	// 未显式时注入「沙箱预设绑定」默认（对齐 DSH permission-presets：full→never、其餘→ask）。
	approvalPolicy := core.ApprovalPolicyName(core.SandboxApprovalDefault(core.ParseSandboxPolicy(os.Getenv("DSC_SANDBOX"))))
	if os.Getenv("DSC_APPROVAL") != "" {
		approvalPolicy = core.ApprovalPolicyName(core.DefaultApprovalPolicy())
	}
	for i := range merged.Plugins {
		e := &merged.Plugins[i]
		if e.Env == nil {
			e.Env = map[string]string{}
		}
		e.Env["DSC_MODE"] = mode
		e.Env["DSC_WORKSPACE_ROOT"] = workspaceRoot
		e.Env["DSC_SANDBOX_POLICY"] = sandboxPolicy
		e.Env["DSC_APPROVAL_POLICY"] = approvalPolicy
		// DSC_HOST_PID：宿主进程 PID，供插件 hostLivenessWatchdog（sdk/host_watchdog.go）
		// 定时检测宿主是否存活——宿主崩溃/OOM/被强杀时插件主动退出，避免残留孤儿进程。
		// 对齐 DSH Node.js 子进程的父进程 PID 监控（process.ppid + process.kill(ppid, 0)）。
		e.Env["DSC_HOST_PID"] = strconv.Itoa(os.Getpid())
	}
}

// resolveActiveModelName 解析当前 provider 的模型名（DSC_ 前缀优先、旧名回退）。
// 单一来源：TUI 展示与 request/header 的信封字段都用它，避免两处各解析一遍。
func resolveActiveModelName(llmEntries []core.PluginEntry, activeLLMName string) string {
	for _, e := range llmEntries {
		if e.Name != activeLLMName {
			continue
		}
		provider := strings.ToUpper(strings.TrimPrefix(e.Name, "llm-"))
		if v := groupEnv(e.Env, provider, envGroupPreferDSC(e.Env, provider), "MODEL"); v != "" {
			return v
		}
		break
	}
	for _, provider := range []string{"OPENAI", "ANTHROPIC", "OLLAMA"} {
		if v := osGroupEnv(provider, osEnvGroupPreferDSC(provider), "MODEL"); v != "" {
			return v
		}
	}
	return "Agentic-Model"
}

// injectModelName 向所有插件注入 DSC_MODEL（模型名）：agent 写 request/header 时把
// 它作为信封字段（插件自己不解析 provider 配置）。显式配置优先，不覆盖已有值。
func injectModelName(merged *core.Config, model string) {
	if model == "" {
		return
	}
	for i := range merged.Plugins {
		e := &merged.Plugins[i]
		if e.Env == nil {
			e.Env = map[string]string{}
		}
		if _, ok := e.Env["DSC_MODEL"]; ok {
			continue
		}
		e.Env["DSC_MODEL"] = model
	}
}
