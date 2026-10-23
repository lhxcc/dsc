package main

import (
	"context"
	"encoding/json"
	"os/exec"
	"testing"

	dsc "dsc-sdk"
	"dsc/core"
	"dsc/proto"
	"dsc/proto/metadata"

	plugin "github.com/hashicorp/go-plugin"
)

// assertView 校验工具结果经完整 gRPC 链路透传的 ViewJson。
func assertView(t *testing.T, resp *proto.ExecuteToolResponse) core.ToolView {
	t.Helper()
	if resp.ViewJson == "" {
		t.Fatalf("ViewJson 为空（ViewFn 未生效或 gRPC 透传缺失）: %+v", resp)
	}
	var v core.ToolView
	if err := json.Unmarshal([]byte(resp.ViewJson), &v); err != nil {
		t.Fatalf("ViewJson 非法: %v", err)
	}
	if v.Kind == "" {
		t.Fatalf("ViewJson 缺 kind: %q", resp.ViewJson)
	}
	return v
}

// TestE2EWithHostClient 端到端验证 tool-code-review 能被宿主拉起、暴露三个只读工具，
// 且无 LLM 互联时评审工具明确报错（而不是静默返回空结果）。
func TestE2EWithHostClient(t *testing.T) {
	dir := t.TempDir()
	exe := dsc.PJoin(dir, "tool-code-review")
	if out, err := exec.Command("go", "build", "-o", exe, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  core.Handshake,
		Plugins:          map[string]plugin.Plugin{},
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Cmd:              exec.Command(exe),
	})
	defer client.Kill()

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	grpcClient, ok := rpcClient.(*plugin.GRPCClient)
	if !ok {
		t.Fatalf("unexpected client type %T", rpcClient)
	}
	conn := grpcClient.Conn
	ctx := context.Background()

	info, err := metadata.NewPluginMetadataClient(conn).GetInfo(ctx, &metadata.Empty{})
	if err != nil || info.Type != "tool" || info.Name != "code-review" || info.ApiVersion != "1.0" {
		t.Fatalf("GetInfo = %+v, err %v", info, err)
	}

	tc := proto.NewToolServiceClient(conn)
	list, err := tc.ListTools(ctx, &proto.ListToolsRequest{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	names := map[string]bool{}
	readOnly := map[string]bool{}
	for _, tool := range list.Tools {
		names[tool.Name] = true
		for _, c := range tool.Capabilities {
			if c == "read-only" {
				readOnly[tool.Name] = true
			}
		}
	}
	for _, want := range []string{"code_review_diff", "code_review_scan", "code_review_rules"} {
		if !names[want] {
			t.Fatalf("missing tool %s, got %+v", want, list.Tools)
		}
		if !readOnly[want] {
			t.Fatalf("%s should declare read-only capability", want)
		}
	}

	run := func(tool, args string) *proto.ExecuteToolResponse {
		t.Helper()
		resp, err := tc.ExecuteTool(ctx, &proto.ExecuteToolRequest{ToolName: tool, ArgumentsJson: args})
		if err != nil {
			t.Fatalf("ExecuteTool(%s): %v", tool, err)
		}
		return resp
	}

	// code_review_rules：纯本地能力，无需 LLM —— 结果与表格视图都应透传。
	resp := run("code_review_rules", `{"paths":["a.go","web/app.tsx"]}`)
	if resp.Error != "" {
		t.Fatalf("code_review_rules = %+v", resp)
	}
	var rules rulesResult
	if err := json.Unmarshal([]byte(resp.Content), &rules); err != nil || len(rules.Rules) != 2 {
		t.Fatalf("rules content = %q, err %v", resp.Content, err)
	}
	if baseName(rules.Rules[0].Path) != "a.go" || rules.Rules[0].Rule != "go" {
		t.Fatalf("rules = %+v", rules.Rules)
	}
	if v := assertView(t, resp); v.Kind != "table" || v.Title != "Review Rules" || len(v.Rows) != 2 {
		t.Fatalf("rules view = %+v", v)
	}

	// 无 LLM 互联：评审工具必须报错说明原因（不能返回 ok + 空评论）。
	if resp := run("code_review_diff", `{}`); resp.Error == "" {
		t.Fatalf("code_review_diff without LLM should error, got %+v", resp)
	}
	if resp := run("code_review_scan", `{"paths":["a.go"]}`); resp.Error == "" {
		t.Fatalf("code_review_scan without LLM should error, got %+v", resp)
	}

	// 非法参数：错误信息应指出入参问题。
	if resp := run("code_review_diff", `not-json`); resp.Error == "" {
		t.Fatalf("invalid args should error, got %+v", resp)
	}
}
