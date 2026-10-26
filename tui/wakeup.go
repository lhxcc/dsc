package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"charm.land/bubbletea/v2"
	"dsc/jobs"
)

// 后台任务完成通知唤醒（对齐 DSH tool-jobs 的 completionDelivery: wakeup）。
// Manager 侧 jobs 落定 → OnJobDone 回调 → tea.Program.Send(jobDoneMsg) 送进
// 事件循环：空闲时自动开启一轮（完成通知作为输入，模型据此 job_output 收集）；
// 忙碌或预算耗尽时排队，下一条用户消息前置注入。用户消息恢复唤醒预算。

// maxConsecutiveWakes 可由唤醒开启的连续轮数上限（对齐 DSH 默认 3），
// 防自激：被唤醒的一轮可能启动新的后台任务，其完成又唤醒同一 owner。
const maxConsecutiveWakes = 3

// jobDoneMsg 后台任务完成事件（Manager 侧 jobs 落定回调 → 事件循环）。
type jobDoneMsg struct {
	snapshot jobs.JobSnapshot
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

// splitJobDoneNotices 从用户消息内容前端剥离后台任务完成通知，返回通知列表与
// 剩余的用户正文。唤醒机制会把通知前置拼进用户消息（见 startTurn 的 sendText），
// 实时视图只把通知渲染成系统提示、用户气泡只放用户正文；重放时按同一形态拆开，
// 避免「通知与用户消息粘连成一块」。旧会话（双前缀、含换行 label）同样能匹配。
func splitJobDoneNotices(content string) (notices []string, rest string) {
	rest = content
	for {
		m := jobDoneNoticeRe.FindString(rest)
		if m == "" {
			break
		}
		notices = append(notices, m)
		rest = strings.TrimPrefix(rest[len(m):], "\n\n")
	}
	return notices, rest
}

// noticeBubble 渲染完成通知行（系统提示样式，不冒充用户消息）。
func noticeBubble(text string) string {
	return dimSty.Render(text)
}

// startTurn 启动一轮 agent 运行（Enter 用户输入与完成通知唤醒共用）。
// images 为本轮附带的图像 data URL（唤醒场景为 nil）；warning 非空时在用户消息
// 之后追加一条系统提示（如「图片未随消息发送」），唤醒场景为空。
func (m *Model) startTurn(text string, images []string, rendered string, warning string) tea.Cmd {
	m.appendMessage(rendered)
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
	if m.wakeBudget <= 0 {
		return nil
	}
	m.wakeBudget--
	text := strings.Join(m.pendingWakeups, "\n\n")
	m.pendingWakeups = nil
	return m.startTurn(text, nil, noticeBubble(text), "")
}
