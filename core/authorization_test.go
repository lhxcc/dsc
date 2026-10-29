package core

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"

	"dsc/userquestions"
)

// 这一组钉住「范围 + 粒度」的语义：范围由工具声明，粒度由策略决定，两者正交。
//
//	工具级（默认）：每次调用都问；不记账（记账等于悄悄放行）。
//	范围级：同一范围问一次、记住；有未授权范围时才问。
//	never：缺失范围时 fail-closed 拒绝；**已预授予**的范围照常放行（那是人写进配置的
//	      明确决定，不是「问不到就放行」）。

// scopeTool 一个声明范围的进程内工具，可选是否也实现 ApprovalRequester。
type scopeTool struct {
	name   string
	scopes []string
}

func (t *scopeTool) Name() string                      { return t.name }
func (t *scopeTool) Description() string               { return "scope tool" }
func (t *scopeTool) ParametersSchema() json.RawMessage { return json.RawMessage(`{}`) }
func (t *scopeTool) Execute(context.Context, json.RawMessage) (string, error) {
	return "ran", nil
}
func (t *scopeTool) Scopes() []string { return t.scopes }
func (t *scopeTool) ApprovalReason(string) string {
	return "touches " + strings.Join(t.scopes, ", ")
}

// countingApprovalManager 挂审批门 + 一个会回答的 provider，并统计被问了几次。
func countingApprovalManager(t *testing.T, policy ApprovalPolicy, tool ToolDefinition) (*Manager, *int32) {
	t.Helper()
	m := newRouterManager()
	var asks int32
	if err := m.RegisterUserQuestionProvider(func(context.Context, *userquestions.Request) (*userquestions.Answer, error) {
		atomic.AddInt32(&asks, 1)
		return &userquestions.Answer{Answers: []userquestions.AnswerItem{{
			ID: "approval", Selected: []string{approvalAllowLabel},
		}}}, nil
	}); err != nil {
		t.Fatalf("RegisterUserQuestionProvider: %v", err)
	}
	if err := m.toolRegistry.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.SetApprovalPolicy(policy)
	m.events.OnWaterfall(EventToolPreExecute, m.toolApprovalGate())
	return m, &asks
}

// TestToolGranularityAsksEveryTime 默认粒度：两次调用问两次，且**不记账**。
func TestToolGranularityAsksEveryTime(t *testing.T) {
	t.Setenv("DSC_APPROVAL_GRANULARITY", "tool")
	tool := &scopeTool{name: "spawn_once", scopes: []string{ScopeProcessSpawn}}
	m, asks := countingApprovalManager(t, ApprovalAsk, tool)

	for i := 0; i < 2; i++ {
		if _, err := m.ExecuteTool(context.Background(), tool.name, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("第 %d 次调用应放行：%v", i+1, err)
		}
	}
	if got := atomic.LoadInt32(asks); got != 2 {
		t.Fatalf("工具级应每次都问，问了 %d 次", got)
	}
	if m.ScopeGranted(ScopeProcessSpawn) {
		t.Fatal("工具级不该记账（记账等于后续悄悄放行）")
	}
}

// TestScopeGranularityAsksOnce 范围级：问一次、记住，第二次不再打断。
func TestScopeGranularityAsksOnce(t *testing.T) {
	t.Setenv("DSC_APPROVAL_GRANULARITY", "scope")
	tool := &scopeTool{name: "spawn_once", scopes: []string{ScopeProcessSpawn}}
	m, asks := countingApprovalManager(t, ApprovalAsk, tool)

	for i := 0; i < 3; i++ {
		if _, err := m.ExecuteTool(context.Background(), tool.name, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("第 %d 次调用应放行：%v", i+1, err)
		}
	}
	if got := atomic.LoadInt32(asks); got != 1 {
		t.Fatalf("范围级只该问一次，问了 %d 次", got)
	}
	if !m.ScopeGranted(ScopeProcessSpawn) {
		t.Fatal("同意后应记入授权账本")
	}
	// 另一个工具声明同一范围：共享这次授权（这正是「按范围」的意义）
	other := &scopeTool{name: "spawn_again", scopes: []string{ScopeProcessSpawn}}
	if err := m.toolRegistry.Register(other); err != nil {
		t.Fatalf("register other: %v", err)
	}
	if _, err := m.ExecuteTool(context.Background(), other.name, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("共享范围应直接放行：%v", err)
	}
	if got := atomic.LoadInt32(asks); got != 1 {
		t.Fatalf("同范围不该再问，问了 %d 次", got)
	}
}

// TestScopeOnlyAsksForMissingScopes 一次调用触多个范围时，只问缺的那些。
func TestScopeOnlyAsksForMissingScopes(t *testing.T) {
	t.Setenv("DSC_APPROVAL_GRANULARITY", "scope")
	m, asks := countingApprovalManager(t, ApprovalAsk, &scopeTool{
		name:   "multi",
		scopes: []string{ScopeProcessSpawn, ScopeNetEgress},
	})
	m.SeedScopes([]string{ScopeProcessSpawn}) // 配置预授予其中一个

	if _, err := m.ExecuteTool(context.Background(), "multi", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("应放行：%v", err)
	}
	if got := atomic.LoadInt32(asks); got != 1 {
		t.Fatalf("有未授权范围时应问一次，问了 %d 次", got)
	}
	if !m.ScopeGranted(ScopeNetEgress) {
		t.Fatal("缺失的范围应在同意后记账")
	}
}

// TestUngrantedScopeRejectedUnderNever 策略 never = 不能问 → fail-closed 拒绝。
func TestUngrantedScopeRejectedUnderNever(t *testing.T) {
	t.Setenv("DSC_APPROVAL_GRANULARITY", "scope")
	tool := &scopeTool{name: "spawn", scopes: []string{ScopeProcessSpawn}}
	m, asks := countingApprovalManager(t, ApprovalNever, tool)

	if _, err := m.ExecuteTool(context.Background(), tool.name, json.RawMessage(`{}`)); err == nil {
		t.Fatal("never 下未授权范围应被拒绝")
	}
	if got := atomic.LoadInt32(asks); got != 0 {
		t.Fatalf("never 下不应弹审批，问了 %d 次", got)
	}
	if m.ScopeGranted(ScopeProcessSpawn) {
		t.Fatal("被拒绝的范围不该记账")
	}
}

// TestPreGrantedScopeAllowsUnderNever 配置预授予 + never：放行。
//
// 这不是矛盾：never 的语义是「不要问我，需要我点头的就拒掉」；而预授予是人**写进配置**
// 的决定，它已经把「需要点头」变成了「已经点头」。真正要防的是「问不到就放行」，这里
// 每一步都有明确授权来源。
func TestPreGrantedScopeAllowsUnderNever(t *testing.T) {
	t.Setenv("DSC_APPROVAL_GRANULARITY", "scope")
	tool := &scopeTool{name: "spawn", scopes: []string{ScopeProcessSpawn}}
	m, asks := countingApprovalManager(t, ApprovalNever, tool)
	m.SeedScopes([]string{ScopeProcessSpawn})

	if _, err := m.ExecuteTool(context.Background(), tool.name, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("预授予范围应放行：%v", err)
	}
	if got := atomic.LoadInt32(asks); got != 0 {
		t.Fatalf("预授予范围不该再问，问了 %d 次", got)
	}
}

// TestParsedGranularityDefaultsToTool 未知/空值一律按工具级（安全的一侧）。
func TestParsedGranularityDefaultsToTool(t *testing.T) {
	for _, in := range []string{"", "  ", "tool", "bogus"} {
		if got := ParseApprovalGranularity(in); got != GranularityPerTool {
			t.Fatalf("ParseApprovalGranularity(%q) = %q，期望 tool", in, got)
		}
	}
	for _, in := range []string{"scope", "SCOPES", " Scope "} {
		if got := ParseApprovalGranularity(in); got != GranularityPerScope {
			t.Fatalf("ParseApprovalGranularity(%q) = %q，期望 scope", in, got)
		}
	}
}
