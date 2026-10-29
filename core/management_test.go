package core

import (
	"encoding/json"
	"strings"
	"testing"
)

// 管理面是**能力**，呈现层只是消费它的东西。这一组钉住那份清单的形状：
//
//   - 谁属于哪个管理面，由工具自己说（ManagementGroup / proto.Tool.management）；
//   - 清单里既有「模型也能用」的，也有「只给人用」的（ModelVisible 区分，但都列出来）；
//   - 汇总只有一个出口，呈现层不必认识任何子系统名字。

// TestManagementOperationsAggregate 汇总是按归属聚合的，且命名由工具名推出。
func TestManagementOperationsAggregate(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	ops := m.ManagementOperations()

	want := []string{"mcp/connect", "mcp/disconnect", "mcp/list", "mcp/load", "mcp/unload"}
	var got []string
	for _, op := range ops {
		got = append(got, op.Group+"/"+op.Name)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("管理操作清单不符：\n got=%v\nwant=%v", got, want)
	}
	for _, op := range ops {
		if op.Tool == "" || len(op.Schema) == 0 {
			t.Fatalf("%s 缺工具名或参数 schema：%+v", op.Name, op)
		}
		if op.Summary == "" || strings.Contains(op.Summary, "\n") {
			t.Fatalf("%s 的说明应是显示用的一行：%q", op.Name, op.Summary)
		}
	}
}

// TestManagementOperationsIncludeHidden 关闭状态下这些操作对模型不可见，但**仍然**
// 出现在清单里——否则呈现层连「怎么打开它」都显示不出来（/mcp on 正是这一类操作）。
func TestManagementOperationsIncludeHidden(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	for _, op := range m.ManagementOperations() {
		if op.ModelVisible {
			t.Fatalf("MCP 默认关闭时 %s 不应对模型可见", op.Name)
		}
	}

	m.SetMCPToolsEnabled(true)
	visible := 0
	for _, op := range m.ManagementOperations() {
		if op.ModelVisible {
			visible++
		}
	}
	// 打开但没有服务器：只有入口 connect 对模型可见（其余是空操作，藏起来）
	if visible != 1 {
		t.Fatalf("打开且无服务器时只应有入口对模型可见，实际 %d 个", visible)
	}
}

// TestManagementOperationsAreDeclaredNotNamed 归属是声明出来的，不是按名字前缀猜的：
// 插件工具（RemoteTool）只要带上 management 就出现在对应组里。
func TestManagementOperationsAreDeclaredNotNamed(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	rt := &RemoteTool{
		name:        "demo_frobnicate",
		description: "Frobnicate the widget.\nLonger explanation not shown here.",
		schema:      json.RawMessage(`{"type":"object"}`),
		client:      &approvalStub{},
		management:  "demo",
	}
	if err := m.toolRegistry.Register(rt); err != nil {
		t.Fatalf("register: %v", err)
	}

	var found *ManagementOperation
	for _, op := range m.ManagementOperations() {
		if op.Group == "demo" {
			o := op
			found = &o
		}
	}
	if found == nil {
		t.Fatal("插件声明的管理操作未出现在清单里")
	}
	if found.Name != "frobnicate" || found.Tool != "demo_frobnicate" {
		t.Fatalf("操作名应由工具名剥离组前缀得到：%+v", found)
	}
	if found.Summary != "Frobnicate the widget." {
		t.Fatalf("说明应只取第一行：%q", found.Summary)
	}

	// 普通工具（没有归属声明）不得混进管理面
	for _, op := range m.ManagementOperations() {
		if op.Group == "" {
			t.Fatalf("空归属不应出现在清单里：%+v", op)
		}
	}
}
