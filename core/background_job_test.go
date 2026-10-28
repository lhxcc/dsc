package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"dsc/proto"
)

// slowTool 执行一段时间才返回，用于观察空闲看门狗是否生效。
type slowTool struct {
	name string
	d    time.Duration
}

func (t *slowTool) Name() string                      { return t.name }
func (t *slowTool) Description() string               { return "slow tool" }
func (t *slowTool) ParametersSchema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *slowTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	select {
	case <-time.After(t.d):
		return "done", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// idlePolicy 返回只含空闲预算裁决的策略客户端。
func idlePolicy(idleMS int) *mockPolicyClient {
	pc := newMockPolicyClient()
	pc.decide = func(ev *proto.PolicyEvent) *proto.PolicyDecision {
		if ev.GetKind() == policyEventExecute {
			return &proto.PolicyDecision{Timeout: &proto.TimeoutSpec{IdleMs: int64(idleMS), Message: "idle timeout"}}
		}
		return &proto.PolicyDecision{}
	}
	return pc
}

// TestBackgroundJobSurvivesIdleWatchdog 后台任务不装空闲看门狗。
//
// 语义：命令转入后台意味着「已交出去，模型可以干别的」。几十秒不吐字的构建/测试
// 是常态，用「无输出」判死它既会丢掉工作，也和前台宽限期自相矛盾（那时它正被允许
// 安静地跑）。终止只应走 job_kill。前台执行仍照旧受空闲预算约束。
func TestBackgroundJobSurvivesIdleWatchdog(t *testing.T) {
	m := newPipelineManager(t)
	if err := m.toolRegistry.Register(&slowTool{name: "slow-bg", d: 250 * time.Millisecond}); err != nil {
		t.Fatalf("register: %v", err)
	}
	m.bridgePolicyToPipeline("timeout-policy", idlePolicy(50))

	// 后台执行域：空转 250ms 远超 50ms 预算，仍须跑完
	bg := WithBackgroundJob(context.Background())
	out, err := m.ExecuteTool(bg, "slow-bg", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("后台任务不应被空闲预算取消: %v", err)
	}
	if out != "done" {
		t.Fatalf("out = %q", out)
	}

	// 前台执行域：同样的工具仍受预算约束（行为未被削弱）
	if _, err := m.ExecuteTool(context.Background(), "slow-bg", json.RawMessage(`{}`)); err == nil {
		t.Fatal("前台执行应被空闲看门狗取消（说明策略仍在生效）")
	}
}

// TestDetachJobContextCarriesBackgroundMark 后台管道的 ctx 必须自带标记，
// 否则看门狗会在后台任务上照常安装。
func TestDetachJobContextCarriesBackgroundMark(t *testing.T) {
	ctx := WithCaller(context.Background(), "sess-1")
	d := detachJobContext(ctx)
	if !IsBackgroundJob(d) {
		t.Fatal("后台 ctx 应带后台执行域标记")
	}
	if CallerFromContext(d) != "sess-1" {
		t.Fatal("归属信息不得丢失")
	}
	if IsBackgroundJob(context.Background()) {
		t.Fatal("普通 ctx 不应被误判为后台任务")
	}
}
