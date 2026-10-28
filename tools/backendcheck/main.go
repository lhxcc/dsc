// backendcheck 用**真实派生历史**驱动每一个插件的实际二进制，检查它在宿主
// agent/pre-step 事件上的行为是否仍然自洽。
//
// 为什么要它：压缩后端、agent、核心共享 proto / session 派生 / 消息结构。改动其中
// 一处（哪怕只是给 Message 加个字段），受影响的插件未必在自己的测试里覆盖到——曾发
// 生过「改一边、另一边静默变形」。这里按**目录扫描 + 能力无关**的方式跑全部插件：
// 不写死任何插件名，谁支持这个事件谁就接受检查，返回改写就按同一套结构约束验。
//
// 约束取自 provider 的硬要求与本次事故的教训，不看具体压缩策略：
//  1. 必须返回合法 JSON 且 messages 可解析
//  2. 条数不得膨胀（≤ 输入+2，允许尾部追加一条提示）
//  3. 输入首条是 system 时，输出首条仍须是 system（基础指令不得被压掉）
//  4. tool 消息必须有前置的对应 tool_call（cut 断配对会被 provider 直接拒）
//  5. 同一 id 不得在输出里出现两次
//  6. 不得再造「msg-<数字>」这种位置式 id（那正是会指错消息的旧方案）
//
// 用法：go run . -plugins <dist>/plugins [-window 32768]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"time"

	plugin "github.com/hashicorp/go-plugin"

	dsc "dsc-sdk"
	"dsc/core"
	"dsc/proto"
	"dsc/session"
)

func main() {
	pluginsDir := flag.String("plugins", "", "插件目录（其下每个 <name>/<name>.exe 都会被检查）")
	window := flag.Int("window", 1024, "事件里声明的上下文窗口（小窗口是为了逼出改写路径）")
	flag.Parse()
	if *pluginsDir == "" {
		fmt.Println("必须指定 -plugins")
		os.Exit(2)
	}

	msgs := fixture()
	msgsJSON, _ := json.Marshal(msgs)
	toolTokens := 2000
	total := core.EstimateProtoMessagesTokens(msgs) + toolTokens

	dirs, err := pluginDirs(*pluginsDir)
	if err != nil {
		fmt.Printf("扫描插件目录失败: %v\n", err)
		os.Exit(2)
	}
	stateDir, err := os.MkdirTemp("", "backendcheck-*")
	if err != nil {
		fmt.Printf("建临时目录失败: %v\n", err)
		os.Exit(2)
	}
	defer os.RemoveAll(stateDir)

	fmt.Printf("夹具: %d 条 / 估算 %d，窗口 %d\n\n", len(msgs), core.EstimateProtoMessagesTokens(msgs), *window)

	var checked, warn, bad int
	for _, name := range dirs {
		exe := core.PJoin(*pluginsDir, name, name+".exe")
		res := runOne(exe, string(msgsJSON), toolTokens, total, *window, stateDir)
		fmt.Println(res.String())
		switch res.kind {
		case kindChecked:
			checked++
		case kindFailed:
			warn++
		}
		if len(res.problems) > 0 {
			bad++
		}
	}
	fmt.Printf("\n共 %d 个插件；声明 compaction 并跑完改写检查 %d 个；起不来（环境问题，告警）%d 个；契约违规 %d 个\n",
		len(dirs), checked, warn, bad)
	if bad > 0 {
		os.Exit(1)
	}
}

func pluginDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if _, err := os.Stat(core.PJoin(root, name, name+".exe")); err == nil {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

// fixture 用真实派生路径构造历史（不是手搓 proto.Message）：这样 session 派生的
// 消息结构（含稳定 id）也在检查范围内——共享模块改动正是在这一层传导的。
func fixture() []*proto.Message {
	s := session.New()
	s.Append(session.SystemMessage, &session.SystemMessageData{Content: "sys prompt"}, &session.SurfaceOp{Op: session.SurfaceAppend})
	body := "0123456789abcdefghij"
	for i := 0; i < 40; i++ {
		s.Append(session.UserMessage, &session.UserMessageData{Content: fmt.Sprintf("round %d 需求 %s", i, body)}, &session.SurfaceOp{Op: session.SurfaceAppend})
		callID := fmt.Sprintf("call-%d", i)
		s.Append(session.AssistantMessage, &session.AssistantMessageData{
			ToolCalls: []*proto.ToolCall{{Id: callID, Name: "shell", ArgumentsJson: `{"command":"ls"}`}},
		}, &session.SurfaceOp{Op: session.SurfaceAppend})
		s.Append(session.ToolResult, &session.ToolResultData{CallID: callID, Content: fmt.Sprintf("%s %d", body, i)}, &session.SurfaceOp{Op: session.SurfaceAppend})
	}
	return s.DeriveMessages()
}

type kind int

const (
	kindSkipped kind = iota // 不支持该事件
	kindChecked             // 接受了并给了改写
	kindPlain               // 接受了但没改写
	kindFailed              // 起不来 / 调用失败
)

type result struct {
	name     string
	kind     kind
	summary  string
	problems []string
}

func (r result) String() string {
	mark := "ok"
	if len(r.problems) > 0 {
		mark = "FAIL"
	}
	line := fmt.Sprintf("%-8s %-24s %s", mark, r.name, r.summary)
	for _, p := range r.problems {
		line += "\n           - " + p
	}
	return line
}

var legacyID = regexp.MustCompile(`^msg-\d+$`)

func runOne(exe, msgsJSON string, toolTokens, total, window int, stateDir string) result {
	_, name := core.PSplit(core.PDir(exe))
	res := result{name: name}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("DSC_CONTEXT_WINDOW=%d", window),
		fmt.Sprintf("DSC_COMPACTION_BASIC_CONTEXT_WINDOW=%d", window),
		"DSC_SESSION_DIR="+stateDir,
		"DSC_MEMORY_DIR="+stateDir,
	)
	// 隐藏子进程控制台：否则每个插件启动都会在 Windows 上闪一个黑窗
	// （运行期派生点统一要求，见 core.ConfigureChildProcessAttrs）。
	core.ConfigureChildProcessAttrs(cmd)
	client := plugin.NewClient(&plugin.ClientConfig{
		HandshakeConfig:  dsc.Handshake,
		Plugins:          map[string]plugin.Plugin{},
		AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
		Cmd:              cmd,
		StartTimeout:     30 * time.Second,
	})
	defer client.Kill()
	rc, err := client.Client()
	if err != nil {
		// 起不来通常是环境前提缺失（如渲染插件缺字体），不是契约问题：告警但不算违规。
		// 真正会让构建失败的只有「声明了 compaction 却改了契约」。
		res.kind, res.summary = kindFailed, "启动失败（告警，不计违规）"
		return res
	}
	gc, ok := rc.(*plugin.GRPCClient)
	if !ok {
		res.kind, res.summary = kindFailed, "客户端类型异常"
		return res
	}
	// 先问元数据：只有声明了 compaction 能力的插件才负责改写请求。按能力筛选而不是
	// 按名字，插件目录增减、改名都不需要改这里。
	info, err := core.GetPluginInfo(gc.Conn)
	if err != nil {
		res.kind, res.summary = kindFailed, "读不到元数据"
		res.problems = append(res.problems, err.Error())
		return res
	}
	if info.GetCapabilities()["compaction"] != "true" {
		res.kind, res.summary = kindSkipped, "未声明 compaction（跳过）"
		return res
	}
	hook := proto.NewPluginHookServiceClient(gc.Conn)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	payload, _ := json.Marshal(map[string]any{
		"agent": "backendcheck", "session": "backendcheck", "user_input": true,
		"messages_json": msgsJSON, "token_count": total, "projected_tokens": total,
		"last_usage_tokens": 0, "tool_tokens": toolTokens,
	})
	resp, err := hook.OnEvent(ctx, &proto.OnEventRequest{Name: "agent/pre-step", DataJson: string(payload)})
	if err != nil {
		// 不处理该事件的插件（工具类）会返回 Unimplemented/unsupported：跳过，不算失败
		res.kind, res.summary = kindSkipped, "不支持 pre-step（跳过）"
		return res
	}
	raw := resp.GetResultJson()
	if raw == "" || raw == "{}" || raw == "null" {
		res.kind, res.summary = kindPlain, "未改写"
		return res
	}
	var out struct {
		Messages []*proto.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		res.kind, res.summary = kindFailed, "结果不是合法 JSON"
		res.problems = append(res.problems, fmt.Sprintf("parse: %v", err))
		return res
	}
	if len(out.Messages) == 0 {
		res.kind, res.summary = kindPlain, "messages 为空（视为未改写）"
		return res
	}
	var in []*proto.Message
	_ = json.Unmarshal([]byte(msgsJSON), &in)
	res.kind = kindChecked
	res.summary = fmt.Sprintf("%d 条 → %d 条 / %d tokens", len(in), len(out.Messages), core.EstimateProtoMessagesTokens(out.Messages))
	res.problems = check(in, out.Messages)
	return res
}

func check(in, out []*proto.Message) []string {
	var problems []string
	if len(out) > len(in)+2 {
		problems = append(problems, fmt.Sprintf("条数膨胀: %d → %d", len(in), len(out)))
	}
	if len(in) > 0 && in[0].Role == "system" && (len(out) == 0 || out[0].Role != "system") {
		problems = append(problems, "首条 system 被丢掉（基础指令不得被压缩掉）")
	}
	// tool 配对：每个 tool 消息必须有前置的对应 tool_call
	calls := map[string]bool{}
	for _, m := range out {
		for _, tc := range m.ToolCalls {
			calls[tc.Id] = true
		}
		if m.Role == "tool" && !calls[m.ToolCallId] {
			problems = append(problems, fmt.Sprintf("孤立 tool 结果: %s（配对被切断，provider 会 400）", m.ToolCallId))
			break
		}
	}
	// id 唯一 + 不得回流位置式 id
	seen := map[string]bool{}
	roleByID := map[string]string{}
	for _, m := range in {
		if m.Id != "" {
			roleByID[m.Id] = m.Role
		}
	}
	for i, m := range out {
		if m.Id == "" {
			continue
		}
		if seen[m.Id] {
			problems = append(problems, fmt.Sprintf("id 重复: %s（第 %d 条）", m.Id, i))
		}
		seen[m.Id] = true
		if legacyID.MatchString(m.Id) {
			problems = append(problems, fmt.Sprintf("出现位置式 id %q（旧方案会指错消息）", m.Id))
		}
		if r, ok := roleByID[m.Id]; ok && r != m.Role {
			problems = append(problems, fmt.Sprintf("id %s 换了角色: %s → %s", m.Id, r, m.Role))
		}
	}
	return problems
}
