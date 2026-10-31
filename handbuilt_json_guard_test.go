package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// handbuiltJSONAllowMarker 是「本行确实需要手拼 JSON 字面量」的唯一豁免标记。
// 豁免必须带理由；允许的场合只有「逐字节必须与对手协议一致」这类，不能因为懒得
// 包一层 Marshal 就用。
const handbuiltJSONAllowMarker = "//dsc:handbuilt-json-ok"

// jsonBuilderCalls 会把手拼的字符串直接变成工具结果 / 网络载荷的调用。
var jsonBuilderCalls = []string{"Sprintf(", "WriteString(", "Fprintf(", "Write("}

// TestNoHandBuiltToolJSON 插件不得手拼 JSON：结构化结果一律交 json.Marshal。
//
// 为什么用测试来守：这类破坏**编译得过、跑得通**，只在特定数据下现形——错误文本
// 里带一个引号、一个换行、或一条 Windows 路径（`C:\Users\...` 的 `\U` 不是合法
// JSON 转义），手拼出来的就是坏 JSON。后果有两层：模型收到半截结构；投影层的
// TOON 归一化（session.toonizeToolContent）对解析失败的内容只能整条放弃、退回
// 原文——省字节的努力全部作废，而「错误信息写得越详细」恰恰越容易踩。
//
// 做法：遍历每个插件（含其 internal 子包）的 .go 源码，找「字符串字面量紧跟调用
// 括号、且以 {" / [{ / [" 开头」的写法。只看 plugins/ 子树：核心里没有工具结果，
// 且 core 已有 filepath 黑名单等更严的守卫。
//
// 边界说明：
//   - 跳过 _test.go：夹具里手写期望 JSON 是数据，不是运行时产出。
//   - 跳过 testdata/temp/vendor/node_modules：非编译产物或第三方。
//   - JSON Schema（json.RawMessage(`{"type":"object"...`)）不会命中：它是字面量
//     直接赋值，没有经过 Sprintf/WriteString，不需要转义任何人给的值。
func TestNoHandBuiltToolJSON(t *testing.T) {
	root := repoRoot(t)
	pluginsDir := filepath.Join(root, "plugins")
	if entries, err := os.ReadDir(pluginsDir); err != nil || len(entries) < 5 {
		t.Fatalf("读取 plugins/ 失败或目录异常（%v）——本测试不能空转通过", err)
	}

	type hit struct {
		file string
		line int
		text string
	}
	var hits []hit

	err := filepath.WalkDir(pluginsDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			if skipPluginScanDir(d.Name()) {
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
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, handbuiltJSONAllowMarker) {
				continue
			}
			if handbuiltJSONLiteral(line) {
				hits = append(hits, hit{file: rel, line: i + 1, text: strings.TrimSpace(line)})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 plugins/ 失败: %v", err)
	}

	if len(hits) > 0 {
		sort.Slice(hits, func(i, j int) bool {
			if hits[i].file != hits[j].file {
				return hits[i].file < hits[j].file
			}
			return hits[i].line < hits[j].line
		})
		var b strings.Builder
		b.WriteString("插件里出现了手拼 JSON——结构化结果必须交 json.Marshal：\n")
		for i, h := range hits {
			if i >= 12 {
				b.WriteString("  …（其余省略）\n")
				break
			}
			b.WriteString("  " + h.file + ":" + itoa(h.line) + "  " + trim(h.text, 110) + "\n")
		}
		b.WriteString("\n原因：值里一旦有引号/换行/反斜杆就是坏 JSON，模型收到半截结构，")
		b.WriteString("\nTOON 归一化也只能整条放弃（退回原文，白费一次压缩）。")
		b.WriteString("\n修法：改成 json.Marshal(map[string]any{...})；确需逐字节控制时在该行加 ")
		b.WriteString(handbuiltJSONAllowMarker + " <理由>。")
		t.Fatal(b.String())
	}
}

// TestHandbuiltJSONDetector 锁定检测器的边界：抓得到手拼、不误伤格式串与 Schema。
func TestHandbuiltJSONDetector(t *testing.T) {
	bad := []string{
		"return fmt.Sprintf(`{\"success\":false,\"error\":\"%s\"}`, err.Error()), nil",
		`return fmt.Sprintf("{\"ok\":%v}", ok), nil`,
		"return fmt.Sprintf(`[{\"id\":%d}]`, id), nil",
		"sb.WriteString(`{\"a\":1}`)",
		"fmt.Fprintf(&b, `{\"msg\":%q}`, m)",
	}
	for _, src := range bad {
		if !handbuiltJSONLiteral(src) {
			t.Errorf("应判为手拼 JSON: %s", src)
		}
	}

	good := []string{
		`b, _ := json.Marshal(map[string]any{"success": false})`,
		`return fmt.Sprintf("%s: %d", name, n), nil`,
		`return fmt.Sprintf("{%d,%d}", x, y), nil`, // 不是 JSON（无引号键）
		"Schema: json.RawMessage(`{\"type\":\"object\"}`)",
		`fmt.Fprintf(&b, "  %s — %s\n", r.name, r.desc)`,
		`sb.WriteString("已连接")`,
		`return failResult(fmt.Sprintf("未找到元素 '%s': %s", sel, err.Error())), nil`,
	}
	for _, src := range good {
		if handbuiltJSONLiteral(src) {
			t.Errorf("不应判为手拼 JSON: %s", src)
		}
	}
}

// handbuiltJSONLiteral 报告该行是否出现「字符串字面量以 JSON 结构开头」的拼接调用。
//
// 判据是字面量内容的前两个字符：`{"`（对象）、`[{` / `["`（数组）。`{%`、`{%d`
// 这类是格式串占位，不算；JSON Schema 走 json.RawMessage 直接赋值，不经过这些
// 调用，也不会命中。
func handbuiltJSONLiteral(line string) bool {
	for _, call := range jsonBuilderCalls {
		idx := strings.Index(line, call)
		if idx < 0 {
			continue
		}
		lit, ok := firstStringLiteral(line[idx+len(call):])
		if !ok {
			continue
		}
		lit = strings.TrimLeft(lit, " \t")
		if strings.HasPrefix(lit, `{"`) || strings.HasPrefix(lit, `[{`) || strings.HasPrefix(lit, `["`) {
			return true
		}
	}
	return false
}

// firstStringLiteral 取参数里第一个字符串字面量的**内容**（识别双引号与反引号，
// 双引号内容按 Go 规则还原 \" 之类的转义——否则 `"{\"ok\":1}"` 这种手拼写法漏检）。
func firstStringLiteral(s string) (string, bool) {
	i := 0
	for i < len(s) && s[i] != '"' && s[i] != '`' {
		if s[i] == ')' { // 调用括号已闭合：没有字面量参数
			return "", false
		}
		i++
	}
	if i >= len(s) {
		return "", false
	}
	quote := s[i]
	i++
	var b strings.Builder
	for i < len(s) {
		c := s[i]
		if quote == '"' && c == '\\' && i+1 < len(s) {
			b.WriteByte(s[i+1]) // \" -> "、\\ -> \；检测只看开头两个字符，够用
			i += 2
			continue
		}
		if c == quote {
			return b.String(), true
		}
		b.WriteByte(c)
		i++
	}
	return b.String(), true // 单行被截断（多行字面量）：已收集到的部分同样可判
}

// skipPluginScanDir 报告扫描插件源码时整目录跳过的名字。
func skipPluginScanDir(name string) bool {
	switch name {
	case "testdata", "temp", "vendor", "node_modules", "fonts", "dist":
		return true
	}
	return strings.HasPrefix(name, ".")
}
