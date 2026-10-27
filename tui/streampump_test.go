package tui

import (
	"testing"

	"charm.land/bubbletea/v2"
	"dsc/core"
)

// submitAndDrain 提交一条输入，并把流式帧按 bubbletea 的真实语义泵完。
//
// 这里必须展开 tea.BatchMsg：模型一次返回里可能同时给出「继续泵流」和「点亮工作行
// 时钟」两条命令（见 armAnim），旧写法只取第一条会让泵流断掉——测试于是
// 在「内容永远不会累积」的假状态下断言，反而掩盖问题。
// 时钟节拍消息（animTickMsg / elapsedTickMsg）与会话内容无关，且其续链命令带真实
// 延时（tea.Tick），回灌只会让测试白等，故跳过且不续链。
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
//
// 先把在途计数清零：串行测试没法表示「一个读正阻塞在通道上」——submitAndDrain
// 停在帧边界时，最后一个泵命令还没被执行，计数会悬空。真实运行里命令一定会被执行
// 并把计数回收，所以这里按「上一泵已完成」继续，与真实语义等价。
func pumpNextFrame(m *Model) tea.Cmd {
	m.pumpInflight = 0
	return m.pumpStreamIfOpen()
}

// TestPumpStreamDoesNotDoubleRead 流式期间按键会顺手调用 pumpStreamIfOpen 想续流。
// 若此时上一个读还阻塞在 <-streamCh 上，再起一个读就成了「双消费者」：两帧的到达
// 顺序不再等于生成顺序，buffer 里被拼成 B+A——用户看到的正是「个别词句语序颠倒」。
func TestPumpStreamDoesNotDoubleRead(t *testing.T) {
	m := newRenderCacheModel(t)
	ch := make(chan *core.RunStreamResponse, 2)
	ch <- &core.RunStreamResponse{Status: "streaming", Output: "A"}
	m.streamCh = ch
	m.streamInput = "hi"

	first := m.pumpStreamIfOpen()
	if first == nil {
		t.Fatal("空闲的流应能起泵")
	}
	if again := m.pumpStreamIfOpen(); again != nil {
		t.Fatal("已有读在途时不得再起第二个读（会造成增量帧乱序）")
	}

	updated, _ := m.Update(first())
	m2 := updated.(*Model)
	// 处理这一帧时先回收一个（1→0），随后链上又起下一个泵（→1）。若回收没发生，
	// 这里会是 2，且每帧都会多一个永不归还的读。
	if m2.pumpInflight != 1 {
		t.Fatalf("泵链应稳定在 1 个在途读，got %d", m2.pumpInflight)
	}
	if again := m2.pumpStreamIfOpen(); again != nil {
		t.Fatal("链上已有泵时再次按键不得另起一个读")
	}
}
