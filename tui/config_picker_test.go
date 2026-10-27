package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"charm.land/bubbletea/v2"
	"dsc/core"
)

// newConfigTestModel 造一个带真实 Manager（指向临时 config/）的 TUI 模型。
func newConfigTestModel(t *testing.T) (*Model, string) {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfgDir, 0755); err != nil {
		t.Fatal(err)
	}
	active := filepath.Join(cfgDir, "config.yaml")
	if err := os.WriteFile(active, []byte("model: old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	mgr := core.NewManager(&core.ManagerConfig{ExecDir: dir})
	mgr.SetConfigPath(active)
	m := New(&stubAgent{frames: []*core.RunStreamResponse{{Status: "success"}}}, mgr, context.Background(), "m", "minimal", 131072)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m, active
}

// TestConfigPickerPagingAndClamp 翻页与边界：光标始终在页内可见，且不越界。
func TestConfigPickerPagingAndClamp(t *testing.T) {
	items := make([]core.ConfigProfileSummary, 20)
	for i := range items {
		items[i].Name = "config.P" + string(rune('A'+i)) + ".yaml"
	}
	p := &configPicker{items: items, perPage: 8}

	if p.visible() != 8 || p.offset != 0 {
		t.Fatalf("初始页 = offset %d visible %d, want 0/8", p.offset, p.visible())
	}
	p.move(9) // 第 10 条 → 需要滚到 offset=2 才可见
	if p.cursor != 9 || p.offset != 2 {
		t.Fatalf("下移 9 后 cursor=%d offset=%d, want 9/2", p.cursor, p.offset)
	}
	p.move(-100) // 夹到顶部
	if p.cursor != 0 || p.offset != 0 {
		t.Fatalf("上移越界后 cursor=%d offset=%d, want 0/0", p.cursor, p.offset)
	}
	p.move(1000) // 夹到底部
	if p.cursor != len(items)-1 {
		t.Fatalf("下移越界后 cursor=%d, want %d", p.cursor, len(items)-1)
	}
	// 页起点不得大于「最后满页」的起点（否则底部会露出空白）
	if want := len(items) - p.perPage; p.offset != want {
		t.Fatalf("到底后 offset=%d, want %d", p.offset, want)
	}
	// PgUp/PgDn 按页移动
	p.move(-p.perPage)
	if p.cursor != len(items)-1-p.perPage {
		t.Fatalf("PgUp 后 cursor=%d", p.cursor)
	}
}

// TestConfigPickerViewShowsPageAndActive 选单要能看见：当前页范围、总条数、当前生效档。
func TestConfigPickerViewShowsPageAndActive(t *testing.T) {
	m, active := newConfigTestModel(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(active), "config.Same.yaml"), []byte("model: old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(active), "config.Other.yaml"), []byte("model: other\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(active), "config.example.yaml"), []byte("model: tpl\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m.openConfigPicker()
	if m.configPicker == nil {
		t.Fatal("应打开选单")
	}
	v := m.configPickerView()
	for _, want := range []string{"config.Same.yaml", "config.Other.yaml", "当前", "1-2/2"} {
		if !strings.Contains(v, want) {
			t.Fatalf("选单缺少 %q:/n%s", want, v)
		}
	}
	if strings.Contains(v, "example") {
		t.Fatalf("模板不应进选单:\n%s", v)
	}
}

// TestConfigPickerInterceptsKeys 选单激活时按键不得落进输入框（否则用户选的档名
// 会被打进消息里）。
func TestConfigPickerInterceptsKeys(t *testing.T) {
	m, active := newConfigTestModel(t)
	if err := os.WriteFile(filepath.Join(filepath.Dir(active), "config.A.yaml"), []byte("model: a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m.openConfigPicker()
	m.input.SetValue("")
	m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	if got := m.input.Value(); got != "" {
		t.Fatalf("选单激活时按键应被拦截，输入框却得到 %q", got)
	}
	// Esc 收起
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.configPicker != nil {
		t.Fatal("Esc 应收起选单")
	}
}

// TestConfigPickerEnterSwitchesAndQuits 回车选中 → 核心备份并替换 config.yaml →
// 程序退出（配置无法热生效），退出提示带档名与备份路径。
func TestConfigPickerEnterSwitchesAndQuits(t *testing.T) {
	m, active := newConfigTestModel(t)
	dir := filepath.Dir(active)
	if err := os.WriteFile(filepath.Join(dir, "config.DeepSeek.yaml"), []byte("model: deepseek\nplugins: []\n"), 0644); err != nil {
		t.Fatal(err)
	}
	m.openConfigPicker()
	if m.configPicker == nil || m.configPicker.items[0].Name != "config.DeepSeek.yaml" {
		t.Fatalf("选单内容不符: %+v", m.configPicker)
	}

	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("回车应返回切换命令")
	}
	// 执行切换命令 → 得到结果消息
	msg, ok := cmd().(configSwitchMsg)
	if !ok {
		t.Fatalf("命令应返回 configSwitchMsg, got %T", cmd())
	}
	if msg.err != nil {
		t.Fatalf("切换失败: %v", msg.err)
	}
	// 结果消息喂回 Update：应设置退出提示并请求退出
	_, quit := m.Update(msg)
	if m.exitNote == "" || !strings.Contains(m.exitNote, "config.DeepSeek.yaml") {
		t.Fatalf("退出提示缺失: %q", m.exitNote)
	}
	if quit == nil {
		t.Fatal("空闲时应请求退出（重新启动加载新配置）")
	}
	if _, isQuit := quit().(tea.QuitMsg); !isQuit {
		t.Fatal("应返回 tea.Quit")
	}
	// 文件确实换了，且旧内容进了 config-backups/
	got, _ := os.ReadFile(active)
	if string(got) != "model: deepseek\nplugins: []\n" {
		t.Fatalf("config.yaml 未切换: %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "config-backups"))
	if len(entries) != 1 {
		t.Fatalf("应有 1 份备份, got %d", len(entries))
	}
}

// TestConfigPickerBusyDoesNotQuit 回合进行中切换成功也不退出（不能为了换配置丢掉
// 正在进行的工作），只提示重启。
func TestConfigPickerBusyDoesNotQuit(t *testing.T) {
	m, _ := newConfigTestModel(t)
	m.thinking = true
	_, quit := m.Update(configSwitchMsg{name: "config.X.yaml", backup: "/tmp/b.yaml"})
	if quit != nil {
		t.Fatal("回合进行中不应退出")
	}
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "config.X.yaml") || !strings.Contains(joined, "重启") {
		t.Fatalf("应提示已切换且需重启:\n%s", joined)
	}
}

// TestOpenConfigPickerNoProfiles 没有档位时给出可操作的提示，而不是空选单。
func TestOpenConfigPickerNoProfiles(t *testing.T) {
	m, _ := newConfigTestModel(t)
	m.openConfigPicker()
	if m.configPicker != nil {
		t.Fatal("没有档位时不应打开空选单")
	}
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "config.<名称>.yaml") {
		t.Fatalf("应说明如何新增档位:\n%s", joined)
	}
}

// TestOpenConfigPickerWithoutManager 管理器不可用要明确报错。
func TestOpenConfigPickerWithoutManager(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	m.openConfigPicker()
	if m.configPicker != nil {
		t.Fatal("无管理器时不应打开选单")
	}
	if joined := strings.Join(m.lines, "\n"); !strings.Contains(joined, "插件管理器不可用") {
		t.Fatalf("应提示管理器不可用:\n%s", joined)
	}
}

// TestConfigPickerCursorOffset 选单展开时真实光标必须仍在 composer 里：
// viewport 会等量收缩（syncInputHeight → vpHeight），所以光标的**绝对行**不变；
// 若光标算式漏算选单行数，它会上移一个选单高度、正好落在选单第一行上
// （现场：那个闪动方块压在 ▸ 上，第一个条目看起来没有选中标记）。
func TestConfigPickerCursorOffset(t *testing.T) {
	m, active := newConfigTestModel(t)
	dir := filepath.Dir(active)
	for _, n := range []string{"config.A.yaml", "config.B.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("model: x\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	m.input.Focus() // 输入框聚焦才有真实光标（真实运行时打开选单前它一直是聚焦的）
	base := m.inputCursorAbs()
	if base == nil {
		t.Fatal("空闲时应有真实光标")
	}

	m.openConfigPicker()
	if m.configPicker == nil {
		t.Fatal("应打开选单")
	}
	rows := m.configPickerRows()
	after := m.inputCursorAbs()
	if after == nil {
		t.Fatal("选单展开时仍应有真实光标（只是位置要算对）")
	}

	// ① 选单高度必须与真实渲染行数一致，否则下面的判断只是自洽而非正确。
	if n := strings.Count(m.configPickerView(), "\n") + 1; n != rows {
		t.Fatalf("configPickerRows()=%d 与渲染行数 %d 不一致", rows, n)
	}
	// ② composer 位置不变（viewport 已等量收缩）
	if after.Y != base.Y {
		t.Fatalf("光标绝对行变化 %d → %d（viewport 收缩与选单高度不相等）", base.Y, after.Y)
	}
	// ③ 光标必须落在选单**下方**（漏算选单行数时它会上移进选单区）
	pickerBottom := titleRows + m.viewport.Height() + rows
	if after.Y <= pickerBottom {
		t.Fatalf("光标 Y=%d 落在选单区内（选单底行 %d）——这就是「光标错位」", after.Y, pickerBottom)
	}

	// Esc 收起后回到原位
	m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if back := m.inputCursorAbs(); back == nil || back.Y != base.Y {
		t.Fatalf("收起选单后光标未复位: %+v, want Y=%d", back, base.Y)
	}
}

// TestConfigPickerMarkerRendered 选中行要有 ▸ 标记（现场它被错位光标盖住了，
// 看起来像「没有选中项」）。
func TestConfigPickerMarkerRendered(t *testing.T) {
	m, active := newConfigTestModel(t)
	dir := filepath.Dir(active)
	for _, n := range []string{"config.A.yaml", "config.B.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("model: x\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	m.openConfigPicker()
	v := m.configPickerView()
	if strings.Count(v, "▸") != 1 {
		t.Fatalf("选单应有且仅有一个 ▸ 选中标记:\n%s", v)
	}
	// 下移后标记跟着走
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	v2 := m.configPickerView()
	if strings.Count(v2, "▸") != 1 || v == v2 {
		t.Fatalf("下移后标记应移动:\n%s", v2)
	}
}
