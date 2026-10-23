package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"dsc/jobs"
	"dsc/proto"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// parallelMockHook 并发分发测试用客户端：arrived 用于证明多个客户端同时进入，
// release 阻塞到测试放行；errText 非空时返回插件级错误。
type parallelMockHook struct {
	hookMockClient
	arrived chan struct{}
	release chan struct{}
	errText string
}

func (c *parallelMockHook) OnEvent(context.Context, *proto.OnEventRequest, ...grpc.CallOption) (*proto.OnEventResponse, error) {
	if c.arrived != nil {
		c.arrived <- struct{}{}
	}
	if c.release != nil {
		<-c.release
	}
	return &proto.OnEventResponse{Error: c.errText}, nil
}

// TestDispatchEventParallel 回归跨进程事件的 parallel 模式：所有插件必须并发
// 进入（串行实现会在只到达一个客户端时阻塞超时），并聚合任一插件返回的错误。
func TestDispatchEventParallel(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	m.mu.Lock()
	m.toolHookClients["a"] = &parallelMockHook{arrived: arrived, release: release}
	m.toolHookClients["b"] = &parallelMockHook{arrived: arrived, release: release}
	m.toolHookOrder = []string{"a", "b"}
	m.mu.Unlock()

	const ev EventName = "test/parallel"
	eventDispatchMode[ev] = "parallel"
	defer delete(eventDispatchMode, ev)

	done := make(chan error, 1)
	go func() {
		_, err := m.dispatchEventToPlugins(ev, map[string]string{"k": "v"})
		done <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(2 * time.Second):
			t.Fatal("parallel dispatch did not reach both plugins concurrently")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("parallel dispatch: %v", err)
	}

	// 错误聚合：任一插件报错即返回（其余插件仍完成）
	errEv := EventName("test/parallel-error")
	eventDispatchMode[errEv] = "parallel"
	defer delete(eventDispatchMode, errEv)
	m.mu.Lock()
	m.toolHookClients["a"] = &parallelMockHook{errText: "plugin-a failed"}
	m.toolHookClients["b"] = &parallelMockHook{}
	m.mu.Unlock()
	if _, err := m.dispatchEventToPlugins(errEv, nil); err == nil || !strings.Contains(err.Error(), "plugin-a failed") {
		t.Fatalf("parallel error aggregation = %v, want plugin error", err)
	}
}

// transportFailHook 模拟「插件进程已退出」：OnEvent 返回连接错误（而非插件级
// error）。这类失败必须按不可用跳过，不能当作语义 veto。
type transportFailHook struct {
	hookMockClient
	calls int
}

func (c *transportFailHook) OnEvent(context.Context, *proto.OnEventRequest, ...grpc.CallOption) (*proto.OnEventResponse, error) {
	c.calls++
	return nil, status.Error(codes.Unavailable, "connection refused")
}

// TestDispatchEventSkipsDeadHookPlugin 回归：插件进程崩溃后，事件派发不得因
// 传输层失败而否决整轮请求（历史表现：agent/pre-step 被 veto，会话永久卡死）；
// 同一插件只拨号一次，之后按不可用跳过。
func TestDispatchEventSkipsDeadHookPlugin(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	dead := &transportFailHook{}
	m.mu.Lock()
	m.toolHookClients["dead"] = dead
	m.toolHookClients["live"] = &hookMockClient{}
	m.toolHookOrder = []string{"dead", "live"}
	m.mu.Unlock()

	const ev EventName = "test/waterfall-dead"
	eventDispatchMode[ev] = "waterfall"
	defer delete(eventDispatchMode, ev)

	res, err := m.dispatchEventToPlugins(ev, map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("transport failure must not veto: %v", err)
	}
	if res == nil {
		t.Fatal("expected passthrough result from waterfall")
	}
	if dead.calls != 1 {
		t.Fatalf("dead plugin calls = %d, want 1", dead.calls)
	}
	// 第二次派发：死插件已被标记，不再拨号。
	if _, err := m.dispatchEventToPlugins(ev, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if dead.calls != 1 {
		t.Fatalf("dead plugin dialed again: calls = %d", dead.calls)
	}
}

// TestDispatchEventStillHonorsPluginVeto 插件显式返回 error 仍是语义 veto。
func TestDispatchEventStillHonorsPluginVeto(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	m.mu.Lock()
	m.toolHookClients["vetoer"] = &parallelMockHook{errText: "policy denied"}
	m.toolHookOrder = []string{"vetoer"}
	m.mu.Unlock()

	const ev EventName = "test/waterfall-veto"
	eventDispatchMode[ev] = "waterfall"
	defer delete(eventDispatchMode, ev)

	if _, err := m.dispatchEventToPlugins(ev, map[string]string{"k": "v"}); err == nil || !strings.Contains(err.Error(), "policy denied") {
		t.Fatalf("explicit plugin error = %v, want veto", err)
	}
	// 显式 veto 不标记插件不可用：下一次仍应派发。
	if _, err := m.dispatchEventToPlugins(ev, map[string]string{"k": "v"}); err == nil {
		t.Fatal("second dispatch should still veto")
	}
}

// TestPluginNotifyJobDone 验证 job/done 特化：插件通知的任务快照被解析为
// JobSnapshot 并发布 JobDoneEvent（TUI 完成通知唤醒体系复用）。
func TestPluginNotifyJobDone(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	got := make(chan jobs.JobSnapshot, 1)
	m.OnEvent(JobDoneEvent, func(ctx EventContext) (any, error) {
		if s, ok := ctx.Data.(jobs.JobSnapshot); ok {
			got <- s
		}
		return nil, nil
	})
	srv := &coreNotifyServer{m: m}
	_, err := srv.Notify(context.Background(), &proto.NotifyRequest{
		Name: string(JobDoneEvent),
		Data: `{"ID":"novelforge-1","Kind":"novelforge","Label":"逆推生成","Status":"completed"}`,
	})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case s := <-got:
		if s.ID != "novelforge-1" || s.Kind != "novelforge" || s.Status != jobs.StatusCompleted {
			t.Fatalf("snapshot = %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("JobDoneEvent should be delivered")
	}

	// 坏 data → 错误，不发布
	if _, err := srv.Notify(context.Background(), &proto.NotifyRequest{
		Name: string(JobDoneEvent), Data: `{bad`,
	}); err == nil {
		t.Fatal("bad job/done data should fail")
	}
}

// TestPluginNotifyCustomEvent 验证插件自定义事件：data 按 JSON 解析后广播。
func TestPluginNotifyCustomEvent(t *testing.T) {
	m := NewManager(&ManagerConfig{})
	got := make(chan any, 1)
	m.OnEvent(EventName("novelforge/progress"), func(ctx EventContext) (any, error) {
		got <- ctx.Data
		return nil, nil
	})
	srv := &coreNotifyServer{m: m}
	_, err := srv.Notify(context.Background(), &proto.NotifyRequest{
		Name: "novelforge/progress", Data: `{"chapter":3}`,
	})
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	select {
	case d := <-got:
		obj, ok := d.(map[string]any)
		if !ok || obj["chapter"] != float64(3) {
			t.Fatalf("custom event data = %#v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("custom event should be delivered")
	}
}
