package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestUserGrantAllBypassesApprovalGate 进程级预授权（-with-user-grant）让需要审批的工具
// 直接执行，关掉又回到 fail-closed。
//
// 这条开关的存在理由：无人值守入口（-input / -headless）没有问询通道，审批门会把所有
// 写操作拒掉——模型连装个插件都做不到，而它并没有越权，只是没有人可以问。开关由启动
// 参数设置，等价于用户预先对每次询问都点了 Allow。
//
// 两个方向都要守：打开时确实放行，关掉时确实拒绝（否则等于把审批永久关死）。
func TestUserGrantAllBypassesApprovalGate(t *testing.T) {
	m := approvalTestManager(t, SandboxWorkspaceWrite, ApprovalAsk)
	_ = m.toolRegistry.Register(&askApprovalTool{name: "gated_op", reason: "destructive"})

	// 无问询通道 + 无预授权 → fail-closed。
	if _, err := m.ExecuteTool(context.Background(), "gated_op", json.RawMessage(`{}`)); err == nil {
		t.Fatal("没有审批通道时应当拒绝")
	} else if !strings.Contains(err.Error(), "no approval channel") {
		t.Fatalf("应报审批通道缺失，got %v", err)
	}

	// 打开预授权 → 直接放行。
	m.SetUserGrantAll(true)
	res, err := m.ExecuteTool(context.Background(), "gated_op", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("预授权下应放行，got %v", err)
	}
	if res != "mock-result" {
		t.Fatalf("结果应来自工具本身，got %q", res)
	}

	// 关掉 → 回到拒绝（开关不是一次性的）。
	m.SetUserGrantAll(false)
	if _, err := m.ExecuteTool(context.Background(), "gated_op", json.RawMessage(`{}`)); err == nil {
		t.Fatal("关掉预授权后应重新拒绝")
	}
}

// TestUserGrantAllApprovesSandboxEscalation 预授权同样覆盖沙箱升级门。
//
// 两条门必须一起管：只放开工具审批、把升级门留着，无人值守下模型仍然写不了工作区外
// 的文件——这跟 TUI 里点 Allow 的效果不一致（那边一次批准就包含升级）。
func TestUserGrantAllApprovesSandboxEscalation(t *testing.T) {
	orig := WorkspaceRoot
	WorkspaceRoot = t.TempDir()
	defer func() { WorkspaceRoot = orig }()

	m := approvalTestManager(t, SandboxWorkspaceWrite, ApprovalAsk)
	_ = m.toolRegistry.Register(&mockTool{name: "str_replace_editor"})

	args := escArgs("danger-full-access", "目标文件在工作区之外")
	// 无通道 + 无预授权：升级请求被拒。
	if _, err := m.ExecuteTool(context.Background(), "str_replace_editor", args); err == nil {
		t.Fatal("没有审批通道时升级应被拒绝")
	}

	// 预授权：升级放行（工具实际执行失败与否无所谓，这里只看门是否放行）。
	m.SetUserGrantAll(true)
	_, err := m.ExecuteTool(context.Background(), "str_replace_editor", args)
	if err != nil && strings.Contains(err.Error(), "approval") {
		t.Fatalf("预授权下升级不应再被审批拦，got %v", err)
	}
}
