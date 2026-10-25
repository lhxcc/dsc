package core

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// 能力与「宿主凭据」的对应关系。
//
// 为什么需要这张表：某些能力要工作，插件必须拿到宿主自己的机密——例如代理宿主管理
// API 的插件需要 DSC_ADMIN_TOKEN。宿主**不会**按插件名自动放行这类凭据：一旦自动
// 放行，装了某个插件就等于把管理权限交出去，而用户未必审到插件声明了什么。所以
// 授予必须是**显式**的（插件的 config.yaml `env:` 块），本表只负责在"忘了授予"时
// 把它说出来。
//
// 键是**能力名**（协议词汇），值是该能力运行时需要的环境变量键。这里永远不出现
// 插件名——判断依据是"谁声明了该能力"，而不是"是哪个插件"。
var capabilitySecrets = map[string][]string{
	"admin": {"DSC_ADMIN_TOKEN"},
}

// CapabilitySecretKeys 返回某能力需要的宿主凭据键（无要求时返回 nil）。
func CapabilitySecretKeys(capability string) []string {
	return capabilitySecrets[capability]
}

// MissingCapabilityGrants 找出「声明了需要宿主凭据的能力、却没被显式授予」的插件，
// 返回可直接打印的说明（一切正常时返回 nil）。
//
// grants 为「插件名 → 该插件 config.yaml 里 env: 声明的键值」，由调用方从配置取
// ——核心不持有配置条目，也不该把配置语义复制一份。
//
// 判定条件（缺一不报）：
//  1. 宿主进程自己**设置**了该凭据（没设说明该能力当前不需要认证，报了是噪声）
//  2. 插件经 Requires 声明了对该能力的依赖（能力的消费者，而不是路过）
//  3. 该插件的 env: 里没有这个键（确实没被授予）
//
// 三种情况都符合却不授予，插件运行时会静默 401/失败——这正是最难自查的一类问题，
// 所以在加载时给一条指名道姓的告警。
func (m *Manager) MissingCapabilityGrants(grants map[string]map[string]string) []string {
	if m == nil {
		return nil
	}
	// 能力名 → 声明依赖它的插件集合
	consumers := make(map[string][]string)
	m.mu.RLock()
	for name, info := range m.coreMetadata {
		if info == nil {
			continue
		}
		for key := range info.GetCapabilities() {
			if reqType, capability, ok := parseRequiresCapability(key); ok && reqType != "" && capability != "" {
				consumers[capability] = append(consumers[capability], name)
			}
		}
	}
	m.mu.RUnlock()

	var out []string
	for capability, keys := range capabilitySecrets {
		plugins := consumers[capability]
		if len(plugins) == 0 {
			continue
		}
		sort.Strings(plugins)
		for _, key := range keys {
			if os.Getenv(key) == "" {
				continue // 宿主没启用该凭据，能力照常可用（无认证）
			}
			for _, plugin := range plugins {
				if grants[plugin][key] != "" {
					continue // 已显式授予
				}
				out = append(out, fmt.Sprintf(
					"plugin %q depends on capability %q, which needs host credential %s, but it was not granted: "+
						"add `env: {%s: <same value as the host>}` to its entry in config.yaml "+
						"(the host never grants credentials by plugin name)",
					plugin, capability, key, key))
			}
		}
	}
	sort.Strings(out)
	return out
}

// GrantMap 把配置条目整理成 MissingCapabilityGrants 需要的形式（插件名 → env 键值）。
//
// 抽出来的理由：调用方（main）只该关心"把配置交进来"，键名归一（大小写、去空白）
// 这类细节放在一处，避免两处各写一遍。
func GrantMap(entries map[string]map[string]string) map[string]map[string]string {
	out := make(map[string]map[string]string, len(entries))
	for plugin, env := range entries {
		norm := make(map[string]string, len(env))
		for k, v := range env {
			norm[strings.TrimSpace(k)] = v
		}
		out[plugin] = norm
	}
	return out
}
