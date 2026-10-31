package toon_test

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/toon-format/toon-go"
)

// decodeNestedHeader 解一份「深度 n 的嵌套字段组」：a[1]{x{x{…x{y}…}}}: + 一行数据。
func decodeNestedHeader(t *testing.T, n int) {
	t.Helper()
	in := "a[1]{" + strings.Repeat("x{", n) + "y" + strings.Repeat("}", n) + "}:\n  1"
	if _, err := toon.Decode([]byte(in), toon.WithStrictMode(true)); err != nil {
		t.Fatalf("depth=%d 应能解析: %v", n, err)
	}
}

// TestNestedFieldGroupDepthIsLinear 嵌套字段组的解析代价必须与输入长度成正比。
//
// 旧实现对每一层都重新扫描/复制「剩余整段 header」：外层切分 O(n)、每次递归的子层
// 又各来一遍，总计 O(depth²)——24 KB / 8000 层要 1.5s、分配 815 MB，一份几十 KB 的
// 文本就能把进程拖垮（toon-format/toon-go#33）。这类文本来自不可信调用方，所以是
// 一个现成的 CPU/内存拒绝服务点。
//
// 断言用**分配量之比**而不是绝对耗时：机器快慢不影响内存，二次与线性在这里差 4 倍
// 以上，阈值 8 留足余量（线性实测比值约 5，旧实现约 19）。
func TestNestedFieldGroupDepthIsLinear(t *testing.T) {
	allocFor := func(n int) uint64 {
		in := "a[1]{" + strings.Repeat("x{", n) + "y" + strings.Repeat("}", n) + "}:\n  1"
		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		if _, err := toon.Decode([]byte(in), toon.WithStrictMode(true)); err != nil {
			t.Fatalf("depth=%d 应能解析: %v", n, err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	small := allocFor(2000)
	large := allocFor(8000) // 深度 ×4
	if small == 0 {
		t.Fatal("分配量测不到，断言失效")
	}
	if ratio := float64(large) / float64(small); ratio > 8 {
		t.Fatalf("深度 ×4 时分配量涨了 %.1f 倍（线性应约 4，二次约 16）：深度 2000 用 %d KB，8000 用 %d KB",
			ratio, small>>10, large>>10)
	}
	// 绝对上限：8000 层（24 KB 输入）不该吃掉几十 MB 以上。
	if large > 100<<20 {
		t.Fatalf("8000 层分配 %d MB，超出合理范围", large>>20)
	}
}

// TestNestedFieldGroupDeepEnoughToBeUsable 深度上千层仍能正确解出结构，且不会栈溢出。
func TestNestedFieldGroupDeepEnoughToBeUsable(t *testing.T) {
	const depth = 5000
	start := time.Now()
	decodeNestedHeader(t, depth)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("depth=%d 用了 %v（应在毫秒级）", depth, elapsed)
	}
}

// TestNestedFieldGroupRejectsMalformed 结构错误仍要被拒绝，不能被线性化顺手放过。
func TestNestedFieldGroupRejectsMalformed(t *testing.T) {
	cases := []string{
		"a[1]{x{y}:",     // 括号未闭合
		"a[1]{x{y}}}:",   // 多余右括号
		"a[1]{x{y}z}:",   // 嵌套组后面还有内容
		"a[1]{x,}:",      // 尾随分隔符 → 空 entry
		"a[1]{,x}:",      // 首部分隔符 → 空 entry
		"a[1]{x{\"y}:",   // 引号未闭合
		"a[1]{x{a,b}c}:", // 嵌套组后跟非分隔符
	}
	for _, in := range cases {
		if _, err := toon.DecodeString(in); err == nil {
			t.Errorf("应拒绝畸形输入 %q", in)
		}
	}
}
