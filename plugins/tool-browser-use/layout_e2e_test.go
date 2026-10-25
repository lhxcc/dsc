package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
)

// layoutSampleHTML 是一个「排版全靠 CSS」的最小页面：flex 导航、flex 卡片、表格、
// 带内联链接的段落。用它验证取文能保住视觉行——innerText 会把前三者拆成一词一行。
const layoutSampleHTML = `<!doctype html><html><head><meta charset="utf-8"><style>
body{font-family:sans-serif;font-size:16px;margin:0}
.nav{display:flex;gap:24px;padding:12px}
.card{display:flex;gap:16px;padding:12px}
table{border-collapse:collapse}td,th{border:1px solid #ccc;padding:6px}
</style></head><body>
<div class="nav"><a href="#">首页</a><a href="#">产品</a><a href="#">关于我们</a><a href="#">联系方式</a></div>
<div class="card"><span>价格</span><span>¥199</span><span>库存</span><span>现货</span></div>
<table><tr><th>姓名</th><th>年龄</th><th>城市</th></tr>
<tr><td>张三</td><td>28</td><td>广州</td></tr>
<tr><td>李四</td><td>31</td><td>深圳</td></tr></table>
<p>这是一段比较长的说明文字，里面嵌了一个<a href="#">内联链接</a>，用来验证自动换行之后每一行是否还落在原来的位置，而不是把整段挤成一行、或者反过来把整段拆散。</p>
</body></html>`

// TestLayoutOnRealPage 真浏览器端到端：DOM 测量 + Go 排版合起来是否保住版面。
//
// 需要本机 chromium，默认跳过（CI 上没有浏览器）。要跑：
//
//	DSC_BROWSER_E2E=1 [DSC_BROWSER_BIN=<chrome 路径>] go test -run TestLayoutOnRealPage -v
func TestLayoutOnRealPage(t *testing.T) {
	if os.Getenv("DSC_BROWSER_E2E") != "1" {
		t.Skip("需要真实浏览器：设置 DSC_BROWSER_E2E=1 后运行")
	}

	dir := t.TempDir()
	htmlPath := filepath.Join(dir, "sample.html")
	if err := os.WriteFile(htmlPath, []byte(layoutSampleHTML), 0o644); err != nil {
		t.Fatal(err)
	}

	l := launcher.New().Headless(true).Leakless(false)
	if bin := os.Getenv("DSC_BROWSER_BIN"); bin != "" {
		l = l.Bin(bin)
	}
	u, err := l.Launch()
	if err != nil {
		t.Fatalf("启动浏览器失败（可用 DSC_BROWSER_BIN 指定路径）: %v", err)
	}
	browser := rod.New().ControlURL(u)
	if err := browser.Connect(); err != nil {
		t.Fatalf("连接浏览器: %v", err)
	}
	defer browser.MustClose()

	page := browser.MustPage("file:///" + filepath.ToSlash(htmlPath))
	defer page.MustClose()
	page.MustWaitLoad()
	// 窄视口：逼出段落自动换行，覆盖「按行框切分文本」那条路径
	page.MustSetViewport(420, 800, 1, false)
	_ = page.WaitStable(300 * 1e6)

	out, ok := renderLayout(page.MustEval(layoutExtractJS).Str())
	if !ok {
		t.Fatal("真实页面上排版失败（应退回 innerText 的路径被触发了）")
	}
	lines := strings.Split(out, "\n")

	lineOf := func(sub string) int {
		for i, ln := range lines {
			if strings.Contains(ln, sub) {
				return i
			}
		}
		t.Fatalf("输出里找不到 %q：\n%s", sub, out)
		return -1
	}

	// 1. flex 导航：四个链接必须在同一行
	if got := []int{lineOf("首页"), lineOf("产品"), lineOf("关于我们"), lineOf("联系方式")}; got[0] != got[1] || got[1] != got[2] || got[2] != got[3] {
		t.Fatalf("导航应保持一行，实际分散在多行：%v\n%s", got, out)
	}
	// 2. flex 卡片：字段与值同一行
	if lineOf("价格") != lineOf("¥199") || lineOf("库存") != lineOf("现货") {
		t.Fatalf("卡片字段与值应在同一行：\n%s", out)
	}
	// 3. 表格：表头三列同行，三行数据各自成行且列对齐
	if lineOf("姓名") != lineOf("年龄") || lineOf("年龄") != lineOf("城市") {
		t.Fatalf("表头应在一行：\n%s", out)
	}
	if lineOf("张三") != lineOf("28") || lineOf("28") != lineOf("广州") {
		t.Fatalf("同一行数据应在一行：\n%s", out)
	}
	if lineOf("张三") == lineOf("姓名") || lineOf("李四") == lineOf("张三") {
		t.Fatalf("不同数据行不能合并：\n%s", out)
	}
	header, row := lines[lineOf("姓名")], lines[lineOf("张三")]
	col := func(line, sub string) int {
		i := strings.Index(line, sub)
		if i < 0 {
			t.Fatalf("%q 里没有 %q", line, sub)
		}
		return cellWidth(line[:i]) // 按显示列比：汉字占两列
	}
	if a, b := col(header, "年龄"), col(row, "28"); a != b {
		t.Fatalf("第二列未对齐（年龄@%d, 28@%d）：\n%q\n%q", a, b, header, row)
	}
	if a, b := col(header, "城市"), col(row, "广州"); a != b {
		t.Fatalf("第三列未对齐（城市@%d, 广州@%d）：\n%q\n%q", a, b, header, row)
	}
	// 4. 被自动换行的段落：拆成多行（不是压成一行），内联链接与它前面那句话在同一行
	if first, last := lineOf("这是一段比较长的说明文字"), lineOf("整段挤成一行"); last-first < 2 {
		t.Fatalf("窄视口下段落应换成多行，实际只占 %d 行：\n%s", last-first+1, out)
	}
	if link, head := lineOf("内联链接"), lineOf("这是一段比较长的说明文字"); link != head {
		t.Fatalf("内联链接应留在它视觉所在的段落首行：link=%d head=%d\n%s", link, head, out)
	}
}
