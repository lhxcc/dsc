package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"dsc/proto"
)

// CapabilityTool 可选接口：宿主内部工具可报告自身声明的能力标签，供宿主按
// 「能力」而非「插件名」做前置门控（对齐 ApprovalRequester 可选接口风格）。
type CapabilityTool interface {
	// HasCapability 返回该工具是否声明了指定能力标签。
	HasCapability(cap string) bool
}

// RemoteTool 實現了 ToolDefinition，通過 gRPC 調用遠程工具
type RemoteTool struct {
	name         string
	description  string
	schema       json.RawMessage
	client       proto.ToolServiceClient
	capabilities []string

	// hidden 由插件声明：注册了但不进模型可见目录（见 HiddenTool）。
	hidden bool
	// scopes 由插件声明：本工具可能触碰的权限范围（见 core/authorization.go）。
	// 非空即表示需要审批——审批是能力的权限属性，不是一段文案；具体到这一次的说明
	// 由 ApprovalReason RPC 给（同一工具不同参数，风险不同）。
	scopes []string
}

// HasCapability 报告该工具是否声明了指定能力标签（源自插件工具声明的 proto.Tool.capabilities。
// 空集合对任意能力返回 false，向后兼容无能力声明的旧工具）。
func (r *RemoteTool) HasCapability(cap string) bool {
	for _, c := range r.capabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// Capabilities 返回插件声明的完整能力标签列表（proto.Tool.capabilities 原样透传）。
// 带参数的能力 token（如 writes-path:<argName>）需经本方法枚举才能解析。
func (r *RemoteTool) Capabilities() []string {
	return r.capabilities
}

// remoteToolFromProto 把插件的工具声明转成宿主工具。
//
// **唯一的构造点**：以前 ListTools 的两条路径（启动期 listStagedTools 与运行期
// syncPluginTools）各自手写一遍字段赋值，结果一加 hidden / scopes 就只改了其中
// 一处——插件声明了，宿主当没听见。跨层字段保真只能靠一处赋值来保证。
func remoteToolFromProto(client proto.ToolServiceClient, t *proto.Tool) *RemoteTool {
	return &RemoteTool{
		name:         t.GetName(),
		description:  t.GetDescription(),
		schema:       json.RawMessage(t.GetParametersJson()),
		client:       client,
		capabilities: t.GetCapabilities(),
		hidden:       t.GetHidden(),
		scopes:       t.GetScopes(),
	}
}

// IsHidden 实现 HiddenTool：插件声明不曝光的工具不进模型目录。
//
// 隐藏不等于不可用——它仍在注册表里，后台任务/子代理/按名调用照常；只是不随请求
// 发给模型。判定每分钟都可能变（插件下次 ListTools 说了算，宿主 ~800ms 同步一次），
// 所以这里每次都读当前值，不做缓存。
func (r *RemoteTool) IsHidden() bool { return r.hidden }

// Scopes 实现 ScopeDeclarer：插件声明的权限范围。
func (r *RemoteTool) Scopes() []string { return r.scopes }

// approvalReason 返回本次调用需要审批的理由（空串 = 不需要）。
//
// 要不要审批看 scopes（静态声明），具体说明问插件（ApprovalReason RPC，按本次参数）。
// 插件未实现该 RPC 时退回一句按范围拼出来的说明——**不能因为问不到就放行**：审批门是
// 安全边界，说不清理由时仍要问，只有「没声明任何范围」才算不需要审批。
func (r *RemoteTool) approvalReason(ctx context.Context, argumentsJSON string) string {
	if len(r.scopes) == 0 {
		return ""
	}
	fallback := "touches " + strings.Join(r.scopes, ", ")
	if r.client == nil {
		return fallback
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := r.client.ApprovalReason(callCtx, &proto.ToolApprovalRequest{
		ToolName:      r.name,
		ArgumentsJson: argumentsJSON,
	})
	if err != nil || resp.GetReason() == "" {
		return fallback
	}
	return resp.GetReason()
}

func (r *RemoteTool) Name() string {
	return r.name
}

func (r *RemoteTool) Description() string {
	return r.description
}

func (r *RemoteTool) ParametersSchema() json.RawMessage {
	return r.schema
}

func (r *RemoteTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	result, _, err := r.ExecuteWithView(ctx, args)
	return result, err
}

// ExecuteWithView 与 Execute 语义相同，额外返回插件的 ViewJson（Tool.ViewFn 产物）。
// 宿主聚合路径据此透传插件视图，避免视图在聚合层被丢弃。
//
// 通用 panic recover：插件 gRPC 客户端调用若 panic（如 nil client / 序列化失败），
// 转为错误返回而非崩溃宿主。插件 SDK 层（sdk/tool.go）已先行 recover 一次（Handler
// panic），本层兜底覆盖 gRPC 传输异常与未用 SDK 的插件。对齐「工具意外不中断会话」
// 的最终防线设计（与 core.executeToolBody 的 panic recover 互补）。
// ExecuteWithViewAndImages 实现 ViewImageExecutor：单次 gRPC 往返同时带回插件
// 结果、视图与图像附件（ExecuteToolResponse.images，如 computer-use 截图）。
func (r *RemoteTool) ExecuteWithViewAndImages(ctx context.Context, args json.RawMessage) (result string, viewJSON string, images []string, errRet error) {
	defer func() {
		if rv := recover(); rv != nil {
			errRet = fmt.Errorf("tool %s panicked (remote): %v", r.name, rv)
		}
	}()
	resp, err := r.client.ExecuteTool(ctx, &proto.ExecuteToolRequest{
		ToolName:      r.name,
		ArgumentsJson: string(args),
		// 调用方身份随调用下发：插件据此做「按会话隔离」的状态（如持久 shell 的
		// cwd）。此前这两个字段在这一跳被丢弃，插件侧永远拿不到会话 id，只能退化
		// 成全局单例——多个对话会共用同一个 shell。
		SessionId:  CallerFromContext(ctx),
		ToolCallId: ToolCallIDFromContext(ctx),
	})
	if err != nil {
		return "", "", nil, err
	}
	if resp.Error != "" {
		return "", "", nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Content, resp.ViewJson, resp.Images, nil
}

// ExecuteWithView 实现 ViewExecutor（委托合一接口，丢弃图像），保持既有调用方兼容。
func (r *RemoteTool) ExecuteWithView(ctx context.Context, args json.RawMessage) (result string, viewJSON string, errRet error) {
	result, viewJSON, _, err := r.ExecuteWithViewAndImages(ctx, args)
	return result, viewJSON, err
}

// LastActivity 查询本次调用最近一次活动时间（插件侧 GetActivity）。
//
// 用于「活跃续命」：宿主的前台等待预算按「距上次活动」计算，有输出就继续等，
// 持续无输出才把任务转入后台。旧插件没实现该方法（Unimplemented）时返回 false，
// 调用方退回固定预算——两侧互不因缺对方而失败。
func (r *RemoteTool) LastActivity(ctx context.Context, callID string) (time.Time, bool) {
	if r.client == nil || callID == "" {
		return time.Time{}, false
	}
	resp, err := r.client.GetActivity(ctx, &proto.GetActivityRequest{ToolCallId: callID})
	if err != nil || resp.GetLastActivityUnixMs() <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(resp.GetLastActivityUnixMs()), true
}
