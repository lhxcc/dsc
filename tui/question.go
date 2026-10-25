package tui

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"dsc/userquestions"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// questionBoxSty 问题覆盖层边框样式。
var questionBoxSty = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(accent).
	Padding(0, 1)

// accentSty 高亮（当前选中项）。
var accentSty = lipgloss.NewStyle().Foreground(accent)

// customOptionLabel 是问题覆盖层末尾追加的"自定义回答"兜底选项。
// 模型给出的固定选项之后，始终追加这一项作为逃生通道——选中后覆盖层消失，
// 焦点自动落到底部主输入框（复用 IME/多行/动态高度等既有能力），用户输入
// 文本回车即作为该问题的回答经 AnswerItem.Custom 字段返回模型。
const customOptionLabel = "✎ 其他（手动输入）"

// 用户评审通道的 TUI 端：注册为 Manager 的 UserQuestionProvider。
// askProvider 在 Manager（gRPC handler）goroutine 中阻塞等待，把问题经
// bubbletea 事件循环渲染给用户，用户选择后把回答回传。

// questionMsg 把评审请求送进 bubbletea 事件循环；request 为 nil 表示清除当前问题。
type questionMsg struct {
	request *userquestions.Request
	answer  chan *userquestions.Answer
	err     chan error
}

// pendingQuestion 当前待回答的问题队列（ask_user_question 可一次携带多个问题，
// TUI 逐个呈现；单问题即队列长度为 1，plan-review 评审同样走此通道）。
type pendingQuestion struct {
	request *userquestions.Request
	answer  chan *userquestions.Answer
	err     chan error
	current int                        // 当前问题索引（队列推进）
	answers []userquestions.AnswerItem // 已收集的回答
	cursor  int                        // 当前问题选中项索引（含兜底项在内）
	multi   map[int]bool               // 多选：已勾选的选项索引

	// customMode 自定义文字输入：无论模型给出何种选项，都保留自由输入逃生通道。
	// 进入后复用底部主输入框（composer）接收文本，Enter 提交并转交 ask_user_question，
	// 输入内容经 AnswerItem.Custom 返回模型。
	//
	// 选中末尾的"✎ 其他（手动输入）"项触发此模式：覆盖层消失，焦点落到主输入框，
	// 用户输入文本回车即作为该问题回答。Esc 返回选项列表（仅 customMode 由选项触发时
	// 才可返回；c 键直接进入的同此行为）。
	customMode bool

	// scroll 覆盖层正文的滚动偏移（行）。只在内容超过终端可用高度时才非零：
	// 模型把问题/选项写得很长时，框体会先自动长高（auto-grow），长到顶到屏幕
	// 上限才开窗滚动——用户仍能读到全部内容，而不是被裁掉开头。
	scroll int
}

// askProvider 宿主注册的 UI provider：把问题发给 TUI 并阻塞等待回答。
func (m *Model) askProvider(ctx context.Context, req *userquestions.Request) (*userquestions.Answer, error) {
	answer := make(chan *userquestions.Answer, 1)
	errc := make(chan error, 1)
	if m.program == nil {
		return nil, &userquestions.Error{Code: userquestions.ErrNoProvider, Err: fmt.Errorf("tui program not running")}
	}
	m.program.Send(questionMsg{request: req, answer: answer, err: errc})
	defer func() { m.program.Send(questionMsg{}) }() // 无论结果如何，清除 TUI 覆盖层
	select {
	case ans := <-answer:
		return ans, nil
	case e := <-errc:
		return nil, e
	case <-ctx.Done():
		return nil, &userquestions.Error{Code: userquestions.ErrAskAborted, Err: ctx.Err()}
	}
}

// isEnterKey 判断是否 Enter：优先按物理键码判断（不同终端的 String() 文本可能
// 是 "enter" / "\r" 等），避免选择项确认在所有终端上都可靠触发。
func isEnterKey(msg tea.KeyPressMsg) bool {
	return msg.Code == tea.KeyEnter || msg.String() == "enter" || msg.String() == "\r"
}

// printableInput 返回本次按键可插入的可见文本（含 IME 提交的多字符文本）；
// 控制键、纯空白返回空串（不触发「输入即自定义回答」）。
func printableInput(msg tea.KeyPressMsg) string {
	text := msg.Text
	if text == "" && msg.Code >= 32 {
		text = string(msg.Code)
	}
	if strings.TrimSpace(text) == "" {
		return ""
	}
	for _, r := range text {
		if unicode.IsControl(r) {
			return ""
		}
	}
	return text
}

// handleQuestionKey 问题覆盖层激活时的按键处理：方向选择、Space 勾选（多选）、
// Enter 确认（队列中回答完当前问题后推进下一个）、Esc 放弃；
// 选中末尾"✎ 其他（手动输入）"项触发自定义文字输入（覆盖层消失、焦点落到主输入框）。
// c/o 同样可进入自定义输入（键盘快捷通道）；直接键入其它可见字符也会立即进入
// 自定义输入并保留该字符（不必先按 Enter 选中兜底项）。
//
// 任何改变 m.question 或 q.customMode 的分支都必须经 syncViewportHeight 同步
// viewport 实际高度——否则 vpHeight() 计算正确但 m.viewport.Height() 仍为旧值，
// viewport.View() 输出多余行数把覆盖层推到终端可视区外（AltScreen 截断），
// 表现为模型提问后部分选项被切掉。
func (m *Model) handleQuestionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	q := m.question
	if q == nil || len(q.request.Questions) == 0 {
		return m, nil
	}
	// 自定义文字输入模式：输入全部用于编辑自定义回答
	if q.customMode {
		return m.handleCustomInputKey(msg)
	}
	opts := m.questionOptions(q)
	multi := q.request.Questions[q.current].MultiSelect
	if isEnterKey(msg) {
		if q.answer == nil {
			m.question = nil
			m.syncViewportHeight()
			m.render()
			return m, nil
		}
		// 选中末尾的"✎ 其他（手动输入）"项 → 进入自定义输入模式（覆盖层消失）
		if m.isCustomOption(q, q.cursor) {
			return m, m.enterCustomInputMode(q)
		}
		q.answers = append(q.answers, m.buildAnswerItem(q, multi))
		return m, m.advanceOrSubmit(q)
	}
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit
	case "esc", "ctrl+c":
		m.question = nil
		if q.err != nil {
			q.err <- &userquestions.Error{Code: userquestions.ErrCanceled, Err: fmt.Errorf("user dismissed the question")}
		}
		m.syncViewportHeight()
		m.render()
		return m, nil
	case "up", "k", "ctrl+p":
		if len(opts) > 0 {
			q.cursor = (q.cursor - 1 + len(opts)) % len(opts)
		}
		m.render()
		return m, nil
	case "down", "j", "ctrl+n", "tab":
		if len(opts) > 0 {
			q.cursor = (q.cursor + 1) % len(opts)
		}
		m.render()
		return m, nil
	case "left", "h":
		// 横向方向键同样映射为向上选择（部分用户习惯用左右键翻选项）。
		if len(opts) > 0 {
			q.cursor = (q.cursor - 1 + len(opts)) % len(opts)
		}
		m.render()
		return m, nil
	case "right", "l":
		// 横向方向键同样映射为向下选择（部分用户习惯用左右键翻选项）。
		if len(opts) > 0 {
			q.cursor = (q.cursor + 1) % len(opts)
		}
		m.render()
		return m, nil
	case "pgup", "ctrl+u":
		// 覆盖层内容高于终端时滚动正文（自动长高到顶后的逃生通道）
		m.scrollQuestion(-1)
		return m, nil
	case "pgdown", "ctrl+d":
		m.scrollQuestion(+1)
		return m, nil
	case "space", "x":
		if multi && len(opts) > 0 {
			if q.multi == nil {
				q.multi = map[int]bool{}
			}
			q.multi[q.cursor] = !q.multi[q.cursor]
		}
		m.render()
		return m, nil
	case "c", "o":
		// 键盘快捷通道：直接进入自定义文字输入（与选中末尾项等价）
		return m, m.enterCustomInputMode(q)
	case "ctrl+v":
		// 部分终端不把 Cmd+V/Ctrl+V 转成括号粘贴，而是交回应用；此处手动读
		// 剪贴板（结果以 PasteMsg 回到 Update，由粘贴分支填入自定义回答）。
		// 不拦截的话该键会落到 textarea 的 Paste 绑定并插入其内部空剪贴板。
		return m, readClipboardPaste()
	default:
		// 直接键入可见字符 = 想自己写答案：进入自定义输入并保留该字符，
		// 不必先按 Enter 选中兜底项（修复「选中其他后打字没反应」）。
		if text := printableInput(msg); text != "" {
			cmd := m.enterCustomInputMode(q)
			m.input.InsertString(text)
			m.syncInputHeight()
			m.render()
			return m, cmd
		}
	}
	return m, nil
}

// scrollQuestion 按页滚动覆盖层正文（dir<0 上、>0 下）。只在内容超过终端可用
// 高度时才有实际效果；选中项的可视化由 questionLayoutOf 的钳制逻辑保证。
func (m *Model) scrollQuestion(dir int) {
	q := m.question
	if q == nil {
		return
	}
	lay := m.questionLayoutOf(q)
	page := lay.window - 1
	if page < 1 {
		page = 1
	}
	q.scroll += dir * page
	if maxScroll := len(lay.lines) - lay.window; q.scroll > maxScroll {
		q.scroll = maxScroll
	}
	if q.scroll < 0 {
		q.scroll = 0
	}
	m.syncViewportHeight()
	m.render()
}

// syncViewportHeight 把 vpHeight() 计算结果同步到 viewport 实例的 Height。
// vpHeight() 依据 m.question / customMode / thinking / completion / todoPanel
// 动态计算，但 viewport.View() 渲染时用的是 viewport.Height()——若不同步，
// 覆盖层激活/消失时 viewport 仍按旧高度渲染，行数溢出导致覆盖层被 AltScreen 截断。
// 等价于 syncInputHeight 的 viewport 同步部分，但不触发滚动跟随。
func (m *Model) syncViewportHeight() {
	m.viewport.SetHeight(m.vpHeight())
}

// enterCustomInputMode 进入自定义文字输入模式：覆盖层消失（customMode=true 时
// questionView 返回空串），主输入框清空并聚焦。模型继续在 askProvider 阻塞等待
// 用户输入并提交。
func (m *Model) enterCustomInputMode(q *pendingQuestion) tea.Cmd {
	q.customMode = true
	m.input.SetValue("")
	m.syncInputHeight()
	_ = m.input.Focus()
	m.render()
	return nil
}

// handleCustomInputKey 自定义文字输入模式下的按键处理：其余按键全部转交
// 底部主输入框编辑（复用 IME 中文输入、多行换行、动态高度等既有能力）；
// Enter 从主输入框取值提交、Esc 返回选项、Ctrl+C 放弃整个问题。
func (m *Model) handleCustomInputKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	q := m.question
	// @ 文件引用补全菜单打开时，导航/补全键先由菜单接管（与主输入框行为一致）。
	// 之前自定义回答框完全没有补全：输入 "@" 后无候选，也无法 Tab 选中——用户
	// 想引用一张截图只能手打完整路径。
	if m.completion.active && len(m.completion.items) > 0 {
		switch msg.String() {
		case "up", "ctrl+p":
			m.moveCompletion(-1)
			m.render()
			return m, nil
		case "down", "ctrl+n":
			m.moveCompletion(1)
			m.render()
			return m, nil
		case "tab":
			m.acceptCompletion()
			m.syncInputHeight()
			m.updateCompletion()
			m.render()
			return m, nil
		case "enter":
			// 仍在过滤（未精确命中）时 Enter 是「选中候选」，不是「提交回答」。
			if !m.completionExactLabel() {
				m.acceptCompletion()
				m.syncInputHeight()
				m.updateCompletion()
				m.render()
				return m, nil
			}
		}
	}
	// Enter 判定提到 switch 之前：终端可能以 KeyEnter 之外的形态上报 Enter，
	// 且必须在 default 之前拦截，否则会被当成编辑键交给 textarea。
	if isEnterKey(msg) {
		text := strings.TrimSpace(m.input.Value())
		m.input.SetValue("")
		if q.answer == nil || text == "" {
			// 无通道或空文本：回到选项选择
			// 注意：customMode 由 true→false，覆盖层从隐藏变可见，
			// vpHeight 收缩，必须重新 syncViewportHeight——若先调
			// syncInputHeight（此时 customMode 仍 true）再改 customMode，
			// viewport 会保留旧高度把覆盖层挤出可视区。
			q.customMode = false
			m.syncViewportHeight()
			m.render()
			return m, nil
		}
		m.syncInputHeight() // 输入框高度变化（已清空），同步 viewport
		m.completion = completion{}
		q.answers = append(q.answers, m.buildCustomAnswer(q, q.request.Questions[q.current].MultiSelect, text))
		return m, m.advanceOrSubmit(q)
	}
	switch msg.String() {
	case "ctrl+q":
		return m, tea.Quit
	case "ctrl+c":
		m.question = nil
		if q.err != nil {
			q.err <- &userquestions.Error{Code: userquestions.ErrCanceled, Err: fmt.Errorf("user dismissed the question")}
		}
		m.input.SetValue("")
		m.completion = completion{}
		m.syncViewportHeight()
		m.render()
		return m, nil
	case "ctrl+v":
		// 部分终端不把 Ctrl+V 当括号粘贴，交回应用处理：手动读系统剪贴板
		// （回答框里同样需要，用于粘贴截图路径等）。
		return m, readClipboardPaste()
	case "esc":
		// 返回选项选择，清空输入框
		q.customMode = false
		m.input.SetValue("")
		m.completion = completion{}
		m.syncInputHeight()
		m.render()
		return m, nil
	default:
		// 其余按键交给主输入框编辑（Shift+Enter/Ctrl+J 换行、IME 等由 textarea 处理）。
		// 编辑后刷新补全：输入 "@" 或继续打字都要驱动 @ 文件候选与斜杆菜单，
		// 否则回答框里永远看不到候选列表。
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		m.syncInputHeight()
		m.updateCompletion()
		m.render()
		return m, cmd
	}
}

// advanceOrSubmit 完成当前问题：队列还有下一个问题则推进并重置状态，
// 否则提交整个回答并清除覆盖层。任何路径都需 syncViewportHeight——
// 推进到下一问时覆盖层继续显示但 customMode 可能由 true 转 false
// （覆盖层从隐藏变可见，vpHeight 收缩）；提交完毕 m.question=nil 时
// 覆盖层消失，vpHeight 增长，viewport 需重新撑高。
func (m *Model) advanceOrSubmit(q *pendingQuestion) tea.Cmd {
	if q.current+1 < len(q.request.Questions) {
		q.current++
		q.cursor = 0
		q.multi = nil
		q.customMode = false
		m.syncViewportHeight()
		m.render()
		return nil
	}
	m.question = nil
	q.answer <- &userquestions.Answer{Answers: q.answers}
	m.syncViewportHeight()
	m.render()
	return nil
}

// buildCustomAnswer 构造自定义文字回答：单选时不带 Selected（明确表达
// "模型选项都不合适"），多选时保留已勾选项并附自定义文本，文本经
// AnswerItem.Custom 字段返回模型。
func (m *Model) buildCustomAnswer(q *pendingQuestion, multi bool, text string) userquestions.AnswerItem {
	qq := q.request.Questions[q.current]
	item := userquestions.AnswerItem{ID: qq.ID, Custom: text}
	if multi {
		for i, o := range m.modelOptions(q) { // 仅遍历模型选项（不含兜底项）
			if q.multi[i] {
				item.Selected = append(item.Selected, o.Label)
			}
		}
	}
	return item
}

// buildAnswerItem 按当前问题与交互模式构造回答：单选取光标项；
// 多选取已勾选项（按选项顺序）；无选项时视为空选择确认（selected 为 []）。
// 兜底项永远由 enterCustomInputMode 处理，不会走到这里。
func (m *Model) buildAnswerItem(q *pendingQuestion, multi bool) userquestions.AnswerItem {
	qq := q.request.Questions[q.current]
	item := userquestions.AnswerItem{ID: qq.ID}
	opts := m.questionOptions(q)
	if multi {
		for i, o := range opts {
			if m.isCustomOption(q, i) {
				continue // 兜底项不参与多选提交
			}
			if q.multi[i] {
				item.Selected = append(item.Selected, o.Label)
			}
		}
	} else if len(opts) > 0 && q.cursor < len(opts) && !m.isCustomOption(q, q.cursor) {
		item.Selected = []string{opts[q.cursor].Label}
	}
	return item
}

// modelOptions 返回当前问题模型给出的原始选项（不含兜底项）。
func (m *Model) modelOptions(q *pendingQuestion) []userquestions.Option {
	if q == nil || len(q.request.Questions) == 0 {
		return nil
	}
	return q.request.Questions[q.current].Options
}

// questionOptions 返回当前问题的可选项：模型给出的选项 + 末尾追加的"✎ 其他（手动输入）"兜底项。
// 兜底项始终存在（即使模型只给 1 个选项甚至 0 个），保证用户在任何情况下都能输入自由文本。
func (m *Model) questionOptions(q *pendingQuestion) []userquestions.Option {
	if q == nil || len(q.request.Questions) == 0 {
		return nil
	}
	model := q.request.Questions[q.current].Options
	out := make([]userquestions.Option, 0, len(model)+1)
	out = append(out, model...)
	out = append(out, userquestions.Option{
		Label:       customOptionLabel,
		Description: "选项不合适时手动输入回答",
	})
	return out
}

// isCustomOption 判断索引是否指向末尾的兜底项。
func (m *Model) isCustomOption(q *pendingQuestion, idx int) bool {
	opts := m.questionOptions(q)
	return len(opts) > 0 && idx == len(opts)-1
}

// questionView 渲染问题覆盖层（无问题或处于自定义输入模式时返回空串：
// 自定义模式下覆盖层消失，焦点已落到主输入框）。
// 自定义模式下保留一行提示，让用户知道当前正在回答模型问题、Enter 提交后模型
// 才会继续——避免误以为是常规消息输入。
// questionBoxWidth 覆盖层的内容宽度：终端宽度减去边框（2 列）与内边距（左右各 1），
// 上限 100 列（再宽难读）。折行由 lipgloss 的 Width 完成——模型一段长文字不会再把
// 框撑出屏幕右侧被裁掉。
func (m *Model) questionBoxWidth() int {
	if m.width <= 0 {
		// 尚未收到首个 WindowSizeMsg（测试或极早期）：按 80 列排版，
		// 而不是压到 20 列把每行都折成碎片。
		return 76
	}
	w := m.width - 4
	if w > 100 {
		w = 100
	}
	if w < 20 {
		w = 20
	}
	return w
}

// questionChromeRows 是「消息区 + 问题覆盖层」之外界面固定占用的行数。
// 与 vpHeight 的其余扣减项保持一致（标题/状态/提示/边框/输入框/思考行/待办面板）。
func (m *Model) questionChromeRows() int {
	h := titleRows + statusRows + infoRows + boxBorder + m.input.Height()
	if m.workingLineVisible() {
		h += thinkingRow
	}
	return h + m.todoPanelRows()
}

// questionBudgetRows 问题覆盖层可用的最大总行数（含边框）：把消息区压到
// minQuestionViewportRows 行之前，覆盖层可以一直长高——模型写多少就长多高，
// 而不是像以前那样无上限地长、最后被 AltScreen 从顶部裁掉（用户看不到开头）。
func (m *Model) questionBudgetRows() int {
	budget := m.high - m.questionChromeRows() - minQuestionViewportRows
	if budget < 6 {
		budget = 6
	}
	return budget
}

// minQuestionViewportRows 覆盖层展开时消息区至少保留的行数（有上下文可参考）。
const minQuestionViewportRows = 3

// questionLayout 是覆盖层的排版结果：已折行的正文、每个选项的起始行、可见窗口。
type questionLayout struct {
	width    int
	lines    []string
	optStart []int // 每个选项（含兜底项）在 lines 中的起始下标
	window   int   // 可见行数（< len(lines) 时需要滚动）
	scroll   int   // 顶部被滚掉的行数
	total    int
	// overflow 表示内容被折叠（需要滚动）：此时框内底部多一行滚动提示，
	// 预算必须为它留一行——否则渲染结果比 questionRows() 多一行，
	// 整屏就超出终端高度（AltScreen 从顶部裁掉问题开头）。
	overflow bool
}

// wrapToWidth 把一段文本按宽度折行，并把每行补齐到 w 列。
//
// 补白是必须的：框体不再设置 Width（否则 lipgloss 会按「含内边距的总宽」再折一次，
// 把已经折好的行又切碎，实际渲染行数多于预算 → 整屏超出终端高度、顶部被裁）。
// 行宽统一由这里保证，框体只负责加边框与内边距。
func wrapToWidth(s string, w int) []string {
	pad := lipgloss.NewStyle().Width(w)
	if s == "" {
		return []string{pad.Render("")}
	}
	out := strings.Split(lipgloss.NewStyle().Width(w).Render(s), "\n")
	for i := range out {
		out[i] = pad.Render(strings.TrimRight(out[i], " "))
	}
	return out
}

// questionLayoutOf 计算当前问题的排版：先拼全文并按宽度折行，再按预算开窗。
//
// 行数不设「最多显示 N 行」的硬上限，而是让窗口吃满预算（auto-grow）；只有内容
// 真的超过终端可用高度时才开窗滚动，并保证当前选中项始终在窗口内——否则模型写了
// 十几个选项时，光标能移到看不见的地方。
func (m *Model) questionLayoutOf(q *pendingQuestion) questionLayout {
	qq := q.request.Questions[q.current]
	w := m.questionBoxWidth()

	title := "问题"
	if qq.Header != "" {
		title = qq.Header
	}
	if len(q.request.Questions) > 1 {
		title = fmt.Sprintf("%s (%d/%d)", title, q.current+1, len(q.request.Questions))
	}

	var lines []string
	lines = append(lines, wrapToWidth(assistantMark+" DSC · "+title, w)...)
	lines = append(lines, "")
	lines = append(lines, wrapToWidth(qq.Question, w)...)
	if d := strings.TrimSpace(qq.Detail); d != "" {
		// Detail 是模型给的补充说明（此前 TUI 完全不显示，等于丢掉了模型输出的一半）
		lines = append(lines, "")
		lines = append(lines, wrapToWidth(dimSty.Render(d), w)...)
	}
	lines = append(lines, "")

	opts := m.questionOptions(q)
	multi := qq.MultiSelect
	modelLen := len(opts) - 1
	optStart := make([]int, 0, len(opts))
	for i, o := range opts {
		if i == modelLen {
			lines = append(lines, dimSty.Render(strings.Repeat("─", 20)))
		}
		optStart = append(optStart, len(lines))
		mark := "  "
		if multi {
			if q.multi[i] {
				mark = "✓ "
			}
		} else if i == q.cursor {
			mark = "❯ "
		}
		line := mark + o.Label
		if o.Description != "" {
			line += "  " + dimSty.Render(o.Description)
		}
		if i == q.cursor {
			line = accentSty.Render(line)
		}
		lines = append(lines, wrapToWidth(line, w)...)
	}

	hint := "↑/↓ 或 ←/→ 选择 · Enter 确认 · 末项为自定义输入 · Esc 放弃"
	if multi {
		hint = "Space 勾选 · Enter 确认 · 末项为自定义输入 · Esc 放弃"
	}
	lines = append(lines, "")
	lines = append(lines, wrapToWidth(dimSty.Render(hint), w)...)

	// 窗口高度：终端尺寸未知（尚未 resize）时不限高，先按内容原样铺开——
	// 这时的行数无人能判断，硬开窗只会把内容切碎。
	budget := 1 << 30
	if m.high > 0 {
		budget = m.questionBudgetRows() - 2 // 去掉上下边框
	}
	window := len(lines)
	overflow := false
	if window > budget {
		overflow = true
		window = budget - 1 // 留一行给滚动提示
		if window < 1 {
			window = 1
		}
	}
	scroll := q.scroll
	// 选中项必须在窗口内：移到框外等于让用户盲选
	if len(optStart) > 0 {
		cur := optStart[q.cursor]
		if cur < scroll {
			scroll = cur
		}
		if cur >= scroll+window {
			scroll = cur - window + 1
		}
	}
	if maxScroll := len(lines) - window; scroll > maxScroll {
		scroll = maxScroll
	}
	if scroll < 0 {
		scroll = 0
	}
	q.scroll = scroll

	return questionLayout{
		width: w, lines: lines, optStart: optStart,
		window: window, scroll: scroll, total: len(lines), overflow: overflow,
	}
}

// questionRows 覆盖层当前占用的总行数（含边框），供 vpHeight 扣减。
func (m *Model) questionRows() int {
	if m.question == nil || len(m.question.request.Questions) == 0 {
		return 0
	}
	if m.question.customMode {
		return 1 // 仅一行轻量提示
	}
	lay := m.questionLayoutOf(m.question)
	if lay.overflow {
		return lay.window + 1 + 2 // 可见正文 + 滚动提示 + 上下边框
	}
	return lay.window + 2
}

func (m *Model) questionView() string {
	q := m.question
	if q == nil || len(q.request.Questions) == 0 {
		return ""
	}
	if q.customMode {
		// 自定义文字输入模式：覆盖层主体消失，仅保留一行轻量提示
		return dimSty.Render("✎ 自定义回答：在下方输入框输入，Enter 提交 · Esc 返回选项")
	}
	lay := m.questionLayoutOf(q)
	end := lay.scroll + lay.window
	if end > len(lay.lines) {
		end = len(lay.lines)
	}
	body := strings.Join(lay.lines[lay.scroll:end], "\n")
	if lay.overflow {
		// 有折叠时在框内底部标出位置与滚动键（内容本身不改，全部可读到）
		body += "\n" + dimSty.Render(fmt.Sprintf("↕ %d-%d/%d · PgUp/PgDn 滚动", lay.scroll+1, end, len(lay.lines)))
	}
	return questionBoxSty.Render(body)
}
