package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"dsc/proto"
	"google.golang.org/grpc"
)

// 这一组钉住两条**通用**契约（不针对任何具体子系统）：
//
//	1. 工具自己决定「进不进模型目录」（HiddenTool），装配点不认识任何名字前缀；
//	2. 工具自己决定「要不要审批」，跨进程的插件工具也能给出本次调用的理由。
//
// 它们存在的理由是同一条：以前这两件事都写在宿主里（前者按 mcp__ 前缀在目录装配处
// 过滤，后者只能由进程内工具实现接口），于是每加一个「有曝光/审批策略」的提供方，
// 就要在宿主里再加一段特例。契约搬回提供方之后，宿主保持通用。

// hiddenTool 一个自报隐藏的进程内工具。
type hiddenTool struct {
	name   string
	hidden bool
}

func (t *hiddenTool) Name() string                      { return t.name }
func (t *hiddenTool) Description() string               { return "hidden tool" }
func (t *hiddenTool) ParametersSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *hiddenTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "ran:" + t.name, nil
}
func (t *hiddenTool) IsHidden() bool { return t.hidden }

// TestHiddenToolInvisibleButExecutable 隐藏只影响目录，不影响注册与执行。
//
// 「隐藏 ≠ 不可用」是这套机制的语义边界：后台任务、子代理、按名调用都照常工作，
// 只是模型看不到它的说明、不再为它付每个请求的目录成本。
func TestHiddenToolInvisibleButExecutable(t *testing.T) {
	m := newRouterManager()
	tool := &hiddenTool{name: "quiet_tool", hidden: true}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}

	for _, def := range m.AllToolsProto() {
		if def.GetName() == "quiet_tool" {
			t.Fatal("自报隐藏的工具不应出现在模型目录里")
		}
	}
	if _, ok := m.toolRegistry.Get("quiet_tool"); !ok {
		t.Fatal("隐藏的工具仍应注册（可按名调用）")
	}
	out, err := m.ExecuteTool(context.Background(), "quiet_tool", json.RawMessage(`{}`))
	if err != nil || out != "ran:quiet_tool" {
		t.Fatalf("隐藏工具应仍可执行，got %q err=%v", out, err)
	}

	// 改主意立刻生效：下一次目录装配就看得见，不需要重新注册
	tool.hidden = false
	found := false
	for _, def := range m.AllToolsProto() {
		if def.GetName() == "quiet_tool" {
			found = true
		}
	}
	if !found {
		t.Fatal("取消隐藏后应出现在目录里")
	}
}

// approvalStub 注入 ListTools / ApprovalReason 响应，验证插件声明的两个字段逐跳存活。
type approvalStub struct {
	proto.ToolServiceClient
	tools      []*proto.Tool
	reason     string
	reasonErr  error
	reasonArgs string // 记录宿主传下来的参数，校验「理由按本次调用算」不是空话
}

func (s *approvalStub) ListTools(context.Context, *proto.ListToolsRequest, ...grpc.CallOption) (*proto.ListToolsResponse, error) {
	return &proto.ListToolsResponse{Tools: s.tools}, nil
}

func (s *approvalStub) ApprovalReason(_ context.Context, in *proto.ToolApprovalRequest, _ ...grpc.CallOption) (*proto.ToolApprovalResponse, error) {
	s.reasonArgs = in.GetArgumentsJson()
	if s.reasonErr != nil {
		return nil, s.reasonErr
	}
	return &proto.ToolApprovalResponse{Reason: s.reason}, nil
}

// TestRemoteToolCarriesHiddenAndApprovalHint 两个字段必须从插件声明一路活到 RemoteTool：
// 中间任何一跳丢掉，表现都是「插件说了要审批/要隐藏，宿主当没听见」。
func TestRemoteToolCarriesHiddenAndApprovalHint(t *testing.T) {
	stub := &approvalStub{tools: []*proto.Tool{{
		Name:           "spawn_tool",
		Description:    "d",
		ParametersJson: `{"type":"object"}`,
		Hidden:         true,
		Scopes:         []string{ScopeProcessSpawn},
	}}}
	defs, _, err := listStagedTools(stub)
	if err != nil {
		t.Fatalf("listStagedTools: %v", err)
	}
	rt, ok := defs[0].(*RemoteTool)
	if !ok {
		t.Fatalf("期望 *RemoteTool，得 %T", defs[0])
	}
	if !rt.IsHidden() {
		t.Fatal("hidden 字段在聚合层丢失")
	}
	if got := rt.approvalReason(context.Background(), `{}`); !strings.Contains(got, ScopeProcessSpawn) {
		t.Fatalf("scopes 在聚合层丢失：%q", got)
	}
}

// TestApprovalReasonComesFromPlugin 理由按本次参数向插件索取（不是一串静态文案）。
func TestApprovalReasonComesFromPlugin(t *testing.T) {
	stub := &approvalStub{
		reason: "Spawns `node server.mjs` as a local process",
		tools:  []*proto.Tool{{Name: "spawn_tool", ParametersJson: `{}`, Scopes: []string{ScopeProcessSpawn}}},
	}
	rt := &RemoteTool{name: "spawn_tool", client: stub, scopes: []string{ScopeProcessSpawn}}
	args := `{"command":"node","args":["server.mjs"]}`
	if got := rt.approvalReason(context.Background(), args); !strings.Contains(got, "node server.mjs") {
		t.Fatalf("应使用插件给的具体理由，got %q", got)
	}
	if stub.reasonArgs != args {
		t.Fatalf("宿主应把本次参数传给插件，got %q", stub.reasonArgs)
	}
}

// TestApprovalReasonFallsBackToHint 插件没实现该 RPC（旧插件）时回退到静态说明——
// 但绝不能因为「问不到」就变成放行：审批门是安全边界。
func TestApprovalReasonFallsBackToHint(t *testing.T) {
	stub := &approvalStub{reasonErr: errors.New("unimplemented")}
	rt := &RemoteTool{name: "spawn_tool", client: stub, scopes: []string{ScopeProcessSpawn}}
	if got := rt.approvalReason(context.Background(), `{}`); !strings.Contains(got, ScopeProcessSpawn) {
		t.Fatalf("插件给不出理由时应按范围合成说明，got %q", got)
	}

	// 没声明审批的工具：不问、也不要理由
	plain := &RemoteTool{name: "read_tool", client: stub}
	if got := plain.approvalReason(context.Background(), `{}`); got != "" {
		t.Fatalf("未声明审批的工具不应产生理由，got %q", got)
	}
}

// TestApprovalGateCoversRemoteTools 审批门必须对插件工具生效：策略为 never 时
// 声明了审批的插件工具被自动拒绝（fail-closed），而不是照常执行。
func TestApprovalGateCoversRemoteTools(t *testing.T) {
	m := newRouterManager()
	rt := &RemoteTool{
		name:        "spawn_tool",
		description: "d",
		schema:      json.RawMessage(`{}`),
		client:      &approvalStub{reason: "spawns a local process"},
		scopes:      []string{ScopeProcessSpawn},
	}
	if err := m.toolRegistry.Register(rt); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetApprovalPolicy(ApprovalNever)
	m.events.OnWaterfall(EventToolPreExecute, m.toolApprovalGate())

	out, err := m.ExecuteTool(context.Background(), "spawn_tool", json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("never 策略下声明审批的插件工具应被拒绝，却执行了：%q", out)
	}
	if !strings.Contains(err.Error(), "approval") {
		t.Fatalf("错误信息应说明审批被拒，got %v", err)
	}
}
