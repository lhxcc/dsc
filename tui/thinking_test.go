package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// TestZhAnimationRevealsOneByOne 中文动画是「逐字揭开白方块、停留、再逐字遮回去」：
// 一个周期内露出字数从 0 单调升到满，再单调降回 0，且 0..满 每个值都出现过。
func TestZhAnimationRevealsOneByOne(t *testing.T) {
	a := &thinkAnim{kind: thinkAnimZh}
	n := len(zhThinkText)
	cycle := zhCycle()

	if got := a.zhRevealed(); got != 0 {
		t.Fatalf("第 0 帧应全部被遮，revealed=%d", got)
	}

	var seq []int
	for f := 0; f < cycle; f++ {
		a.frame = f
		seq = append(seq, a.zhRevealed())
	}
	// 前半段逐字增长（每 zhFramesPerRune 帧 +1），后半段逐字减少
	for f := 0; f < n*zhFramesPerRune; f++ {
		if want := f / zhFramesPerRune; seq[f] != want {
			t.Fatalf("frame=%d revealed=%d, want %d", f, seq[f], want)
		}
	}
	if seq[n*zhFramesPerRune] != n {
		t.Fatalf("揭示结束时应满格，got %d", seq[n*zhFramesPerRune])
	}
	for f := n*zhFramesPerRune + zhHoldFrames; f < cycle; f++ {
		if seq[f] > n {
			t.Fatalf("frame=%d revealed 越界: %d", f, seq[f])
		}
	}
	if seq[cycle-1] != 0 {
		t.Fatalf("周期末尾应回到全遮，got %d", seq[cycle-1])
	}
	seen := make([]bool, n+1)
	for _, v := range seq {
		seen[v] = true
	}
	for i := 0; i <= n; i++ {
		if !seen[i] {
			t.Fatalf("一个周期内应出现「露出 %d 字」的帧", i)
		}
	}

	// 满格那一帧，可见文字就是完整文案
	a.frame = n * zhFramesPerRune
	if got := ansi.Strip(a.zhText()); got != string(zhThinkText) {
		t.Fatalf("满格后应为完整文案，got %q", got)
	}
}

// zhCycle 中文动画一个周期的帧数。
func zhCycle() int {
	n := len(zhThinkText)
	return 2*n*zhFramesPerRune + zhHoldFrames + zhGapFrames
}

// TestZhAnimationMasksWithWhiteBlock 被遮的字用白底白字盖住（不是换成别的
// 方块字符）——换成 █ 之类会因终端宽度判定不同让整行抖动。
func TestZhAnimationMasksWithWhiteBlock(t *testing.T) {
	a := &thinkAnim{kind: thinkAnimZh, frame: 0}
	raw := a.zhText()
	if !strings.Contains(raw, "48;2;255;255;255") && !strings.Contains(raw, "48;5;255") {
		t.Fatalf("未遮部分应带白色背景: %q", raw)
	}
	// 遮罩不改变字符本身：剥掉样式后仍是完整文案
	if got := ansi.Strip(raw); got != string(zhThinkText) {
		t.Fatalf("遮罩不应改变可见字符，got %q", got)
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
		// 擦除阶段：词槽被清空，只剩 Deep、一段空白与 ..
		if strings.Contains(text, "Deep"+strings.Repeat(" ", 1+len("SEEKING"))+"..") {
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
