package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 管理面是**能力**，呈现层只是消费它的东西。这一组钉住那份清单的形状：
//
//   - 谁属于哪个管理面，由工具自己说（ManagementGroup / proto.Tool.management）；
//   - 清单里既有「模型也能用」的，也有「只给人用」的（ModelVisible 区分，但都列出来）；
//   - 汇总只有一个出口，呈现层不必认识任何子系统名字。
//
// 测试用假工具而不是某个真实子系统：这是通用契约，钉在具体子系统上，换个实现就得
// 改测试——MCP 搬成插件时正好撞上过这一点。

// demoOp 一个声明管理面归属的假工具。
type demoOp struct {
	name    string
	hidden  bool
	summary string
}

func (t *demoOp) Name() string        { return t.name }
func (t *demoOp) Description() string { return t.summary + "\n第二行不该出现在管理面里。" }
func (t *demoOp) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *demoOp) Execute(context.Context, json.RawMessage) (string, error) {
	return "ok:" + t.name, nil
}
func (t *demoOp) ManagementGroup() string { return "demo" }
func (t *demoOp) IsHidden() bool          { return t.hidden }

// registerDemoOps 注册两条管理操作：一条对模型可见、一条只给人用。
func registerDemoOps(t *testing.T, m *Manager) {
	t.Helper()
	for _, tool := range []*demoOp{
		{name: "demo_alpha", summary: "Alpha 操作。"},
		{name: "demo_beta", hidden: true, summary: "Beta 操作（只给人用）。"},
	} {
		if err := m.toolRegistry.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
}

// TestManagementOperationsAggregate 汇总是按归属聚合的，且命名由工具名推出。
func TestManagementOperationsAggregate(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	registerDemoOps(t, m)

	ops := m.ManagementOperations()
	want := []string{"demo/alpha", "demo/beta"}
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

// TestManagementOperationsIncludeHidden 对模型隐藏的操作仍要出现在清单里：
// 否则呈现层连「怎么打开它」都显示不出来（真实场景：/mcp on 一类开关）。
func TestManagementOperationsIncludeHidden(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	registerDemoOps(t, m)

	seen := map[string]bool{}
	for _, op := range m.ManagementOperations() {
		seen[op.Name] = op.ModelVisible
	}
	if visible, ok := seen["alpha"]; !ok || !visible {
		t.Fatalf("可见操作应标记 model_visible：%v", seen)
	}
	if visible, ok := seen["beta"]; !ok || visible {
		t.Fatalf("隐藏操作应出现且标记不可见：%v", seen)
	}
}

// TestManagementOperationsAreDeclaredNotNamed 归属是声明出来的，不是按名字前缀猜的：
// 插件工具（RemoteTool）只要带上 management 就出现在对应组里。
func TestManagementOperationsAreDeclaredNotNamed(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	rt := &RemoteTool{
		name:        "widget_tune",
		description: "Tune the widget.",
		schema:      json.RawMessage(`{"type":"object"}`),
		client:      &approvalStub{},
		management:  "widget",
	}
	if err := m.toolRegistry.Register(rt); err != nil {
		t.Fatalf("register: %v", err)
	}

	var found *ManagementOperation
	for _, op := range m.ManagementOperations() {
		if op.Group == "widget" {
			o := op
			found = &o
		}
	}
	if found == nil {
		t.Fatal("插件声明的管理操作未出现在清单里")
	}
	if found.Name != "tune" || found.Tool != "widget_tune" {
		t.Fatalf("操作名应由工具名剥离组前缀得到：%+v", found)
	}
	if found.Summary != "Tune the widget." {
		t.Fatalf("说明应只取第一行：%q", found.Summary)
	}
	for _, op := range m.ManagementOperations() {
		if op.Group == "" {
			t.Fatalf("空归属不应出现在清单里：%+v", op)
		}
	}
}

// TestManagementSummaryIsOneShortLine 管理面的说明是一句，不是工具手册。
//
// 工具描述是写给模型的，往往是一整段（实测 MCP 的 connect 有 250 字），原样塞进
// 目录会让面板与命令帮助垮掉。
func TestManagementSummaryIsOneShortLine(t *testing.T) {
	if got := managementSummary("First line.\nSecond line."); got != "First line." {
		t.Fatalf("应只取第一行，got %q", got)
	}
	long := strings.Repeat("字", 200)
	got := managementSummary(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长说明应截断并加省略号，got %q", got)
	}
	if n := len([]rune(got)); n > 81 {
		t.Fatalf("截断后仍是 %d 个字符", n)
	}
	if got := managementSummary("  "); got != "" {
		t.Fatalf("空白说明应为空，got %q", got)
	}
}
