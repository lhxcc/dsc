package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"dsc/coderuntime"
	"dsc/proto"
)

// runCodeToolName PTC 呈现模式的 run_code presentation transport 名称（对齐 DSH：该名保留给
// presentation 层，普通业务工具不能注册或 shadow；native 模式不对模型暴露）。
const runCodeToolName = "run_code"

// runCodeTool 宿主内置 run_code 工具：执行一段 Lua 程序来组合多步工具调用
// （PTC code-runtime 的对外载体）。程序内每个可用工具以「同名 Lua 函数」呈现
// （由工具目录经 coderuntime.GenerateSDK 生成，见 Execute），外加全局 args
// 与原生 tool(name, args)。程序顶层 `return <value>` 即结果；运行失败
// （StopError/Cancelled）以结构化 Result JSON 作为数据返回，不硬失败。
type runCodeTool struct{ m *Manager }

func (t *runCodeTool) Name() string { return runCodeToolName }

func (t *runCodeTool) Description() string {
	return "Write and run a Lua program that composes multiple tool calls into a single step. " +
		"Globals: args (the JSON input object passed as `args`); each available tool is exposed " +
		"as a Lua function callable as <tool_name>{...} (e.g. read_file{path=...}); the raw " +
		"tool(name, args) binding is also available. The program's top-level `return <value>` is " +
		"the result. Use this to batch a sequence of operations that would otherwise need many " +
		"tool calls, or when the user explicitly asks to run code."
}

func (t *runCodeTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"source": {"type": "string", "description": "Lua program source. Globals: args, plus one function per available tool (called as <tool_name>{...}). The top-level 'return <value>' is the result."},
			"args": {"type": "object", "description": "Optional JSON input object exposed to the program as the 'args' global."},
			"timeout_ms": {"type": "integer", "description": "Optional per-run timeout in milliseconds; 0/omitted = unlimited."}
		},
		"required": ["source"]
	}`)
}

func (t *runCodeTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var p struct {
		Source    string `json:"source"`
		Args      any    `json:"args"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", fmt.Errorf("run_code: invalid args: %w; 正确用法: 参数形如 {\"source\": \"local n = grep{pattern=\\\"TODO\\\"}; return n.count\", \"args\": {...}, \"timeout_ms\": 30000}", err)
	}
	if strings.TrimSpace(p.Source) == "" {
		return "", fmt.Errorf("run_code: source is required; 正确用法: {\"source\": \"return args.query\"}，其中 source 是 Lua 程序，顶层 return 即结果")
	}

	// 由工具目录生成 SDK，拼接在用户脚本之前执行。
	// 排除 run_code 自身（避免嵌套/遮蔽歧义）与「返回图像附件」的工具：Lua 绑定只能
	// 拿回字符串，图像会静默丢掉——模型收到一段描述却看不见图，还以为看过了。
	skip := t.imageToolNames()
	skip[runCodeToolName] = true
	sdk := coderuntime.GenerateSDK(toolSpecsFromProto(t.m.AllToolsProto(), skip))
	var timeout time.Duration
	if p.TimeoutMs > 0 {
		timeout = time.Duration(p.TimeoutMs) * time.Millisecond
	}
	bindings := map[string]any{}
	if p.Args != nil {
		bindings["args"] = p.Args
	}

	res := coderuntime.Run(ctx, coderuntime.Options{
		Script: sdk + "\n" + p.Source,
		Tool: func(rctx context.Context, name, argsJSON string) (string, error) {
			return t.m.ExecuteTool(rctx, name, json.RawMessage(argsJSON))
		},
		Bindings: bindings,
		Timeout:  timeout,
	})
	// 失败（StopError/StopCancelled）也作为结构化数据返回，模型可见而非硬中断。
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return "", fmt.Errorf("run_code: render result: %w", err)
	}
	return string(b), nil
}

// ExecuteWithView 与 Execute 语义相同，额外把运行结果声明为结构化视图（RunCode plain 块：
// 徽标 = stop_reason，正文 = 返回值/错误）。视图构造失败不影响执行结果。
func (t *runCodeTool) ExecuteWithView(ctx context.Context, args json.RawMessage) (string, string, error) {
	result, err := t.Execute(ctx, args)
	if err != nil {
		return "", "", err
	}
	view, vErr := runCodeView(ctx, args, result)
	if vErr != nil {
		return result, "", nil
	}
	return result, view, nil
}

// runCodeView 把 coderuntime.Result 的 JSON 渲染为 RunCode plain 视图：
// stop_reason 语义着色（completed 绿 / error 红 / cancelled 黄），正文优先展示程序
// 顶层返回值（错误时展示错误信息），工具调用数/日志数作为元信息行。
func runCodeView(_ context.Context, _ json.RawMessage, result string) (string, error) {
	var r struct {
		Value      any    `json:"value"`
		StopReason string `json:"stop_reason"`
		Error      string `json:"error"`
		ToolCalls  []any  `json:"tool_calls"`
		Logs       []any  `json:"logs"`
	}
	if err := json.Unmarshal([]byte(result), &r); err != nil {
		return "", err
	}
	tone := "green"
	switch r.StopReason {
	case "error":
		tone = "red"
	case "cancelled":
		tone = "yellow"
	}
	body := r.Error
	if body == "" && r.Value != nil {
		if b, err := json.MarshalIndent(r.Value, "", "  "); err == nil {
			body = string(b)
		} else {
			body = fmt.Sprintf("%v", r.Value)
		}
	}
	if body == "" {
		body = "completed (no return value)"
	}
	var meta []string
	if n := len(r.ToolCalls); n > 0 {
		meta = append(meta, fmt.Sprintf("tool calls: %d", n))
	}
	if n := len(r.Logs); n > 0 {
		meta = append(meta, fmt.Sprintf("logs: %d", n))
	}
	if len(meta) > 0 {
		body = strings.Join(meta, " · ") + "\n" + body
	}
	v, err := json.Marshal(ToolView{Kind: "plain", Title: "RunCode", Badge: &ViewBadge{Text: r.StopReason, Tone: tone}, Body: body})
	if err != nil {
		return "", err
	}
	return string(v), nil
}

// imageToolNames 列出「会返回图像附件」的工具名（实现 ViewImageExecutor 的那些）。
//
// 它们不能进 run_code 的 LuA SDK：run_code 的绑定只能把结果作为字符串交回程序，
// 附件无处可放，于是图像被静默丢弃、模型读到一段描述却看不到图。让它在程序里
// 根本不存在，比让它「成功但什么都看不到」诚实。
func (t *runCodeTool) imageToolNames() map[string]bool {
	out := map[string]bool{}
	if t.m == nil {
		return out
	}
	for _, tl := range t.m.GetToolRegistry().VisibleTools() {
		if _, ok := tl.(ViewImageExecutor); ok {
			out[tl.Name()] = true
		}
	}
	return out
}

// toolSpecsFromProto 把宿主工具目录（[]*proto.Tool）转成 SDK 用的 ToolSpec 列表，
// 按工具名排序（稳定、前缀缓存友好）；skip 里的工具排除——既避免同名 Lua 全局下
// 的嵌套/遮蔽歧义（run_code 自身），也用于挡掉图像类工具（见 imageToolNames）。
func toolSpecsFromProto(tools []*proto.Tool, skip map[string]bool) []coderuntime.ToolSpec {
	out := make([]coderuntime.ToolSpec, 0, len(tools))
	for _, t := range tools {
		if skip[t.Name] {
			continue
		}
		out = append(out, coderuntime.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			JSONSchema:  t.ParametersJson,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
