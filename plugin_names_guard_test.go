package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// allowMarker 是「本文件确实需要写出插件名」的唯一豁免标记。
//
// 豁免必须带理由，且只能出现在**构建期工具**里（打包清单、单插件的预置脚本）。
// 运行时路径（core / tui / sdk / agent 侧）没有豁免——那里一旦出现插件名，就说明
// 某种能力被硬编码成了具体实现，而不是经能力机制接入。
const allowMarker = "//dsc:plugin-names-ok"

// TestNoPluginNamesOutsidePlugins 插件是外部能力：核心里出现插件名就是错。
//
// 为什么用测试来守：这类违规**编译得过、跑得通**，只是把耦合悄悄埋下（改插件名
// 要改核心、卸载插件代码还在）。人工 review 会漏，所以让它在 CI/回归里直接失败。
//
// 做法（按用户要求）：动态读取 plugins/ 下所有目录名作为「禁用词」，再遍历仓库中
// 的 Go 源码（排除 plugins/ 自身，否则等于插件自己引用自己，递归假阳性）；命中
// 即报错，并打印 文件:行 与前缀，便于定位。
//
// 边界说明：
//   - 只看 .go 源码：config/*.yaml 与 presets 本来就是「按名选择后端」的地方，
//     文档（.md）是散文，把它们也纳入会把合法用法判成违规。
//   - 跳过 _test.go：测试用真实插件名当夹具，模拟的是「真实插件在场」的场景，
//     属于数据而非运行时接线（本测试自身也不扫描自己）。
//   - 目录 .git / libs / dist / .workbuddy 剔除：版本库、第三方、构建产物。
func TestNoPluginNamesOutsidePlugins(t *testing.T) {
	root := repoRoot(t)
	names := pluginDirNames(t, filepath.Join(root, "plugins"))
	if len(names) < 5 {
		t.Fatalf("只找到 %d 个插件目录（%v）——路径解析可能有问题，本测试不能空转通过", len(names), names)
	}

	type hit struct {
		file string
		line int
		name string
		text string
	}
	var hits []hit
	var badMarkers []string       // 只写标记不写理由
	exempt := map[string]string{} // 文件 → 豁免理由

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 权限问题等直接跳过，不让测试因此失败
		}
		if d.IsDir() {
			if skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)

		if present, reason := markerReason(string(data)); present {
			if len([]rune(reason)) < 6 {
				badMarkers = append(badMarkers, rel)
				return nil
			}
			exempt[rel] = reason
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, name := range names {
				if strings.Contains(line, name) {
					hits = append(hits, hit{file: rel, line: i + 1, name: name, text: strings.TrimSpace(line)})
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历仓库失败: %v", err)
	}

	if len(badMarkers) > 0 {
		t.Fatalf("以下文件申请了豁免但没有写理由（%s <理由>）：%v", allowMarker, badMarkers)
	}

	if len(hits) > 0 {
		// 按文件聚合打印，最多 12 条，避免刷屏
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].file != hits[j].file {
				return hits[i].file < hits[j].file
			}
			return hits[i].line < hits[j].line
		})
		var b strings.Builder
		b.WriteString("核心/宿主代码中出现了插件名——插件是外部能力，必须经能力机制接入：\n")
		for i, h := range hits {
			if i >= 12 {
				b.WriteString("  …（其余省略）\n")
				break
			}
			b.WriteString("  " + h.file + ":" + itoa(h.line) + "  「" + h.name + "」  " + trim(h.text, 90) + "\n")
		}
		b.WriteString("\n修法：改成「找声明了某能力的插件」或走声明式通道（命令、事件、metadata）；")
		b.WriteString("\n确需在构建期工具里枚举插件目录时，在该文件里加 " + allowMarker + " <理由>。")
		t.Fatal(b.String())
	}

}

// repoRoot 由本文件的编译期路径推出仓库根（测试运行目录不一定是根）。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件路径")
	}
	return filepath.Dir(file)
}

// pluginDirNames 读取 plugins/ 下的目录名（即插件标识），排序后返回。
func pluginDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 plugins 目录失败: %v", err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// 下划线开头视为非插件目录（占位、草稿）。
		if strings.HasPrefix(e.Name(), "_") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// skipDir 报告遍历时是否整目录跳过。
func skipDir(name string) bool {
	switch name {
	case ".git", ".workbuddy", "libs", "dist", "plugins", "node_modules":
		return true
	}
	return strings.HasPrefix(name, ".") // .idea/.vscode 等
}

// markerReason 报告文件是否申请了豁免，以及标记后的理由文本。
//
// 分两件事返回是有意的：只有「标记存在但理由为空」才是要报的错；
// 用空串表示「无标记」会把这种后门当成普通文件放过去。
func markerReason(content string) (present bool, reason string) {
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, allowMarker) {
			return true, strings.TrimSpace(strings.TrimPrefix(t, allowMarker))
		}
	}
	return false, ""
}

// trim 截断过长文本，便于一行展示。
func trim(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// itoa 避免为一个整数引入额外依赖。
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
