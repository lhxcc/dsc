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
	// maskSty 白底白字：字符位置被「白方块」盖住。不用 █ 之类的方块字符——
	// 全角块在不同终端按 1 或 2 列渲染，会让整行宽度抖动、行尾跳字。
	maskSty = lipgloss.NewStyle().
		Background(lipgloss.Color("#FFFFFF")).
		Foreground(lipgloss.Color("#FFFFFF"))
)

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
	// zhHoldFrames 全部揭开后的停留帧数，让人有机会读完这行字。
	zhHoldFrames = 6
	// zhGapFrames 全部遮回去后停顿一拍再开始下一轮：连着重遮会让这行字看起来
	// 像在闪，留一拍空档才有节奏。
	zhGapFrames = 2
)

// zhThinkText 中文动画文案（5 个汉字 + 2 个点）。
var zhThinkText = []rune("深度求索中..")

func (a *thinkAnim) zhText() string {
	revealed, hotIdx := a.zhState()

	var b strings.Builder
	for i, r := range zhThinkText {
		if i >= revealed {
			b.WriteString(maskSty.Render(string(r)))
			continue
		}
		if i == hotIdx {
			b.WriteString(thinkHotSty.Render(string(r)))
			continue
		}
		b.WriteString(thinkTextSty.Render(string(r)))
	}
	return b.String()
}

// zhRevealed 返回当前帧仍露在外面的字数（0..全部）。
//
// 一个周期是「逐字揭开 → 停留 → 逐字遮回去 → 停顿」：单向揭示后瞬间整体重遮
// 会像闪一下；反向着遮回去，读起来才是同一段动画在往复。
func (a *thinkAnim) zhRevealed() int {
	revealed, _ := a.zhState()
	return revealed
}

// zhState 返回当前帧的（露出的字数，当前焦点字下标）。
// 焦点字：揭示阶段是刚露出的那个，遮回阶段是即将被遮的那个（-1 表示无）。
func (a *thinkAnim) zhState() (revealed, hotIdx int) {
	n := len(zhThinkText)
	reveal := n * zhFramesPerRune
	cover := reveal
	f := a.frame % (reveal + zhHoldFrames + cover + zhGapFrames)

	switch {
	case f < reveal: // 逐字揭开
		r := f / zhFramesPerRune
		return r, r - 1
	case f < reveal+zhHoldFrames: // 全亮停留
		return n, -1
	case f < reveal+zhHoldFrames+cover: // 逐字遮回
		left := n - (f-reveal-zhHoldFrames)/zhFramesPerRune
		if left < 0 {
			left = 0
		}
		return left, left
	}
	return 0, -1 // 全遮停顿
}

// ---- 英文：大小写翻动 → 擦除 → 改写 ----

const (
	enWaveFrames  = 12 // 大小写波扫过
	enUpperFrames = 3  // 整串大写闪一下
	enEraseFrames = 9  // 逐字擦掉 SEEKING（8 个字母 + 1 拍停留）
	enWriteFrames = 10 // 逐字写出 THINKING（8 个字母 + 2 拍收尾）
	enHoldFrames  = 12 // 定格在 Deep Thinking.. 停留
)

const (
	enPrefix = "Deep "
	enSuffix = ".."
	// enWidth 整段目标显示宽度：按最长的 "Deep Thinking.."（5+8+2）计。
	// 每帧都补到该宽度，阶段切换时行尾不右移——否则同一回合里
	// 「(5 秒 · Ctrl+C 取消)」会左右跳一格。
	enWidth = len(enPrefix) + len("THINKING") + len(enSuffix)
)

func (a *thinkAnim) enText() string {
	wave := enWaveFrames
	upper := wave + enUpperFrames
	erase := upper + enEraseFrames
	write := erase + enWriteFrames
	cycle := write + enHoldFrames

	g := a.frame % cycle

	// 各阶段的词长度不同（Seeking 7 / Thinking 8），故词槽按阶段给尺寸，
	// 末尾再补不可见空格让整段恒定 enWidth 宽。若把短词就地补齐，会渲染出
	// 「Deep Seeking ..」——词与句点之间凭空多一个空格。
	var word []rune
	var hot []bool

	switch {
	case g < wave:
		// 大小写波：相位随位置错开，形成一道扫过的大写带。
		// 用取模到正数的写法，避免 g-i 为负时 Go 的 % 给出负余数。
		word = []rune("Seeking")
		hot = make([]bool, len(word))
		for i := range word {
			// 周期 7 而不是 5：波阶段共 12 帧，周期若整除帧数就会原样重复一遍，
			// 看着像卡住；7 与 12 互质，每帧的形态都不同。
			if ((g-i)%7+7)%7 < 2 {
				word[i] = unicode.ToUpper(word[i])
			}
		}
	case g < upper:
		word = []rune("SEEKING")
		hot = make([]bool, len(word))
		for i := range hot {
			hot[i] = true
		}
	case g < erase:
		// 从左到右逐字擦成空格，未擦到的部分保持大写
		word = []rune("SEEKING")
		hot = make([]bool, len(word))
		for i := range word {
			if i < g-upper {
				word[i] = ' '
			}
		}
	case g < write:
		// 从左到右逐字写出 THINKING，最新写出的一个字高亮。
		// 词槽始终保持 8 宽（未写出的部分留空格）：否则 ".." 会紧贴 Deep，
		// 句点在擦除与写出的交界处突然从行尾跳到行首。
		written := g - erase
		if written > len("THINKING") {
			written = len("THINKING")
		}
		word = []rune(strings.Repeat(" ", len("THINKING")))
		copy(word, []rune("THINKING")[:written])
		hot = make([]bool, len(word))
		if written > 0 {
			hot[written-1] = true
		}
	default:
		// 定格：回到正常大小写的 Deep Thinking..
		word = []rune("Thinking")
		hot = make([]bool, len(word))
	}

	var b strings.Builder
	b.WriteString(thinkTextSty.Render(enPrefix))
	for i, r := range word {
		s := string(r)
		if hot[i] && r != ' ' {
			b.WriteString(thinkHotSty.Render(s))
			continue
		}
		b.WriteString(thinkTextSty.Render(s))
	}
	b.WriteString(thinkTextSty.Render(enSuffix))
	if pad := enWidth - len(enPrefix) - len(word) - len(enSuffix); pad > 0 {
		b.WriteString(strings.Repeat(" ", pad))
	}
	return b.String()
}
