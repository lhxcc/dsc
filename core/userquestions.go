package core

import (
	"context"
	"fmt"

	"dsc/proto"
	"dsc/userquestions"
)

// 用户评审通道（对齐 DSH user-questions）：宿主在 broker 上挂载
// UserQuestionsService；agent 侧工具（如 exit_plan_mode）经 gRPC 调用 Ask，
// 宿主转发给注册的 UI provider（TUI）并阻塞等待用户回答。

// UserQuestionProvider 宿主侧 UI provider：接收提问请求，返回用户回答。
// 实现需感知 ctx 取消（用户放弃/调用中止时返回错误）。
type UserQuestionProvider func(ctx context.Context, req *userquestions.Request) (*userquestions.Answer, error)

// RegisterUserQuestionProvider 注册唯一的 UI provider（TUI 启动时调用；
// 已注册时返回错误，对齐 DSH DUPLICATE_PROVIDER）。
func (m *Manager) RegisterUserQuestionProvider(p UserQuestionProvider) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.userQuestionProvider != nil {
		return &userquestions.Error{Code: "DUPLICATE_PROVIDER", Err: fmt.Errorf("a user-questions provider is already registered")}
	}
	m.userQuestionProvider = p
	return nil
}

// serveUserQuestionsLocked 在 broker 上挂载 UserQuestionsService 并返回 serviceID
// （需已持有 m.mu）。复用 serveUserQuestionsOnBroker 的实际挂载，此处额外登记字段。
func (m *Manager) serveUserQuestionsLocked() (uint32, error) {
	serviceID, err := m.serveUserQuestionsOnBroker(m.broker)
	if err == nil {
		m.userQuestionsServiceID = serviceID
	}
	return serviceID, err
}

// Ask 宿主侧 Ask 实现：校验请求 → 调 provider → 返回回答。
func (m *Manager) Ask(ctx context.Context, req *userquestions.Request) (*userquestions.Answer, error) {
	if err := userquestions.Validate(req); err != nil {
		return nil, err
	}
	m.mu.Lock()
	provider := m.userQuestionProvider
	m.mu.Unlock()
	if provider == nil {
		return nil, &userquestions.Error{Code: userquestions.ErrNoProvider, Err: fmt.Errorf("no user-questions provider is registered")}
	}
	// 提问即将展示给用户：广播事件让通知类插件提醒（用户离开屏幕时靠音效知道模型在等回答）。
	// 放在调 provider 之前、且不因事件失败影响提问——事件只是旁路提醒。
	m.emitUserQuestionAsked(req)
	return provider(ctx, req)
}

// emitUserQuestionAsked 广播「即将提问」事件；无事件总线时静默跳过。
//
// 广播前先看**能力**：没有插件提供 notify 时不广播。这不是省事，而是把
// 「不装通知插件 = 静音」做成用户可控的开关——如果照发不误，谁来接、接不接
// 都得靠插件自觉，用户也就失去了"卸载插件即关闭提醒"这个直觉开关。
// 判据用能力名而非插件名（将来换桌面通知插件，只要声明 notify 即接管）。
func (m *Manager) emitUserQuestionAsked(req *userquestions.Request) {
	if m == nil || m.events == nil || req == nil {
		return
	}
	if !m.HasPluginProvidingCapability("notify") {
		return
	}
	ev := UserQuestionAskedEvent{Questions: len(req.Questions)}
	if len(req.Questions) > 0 {
		ev.Header = req.Questions[0].Header
	}
	m.events.Emit(EventUserQuestionAsked, EventContext{Context: context.Background(), Data: ev})
}

// userQuestionsGRPCServer 暴露给 agent 的 gRPC 服务端。
type userQuestionsGRPCServer struct {
	proto.UnimplementedUserQuestionsServiceServer
	m *Manager
}

func (s *userQuestionsGRPCServer) Ask(ctx context.Context, req *proto.AskRequest) (*proto.AskResponse, error) {
	r := protoAskToDomain(req)
	ans, err := s.m.Ask(ctx, r)
	if err != nil {
		return &proto.AskResponse{Error: errorCode(err), Message: err.Error()}, nil
	}
	resp := &proto.AskResponse{}
	for _, a := range ans.Answers {
		resp.Answers = append(resp.Answers, &proto.AskAnswer{
			Id: a.ID, Selected: a.Selected, Custom: a.Custom,
		})
	}
	return resp, nil
}

// protoAskToDomain 把 gRPC 请求转成领域类型。
func protoAskToDomain(req *proto.AskRequest) *userquestions.Request {
	out := &userquestions.Request{}
	for _, q := range req.GetQuestions() {
		dq := userquestions.Question{
			ID: q.Id, Question: q.Question, Detail: q.Detail, Header: q.Header,
			MultiSelect: q.MultiSelect,
		}
		for _, o := range q.GetOptions() {
			dq.Options = append(dq.Options, userquestions.Option{Label: o.Label, Description: o.Description})
		}
		if it := q.GetIntent(); it != nil {
			dq.Intent = &userquestions.Intent{Kind: it.Kind, Approve: it.Approve}
		}
		out.Questions = append(out.Questions, dq)
	}
	return out
}

// errorCode 从错误中提取稳定错误码（对齐 DSH UserQuestionError codes）。
// provider 应返回带 code 的 *userquestions.Error；其余按内部错误处理。
func errorCode(err error) string {
	if ue, ok := err.(*userquestions.Error); ok {
		return ue.Code
	}
	return "INTERNAL"
}
