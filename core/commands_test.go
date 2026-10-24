package core

import (
	"context"
	"testing"

	"dsc/proto"
	"dsc/proto/metadata"
	"google.golang.org/grpc"
)

// commandHook 实现 RunCommand 的假 hook 客户端（声明式命令转发测试用）。
type commandHook struct {
	*fakeHook
	gotName string
	gotArgs string
}

func (c *commandHook) RunCommand(_ context.Context, req *proto.RunCommandRequest, _ ...grpc.CallOption) (*proto.RunCommandResponse, error) {
	c.gotName, c.gotArgs = req.GetName(), req.GetArgs()
	return &proto.RunCommandResponse{Text: "output of " + req.GetName()}, nil
}

// TestCommandsComeFromDeclarations 命令表**完全来自插件声明**：核心不内置任何
// 命令名，没声明就没有，声明了才出现在表里，且按名排序（结果稳定）。
func TestCommandsComeFromDeclarations(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	m.mu.Lock()
	m.coreMetadata["plugin-b"] = &metadata.PluginInfo{
		Name: "plugin-b",
		Commands: []*metadata.PluginCommand{
			{Name: "zeta", Summary: "第二个插件的命令", Usage: "/zeta"},
		},
	}
	m.coreMetadata["plugin-a"] = &metadata.PluginInfo{
		Name: "plugin-a",
		Commands: []*metadata.PluginCommand{
			{Name: "alpha", Summary: "第一个插件的命令"},
		},
	}
	// 声明了命令但没有 hook 服务：无法执行，菜单里不该出现
	m.coreMetadata["plugin-c"] = &metadata.PluginInfo{
		Name:     "plugin-c",
		Commands: []*metadata.PluginCommand{{Name: "orphan"}},
	}
	m.toolHookClients["plugin-a"] = &commandHook{fakeHook: &fakeHook{}}
	m.toolHookClients["plugin-b"] = &commandHook{fakeHook: &fakeHook{}}
	m.toolHookOrder = []string{"plugin-a", "plugin-b"}
	m.mu.Unlock()

	cmds := m.Commands()
	if len(cmds) != 2 {
		t.Fatalf("应只有 2 条可执行命令（无 hook 服务的被跳过），实际 %+v", cmds)
	}
	if cmds[0].Name != "alpha" || cmds[1].Name != "zeta" {
		t.Fatalf("命令应按名排序: %+v", cmds)
	}
	// Usage 缺省时自动补成 /name
	if cmds[0].Usage != "/alpha" {
		t.Fatalf("Usage 缺省应补 /name，got %q", cmds[0].Usage)
	}
	if m.HasCommand("orphan") {
		t.Fatal("没有 hook 服务的声明不应算可执行命令")
	}
	if m.HasCommand("alpha") != true || m.HasCommand("/alpha") != true {
		t.Fatal("HasCommand 应同时接受带与不带前导 / 的名字")
	}
}

// TestRunCommandForwardsToDeclaringPlugin 转发目标由声明决定：宿主按名字找到
// 声明者，把原始参数原样交过去，返回插件给的文本。
func TestRunCommandForwardsToDeclaringPlugin(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	h := &commandHook{fakeHook: &fakeHook{}}
	m.mu.Lock()
	m.coreMetadata["plugin-a"] = &metadata.PluginInfo{
		Name:     "plugin-a",
		Commands: []*metadata.PluginCommand{{Name: "status", Summary: "看状态"}},
	}
	m.toolHookClients["plugin-a"] = h
	m.toolHookOrder = []string{"plugin-a"}
	m.mu.Unlock()

	text, err := m.RunCommand(context.Background(), "/status", "--json  x")
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if text != "output of status" {
		t.Fatalf("应返回插件给的文本，got %q", text)
	}
	if h.gotName != "status" {
		t.Fatalf("转发名应去掉前导 /，got %q", h.gotName)
	}
	if h.gotArgs != "--json  x" {
		t.Fatalf("参数应原样转发（不重排空白），got %q", h.gotArgs)
	}

	// 未声明的命令：报错而不是静默成功
	if _, err := m.RunCommand(context.Background(), "nope", ""); err == nil {
		t.Fatal("未声明的命令应返回错误")
	}
}

// TestCommandsDuplicateNameFirstWins 同名命令必须给出确定赢家（map 迭代随机，
// 不排序就会两次运行结果不同）。
func TestCommandsDuplicateNameFirstWins(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	h := &commandHook{fakeHook: &fakeHook{}}
	m.mu.Lock()
	m.coreMetadata["plugin-a"] = &metadata.PluginInfo{
		Commands: []*metadata.PluginCommand{{Name: "dup", Summary: "a"}},
	}
	m.coreMetadata["plugin-b"] = &metadata.PluginInfo{
		Commands: []*metadata.PluginCommand{{Name: "dup", Summary: "b"}},
	}
	m.toolHookClients["plugin-a"] = h
	m.toolHookClients["plugin-b"] = &commandHook{fakeHook: &fakeHook{}}
	m.mu.Unlock()

	cmds := m.Commands()
	if len(cmds) != 1 {
		t.Fatalf("同名命令应只留一条，实际 %+v", cmds)
	}
	// 按插件名排序后 plugin-a 先到，赢家固定
	if cmds[0].Plugin != "plugin-a" || cmds[0].Summary != "a" {
		t.Fatalf("赢家应固定为排序靠前者，got %+v", cmds[0])
	}
}

// TestNormalizeCommandName 归一规则：去空白与前导 /，含空白的名字视为非法
// （这种名字永远匹配不到 /xxx 输入）。
func TestNormalizeCommandName(t *testing.T) {
	cases := map[string]string{
		"/context":  "context",
		" context":  "context",
		"/a b":      "",
		"":          "",
		"///double": "//double",
	}
	for in, want := range cases {
		if got := normalizeCommandName(in); got != want {
			t.Errorf("normalizeCommandName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCommandsNilSafe 空管理器不得 panic（前端每帧都可能查询）。
func TestCommandsNilSafe(t *testing.T) {
	var m *Manager
	if got := m.Commands(); got != nil {
		t.Fatalf("nil Manager 应返回 nil，got %+v", got)
	}
	if m.HasCommand("x") {
		t.Fatal("nil Manager 不应报告有命令")
	}
	if _, err := m.RunCommand(context.Background(), "x", ""); err == nil {
		t.Fatal("nil Manager 应报错")
	}
	if got := NewManager(&ManagerConfig{}).Commands(); len(got) != 0 {
		t.Fatalf("没有插件时命令表应为空，got %+v", got)
	}
}
