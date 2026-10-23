package tui

import (
	"math/rand"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
)

// 「模型正在工作」行的文字动画。
//
// 只有一个 1 列宽的 spinner 时，长回合里很难一眼看出程序是活着还是在等 IO：
// 一行不动的提示等于噪音。这里在 spinner 后加一段会说话的文字，并能分辨
// 「正在想」与「正在输出」之外的中间态。
//
// 两种动画，每轮随机选一种（随机点选放在 startTurn，一轮之内不切换，避免
// 文字在一个回合里来回跳）：
//
//	中文：逐字用白方块遮住「深度求索中..」，方块从左往右逐字揭开，全亮后
//	      停留一拍再重新遮住。
//	英文：「Deep Seeking..」先让字母逐字在大小写间翻动（一道大写波扫过），
//	      再整串大写闪一下，然后把 SEEKING 逐字擦掉、逐字写出 THINKING，
//	      最后定格在「Deep Thinking..」。
//
// 两条动画的显示宽度都是固定的：中文每个字无论遮没遮都用原字符渲染（方块是
// 白底白字，宽度由字符本身决定），英文各阶段都补到同一宽度。否则行尾会随动画
// 抖动，比不动还难看。

// 思考动画的样式。正文用比 dimSty 亮的灰，保证「更明显」这个诉求；
// 刚揭示或刚变化的字符用强调色，形成注意力焦点。
var (
	thinkTextSty = lipgloss.NewStyle().Foreground(lipgloss.Color("#CCCCCC"))
	thinkHotSty  = lipgloss.NewStyle().Foreground(accent).Bold(true)
	// maskPlaceholderSty 未揭示位置的占位符：暗灰的全角下划线。
	//
	// 用「全角下划线占位」而不是白色实心块：一排白块虽然抢眼，但像贴了马赛克，
	// 与 TUI 的细线风格不搭；下划线是「待填空」的常规符号，逐字被换成字的过程
	// 就是一行横线被逐个填上，观感轻得多。
	//
	// 必须是全角（U+FF3F）：半角 "_" 只有 1 列宽，而它替换的是 2 列宽的汉字，
	// 逐字揭示时整行会左缩右跳。全角下划线与汉字同宽，行宽才稳。
	maskPlaceholderSty = lipgloss.NewStyle().
				Foreground(lipgloss.Color("#666666"))
)

// zhMaskRune 未揭示的中文字用全角下划线占位（与汉字同为 2 列宽）。
const zhMaskRune = '＿'

// thinkAnimKind 是动画语种。
type thinkAnimKind int

const (
	thinkAnimZh thinkAnimKind = iota // 「深度求索中..」逐字揭示
	thinkAnimEn                      // 「Deep Seeking..」→「Deep Thinking..」
)

// thinkAnim 是一轮之内保持不变的动画状态；frame 由 spinner 的 tick 推进
// （每 tick 一帧，约 100ms）。用 spinner 的节拍而不是自己的定时器：两者
// 同时存在只会多出一条永远在跑的时钟，且容易与 spinner 的启停错位。
type thinkAnim struct {
	kind  thinkAnimKind
	frame int
}

// reset 开始新一轮：重新随机选语种并归零帧号。
func (a *thinkAnim) reset() {
	a.kind = randThinkKind()
	a.frame = 0
}

// tick 推进一帧。
func (a *thinkAnim) tick() { a.frame++ }

// randThinkKind 随机选一种动画。Go 1.20+ 的全局 rand 已自动播种，
// 这里不需要（也不该）自己持有带锁的源。
func randThinkKind() thinkAnimKind {
	if rand.Intn(2) == 0 {
		return thinkAnimZh
	}
	return thinkAnimEn
}

// text 返回当前帧应显示的文字（已上色，宽度固定）。
func (a *thinkAnim) text() string {
	if a.kind == thinkAnimZh {
		return a.zhText()
	}
	return a.enText()
}

// ---- 中文：逐字揭示 ----

const (
	// zhFramesPerRune 每个字「被揭开」所占的帧数。2 帧 ≈ 200ms，7 个字
	// 大约 1.4s 走完，比每帧一字慢一点，看得出是逐字而不是闪一下。
	zhFramesPerRune = 2
	// zhHoldFrames 全部揭开后的停留帧数，让人有机会读完这行字，再从头重来。
	zhHoldFrames = 10
)

// 中文动画文案：要被逐字揭示的部分，与始终可见的句点分开。
// 句点不参与遮罩——把「.」也换成下划线，一行里会混进两条比字宽短的下划线，
// 看着像排版出错；点本来就只是句读，不需要被「揭示」。
var zhThinkRunes = []rune("深度求索中")

const zhThinkDots = ".."

// zhThinkText 完整文案（正文 + 句点），仅用于测试与宽度核对。
func zhThinkText() string {
	return string(zhThinkRunes) + zhThinkDots
}

func (a *thinkAnim) zhText() string {
	revealed, hotIdx := a.zhState()

	var b strings.Builder
	for i, r := range zhThinkRunes {
		switch {
		case i >= revealed:
			b.WriteString(maskPlaceholderSty.Render(string(zhMaskRune)))
		case i == hotIdx:
			b.WriteString(thinkHotSty.Render(string(r)))
		default:
			b.WriteString(thinkTextSty.Render(string(r)))
		}
	}
	b.WriteString(thinkTextSty.Render(zhThinkDots))
	return b.String()
}

// zhRevealed 返回当前帧仍露在外面的字数（0..全部）。
//
// 一个周期是「逐字揭开 → 停留 → 从头重来」：只往前揭示，不做反向遮回。
// 遮回去等于把刚读到的字再擦掉，读起来像倒放；重来时整行重新被方块盖住，
// 是 loading 指示器该有的重启感。
func (a *thinkAnim) zhRevealed() int {
	revealed, _ := a.zhState()
	return revealed
}

// zhState 返回当前帧的（露出的字数，当前焦点字下标）。
// 焦点字是刚露出的那个（停留期与重来的第一帧没有焦点，返回 -1）。
func (a *thinkAnim) zhState() (revealed, hotIdx int) {
	n := len(zhThinkRunes)
	reveal := n * zhFramesPerRune
	f := a.frame % (reveal + zhHoldFrames)

	if f < reveal {
		r := f / zhFramesPerRune
		return r, r - 1
	}
	return n, -1 // 全亮停留，之后回到第 0 帧重新遮住
}

// ---- 英文：大小写翻动 → 擦除 → 改写 ----

const (
	enWaveFrames  = 12 // 大小写波扫过整句
	enUpperFrames = 2  // 整句大写闪一下
	enEraseFrames = 13 // 逐字擦掉整句（13 个字符位 + 收尾一拍）
	enWriteFrames = 14 // 逐字写出整句（13 个字符位 + 收尾一拍）
	enHoldFrames  = 10 // 定格在 Deep Thinking.. 停留
)

// 英文动画的两个整句（源句 12 字，目的句 13 字）。
//
// 不把源句补成 13 宽：那样词与句点之间会多出一个空格（"Deep Seeking .."）。
// 长度差由渲染末尾的补位吸收，代价是句点在「擦除结束 → 写出开始」时右移一格
// ——那一刻整行正好只剩一条句点，移一格几乎看不出来。
const (
	enSrc = "Deep Seeking"
	enDst = "Deep Thinking"
)

const (
	enSuffix = ".."
	// enWidth 整段目标显示宽度：按最长的 "Deep Thinking.."（5+8+2）计。
	// 每帧都补到该宽度，阶段切换时行尾不右移——否则同一回合里
	// 「(5 秒 · Ctrl+C 取消)」会左右跳一格。
	enWidth = len(enDst) + len(enSuffix)
)

func (a *thinkAnim) enText() string {
	wave := enWaveFrames
	upper := wave + enUpperFrames
	erase := upper + enEraseFrames
	write := erase + enWriteFrames
	cycle := write + enHoldFrames

	g := a.frame % cycle

	// body 是句点之前的整句。
	//
	// 两个词走完全一样的过程：一起被大小写波扫过、一起闪现大写、一起被逐字擦掉、
	// 一起被逐字写回。先前只擦后面的词、把 "Deep" 定住不动，同一行里两种节奏并存，
	// 看着像两段动画拼在一起；整句同进同退才有「一句话被改写」的完整感。
	var body []rune
	var hot []bool

	switch {
	case g < wave:
		// 大小写波：相位按位置逐列错开、按帧推进，形成一道宽 2 列的大写带
		// 从行首扫到行尾。取模到正数：g-i 为负时 Go 的 % 会给出负余数。
		body = []rune(enSrc)
		hot = make([]bool, len(body))
		for i := range body {
			if ((g-i)%7+7)%7 < 2 {
				body[i] = unicode.ToUpper(body[i])
			}
		}
	case g < upper:
		body = []rune(strings.ToUpper(enSrc))
		hot = make([]bool, len(body))
	case g < erase:
		// 整句从左到右一起消失（擦完后的那一拍正好是空白停顿）
		erased := g - upper
		body = []rune(strings.ToUpper(enSrc))
		hot = make([]bool, len(body))
		for i := range body {
			if i < erased {
				body[i] = ' '
			}
		}
	case g < write:
		// 逐字写出新的整句，写出来就是最终形态（不再做一次大小写收束）
		written := g - erase
		dst := []rune(enDst)
		body = []rune(strings.Repeat(" ", len(dst)))
		if written > len(dst) {
			written = len(dst)
		}
		copy(body, dst[:written])
		hot = make([]bool, len(body))
		if written > 0 {
			hot[written-1] = true
		}
	default:
		body = []rune(enDst)
		hot = make([]bool, len(body))
	}

	var b strings.Builder
	for i, r := range body {
		s := string(r)
		if hot[i] && r != ' ' {
			b.WriteString(thinkHotSty.Render(s))
			continue
		}
		b.WriteString(thinkTextSty.Render(s))
	}
	b.WriteString(thinkTextSty.Render(enSuffix))
	if pad := enWidth - len(body) - len(enSuffix); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}
