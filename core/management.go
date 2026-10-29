package core

import (
	"encoding/json"
	"sort"
	"strings"
)

// ManagementOperation 一条管理操作：可以执行、可以审批、可以让模型看见，但**不必**是
// 模型工具。
//
// 为什么与工具分开声明：管理面里有两类东西——
//   - 模型也能用的（`mcp_load`：模型自己决定加载哪些工具）；
//   - 只给人用的（`mcp on|off`：这是人的部署决定，模型不该有这个开关）。
//
// 混在工具表里只能靠「再补一个 hidden_for_model」来区分，而那个补丁迟早会出现在一个
// 不该出现的地方（目录装配、审批门、或者某个消费者旁边）。分开声明之后：操作是操作，
// 是否对模型曝光是它自己的属性（HiddenTool），呈现层则统一从本目录取。
type ManagementOperation struct {
	// Group 所属管理能力名（如 "mcp"）。呈现层据此把它们收成一个管理面。
	Group string
	// Name 操作名（组内唯一，如 "connect"）：呈现层用它做子命令名。
	Name string
	// Tool 真正执行它的工具名（如 "mcp_connect"）——操作可以只是一层引用，
	// 便于同一个实现被多个呈现层以不同名字暴露。
	Tool string
	// Summary 一句话说明（给人看，不是给模型看）。
	Summary string
	// Schema 参数 schema（呈现层据此生成帮助/表单/补全）。
	Schema json.RawMessage
	// ModelVisible 当前是否也进模型目录。呈现层可以据此区分「模型也能用」与
	// 「只给人用」，但不得据此改变可执行性（隐藏 ≠ 不可用）。
	ModelVisible bool
}

// ManagementDeclarer 可选接口：工具声明自己属于哪个管理面。
//
// 值就是管理能力名（"mcp"）。空串表示不是管理操作。进程内工具与插件工具都适用
// （后者经 proto.Tool.management 带上来）。
type ManagementDeclarer interface {
	ManagementGroup() string
}

// ManagementOperations 汇总当前所有管理操作，按（组，名）排序。
//
// 单一出口：TUI 的斜杆命令、admin REST、未来的 WEB 面板都从这里取同一份清单——
// 各自去扫工具表就等于各自实现一遍「哪些算管理操作」，迟早分叉。
func (m *Manager) ManagementOperations() []ManagementOperation {
	names := m.toolRegistry.List()
	out := make([]ManagementOperation, 0, len(names))
	for _, name := range names {
		tool, ok := m.toolRegistry.Get(name)
		if !ok {
			continue
		}
		md, ok := tool.(ManagementDeclarer)
		if !ok {
			continue
		}
		group := strings.TrimSpace(md.ManagementGroup())
		if group == "" {
			continue
		}
		visible := true
		if h, ok := tool.(HiddenTool); ok && h.IsHidden() {
			visible = false
		}
		out = append(out, ManagementOperation{
			Group:        group,
			Name:         managementOpName(group, name),
			Tool:         name,
			Summary:      managementSummary(tool.Description()),
			Schema:       tool.ParametersSchema(),
			ModelVisible: visible,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Group != out[j].Group {
			return out[i].Group < out[j].Group
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// managementOpName 由工具名推出操作名：去掉 "<group>_" 前缀（mcp_connect → connect）。
//
// 只做前缀剥离，不做模糊匹配：操作名是呈现层的子命令名，含糊的推断会让
// `/mcp list` 落到别的工具上。
func managementOpName(group, toolName string) string {
	if rest, ok := strings.CutPrefix(toolName, group+"_"); ok && rest != "" {
		return rest
	}
	return toolName
}

// managementSummary 取描述的第一行并截断到一句（管理面是给人看的，不是工具手册）。
//
// 工具描述往往是给模型的长段落（MCP 那个就有两百多字），原样塞进目录会让面板与
// /mcp 帮助整体垮掉；完整说明仍在工具描述里，呈现层需要时可以另取。
func managementSummary(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const maxRunes = 80
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxRunes {
		return string(r)
	}
	return strings.TrimSpace(string(r[:maxRunes])) + "…"
}
