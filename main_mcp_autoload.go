package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"dsc/core"
	"github.com/hashicorp/go-hclog"
)

// autoConnectMCPFromConfig 扫描 merged 配置中含 config.mcp 的插件条目，
// 启动时自动连接对应的 MCP 服务器（对齐 DSH cordis.yml 的 mcp-client
// 插件实例化——配置即声明，启动即连接）。
//
// patch 文件格式（经 -patch 注入）：
//
//   - name: mcp-memory
//     type: dsc
//     enabled: true          # 必须显式写：Enabled 是普通 bool，省略即为 false → 整条被跳过
//     config:
//     mcp:
//     server_name: memory
//     endpoint: http://localhost:3000/mcp
//     # 或 stdio 传输：
//     # transport: stdio
//     # command: mcp-server-memory
//     # args: ["--port", "3000"]
//     # env: {MEMORY_DB: "/tmp/memory.db"}   # 可选
//     # workdir: /path/to/server             # 可选
//
// 每个条目的 config.mcp 字段含 server_name（必填）与 endpoint（HTTP）或
// transport: stdio + command + args/env（stdio）。
func autoConnectMCPFromConfig(mgr *core.Manager, cfg *core.Config, logger hclog.Logger) {
	if cfg == nil || len(cfg.Plugins) == 0 {
		return
	}
	for _, entry := range cfg.Plugins {
		if len(entry.Config) == 0 {
			continue
		}
		mcpCfg, ok := entry.Config["mcp"].(map[string]any)
		if !ok {
			continue
		}
		// enabled 是普通 bool：YAML 里省略等于 false。MCP 条目唯一的用途就是连接，
		// 漏写这一行会让整份 overlay 静默失效（现场表现是「配了 MCP 却一个工具都没有」）。
		// 不改变语义（disabled 仍然不连），只是把静默跳过变成一条可搜到的告警。
		if !entry.Enabled {
			logger.Warn("patch MCP config present but entry is disabled; add `enabled: true` to use it",
				"plugin", entry.Name)
			continue
		}
		serverName, _ := mcpCfg["server_name"].(string)
		if serverName == "" {
			logger.Warn("patch MCP config missing server_name, skipping", "plugin", entry.Name)
			continue
		}
		endpoint, _ := mcpCfg["endpoint"].(string)
		transport, _ := mcpCfg["transport"].(string)

		var (
			client *core.MCPClient
			err    error
		)
		if strings.EqualFold(transport, "stdio") {
			command, _ := mcpCfg["command"].(string)
			if command == "" {
				logger.Warn("patch MCP stdio config missing command, skipping",
					"plugin", entry.Name, "server_name", serverName)
				continue
			}
			workDir, _ := mcpCfg["workdir"].(string)
			client, err = core.NewMCPStdioClient(mgr, serverName, command,
				mcpStringList(mcpCfg["args"]), mcpEnvList(mcpCfg["env"]), workDir)
		} else {
			if endpoint == "" {
				logger.Warn("patch MCP config missing endpoint (HTTP transport), skipping",
					"plugin", entry.Name, "server_name", serverName)
				continue
			}
			client, err = core.NewMCPClient(mgr, serverName, endpoint)
		}
		if err != nil {
			logger.Error("patch MCP connect failed",
				"plugin", entry.Name, "server_name", serverName, "error", err)
			continue
		}
		if err := client.Connect(context.Background()); err != nil {
			logger.Error("patch MCP connect failed",
				"plugin", entry.Name, "server_name", serverName,
				"endpoint", endpoint, "transport", transport, "error", err)
			continue
		}
		// 注册到 Manager.mcpClients（与 admin API /mcp/connect 同款路径）
		mgr.RegisterMCPClient(serverName, client)
		logger.Info("patch MCP connected",
			"plugin", entry.Name, "server_name", serverName,
			"endpoint", endpoint, "transport", transport,
			"tools", fmt.Sprintf("%d", client.ToolCount()))
	}
}

// mcpStringList 把配置里的数组转成字符串列表（非字符串项忽略）。
func mcpStringList(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// mcpEnvList 把配置里的 env 转成 KEY=VALUE 列表：支持 map（按键升序，确定性）
// 或字符串数组两种写法。
func mcpEnvList(v any) []string {
	switch e := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(e))
		for k := range e {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			out = append(out, k+"="+fmt.Sprint(e[k]))
		}
		return out
	case []any:
		return mcpStringList(e)
	default:
		return nil
	}
}
