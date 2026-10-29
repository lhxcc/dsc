package main

import (
	"context"
	"fmt"

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
//     # env: {MEMORY_DB: "/tmp/memory.db"}   # 可选：只追加在白名单环境之上
//     # cwd: /path/to/server                 # 可选
//
// 每个条目的 config.mcp 字段含 server_name（必填），以及二者之一：
//   - endpoint：streamable HTTP 地址；
//   - transport: stdio + command + args（+ 可选 env / cwd）：本机拉起子进程。
//
// 两种传输与 mcp_connect 工具共用同一份解析与同一个连接实现（core.MCPSpecFromMap /
// Manager.ConnectMCPServerSpec）——配置与工具各写一份，迟早会在字段上分叉。
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
		// 工具入参与配置条目共用一份解析（core.MCPSpecFromMap）：支持两种传输，
		// 且不会出现「配置认这个字段、工具认那个字段」的分叉。
		spec, err := core.MCPSpecFromMap(mcpCfg)
		if err != nil {
			logger.Error("patch MCP config invalid, skipping",
				"plugin", entry.Name, "error", err)
			continue
		}
		if spec.ServerName == "" {
			logger.Warn("patch MCP config missing server_name, skipping", "plugin", entry.Name)
			continue
		}
		if _, err := spec.TransportKind(); err != nil {
			logger.Error("patch MCP config incomplete, skipping",
				"plugin", entry.Name, "server_name", spec.ServerName, "error", err)
			continue
		}
		// 经与 admin API / mcp_connect 相同的实现连接（唯一实现，避免三条入口各写一份）
		count, err := mgr.ConnectMCPServerSpec(context.Background(), spec)
		if err != nil {
			logger.Error("patch MCP connect failed",
				"plugin", entry.Name, "server_name", spec.ServerName,
				"transport", spec.Transport, "error", err)
			continue
		}
		logger.Info("patch MCP connected",
			"plugin", entry.Name, "server_name", spec.ServerName,
			"transport", spec.Transport, "tools", fmt.Sprintf("%d", count))
	}
}
