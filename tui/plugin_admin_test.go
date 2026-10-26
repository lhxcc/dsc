package tui

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// TestPluginAdminArgsBuild 每条斜杆子命令都必须映射到宿主**已有的**工具与参数形状：
// 前端只做「拼参数」，不复制校验/备份/热加载逻辑；插件名一律动态枚举。
func TestPluginAdminArgsBuild(t *testing.T) {
	cases := []struct {
		sub    string
		argv   []string
		tool   string
		expect map[string]any
	}{
		{
			sub: "list", argv: nil, tool: "list_dsc_plugins",
			expect: map[string]any{},
		},
		{
			sub: "install", argv: []string{"some-plugin", "D:/tmp/src"}, tool: "install_dsc_plugin",
			expect: map[string]any{"name": "some-plugin", "source": "D:/tmp/src", "type": "tool"},
		},
		{
			sub: "install", argv: []string{"some-plugin", "D:/tmp/src", "--type", "agent"}, tool: "install_dsc_plugin",
			expect: map[string]any{"name": "some-plugin", "source": "D:/tmp/src", "type": "agent"},
		},
		{
			sub: "remove", argv: []string{"some-plugin"}, tool: "uninstall_dsc_plugin",
			expect: map[string]any{"name": "some-plugin"},
		},
		{
			sub: "upgrade", argv: []string{"some-plugin", "2.1.0", "D:/tmp/new.exe"}, tool: "upgrade_dsc_plugin",
			expect: map[string]any{"name": "some-plugin", "version": "2.1.0", "source": "D:/tmp/new.exe"},
		},
		{
			sub: "load", argv: []string{"some-plugin"}, tool: "load_dsc_plugin",
			expect: map[string]any{"name": "some-plugin", "persist": false},
		},
		{
			sub: "load", argv: []string{"some-plugin", "--persist", "--type=dsc"}, tool: "load_dsc_plugin",
			expect: map[string]any{"name": "some-plugin", "persist": true, "type": "dsc"},
		},
		{
			sub: "unload", argv: []string{"some-plugin", "-p"}, tool: "unload_dsc_plugin",
			expect: map[string]any{"name": "some-plugin", "persist": true},
		},
	}
	for _, c := range cases {
		spec, ok := pluginAdminSubs[c.sub]
		if !ok {
			t.Fatalf("子命令 %q 未定义", c.sub)
		}
		if spec.tool != c.tool {
			t.Fatalf("%s 应映射到 %s，实际 %s", c.sub, c.tool, spec.tool)
		}
		pos, opts, err := parsePluginArgs(c.argv)
		if err != nil {
			t.Fatalf("%s 参数解析失败: %v", c.sub, err)
		}
		got, err := spec.build(pos, opts)
		if err != nil {
			t.Fatalf("%s 构造参数失败: %v", c.sub, err)
		}
		if !reflect.DeepEqual(got, c.expect) {
			t.Fatalf("%s(%v) = %#v, want %#v", c.sub, c.argv, got, c.expect)
		}
	}
}

// TestPluginAdminAliases 别名折叠与未知子命令：别名必须命中规范名，未知必须为空
// （空 = 显示用法，而不是偷偷猜一个工具去执行）。
func TestPluginAdminAliases(t *testing.T) {
	for alias, want := range map[string]string{
		"ls": "list", "rm": "remove", "uninstall": "remove",
		"up": "upgrade", "install": "install", "LIST": "list",
	} {
		if got := normalizePluginSub(alias); got != want {
			t.Fatalf("normalize(%q) = %q, want %q", alias, got, want)
		}
	}
	if got := normalizePluginSub("destroy-everything"); got != "" {
		t.Fatalf("未知子命令应归一为空，got %q", got)
	}
}

// TestPluginAdminRejectsBadArgs 缺参数 / 未知选项必须当场报错（不把半截参数发给工具，
// 否则安装工具会去拷一个空路径）。
func TestPluginAdminRejectsBadArgs(t *testing.T) {
	bad := []struct {
		sub  string
		argv []string
	}{
		{"install", []string{"only-name"}},
		{"remove", nil},
		{"upgrade", []string{"name", "1.0.0"}},
		{"load", nil},
		{"install", []string{"n", "src", "--nope"}},
		{"install", []string{"n", "src", "--type"}},
	}
	for _, c := range bad {
		spec := pluginAdminSubs[c.sub]
		pos, opts, err := parsePluginArgs(c.argv)
		if err == nil {
			_, err = spec.build(pos, opts)
		}
		if err == nil {
			t.Fatalf("%s(%v) 应报错", c.sub, c.argv)
		}
	}
}

// TestPluginAdminWithoutManager 管理器不可用时要明确报错，而不是静默什么都不做。
func TestPluginAdminWithoutManager(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	if cmd := m.runPluginAdmin("install", []string{"n", "src"}); cmd != nil {
		t.Fatal("管理器不可用时不应发起命令")
	}
	joined := strings.Join(m.lines, "\n")
	if !strings.Contains(joined, "插件管理器不可用") {
		t.Fatalf("应提示管理器不可用, got %q", joined)
	}
}

// TestPluginAdminHelpListsSubcommands 帮助内容是**动态生成**的（遍历子命令表），
// 所以新增子命令不会忘记写文档；同时不得出现任何具体插件名。
func TestPluginAdminHelpListsSubcommands(t *testing.T) {
	m := New(&stubAgent{}, nil, context.Background(), "m", "minimal", 131072)
	if cmd := m.runPluginAdmin("help", nil); cmd != nil {
		t.Fatal("help 不应执行命令")
	}
	joined := strings.Join(m.lines, "\n")
	for sub := range pluginAdminSubs {
		if !strings.Contains(joined, "/plugin "+sub) {
			t.Fatalf("帮助缺少子命令 %s：%q", sub, joined)
		}
	}
}

// TestRenderPluginAdminResultList 列表渲染：从工具 JSON 动态取行（不依赖任何插件名），
// 输出含名称/类型/状态与计数。
func TestRenderPluginAdminResultList(t *testing.T) {
	raw := `{"plugins":[{"name":"alpha","type":"tool","state":"loaded","enabled":true},` +
		`{"name":"beta","type":"dsc","state":"orphan","enabled":false}],"count":2}`
	out := renderPluginAdminResult(raw)
	for _, want := range []string{"alpha", "beta", "tool", "orphan", "共 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("列表渲染缺少 %q:\n%s", want, out)
		}
	}
	// 非 JSON（工具报错文本）原样返回，不做二次解析
	if got := renderPluginAdminResult("boom: not json"); got != "boom: not json" {
		t.Fatalf("非 JSON 应原样返回, got %q", got)
	}
}
