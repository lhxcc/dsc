package dsc

import (
	"dsc/core/agentclient"
	"dsc/core/llmclient"
	"dsc/core/notify"
	"dsc/core/toolclient"
)

// 互通客户端的类型别名（分层：core ← sdk ← plugins）。
//
// 这四个客户端是「插件→宿主」的唯一通道：聚合 LLM、聚合 Tool、插件通知、主 agent。
// Interconnect 的 getter 返回的就是它们的实例，插件日常调用不需要命名类型；但有些
// 场景必须能写出来——把实例存进自己的结构体字段、或作为参数传给内部包（LUA / SQL
// 绑定层就是后者：脚本侧的 dsc.llm / dsc.tool / dsc.notify 需要持有客户端）。
//
// 不提供别名的后果是那些插件只能 import core 的具体子包，而 core 是持续重构的内核
// （AGENTS.md §10.5），绑定层会跟着内核被动漂移。
//
// 一律用**类型别名**（type X = ...）而不是包装类型：方法集与类型身份完全一致，插件
// 与宿主之间传参零转换，也不会多出一层可以塞进第二份实现的壳。
type (
	// AgentClient 宿主主 agent 桥接客户端（RunStream / InjectMessage）。
	AgentClient = agentclient.Client
	// LLMClient 宿主聚合 LLM 客户端。
	LLMClient = llmclient.Client
	// ToolClient 宿主聚合 Tool 客户端（经它调用任意插件的工具）。
	ToolClient = toolclient.Client
	// NotifyClient 宿主插件通知客户端（把事件发布到宿主事件总线）。
	NotifyClient = notify.Notifier
)
