package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"dsc/proto"
	"dsc/proto/metadata"
	"github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// LLMProvider 是插件必须实现的业务接口
type LLMProvider interface {
	// Chat 以非流式方式调用 LLM；maxTokens <= 0 表示使用服务端默认
	Chat(ctx context.Context, messages []Message, tools []Tool, maxTokens int) (*ChatResponse, error)
	// ChatStream 以流式方式调用 LLM，返回增量内容通道；通道关闭表示该轮结束。
	//
	// maxTokens 是本帧的输出预算，由宿主统一决定（它才同时知道窗口、本帧表面与「服务端
	// 亲口认过的上限」）。三种语义与 Chat 一致：>0 携带该值、0 = 用插件配置、<0 = 明确
	// 不携带 max_tokens 字段。插件不该自己再按窗口算一遍——那会让每个 provider 各有一套
	// 估算，且改了窗口策略要改 N 个插件。
	ChatStream(ctx context.Context, messages []Message, tools []Tool, maxTokens int) (<-chan *ChatStreamResponse, error)
	Name(ctx context.Context) string
	Version(ctx context.Context) string
	HealthCheck(ctx context.Context) error
	// VisionEnabled 报告该 LLM 是否启用图像输入（供宿主经 GetInfo 的 capabilities
	// 暴露给 TUI 等上层，用于「图片已解析但模型不支持」时的用户提示）。
	VisionEnabled() bool
}

// Message 消息结构体
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"` // assistant 消息回传的工具调用
	// Images 图像附件（data:image/...;base64,... 数据 URL；user 消息与工具结果消息均可承载，视觉模型可见）
	Images []string `json:"images,omitempty"`
}

// Tool 工具结构体
type Tool struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	ParametersJSON string `json:"parameters_json"`
}

// ChatResponse 聊天响应结构体
type ChatResponse struct {
	Content      string     `json:"content"`
	FinishReason string     `json:"finish_reason"`
	ToolCalls    []ToolCall `json:"tool_calls"`
}

// ChatStreamResponse 是 LLM 流式响应中的一帧增量内容
type ChatStreamResponse struct {
	Content      string     `json:"content"`       // 增量文本
	FinishReason string     `json:"finish_reason"` // 非空表示该轮结束
	ToolCalls    []ToolCall `json:"tool_calls"`
	Error        string     `json:"error,omitempty"`
	Usage        *Usage     `json:"usage,omitempty"`     // 本轮 token 用量（finish_reason 帧携带）
	Reasoning    string     `json:"reasoning,omitempty"` // 思考过程增量文本（DeepSeek reasoning_content / Anthropic thinking 等）
}

// ToolCall 工具调用结构体
// unparseableArgs reports a tool call whose arguments are not valid JSON.
//
// The service hands the model's text through, and a model can write a call it cannot close: one missing </parameter>
// makes the engine's parser fold the rest of the call into the first value, and the assembled arguments then end
// inside a string (reported from a 2080 Ti box: `{"command":"cd F:/X && sed -i '968d' a.xml <parameter=description>
// Delete line 968 </tool_call>`).  Running that call with an empty argument set - which this file used to do - runs
// the tool on a guess.  The message keeps the beginning of what arrived, because that is what tells a reader whether
// the model or the parser is at fault.
//
// The message is for the MODEL, not for this process: it travels back as that call's tool result, so the model reads
// what it actually wrote and can close the tag on the next step.  Ending the turn instead (which is what this file
// did once) turns one malformed call into a dead session - the model never learns what was wrong, and a user's only
// recovery is to say "continue".  See DecodeToolArguments for where the original text is kept.
func unparseableArgs(name, argumentsJSON string, cause error) error {
	shown := argumentsJSON
	if len(shown) > 200 {
		shown = shown[:200] + "..."
	}
	return fmt.Errorf("tool call %q has arguments that are not valid JSON (%v); the model's call was probably left "+
		"unterminated (a missing </parameter> folds the rest of the call into the first value). Arguments as received: %s",
		name, cause, shown)
}

// DecodeToolArguments parses a call's argument text, returning the parse error instead of swallowing it.
//
// On failure the caller must keep raw: it is the only copy of what the model wrote, and it is what the model needs to
// see in order to fix its own call (it comes back as the tool result and stays in the history verbatim).  Success
// returns raw == "" so that callers serialise Arguments exactly as they always did - the history then keeps the very
// same bytes the model emitted, which is what keeps a prompt prefix stable.
//
// Exported because LLM provider plugins sit on the other side of a process boundary: the argument text arrives there
// as a string, and that is the last place the original exists (see sdk.DecodeToolArguments).
func DecodeToolArguments(name, argumentsJSON string) (args map[string]interface{}, raw string, err error) {
	if strings.TrimSpace(argumentsJSON) == "" {
		// Nothing at all: a call with no arguments is well formed (a tool taking none), so this is not a failure.
		return nil, "", nil
	}
	if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil {
		return nil, argumentsJSON, unparseableArgs(name, argumentsJSON, err)
	}
	return args, "", nil
}

// toolCallFromProto converts a proto tool call to the local form. It never fails: arguments that do not parse are
// carried as RawArguments, so dispatch still happens and the tool itself decides what to say about them.
func toolCallFromProto(tc *proto.ToolCall) ToolCall {
	args, raw, _ := DecodeToolArguments(tc.GetName(), tc.GetArgumentsJson())
	return ToolCall{ID: tc.GetId(), Name: tc.GetName(), Arguments: args, RawArguments: raw}
}

type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`

	// RawArguments 是模型交付这份调用时的参数**原文**（未经解析的 JSON 文本）。
	//
	// 存在的理由只有一个：让参数解析失败的调用也能原样回到模型眼前。字段名照「原文」
	// 写，不叫 Parsed/Valid，因为唯一用处就是别把它重新序列化一遍。
	//
	// 为什么不能只靠 Arguments：参数不合法时那份 map 是空的，再 json.Marshal 出来是
	// "null"。模型随后在历史里看到自己「调过 exec_command、参数 null」，无从知道它
	// 写错在哪（漏了一个 </parameter>），也就改不了——它只会换个工具或换个说法蒙一次。
	//
	// 只有「解析失败」的调用填这个字段：成功的调用照旧走 Arguments，于是模型在历史里
	// 看到的字节与它当初发出的完全一致（json.Marshal 对 map 键排序，而原文本来也是
	// 同一条 marshal 出来的）。空值表示「原文没有保留」，消费方必须回退到 Arguments，
	// **不能**把它读成"参数为空"。
	RawArguments string `json:"raw_arguments,omitempty"`
}

// ArgumentsJSON 返回这份调用的参数文本，供**保真**传递（proto 往返、历史回放）。
//
// 解析失败时返回原文——这是本方法唯一的存在理由，也是它必须被用在每一条
// 「把调用交给别处」的路径上的原因（见 protoToolCallArguments）。
func (tc ToolCall) ArgumentsJSON() string {
	if tc.RawArguments != "" {
		return tc.RawArguments
	}
	if tc.Arguments == nil {
		// nil map 会被 json.Marshal 写成 "null"，而 provider 端把参数当 JSON 对象解析
		//（OpenAI 的 function.arguments 是对象文本，null 让整项 tool_call 不合法）。
		// 一个不带参数的工具调用应当序列化成 {}。
		return "{}"
	}
	b, err := json.Marshal(tc.Arguments)
	if err != nil || len(b) == 0 {
		return "{}"
	}
	return string(b)
}

// LLMGRPCPlugin 是 go-core 的 LLM 适配器
type LLMGRPCPlugin struct {
	core.Plugin
	Impl LLMProvider
}

func (p *LLMGRPCPlugin) GRPCServer(broker *core.GRPCBroker, s *grpc.Server) error {
	proto.RegisterLLMServiceServer(s, &llmGRPCServer{impl: p.Impl})
	metadata.RegisterPluginMetadataServer(s, &llmMetadataServer{impl: p.Impl})
	return nil
}

// llmMetadataServer 提供 LLM 插件的元數據服務，供宿主按配置加載時做類型/版本校驗
type llmMetadataServer struct {
	metadata.UnimplementedPluginMetadataServer
	impl LLMProvider
}

// CapabilityLLM 是所有 LLM 插件默认提供的能力键。agent 经 sdk.Config.Requires
// 声明 Requires: [{Type: "llm", Capability: CapabilityLLM}] 即可表达「依赖一个 LLM
// 插件」——宿主据此按能力（而非插件名）解析 primary LLM provider。对齐 DSH/Cordis
// 的 provide + inject 模型：所有 LLM 插件 provide "llm" 能力，agent inject "llm"。
const CapabilityLLM = "llm"

func (s *llmMetadataServer) GetInfo(ctx context.Context, _ *metadata.Empty) (*metadata.PluginInfo, error) {
	return &metadata.PluginInfo{
		Type:       "llm",
		Name:       s.impl.Name(ctx),
		Version:    s.impl.Version(ctx),
		ApiVersion: "1.0",
		Capabilities: map[string]string{
			// 所有 LLM 插件默认提供 "llm" 能力，供 agent 经 Requires 声明依赖
			CapabilityLLM:     "true",
			"supports_images": strconv.FormatBool(s.impl.VisionEnabled()),
		},
	}, nil
}

func (p *LLMGRPCPlugin) GRPCClient(ctx context.Context, broker *core.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return &llmGRPCClient{client: proto.NewLLMServiceClient(c)}, nil
}

// --- gRPC 服务端实现 ---
type llmGRPCServer struct {
	proto.UnimplementedLLMServiceServer
	impl LLMProvider
}

// NewLLMServiceServer 构造一个 LLMService gRPC 服务端，供 SDK 等外部包绕过
// LLMGRPCPlugin.GRPCServer 自行注册服务（避免双重注册 metadata server）。
// impl 由插件实现 core.LLMProvider 接口提供。
func NewLLMServiceServer(impl LLMProvider) proto.LLMServiceServer {
	return &llmGRPCServer{impl: impl}
}

// convertLLMChatRequest 将 proto 请求转换为内部消息与工具列表。
//
// 委托给 protoMessagesToPlugin：这两处曾是各写一份的等价转换，结果一处抄了
// ToolCallId、一处没抄（tool 消息缺 tool_call_id 会让 OpenAI/DeepSeek 直接 422）。
// 同一件事只留一份实现（AGENTS.md「重复逻辑必须抽取」），避免再次漂移。
func convertLLMChatRequest(req *proto.ChatRequest) ([]Message, []Tool) {
	return protoMessagesToPlugin(req)
}

// convertToolCallsToProto 将内部工具调用列表转换为 proto 形式。
//
// 走 ArgumentsJSON() 而不是 json.Marshal(tc.Arguments)：解析失败的调用带的是原文（RawArguments），
// 重新序列化会把原文换成 "null"，模型就再也看不到自己写错了什么。
func convertToolCallsToProto(toolCalls []ToolCall) []*proto.ToolCall {
	out := make([]*proto.ToolCall, len(toolCalls))
	for i, tc := range toolCalls {
		out[i] = &proto.ToolCall{Id: tc.ID, Name: tc.Name, ArgumentsJson: tc.ArgumentsJSON()}
	}
	return out
}

// UsageToProto 将内部 Usage 转换为 proto 形式（nil 返回 nil）
func UsageToProto(u *Usage) *proto.Usage {
	if u == nil {
		return nil
	}
	return &proto.Usage{
		PromptTokens:             u.PromptTokens,
		CompletionTokens:         u.CompletionTokens,
		TotalTokens:              u.TotalTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		ContextTokens:            u.ContextTokens,
	}
}

// UsageFromProto 将 proto Usage 转换为内部形式（nil 返回 nil）
func UsageFromProto(u *proto.Usage) *Usage {
	if u == nil {
		return nil
	}
	return &Usage{
		PromptTokens:             u.PromptTokens,
		CompletionTokens:         u.CompletionTokens,
		TotalTokens:              u.TotalTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
		ContextTokens:            u.ContextTokens,
	}
}

func (s *llmGRPCServer) Chat(ctx context.Context, req *proto.ChatRequest) (*proto.ChatResponse, error) {
	messages, tools := convertLLMChatRequest(req)

	resp, err := s.impl.Chat(ctx, messages, tools, int(req.MaxTokens))
	if err != nil {
		return nil, err
	}

	return &proto.ChatResponse{
		Content:      resp.Content,
		FinishReason: resp.FinishReason,
		ToolCalls:    convertToolCallsToProto(resp.ToolCalls),
	}, nil
}

func (s *llmGRPCServer) ChatStream(req *proto.ChatRequest, stream proto.LLMService_ChatStreamServer) error {
	messages, tools := convertLLMChatRequest(req)

	ch, err := s.impl.ChatStream(stream.Context(), messages, tools, int(req.MaxTokens))
	if err != nil {
		return err
	}
	for item := range ch {
		if item.Error != "" {
			return status.Errorf(codes.Unknown, "LLM stream error: %s", item.Error)
		}
		if err := stream.Send(&proto.ChatStreamResponse{
			Content:      item.Content,
			FinishReason: item.FinishReason,
			ToolCalls:    convertToolCallsToProto(item.ToolCalls),
			Usage:        UsageToProto(item.Usage),
			Reasoning:    item.Reasoning,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *llmGRPCServer) Name(ctx context.Context, req *proto.NameRequest) (*proto.NameResponse, error) {
	return &proto.NameResponse{Name: s.impl.Name(ctx)}, nil
}

func (s *llmGRPCServer) Version(ctx context.Context, req *proto.VersionRequest) (*proto.VersionResponse, error) {
	return &proto.VersionResponse{Version: s.impl.Version(ctx)}, nil
}

func (s *llmGRPCServer) HealthCheck(ctx context.Context, req *proto.HealthCheckRequest) (*proto.HealthCheckResponse, error) {
	err := s.impl.HealthCheck(ctx)
	status := "okay"
	msg := ""
	if err != nil {
		status = "error"
		msg = err.Error()
	}
	return &proto.HealthCheckResponse{Status: status, Message: msg}, nil
}

// --- gRPC 客户端代理 ---
type llmGRPCClient struct {
	client proto.LLMServiceClient
}

// convertToProtoMessages 将内部消息列表转换为 proto 形式
func convertToProtoMessages(messages []Message) []*proto.Message {
	protoMessages := make([]*proto.Message, len(messages))
	for i, m := range messages {
		pm := &proto.Message{Role: m.Role, Content: m.Content, ToolCallId: m.ToolCallID, Images: m.Images}
		if len(m.ToolCalls) > 0 {
			pm.ToolCalls = make([]*proto.ToolCall, len(m.ToolCalls))
			for j, tc := range m.ToolCalls {
				pm.ToolCalls[j] = &proto.ToolCall{Id: tc.ID, Name: tc.Name, ArgumentsJson: tc.ArgumentsJSON()}
			}
		}
		protoMessages[i] = pm
	}
	return protoMessages
}

// convertToProtoTools 将内部工具列表转换为 proto 形式
func convertToProtoTools(tools []Tool) []*proto.Tool {
	protoTools := make([]*proto.Tool, len(tools))
	for i, t := range tools {
		protoTools[i] = &proto.Tool{Name: t.Name, Description: t.Description, ParametersJson: t.ParametersJSON}
	}
	return protoTools
}

func (c *llmGRPCClient) Chat(ctx context.Context, messages []Message, tools []Tool, maxTokens int) (*ChatResponse, error) {
	req := &proto.ChatRequest{
		Messages:  convertToProtoMessages(messages),
		Tools:     convertToProtoTools(tools),
		MaxTokens: int32(maxTokens),
	}

	var resp *proto.ChatResponse
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		resp, err = c.client.Chat(ctx, req)
		if err == nil {
			break
		}
		// 如果是參數錯誤（InvalidArgument），直接返回
		if s, ok := status.FromError(err); ok && s.Code() == codes.InvalidArgument {
			return nil, err
		}
		// 其他錯誤重試
		time.Sleep(time.Duration(1<<attempt) * time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("LLM call failed after retries: %w", err)
	}

	// 参数不解析不是「这次调用失败」：照常把调用交给上层，原文留在 RawArguments 里，
	// 由工具把它当作结果回给模型（模型自己改）。这里中止会把一次格式错误变成死会话。
	toolCalls := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		toolCalls[i] = toolCallFromProto(tc)
	}

	return &ChatResponse{
		Content:      resp.Content,
		FinishReason: resp.FinishReason,
		ToolCalls:    toolCalls,
	}, nil
}

func (c *llmGRPCClient) ChatStream(ctx context.Context, messages []Message, tools []Tool, maxTokens int) (<-chan *ChatStreamResponse, error) {
	req := &proto.ChatRequest{
		Messages:  convertToProtoMessages(messages),
		Tools:     convertToProtoTools(tools),
		MaxTokens: int32(maxTokens),
	}
	stream, err := c.client.ChatStream(ctx, req)
	if err != nil {
		return nil, err
	}

	ch := make(chan *ChatStreamResponse)
	go func() {
		defer close(ch)
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				return
			}
			if err != nil {
				ch <- &ChatStreamResponse{Error: err.Error()}
				return
			}
			// 参数不解析**不**中止这一帧（更不中止整轮）：调用照常交给 agent，原文留在
			// RawArguments 里。agent 派发它，工具把「你这段参数不是合法 JSON，这是你写下
			// 的原文」当作结果回给模型，模型在下一步补齐 </parameter> 重试。
			//
			// 早先这里是 `ch <- Error; return`，而 loop 收到帧里的 Error 就结束整轮——
			// 一次漏写的闭合标签于是变成一个死会话：模型永远不知道自己错在哪，用户只能
			// 手动说「继续」。
			toolCalls := make([]ToolCall, len(resp.ToolCalls))
			for i, tc := range resp.ToolCalls {
				toolCalls[i] = toolCallFromProto(tc)
			}
			ch <- &ChatStreamResponse{
				Content:      resp.Content,
				FinishReason: resp.FinishReason,
				ToolCalls:    toolCalls,
				Usage:        UsageFromProto(resp.Usage),
				Reasoning:    resp.Reasoning,
			}
		}
	}()
	return ch, nil
}

func (c *llmGRPCClient) Name(ctx context.Context) string {
	resp, _ := c.client.Name(ctx, &proto.NameRequest{})
	if resp == nil {
		return ""
	}
	return resp.Name
}

func (c *llmGRPCClient) Version(ctx context.Context) string {
	resp, _ := c.client.Version(ctx, &proto.VersionRequest{})
	if resp == nil {
		return ""
	}
	return resp.Version
}

func (c *llmGRPCClient) HealthCheck(ctx context.Context) error {
	resp, err := c.client.HealthCheck(ctx, &proto.HealthCheckRequest{})
	if err != nil {
		return err
	}
	if resp.Status != "okay" {
		return fmt.Errorf("health check failed: %s", resp.Message)
	}
	return nil
}

// VisionEnabled 实现 LLMProvider——宿主侧客户端仅需满足接口（真实视觉能力由
// 服务端 llmMetadataServer.GetInfo 上报 capabilities，TUI 经 GetPluginMetadata 读取），
// 故此处恒返回 true。
func (c *llmGRPCClient) VisionEnabled() bool { return true }

// 编译期守卫：确保 llmGRPCClient 完整实现 LLMProvider 接口。此前漏加方法仅导致
// 运行时 raw.(LLMProvider) 断言失败（编译不报、运行才炸），此断言把问题前移到编译期。
var _ LLMProvider = (*llmGRPCClient)(nil)
