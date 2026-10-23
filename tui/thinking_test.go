package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestZhAnimationRevealsOneByOne 中文动画只往前走：逐字揭开、停留、然后整行
// 重新被盖住再来一遍——不做反向遮回（把刚读到的字再擦掉，读起来像倒放）。
func TestZhAnimationRevealsOneByOne(t *testing.T) {
	a := &thinkAnim{kind: thinkAnimZh}
	n := len(zhThinkRunes)
	reveal := n * zhFramesPerRune
	cycle := zhCycle()

	if got := a.zhRevealed(); got != 0 {
		t.Fatalf("第 0 帧应全部被遮，revealed=%d", got)
	}

	// 揭示段：每 zhFramesPerRune 帧恰好多露出一个字，严格单调不降
	prev := -1
	for f := 0; f < reveal; f++ {
		a.frame = f
		got := a.zhRevealed()
		if want := f / zhFramesPerRune; got != want {
			t.Fatalf("frame=%d revealed=%d, want %d", f, got, want)
		}
		if got < prev {
			t.Fatalf("frame=%d 露出字数回退（%d → %d）：不应有反向遮回", f, prev, got)
		}
		prev = got
	}

	// 停留段：全亮不动
	for f := reveal; f < cycle; f++ {
		a.frame = f
		if got := a.zhRevealed(); got != n {
			t.Fatalf("停留期 frame=%d 应保持全亮，got %d", f, got)
		}
	}

	// 下一轮从头重来：整行重新被方块盖住
	a.frame = cycle
	if got := a.zhRevealed(); got != 0 {
		t.Fatalf("新周期应从全遮开始，got %d", got)
	}

	// 满格时可见文字就是完整文案
	a.frame = reveal
	if got := ansi.Strip(a.zhText()); got != zhThinkText() {
		t.Fatalf("满格后应为完整文案，got %q", got)
	}
}

// zhCycle 中文动画一个周期的帧数：揭示 + 停留。
func zhCycle() int {
	return len(zhThinkRunes)*zhFramesPerRune + zhHoldFrames
}

// TestZhAnimationMasksWithUnderline 未揭示的位置是全角下划线占位，不是白块、
// 也不是原字符：半角 "_" 只有 1 列宽（会与 2 列宽的汉字错位），故用 U+FF3F；
// 句点不参与遮罩，始终可见。
func TestZhAnimationMasksWithUnderline(t *testing.T) {
	a := &thinkAnim{kind: thinkAnimZh, frame: 0}
	raw := a.zhText()

	// 第 0 帧：正文全是下划线，句点照常显示
	got := ansi.Strip(raw)
	want := strings.Repeat(string(zhMaskRune), len(zhThinkRunes)) + zhThinkDots
	if got != want {
		t.Fatalf("第 0 帧应为全下划线 + 句点，got %q want %q", got, want)
	}
	if strings.Contains(raw, "48;") {
		// 不该再有背景色（白块时代靠背景遮字）
		t.Fatalf("占位符不该用背景色: %q", raw)
	}

	// 全揭示后是完整文案，一个下划线都不剩
	a.frame = len(zhThinkRunes) * zhFramesPerRune
	if got := ansi.Strip(a.zhText()); got != zhThinkText() {
		t.Fatalf("全揭示后应为完整文案，got %q", got)
	}
	if strings.Contains(ansi.Strip(a.zhText()), string(zhMaskRune)) {
		t.Fatal("全揭示后不应残留占位符")
	}

	// 逐字替换：第 k 个位置被揭开后，它不再是下划线
	a.frame = zhFramesPerRune // 揭开 1 个字
	r := []rune(ansi.Strip(a.zhText()))
	if r[0] != zhThinkRunes[0] {
		t.Fatalf("首字应已揭示，got %q", string(r[0]))
	}
	if r[1] != zhMaskRune {
		t.Fatalf("第二字应仍为占位符，got %q", string(r[1]))
	}
}

// TestEnAnimationMorphsToThinking 英文动画的三段：SEEKING 大小写翻动 →
// 擦除 → 写出 THINKING → 定格在 Deep Thinking..。
func TestEnAnimationMorphsToThinking(t *testing.T) {
	a := &thinkAnim{kind: thinkAnimEn}
	cycle := enWaveFrames + enUpperFrames + enEraseFrames + enWriteFrames + enHoldFrames

	var sawUpper, sawEmptyWord, sawThinking bool
	for f := 0; f < 2*cycle; f++ {
		a.frame = f
		text := ansi.Strip(a.enText())
		if strings.Contains(text, "SEEKING") {
			sawUpper = true
		}
		// 擦除阶段：整句被清空，只剩一条句点
		if strings.Contains(text, strings.Repeat(" ", len(enSrc))+"..") {
			sawEmptyWord = true
		}
		if strings.Contains(text, "Deep Thinking..") {
			sawThinking = true
		}
	}
	if !sawUpper {
		t.Fatal("应出现全大写 SEEKING")
	}
	if !sawEmptyWord {
		t.Fatal("应出现擦除后的空词槽（Deep       ..）")
	}
	if !sawThinking {
		t.Fatal("应收束到 Deep Thinking..")
	}

	// 大小写波阶段只翻大小写、不换字母：忽略大小写应恒等于 Deep Seeking..
	for f := 0; f < enWaveFrames; f++ {
		a.frame = f
		got := strings.TrimRight(ansi.Strip(a.enText()), " ")
		if !strings.EqualFold(got, "Deep Seeking..") {
			t.Fatalf("波形阶段 frame=%d 应仍为 Deep Seeking..（仅大小写变化），got %q", f, got)
		}
	}
	// 且确实在翻动：本段内不应每一帧都完全一致
	a.frame = 0
	first := ansi.Strip(a.enText())
	sawDifferent := false
	for f := 1; f < enWaveFrames; f++ {
		a.frame = f
		if ansi.Strip(a.enText()) != first {
			sawDifferent = true
			break
		}
	}
	if !sawDifferent {
		t.Fatal("大小写波应逐帧变化")
	}
}

// TestThinkAnimWidthIsStable 两条动画在每一帧的显示宽度都必须固定：
// 宽度跳动会让行尾字符左右抖，比不做动画更难看。
func TestThinkAnimWidthIsStable(t *testing.T) {
	zh := &thinkAnim{kind: thinkAnimZh}
	zhWant := -1
	for f := 0; f < zhCycle(); f++ {
		zh.frame = f
		w := ansi.StringWidth(zh.text())
		if zhWant < 0 {
			zhWant = w
		} else if w != zhWant {
			t.Fatalf("中文动画 frame=%d 宽度 %d，应为 %d", f, w, zhWant)
		}
	}

	en := &thinkAnim{kind: thinkAnimEn}
	enWant := -1
	for f := 0; f < enWaveFrames+enUpperFrames+enEraseFrames+enWriteFrames+enHoldFrames; f++ {
		en.frame = f
		w := ansi.StringWidth(en.text())
		if enWant < 0 {
			enWant = w
		} else if w != enWant {
			t.Fatalf("英文动画 frame=%d 宽度 %d，应为 %d", f, w, enWant)
		}
	}
	if zhWant <= 0 || enWant <= 0 {
		t.Fatalf("宽度应大于 0: zh=%d en=%d", zhWant, enWant)
	}
}

// TestThinkAnimKindIsRandomPerTurn 每一轮随机选语种：reset 多次应两种都出现，
// 且 reset 会把帧号归零（新一轮从第一帧起播）。
func TestThinkAnimKindIsRandomPerTurn(t *testing.T) {
	a := &thinkAnim{}
	seenZh, seenEn := false, false
	for i := 0; i < 200; i++ {
		a.frame = 99
		a.reset()
		if a.frame != 0 {
			t.Fatalf("reset 后帧号应归零，got %d", a.frame)
		}
		switch a.kind {
		case thinkAnimZh:
			seenZh = true
		case thinkAnimEn:
			seenEn = true
		default:
			t.Fatalf("未知动画语种: %v", a.kind)
		}
	}
	if !seenZh || !seenEn {
		t.Fatalf("随机应覆盖两种动画: zh=%v en=%v", seenZh, seenEn)
	}
}

// TestThinkAnimFrameAdvancesWithSpinnerTick 帧号由 spinner 的 tick 推进：
// 不另开时钟，spinner 停则动画停。
func TestThinkAnimFrameAdvancesWithSpinnerTick(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	before := m.thinkAnim.frame
	m.thinking = true
	model, _ := m.Update(m.spinner.Tick())
	if got := model.(*Model).thinkAnim.frame; got != before+1 {
		t.Fatalf("spinner tick 应推进一帧: %d → %d", before, got)
	}

	// 空闲时（无 thinking/streaming）不推进
	idle := model.(*Model)
	idle.thinking, idle.streaming = false, false
	f := idle.thinkAnim.frame
	model2, _ := idle.Update(idle.spinner.Tick())
	if got := model2.(*Model).thinkAnim.frame; got != f {
		t.Fatalf("空闲时不应推进动画帧: %d → %d", f, got)
	}
}

// TestEnAnimationMovesTheFirstWord 前后两个词必须走同一套过程：先前只擦后面的
// 词、把 Deep 定住，同一行两种节奏并存，像两段动画拼在一起。
func TestEnAnimationMovesTheFirstWord(t *testing.T) {
	a := &thinkAnim{kind: thinkAnimEn}

	// 波阶段：前面的词的大小写形态必须逐帧变化（波带只有 2 列宽，
	// 不会出现整个 DEEP 同时大写，故按「形态有几种」判断是否在动）
	heads := map[string]bool{}
	for f := 0; f < enWaveFrames; f++ {
		a.frame = f
		heads[ansi.Strip(a.enText())[:len("Deep")]] = true
	}
	if len(heads) < 2 {
		t.Fatalf("前面的 Deep 应随波改变大小写，实际只有 %d 种形态: %v", len(heads), heads)
	}

	// 擦除阶段：整句一起消失——前面那个词也不能定住
	eraseStart := enWaveFrames + enUpperFrames
	sawHeadErased := false
	for f := eraseStart; f < eraseStart+enEraseFrames; f++ {
		a.frame = f
		text := ansi.Strip(a.enText())
		if !strings.HasPrefix(text, "DEEP") && !strings.HasPrefix(text, "Deep") {
			sawHeadErased = true
		}
	}
	if !sawHeadErased {
		t.Fatal("擦除阶段前面的 Deep 也应被擦掉，而不是定住不动")
	}

	// 写出阶段：整句逐字写回，写出来就是最终形态
	writeStart := eraseStart + enEraseFrames
	a.frame = writeStart
	if got := ansi.Strip(a.enText()); strings.Contains(got, "Deep") {
		t.Fatalf("写出起点整句应为空白，got %q", got)
	}
	a.frame = writeStart + 2
	if got := ansi.Strip(a.enText()); !strings.HasPrefix(got, "De") {
		t.Fatalf("写出第 2 字应为 De，got %q", got)
	}

	// 定格：Deep Thinking..，且静止
	a.frame = writeStart + enWriteFrames
	got := ansi.Strip(a.enText())
	if !strings.HasPrefix(got, "Deep Thinking..") {
		t.Fatalf("定格应为 Deep Thinking..，got %q", got)
	}
	a.frame++
	if next := ansi.Strip(a.enText()); next != got {
		t.Fatalf("定格期应静止: %q vs %q", got, next)
	}
}
