package tui

import (
	"context"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	"dsc/userquestions"
	"github.com/charmbracelet/x/ansi"
)

// TestQuestionOverlayRowBudgetAnalysis 模拟用户截图场景：终端高度较小，
// 3 个选项的覆盖层渲染后逻辑行数与 vpHeight 扣减是否一致；并验证
// 总布局（标题+viewport+覆盖层+composer+info+status）是否超出 m.high。
func TestQuestionOverlayRowBudgetAnalysis(t *testing.T) {
	// 模拟用户截图：~37 行终端
	for _, high := range []int{37, 30, 25} {
		t.Run("high="+itoa(high), func(t *testing.T) {
			m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
			m.high = high
			m.width = 93 // 用户截图估算宽度
			m.viewport = viewport.New(viewport.WithWidth(93), viewport.WithHeight(m.vpHeight()))
			m.viewport.SetWidth(93)
			m.viewport.SetHeight(m.vpHeight())
			m.ready = true
			m.input.SetWidth(85)

			ansCh := make(chan *userquestions.Answer, 1)
			errCh := make(chan error, 1)
			m.Update(questionMsg{request: &userquestions.Request{Questions: []userquestions.Question{{
				ID:       "q",
				Question: "請告訴我您想分析哪個專案或哪個目錄的源碼？",
				Options: []userquestions.Option{
					{Label: "當前目錄下的源碼", Description: "當前工作目錄下的所有源碼"},
					{Label: "特定專案的源碼", Description: "特定專案的源碼"},
					{Label: "特定功能的源碼", Description: "特定功能的源碼"},
				},
			}}}, answer: ansCh, err: errCh})

			// 直接数整屏渲染结果的行数——这才是用户看到的最终形态。
			// 此前这里用「按 innerWidth 重新估算折行数」的近似：覆盖层一旦铺满终端宽度，
			// 那种估算会把每行当成两行，得出虚假的溢出结论。真实不变量是 View() 的行数。
			view := m.View().Content
			rows := strings.Split(view, "\n")
			for len(rows) > 0 && strings.TrimSpace(ansi.Strip(rows[len(rows)-1])) == "" {
				rows = rows[:len(rows)-1]
			}
			if len(rows) > high {
				t.Errorf("LAYOUT OVERFLOW: 渲染 %d 行 > 终端 %d 行 → 覆盖层会被裁掉 %d 行",
					len(rows), high, len(rows)-high)
			}
			// 覆盖层必须完整可见：问题与全部选项都在最终视图里
			plain := ansi.Strip(view)
			for _, want := range []string{"請告訴我您想分析哪個專案", "當前目錄下的源碼", "特定專案的源碼", "特定功能的源碼"} {
				if !strings.Contains(plain, want) {
					t.Errorf("high=%d 覆盖层应完整显示 %q", high, want)
				}
			}
		})
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
