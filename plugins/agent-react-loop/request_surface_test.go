package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	dsc "dsc-sdk"

	"dsc/proto"
	"dsc/session"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// 本文件守 agent 与「请求面改写」的边界：
//
//	agent 只跑循环与调度，**不做**窗口手术（不裁剪、不折叠、不做摘要）。
//
// 窗口适配由宿主收口（core/context_ceiling.go：改写完成后按需滑窗；provider 报超窗时
// core/overflow_adjust.go 滑窗后重试），压缩由当值的压缩后端在 agent/pre-step 里完成。
// agent 若自己再裁一刀：①他手里是改写前的列表，会替后端扔掉它要用的材料；②压缩是
// 增量的（前缀指纹 / 块覆盖），残次历史会让它每步重算；③「滑窗窗口内每条都是原始
// 消息」的不变量也只能由宿主那一层保证。消息身份已是稳定 id，所以这里不再涉及「引用
// 错位」——那是旧方案的毛病，早已修掉。

// recordingLLM 记录 Chat（压缩摘要等非流式调用）与 ChatStream（主请求）的入参与次数。
type recordingLLM struct {
	proto.LLMServiceClient

	mu          sync.Mutex
	chatCalls   int
	streamCalls int
	streamMsgs  []*proto.Message
	done        chan struct{} // ChatStream 首次收到请求时关闭
	usagePrompt int32         // 若 >0，主请求流携带该 prompt_tokens（模拟服务端用量）
	// usageFromRequest 为 true 时按本次请求的真实估算回报 prompt_tokens（真实 provider
	// 就是这样：prompt_tokens 反映实际发送量），用于校验展示口径与决策口径同源。
	usageFromRequest bool
}

func (m *recordingLLM) Chat(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (*proto.ChatResponse, error) {
	m.mu.Lock()
	m.chatCalls++
	m.mu.Unlock()
	return &proto.ChatResponse{Content: "非流式响应"}, nil
}

func (m *recordingLLM) ChatStream(ctx context.Context, in *proto.ChatRequest, opts ...grpc.CallOption) (grpc.ServerStreamingClient[proto.ChatStreamResponse], error) {
	m.mu.Lock()
	m.streamCalls++
	m.streamMsgs = in.Messages
	m.mu.Unlock()
	if m.done != nil {
		select {
		case <-m.done:
		default:
			close(m.done)
		}
	}
	var usage *proto.Usage
	switch {
	case m.usageFromRequest:
		est := int32(dsc.EstimateProtoMessagesTokens(in.Messages))
		usage = &proto.Usage{PromptTokens: est, TotalTokens: est + 10}
	case m.usagePrompt > 0:
		usage = &proto.Usage{PromptTokens: m.usagePrompt}
	}
	return &recordingStream{usage: usage}, nil
}

type recordingStream struct {
	sent  bool
	usage *proto.Usage
}

func (s *recordingStream) Recv() (*proto.ChatStreamResponse, error) {
	if !s.sent {
		s.sent = true
		return &proto.ChatStreamResponse{Content: "继续执行", FinishReason: "stop", Usage: s.usage}, nil
	}
	return nil, io.EOF
}
func (s *recordingStream) Header() (metadata.MD, error) { return nil, nil }
func (s *recordingStream) Trailer() metadata.MD         { return nil }
func (s *recordingStream) CloseSend() error             { return nil }
func (s *recordingStream) Context() context.Context     { return context.Background() }
func (s *recordingStream) SendMsg(m any) error          { return nil }
func (s *recordingStream) RecvMsg(m any) error          { return nil }

// buildOversizeSession 构造 rounds 轮、每条约 perMsgTokens 估算 token 的历史（带轮次标记，
// 便于断言「全量历史原样下发」）。总量取 rounds*2*perMsg，远大于测试用的窗口。
func buildOversizeSession(t *testing.T, a *ReactLoopAgent, rounds, perMsgTokens int) {
	t.Helper()
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	// ASCII 约 4 字符/token（dsc.EstimateTextTokens 的口径），故按 4 倍字符数构造
	body := strings.Repeat("x", perMsgTokens*4)
	for i := 0; i < rounds; i++ {
		mark := fmt.Sprintf("#round-%d", i)
		a.sess.Append(session.UserMessage, &session.UserMessageData{Content: mark + body, Source: "user"},
			&session.SurfaceOp{Op: session.SurfaceAppend})
		a.sess.Append(session.AssistantMessage, &session.AssistantMessageData{Content: mark + body},
			&session.SurfaceOp{Op: session.SurfaceAppend})
	}
	markSessionRestored(t, a)
}

// TestAgentSendsFullHistoryVerbatim 历史远超窗口时，agent 仍然**原样下发全量派生历史**：
// 不裁剪、不折叠、不额外发压缩请求。
//
// 这是职责边界的关键回归：窗口装不下不是 agent 的事——宿主会在请求发出前量表面滑窗，
// 压缩后端会在 pre-step 里改写。agent 一旦自己裁，压缩后端的位置引用就全错位。
func TestAgentSendsFullHistoryVerbatim(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	buildOversizeSession(t, a, 10, 10000) // 20 条 × 10k ≈ 200K ≈ 4 倍窗口

	full := a.sess.DeriveMessages()
	before := dsc.EstimateProtoMessagesTokens(full)
	if before <= a.contextWindow*2 {
		t.Fatalf("夹具不够大：估算 %d，应至少为窗口两倍", before)
	}

	llm := &recordingLLM{done: make(chan struct{})}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	var frames []*dsc.RunStreamResponse
	res, err := a.runLoop(context.Background(), "继续", nil, func(f *dsc.RunStreamResponse) {
		frames = append(frames, f)
	})
	if err != nil {
		t.Fatalf("超窗历史不应让 agent 侧报错（窗口适配是宿主的事）: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("结果状态 = %s, 期望 success", res.Status)
	}

	llm.mu.Lock()
	sent := llm.streamMsgs
	streamCalls := llm.streamCalls
	chatCalls := llm.chatCalls
	llm.mu.Unlock()

	if streamCalls == 0 || len(sent) == 0 {
		t.Fatal("主请求未发出")
	}
	// 1) 条数一条不少：system + 全量历史 + 当前输入
	if len(sent) < len(full) {
		t.Fatalf("请求条数 = %d, 派生产生 %d——agent 不得裁掉历史", len(sent), len(full))
	}
	// 2) 最早一轮逐字仍在（裁剪会从最旧处开始丢）
	if !strings.Contains(sent[1].GetContent(), "#round-0") {
		t.Fatalf("最早一轮历史不见了：agent 不得做请求级裁剪")
	}
	// 3) 正文一字不改（折叠会在正文里插入「已折叠」说明）
	for _, m := range sent {
		if strings.Contains(m.GetContent(), "已折叠") || strings.Contains(m.GetContent(), "已省略") {
			t.Fatalf("请求面出现改写痕迹，agent 不得改写消息正文: %.120s", m.GetContent())
		}
	}
	// 4) 没有额外发压缩请求：agent 不实现压缩
	if chatCalls != 0 {
		t.Fatalf("agent 不应自己发起压缩请求（额外 %d 次非流式调用）", chatCalls)
	}
	// 5) 前端也不输出「裁剪/折叠」提示帧：这种手术已不属于 agent
	for _, f := range frames {
		if strings.Contains(f.Output, "上下文裁剪") || strings.Contains(f.Output, "上下文折叠") {
			t.Fatalf("agent 不应输出裁剪/折叠提示: %q", f.Output)
		}
	}
}

// TestAgentSendsSingleOversizeMessageVerbatim 单条消息本身超窗时同样原样下发：
// 「装不下」由宿主判定（单条超窗在本地报错，不截断正文、也不交给 provider 换 400）。
func TestAgentSendsSingleOversizeMessageVerbatim(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 50000
	a.llmServiceID, a.toolServiceID = 1, 1
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	a.sess.Append(session.UserMessage, &session.UserMessageData{Content: "先前的短消息", Source: "user"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	markSessionRestored(t, a)

	llm := &recordingLLM{done: make(chan struct{})}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	paste := strings.Repeat("x", 60*1000*4) // ≈ 60k tokens > 50k 窗口
	if _, err := a.runLoop(context.Background(), paste, nil, func(*dsc.RunStreamResponse) {}); err != nil {
		t.Fatalf("agent 不应因单条超窗报错（宿主负责判定与报错）: %v", err)
	}
	llm.mu.Lock()
	defer llm.mu.Unlock()
	if llm.streamCalls == 0 {
		t.Fatal("应发出请求（正文原样）")
	}
	last := llm.streamMsgs[len(llm.streamMsgs)-1]
	if last.GetContent() != paste {
		t.Fatalf("超长输入被改写了（长度 %d → %d）：agent 不得截断正文", len(paste), len(last.GetContent()))
	}
}

// TestUsageFramesCarryContextTokens 展示口径与决策口径必须同源：
// agent 随 usage 帧上报 core.TokenMeter 的投影值（Usage.ContextTokens），
// 宿主据此显示「已用」，不再自己按 prompt_tokens 另算一套。
func TestUsageFramesCarryContextTokens(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 200000
	a.llmServiceID, a.toolServiceID = 1, 1
	a.sess.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	a.sess.Append(session.UserMessage, &session.UserMessageData{Content: strings.Repeat("x", 4000), Source: "user"},
		&session.SurfaceOp{Op: session.SurfaceAppend})
	markSessionRestored(t, a)

	llm := &recordingLLM{usageFromRequest: true}
	a.llmClient = llm
	a.toolClient = &mockToolClient{}

	var frames []*dsc.RunStreamResponse
	if _, err := a.runLoop(context.Background(), "继续", nil, func(f *dsc.RunStreamResponse) {
		frames = append(frames, f)
	}); err != nil {
		t.Fatalf("runLoop: %v", err)
	}

	sawUsage := false
	for _, f := range frames {
		if f.Usage == nil {
			continue
		}
		sawUsage = true
		if f.Usage.ContextTokens <= 0 {
			t.Fatalf("usage 帧应携带 ContextTokens（上下文投影），实际 %+v", f.Usage)
		}
		// 正常回合里投影应贴近实测 prompt（同一份上下文的两种量法，差一行回复的量级）
		if diff := int(f.Usage.ContextTokens) - int(f.Usage.PromptTokens); diff > 2000 || diff < -2000 {
			t.Fatalf("ContextTokens=%d 与实测 PromptTokens=%d 偏差过大（%+d）",
				f.Usage.ContextTokens, f.Usage.PromptTokens, diff)
		}
	}
	if !sawUsage {
		t.Fatal("未发出任何带 usage 的帧")
	}
}

// TestSwitchSessionResetsUsage 切换会话必须清掉上一会话的用量。
//
// 此前 SwitchSession 只换 sess 与 turnCounter，lastPromptTokens 留着上一会话的值。
// 一旦上个会话占满 1M 窗口，切到新会话后「已用容量」仍显示旧值，新会话的空历史会被
// 判定超窗（宿主侧据此滑窗），界面上的数字也不对。
func TestSwitchSessionResetsUsage(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 1000000
	a.llmServiceID, a.toolServiceID = 1, 1

	a.usageMu.Lock()
	a.lastPromptTokens = 900000
	a.usageMu.Unlock()

	if err := a.SwitchSession(context.Background(), "another-session"); err != nil {
		t.Fatalf("SwitchSession: %v", err)
	}

	a.usageMu.Lock()
	gotTokens := a.lastPromptTokens
	a.usageMu.Unlock()
	if gotTokens != 0 {
		t.Fatalf("切会话后 lastPromptTokens 应清零，实际 %d", gotTokens)
	}
	if a.contextSearchSeen {
		t.Fatal("切会话后 context_search 可见性应重新裁定")
	}
}

// TestAgentDeclaresCompactionDependency 压缩是 agent 的**强依赖**，必须声明在 metadata 里：
// 宿主据此在依赖未满足时把 agent 留在 PENDING 并让启动失败（不按插件名判断、不猜）。
//
// 这条断言看着琐碎，但它守的是「agent 不实现压缩」的另一半：既然自己不做，就必须
// 声明需要谁来提供——删掉一条 Requires，等于允许 agent 在没有压缩能力的环境里裸奔。
func TestAgentDeclaresCompactionDependency(t *testing.T) {
	cfg := agentSDKConfig()
	if cfg.Type != dsc.TypeAgent {
		t.Fatalf("类型 = %v, 期望 agent", cfg.Type)
	}
	want := map[dsc.CapabilityRequirement]bool{
		{Type: "llm", Capability: "llm"}:        false,
		{Type: "dsc", Capability: "compaction"}: false,
	}
	for _, r := range cfg.Requires {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for req, found := range want {
		if !found {
			t.Fatalf("agent 缺少能力依赖声明 %+v（现有 %+v）", req, cfg.Requires)
		}
	}
}
