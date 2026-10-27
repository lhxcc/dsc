package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbletea/v2"
	"dsc/cron"
	"dsc/jobs"
)

// 完成通知唤醒（对齐 DSH tool-jobs 的 completionDelivery: wakeup）。
// 两个来源共用同一条送达路径：
//   - 后台 job 落定（JobDoneEvent）→ jobDoneMsg：用户的显式请求，必然送达；
//   - 定时任务落定（CronDoneEvent）→ cronDoneMsg：**是否送达由任务自己声明**
//     （Job.Notify），因为 cron 是长期循环的程序性工作，重要性各不相同。
//
// 送达语义：空闲时立即开一轮；忙碌（thinking/streaming）时注入当前回合，下一次
// 迭代即可见；注入失败才回落队列，回合结束时补送（不静默丢弃）。
// 通知只喂模型，不进对话区（实时与回放都不渲染）。

// 说明：曾经有「连续唤醒预算」（最多 3 轮自动唤醒）。它唯一的作用是拦住
// 任务完成唤醒，而这些唤醒都由真实任务落定触发——没有任务完成就不会有唤醒，
// 预算只会制造「任务早就完成了、模型却要等下一次用户输入才知道」的延迟。
// 现在忙碌时立即注入、空闲时立即唤醒，故取消该预算。

// jobDoneMsg 后台任务完成事件（Manager 侧 jobs 落定回调 → 事件循环）。
type jobDoneMsg struct {
	snapshot jobs.JobSnapshot
}

// cronDoneMsg 定时任务运行落定事件（CronDoneEvent → 事件循环）。
type cronDoneMsg struct {
	job cron.Job
}

// renderCronDoneNotice 渲染定时任务落定通知（模型据此用 cron_list 取输出）。
func renderCronDoneNotice(j cron.Job) string {
	return fmt.Sprintf("cron job %s (%s) finished [status: %s]. Read its output with cron_list.",
		j.ID, j.Name, j.LastStatus)
}

// renderJobDoneNotice 渲染完成通知文本（对齐 DSH：
// background job <id> (<kind>: <label>) finished [status: ...]）。
//
// Label 本身已带「<kind>: <command>」前缀（core 侧 startBackgroundTool 生成），
// 故不再重复拼 kind——否则会出现「shell: shell: ...」的双前缀。
func renderJobDoneNotice(s jobs.JobSnapshot) string {
	label := s.Label
	switch {
	case label == "":
		label = s.Kind
	case s.Kind != "" && !strings.HasPrefix(label, s.Kind+": "):
		label = s.Kind + ": " + label
	}
	return fmt.Sprintf("background job %s (%s) finished [status: %s]. Read its output with job_output.",
		s.ID, label, s.Status)
}

// jobDoneNoticeRe 匹配一条 renderJobDoneNotice 生成的完成通知。通知里的 label 是
// 命令前缀（core 侧截断 60 字、未清洗换行），可能跨行，故用 (?s) 让 . 跨行、非贪婪
// 到固定结尾后缀；锚定 ^ 只从文本开头剥离，避免误伤正文中间的同形文字。
var jobDoneNoticeRe = regexp.MustCompile(`(?s)^background job \S+ \(.*?\) finished \[status: [^\]]*\]\. Read its output with job_output\.`)

// cronDoneNoticeRe 匹配一条 renderCronDoneNotice 生成的定时任务通知。
var cronDoneNoticeRe = regexp.MustCompile(`(?s)^cron job \S+ \(.*?\) finished \[status: [^\]]*\]\. Read its output with cron_list\.`)

// completionNoticeRes 所有「只给模型看」的通知形态。新增一类通知必须登记到这里，
// 否则它会在回放/用户气泡里冒出来（这类文本是发给模型的唤醒信号，不是用户的话）。

// splitCompletionNotices 从用户消息内容前端剥离完成通知（后台 job 与定时任务两类），
// 返回通知列表与剩余的用户正文。唤醒机制会把通知前置拼进用户消息（见 startTurn 的
// sendText），用户气泡只放用户正文；重放时按同一形态拆开，避免「通知与用户消息粘连
// 成一块」。「旧会话（双前缀、含换行 label）同样能匹配。」
//
// 通知不渲染是有意的：它是给模型的唤醒信号，不是用户说的话。
func splitCompletionNotices(content string) (notices []string, rest string) {
	rest = content
	for {
		matched := ""
		for _, re := range []*regexp.Regexp{jobDoneNoticeRe, cronDoneNoticeRe} {
			if m := re.FindString(rest); m != "" {
				matched = m
				break
			}
		}
		if matched == "" {
			break
		}
		notices = append(notices, matched)
		rest = strings.TrimPrefix(rest[len(matched):], "\n\n")
	}
	return notices, rest
}

// startTurn 启动一轮 agent 运行（Enter 用户输入与完成通知唤醒共用）。
// images 为本轮附带的图像 data URL（唤醒场景为 nil）；warning 非空时在用户消息
// 之后追加一条系统提示（如「图片未随消息发送」），唤醒场景为空。
func (m *Model) startTurn(text string, images []string, rendered string, warning string) tea.Cmd {
	// rendered 为空 = 本轮没有要显示的东西（如后台任务完成唤醒：通知只给模型看）。
	if strings.TrimSpace(rendered) != "" {
		m.appendMessage(rendered)
	}
	if warning != "" {
		m.appendMessage(warnSty.Render(warning))
	}
	m.thinking = true
	// 工作行动画每轮重新随机一种（中/英文案），帧号归零重新起播
	m.thinkAnim.reset()
	// 新一轮运行指标重置：耗时计时起点、下行数据、缓存命中
	m.runStart = time.Now()
	m.elapsed = 0
	m.turnTokens = 0
	m.cacheHit = 0
	m.cacheMiss = 0
	// 速率测量状态同步重置：新轮的 Turn/Step 编号从 1 重新递增，必须清掉上轮的
	// 步追踪（lastSeenTurn/lastSeenStep）与打点，否则首帧会被误判为「编号未变化」
	// 而丢失本步请求开始时刻；速率读数清零后由本轮首次结算重新填充。
	m.lastSeenTurn = 0
	m.lastSeenStep = 0
	m.stepStart = time.Time{}
	m.firstTokenAt = time.Time{}
	m.stepSettled = false
	m.decodeTPS = 0
	m.startTPS = 0
	m.syncInputHeight()
	// 先点火再渲染：工作行是否出现取决于时钟在跑（见 workingLineVisible），
	// 晚一步就会先画一帧没有工作行的界面。
	anim := m.armAnim()
	m.render()
	m.pinnedToBottom = true // 新轮开始：重新钉在底部跟随新内容（对齐 REX 发送消息即重新钉住）
	m.virtualGotoBottom()
	return tea.Batch(m.submitCmd(text, images), anim)
}

// 工作行时钟（armAnim / disarmAnim / animTick / workingLineVisible）见 tui.go。
// 它保证「工作行可见」与「驱动它的时钟在跑」严格同步：spinner 字形与文字动画
// 都由同一条自持链推进，本轮结束时 disarmAnim 停表，工作行整行消失——不会留下
// 「动画停在扫到一半的遮罩帧」这种半成品画面。

// tryWakeup 空闲且预算未耗尽时，把排队通知作为新轮输入自动发送
// （对齐 DSH：完成通知唤醒空闲 owner；忙碌/预算耗尽时通知排队等下一轮）。
// 返回 nil 表示本轮不自动唤醒。
func (m *Model) tryWakeup() tea.Cmd {
	if m.thinking || m.streaming {
		return nil
	}
	if len(m.pendingWakeups) == 0 {
		return nil
	}
	text := strings.Join(m.pendingWakeups, "\n\n")
	m.pendingWakeups = nil
	// 通知**不进对话区**：它是给模型的唤醒信号，不是用户消息。以前这里渲染
	// noticeBubble(text)，用户看到一屏「background job ... finished」。
	// 模型侧内容照旧（text 作为本轮输入），只是不打印。
	return m.startTurn(text, nil, "", "")
}

// jobNoticeInjectedMsg 完成通知的注入结果（仅失败时需要处理）。
type jobNoticeInjectedMsg struct {
	text string
	err  error
}

// injectJobNoticeCmd 把完成通知实时注入当前回合（跨进程 RPC）。
// 与用户输入共用 InjectMessage 通道：注入的内容在下一次 LLM 迭代可见，
// 因此任务一落定模型就能知道，而不必等这一轮结束。
// 通知不进对话区，故不渲染气泡。
func (m *Model) injectJobNoticeCmd(text string) tea.Cmd {
	return func() tea.Msg {
		err := m.agent.InjectMessage(m.ctx, text, nil)
		return jobNoticeInjectedMsg{text: text, err: err}
	}
}

// deliverCompletionNotice 把一条完成通知立即送达模型，返回需要执行的命令。
//   - 忙碌（thinking/streaming）：注入当前回合，下一轮 LLM 迭代即可见；
//   - 空闲：直接唤醒开一轮。
//
// 不再「先排队、等回合结束/下一条用户输入再一次性补发」——那会让早已完成的任务在
// 很久以后才爆发成一片通知。通知不进对话区（不渲染气泡）。
func (m *Model) deliverCompletionNotice(text string) tea.Cmd {
	m.pendingWakeups = append(m.pendingWakeups, text)
	m.render()
	if m.thinking || m.streaming {
		batch := strings.Join(m.pendingWakeups, "\n\n")
		m.pendingWakeups = nil
		return m.injectJobNoticeCmd(batch)
	}
	return m.tryWakeup()
}
