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
// 一行不动的提示等于噪音。这里在 spinner 后加一段会说话的文字。
//
// 两种动画，每轮随机选一种（随机点选放在 startTurn，一轮之内不切换，避免
// 文字在一个回合里来回跳）：
//
//	中文：一道蓝紫扫描光带从左往右推过「深度求索中..」。光带未到之处是被暗紫
//	      色实心块盖住的，光带扫过即点亮成字，身后留一段紫色余晖；扫到句点后
//	      停一拍，整行重新盖回。
//	英文：「Deep Seeking..」整句先被一道大写波扫过，再整句大写闪一下，然后
//	      整句被逐字擦掉、逐字写回「Deep Thinking..」。
//
// 两条动画的显示宽度都是固定的：中文的遮罩是「原字符 + 同色背景」（宽度由字符
// 本身决定），英文各阶段都补到同一宽度。否则行尾会随动画抖动，比不动还难看。

// 思考动画的样式。正文用比 dimSty 亮的灰，保证「更明显」这个诉求；
// 光带与余晖用蓝紫，形成注意力焦点。
var (
	thinkTextSty = lipgloss.NewStyle().Foreground(lipgloss.Color("#CCCCCC"))
	thinkHotSty  = lipgloss.NewStyle().Foreground(accent).Bold(true)

	// 中文扫描光带的四档：未扫到的实心块 → 紧邻光带的前缘 → 光点 → 身后的余晖。
	//
	// 遮罩用「原字符 + 同色背景」而不是换成 █ 之类的方块字符：全角块在不同终端
	// 按 1 或 2 列渲染，逐字推进时整行会抖；同字符渲染则宽度天然不变。
	// 背景与前景同色 = 字被这块颜色吞掉，看起来就是实心的。
	zhBlockSty = lipgloss.NewStyle().
			Background(lipgloss.Color("#2B2455")).
			Foreground(lipgloss.Color("#2B2455"))
	zhBlockNearSty = lipgloss.NewStyle().
			Background(lipgloss.Color("#4A3C9E")).
			Foreground(lipgloss.Color("#4A3C9E"))
	zhScanSty  = lipgloss.NewStyle().Foreground(lipgloss.Color("#E6DCFF")).Bold(true)
	zhTrailSty = lipgloss.NewStyle().Foreground(lipgloss.Color("#A88CFF"))
)

// thinkAnimKind 是动画语种。
type thinkAnimKind int

const (
	thinkAnimZh thinkAnimKind = iota // 「深度求索中..」扫描光带
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

// ---- 中文：蓝紫扫描光带 ----

const (
	// zhFramesPerStep 光带每前进一个字符位所占的帧数（2 帧 ≈ 200ms）。
	zhFramesPerStep = 2
	// zhHoldFrames 光带扫到句点后的停留帧数，让人读完整行再从头开始。
	zhHoldFrames = 6
)

// 中文动画文案：被光带扫过的正文，与始终可见的句点分开。
//
// 句点不参与遮挡：它是「进行中」的句读标记，不是待揭示的内容——盖住它，剩下的
// 「深度求索中」读起来像已经想完了。它照常显示，但光带扫过时同样被点亮。
var zhThinkRunes = []rune("深度求索中")

const zhThinkDots = ".."

// zhThinkText 完整文案（正文 + 句点），仅用于测试与宽度核对。
func zhThinkText() string {
	return string(zhThinkRunes) + zhThinkDots
}

// zhTotalCells 光带要扫过的字符位数（正文 + 句点）。
func zhTotalCells() int {
	return len(zhThinkRunes) + len([]rune(zhThinkDots))
}

// zhCycle 中文动画一个周期的帧数。
func zhCycle() int {
	return zhTotalCells()*zhFramesPerStep + zhHoldFrames
}

func (a *thinkAnim) zhText() string {
	head := a.zhHead()
	n := len(zhThinkRunes)

	var b strings.Builder
	for i, r := range zhThinkRunes {
		s := string(r)
		switch {
		case i == head: // 光点
			b.WriteString(zhScanSty.Render(s))
		case i == head-1: // 身后的余晖
			b.WriteString(zhTrailSty.Render(s))
		case i > head+1: // 尚未扫到：暗紫实心块
			b.WriteString(zhBlockSty.Render(s))
		case i > head: // 紧邻光带的前缘，稍亮一档，做出光晕感
			b.WriteString(zhBlockNearSty.Render(s))
		default: // 已被扫过
			b.WriteString(thinkTextSty.Render(s))
		}
	}
	// 句点常显；光带扫到哪儿，哪儿亮一下。
	for j, r := range []rune(zhThinkDots) {
		s := string(r)
		switch n + j {
		case head:
			b.WriteString(zhScanSty.Render(s))
		case head - 1:
			b.WriteString(zhTrailSty.Render(s))
		default:
			b.WriteString(thinkTextSty.Render(s))
		}
	}
	return b.String()
}

// zhHead 返回当前帧光点所在的字符位（0..最后一位）。
//
// 光带扫到句点末尾就停在那里停留一拍，然后回到第 0 位重新开始——只往前走，
// 不做反向扫回：倒着扫等于把刚点亮的字再盖掉，读起来像倒放。
func (a *thinkAnim) zhHead() int {
	last := zhTotalCells() - 1
	h := (a.frame % zhCycle()) / zhFramesPerStep
	if h > last {
		h = last
	}
	return h
}

// zhRevealed 返回当前帧已点亮的正文字数（0..全部）。
func (a *thinkAnim) zhRevealed() int {
	n := len(zhThinkRunes)
	if h := a.zhHead(); h < n {
		return h
	}
	return n
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
