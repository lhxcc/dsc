package tui

import (
	"fmt"
	"strings"

	"charm.land/bubbletea/v2"
	"dsc/core"
)

// /config 的配置档选单。
//
// 与 /mode 的区别：/mode 切的是 preset（config/presets/<mode>.yaml，只换 tool/policy
// 插件子集，可即时生效）；这里切的是**整份 config.yaml**（provider、插件表、窗口、
// 会话目录……）。整份配置无法安全热生效——新 provider 没装配、旧插件进程还在跑，
// 热替换只会得到一个半新半旧的宿主。所以：选单只负责选择，核心负责「备份 + 原子
// 替换」，随后本进程退出，由用户重新启动加载新配置（提示经 Run 返回，main 打印）。

// configPickerPerPage 选单一页显示的条数（其余用 PgUp/PgDn 翻页）。
const configPickerPerPage = 8

// configPicker 选单状态。
type configPicker struct {
	items   []core.ConfigProfileSummary
	cursor  int // 绝对下标（0 基）
	offset  int // 当前页起始下标
	perPage int
}

// move 移动光标并保证它在当前页内可见。
func (p *configPicker) move(d int) {
	p.cursor += d
	if p.cursor < 0 {
		p.cursor = 0
	}
	if p.cursor > len(p.items)-1 {
		p.cursor = len(p.items) - 1
	}
	p.follow()
}

// follow 按光标位置滚动页起点（翻页与逐条移动共用一处边界逻辑）。
func (p *configPicker) follow() {
	if p.cursor < p.offset {
		p.offset = p.cursor
	}
	if p.cursor >= p.offset+p.perPage {
		p.offset = p.cursor - p.perPage + 1
	}
	if max := len(p.items) - p.perPage; p.offset > max {
		p.offset = max
	}
	if p.offset < 0 {
		p.offset = 0
	}
}

// visible 返回本页应显示的条目数。
func (p *configPicker) visible() int {
	n := len(p.items) - p.offset
	if n > p.perPage {
		n = p.perPage
	}
	if n < 0 {
		n = 0
	}
	return n
}

// openConfigPicker 打开选单（列表完全由磁盘决定：新增一个 config.<x>.yaml 就出现）。
func (m *Model) openConfigPicker() {
	if m.manager == nil {
		m.appendMessage(errorSty.Render("錯誤: 插件管理器不可用"))
		return
	}
	items, err := m.manager.ListConfigProfiles()
	if err != nil {
		m.appendMessage(errorSty.Render("读取配置档失败: ") + err.Error())
		return
	}
	if len(items) == 0 {
		m.appendMessage(assistantNameSty.Render(assistantMark+" DSC · 配置") +
			"\n未找到可切换的配置档。把另一份配置放在 config/ 下、命名为 config.<名称>.yaml 即可" +
			"（config.example.yaml 这类模板会被忽略）。")
		return
	}
	m.configPicker = &configPicker{items: items, perPage: configPickerPerPage}
	m.syncInputHeight()
	m.render()
}

// closeConfigPicker 收起选单（Esc / 切换后）。
func (m *Model) closeConfigPicker() {
	if m.configPicker == nil {
		return
	}
	m.configPicker = nil
	m.syncInputHeight()
	m.render()
}

// handleConfigPickerKey 选单激活时接管按键（不能落到输入框/其他快捷键）。
func (m *Model) handleConfigPickerKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	p := m.configPicker
	switch msg.String() {
	case "esc", "ctrl+c":
		m.closeConfigPicker()
		return m, nil
	case "up", "k", "ctrl+p":
		p.move(-1)
	case "down", "j", "ctrl+n":
		p.move(1)
	case "pgup", "ctrl+u":
		p.move(-p.perPage)
	case "pgdown", "ctrl+d":
		p.move(p.perPage)
	case "home":
		p.cursor = 0
		p.follow()
	case "end":
		p.cursor = len(p.items) - 1
		p.follow()
	case "enter":
		name := p.items[p.cursor].Name
		m.closeConfigPicker()
		return m, m.switchConfigProfileCmd(name)
	}
	m.render()
	return m, nil
}

// switchConfigProfileCmd 执行切换（备份 + 原子替换）——文件 I/O 放后台，别卡界面。
func (m *Model) switchConfigProfileCmd(name string) tea.Cmd {
	manager := m.manager
	return func() tea.Msg {
		backup, err := manager.SwitchConfigProfile(name)
		return configSwitchMsg{name: name, backup: backup, err: err}
	}
}

// configSwitchMsg 切换结果（tea.Cmd → 事件循环）。
type configSwitchMsg struct {
	name   string
	backup string
	err    error
}

// configPickerRows 选单占用的终端行数（viewport 必须等量收缩，否则覆盖层被截断）。
func (m *Model) configPickerRows() int {
	p := m.configPicker
	if p == nil {
		return 0
	}
	// 标题行 + 可见条目 + 页码行 + 上下边框
	return p.visible() + 4
}

// configPickerView 渲染选单：当前档标「当前」，并给出字节数/修改时间便于辨认。
func (m *Model) configPickerView() string {
	p := m.configPicker
	if p == nil {
		return ""
	}
	end := p.offset + p.visible()
	var b strings.Builder
	b.WriteString(accentSty.Render("选择配置档"))
	b.WriteString(dimSty.Render("   ↑↓ 选择 · PgUp/PgDn 翻页 · Enter 切换 · Esc 取消"))
	for i := p.offset; i < end; i++ {
		it := p.items[i]
		mark := "  "
		if i == p.cursor {
			mark = "▸ "
		}
		line := mark + it.Name + dimSty.Render(fmt.Sprintf("  %s  %s", humanBytes(it.Size), it.Modified.Format("01-02 15:04")))
		if it.Active {
			line += dimSty.Render("  当前")
		}
		if i == p.cursor {
			line = accentSty.Render(line)
		}
		b.WriteString("\n" + line)
	}
	b.WriteString("\n" + dimSty.Render(fmt.Sprintf("%d-%d/%d", p.offset+1, end, len(p.items))))
	return questionBoxSty.Render(b.String())
}

// humanBytes 把字节数压成短标签（KB/MB），只用于列表展示。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
