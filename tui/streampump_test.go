package tui

import (
	"charm.land/bubbletea/v2"
)

// submitAndDrain 提交一条输入，并把流式帧按 bubbletea 的真实语义泵完。
//
// 这里必须展开 tea.BatchMsg：模型一次返回里可能同时给出「继续泵流」和「点火动画
// tick」两条命令（见 keepAnimAlive），旧写法只取第一条会让泵流断掉——测试于是
// 在「内容永远不会累积」的假状态下断言，反而掩盖问题。
// 动画节拍消息（spinner.TickMsg / elapsedTickMsg）与会话内容无关，且其续链命令
// 带真实延时（tea.Tick），回灌只会让测试白等，故跳过且不续链。
func submitAndDrain(m *Model, text string, maxFrames int) tea.Model {
	var model tea.Model = m
	queue := []tea.Cmd{m.submitCmd(text, nil)}
	frames := 0
	for len(queue) > 0 && frames <= maxFrames {
		cmd := queue[0]
		queue = queue[1:]
		if cmd == nil {
			continue
		}
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			queue = append(queue, batch...)
			continue
		}
		if _, ok := msg.(streamFrame); !ok {
			continue
		}
		frames++
		var next tea.Cmd
		model, next = model.Update(msg)
		if next != nil {
			queue = append(queue, next)
		}
	}
	return model
}

// pumpNextFrame 返回「从当前打开的流里取下一帧」的命令；流未打开时为 nil。
// 供需要在泵流中途插入交互（拖拽/点击）的用例续接。
func pumpNextFrame(m *Model) tea.Cmd { return m.pumpStreamIfOpen() }
