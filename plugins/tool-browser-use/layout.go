package main

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

// 网页取文的「版面还原」。
//
// 背景：`document.body.innerText` 是按 **DOM 的块级结构** 断行的，不是按视觉位置。
// 现代页面大量用 flex/grid 排版，视觉上并排的导航项、表格单元格、卡片字段在 DOM 里
// 是各自独立的块，于是 innerText 把它们一个词一行地竖着排下来——模型读到的是一根
// 细长的柱状文本，丢失了「谁和谁在同一行」这个最重要的结构信息（表格尤其致命）。
//
// 做法分两步，故意拆开：
//
//  1. JS（layoutExtractJS）只做「测量」：遍历文本节点，量出每段文字在视口里的
//     矩形（x/y/宽/高），连同页面字体的单字符宽度一起 JSON 回来。被自动换行拆成
//     多行的文本节点按各行框宽度比例切分，使同一段里嵌的链接也能落回原来那一行。
//  2. Go（renderLayout）只做「排版」：把垂直重叠的文本块聚成视觉行，行内按 x 排序、
//     按列位补空格，还原成「同一行的东西还在同一行、列大致对齐」的文本。
//
// 之所以把排版放在 Go：它是纯函数、可单元测试（见 layout_test.go），而 JS 里的
// 布局代码在 CI 里没法跑。JS 里的测量逻辑保持尽量笨，出问题也只影响测量精度。
const layoutExtractJS = `() => {
  const body = document.body;
  if (!body) return '';

  const MAX_ITEMS = 20000;
  // 退化矩形要丢掉：视觉隐藏的「跳到内容」链接、1px 占位块被逐字拆成一行一个字，
  // 白占版面还会污染输出。正常的文字行高至少 6px。
  const visible = (r) => r && r.width >= 2 && r.height >= 6;
  const norm = (s) => s.replace(/\s+/g, ' ').trim();

  // 一个字符大约多宽：用页面自己的字体量一次，把像素 x 换算成「列」。
  // 量不出来就退回 8px（16px 正文的常见值），只影响对齐精度，不影响取舍。
  let charW = 8;
  try {
    const bs = getComputedStyle(body);
    const probe = document.createElement('span');
    probe.textContent = 'xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx';
    probe.style.position = 'absolute';
    probe.style.left = '-99999px';
    probe.style.top = '0';
    probe.style.whiteSpace = 'pre';
    probe.style.fontFamily = bs.fontFamily;
    probe.style.fontSize = bs.fontSize;
    body.appendChild(probe);
    const w = probe.getBoundingClientRect().width / probe.textContent.length;
    if (w > 0.5 && w < 60) charW = w;
    probe.remove();
  } catch (e) { /* 量不到就用默认值 */ }

  const skipTags = {
    SCRIPT: 1, STYLE: 1, NOSCRIPT: 1, TEMPLATE: 1, SVG: 1, CANVAS: 1,
    IFRAME: 1, HEAD: 1, META: 1, LINK: 1, TITLE: 1
  };
  // 语义化的页面外壳：导航、侧栏、页脚里的链接通常是全站复用的噪声，取正文时跳过
  const skipRoles = { navigation: 1, banner: 1, contentinfo: 1, complementary: 1 };
  const isChrome = function (el) {
    const tag = el.tagName;
    if (tag === 'NAV' || tag === 'ASIDE' || tag === 'FOOTER') return true;
    const role = el.getAttribute && el.getAttribute('role');
    return !!(role && skipRoles[role]);
  };

  // 视口左右之外的内容一律丢掉：横向轮播会把其它「帧」里的副本排在 x=几千的位置，
  // 它们既不可见、又会在同一视觉行里被排到最右边，把一行撑成几百字。
  const innerW = window.innerWidth || 1280;
  const cols = Math.round(innerW / charW);

  // 判断这个文本节点是不是「接着上一个内联片段写的」：仅在续接时才允许直接拼接。
  // 依据：沿祖先链找到前一个兄弟（跳过内联包装元素如 <b>/<span>），且两侧都没有空白。
  const prevInlineNode = function (n) {
    let cur = n;
    while (cur) {
      if (cur.previousSibling) return cur.previousSibling;
      cur = cur.parentElement;
      if (!cur || cur === body) return null;
      const cs2 = getComputedStyle(cur);
      if (cs2.display.indexOf('inline') !== 0) return null; // 块边界：不算续接
    }
    return null;
  };

  // 排布容器：同一容器（flex/grid 行、表格行、或最近的块级祖先）内的元素才是「同一行的
  // 单元格」；正文与侧栏属于不同容器，永远不会被并到同一行。用 DOM 顺序编号（首次出现）。
  const groupOf = new Map();
  let nextGroup = 0;
  const assign = function (el) {
    if (!groupOf.has(el)) groupOf.set(el, ++nextGroup);
    return groupOf.get(el);
  };
  // 排布容器：同一容器里的元素才可能是「同一行的单元格」；正文与侧栏属于不同容器，
  // 即使在同一高度也绝不被并成一行（并了就是病句）。判定：
  //   1) 语义上就是行容器的祖先：flex / grid / table / table-row；
  //   2) 否则取最近的块级祖先（一个 <p>、一个 <li>、一个 <div> 都算一个容器）。
  // 宁可容器切细一点（多几行），也不要跨容器合并——多一行只是啰嗦，合并错是错的。
  const containerOf = function (el) {
    if (groupOf.has(el)) return groupOf.get(el);
    let cur = el, blockAncestor = el;
    while (cur && cur !== body) {
      if (groupOf.has(cur)) return groupOf.get(cur);
      const cs = getComputedStyle(cur);
      const d = cs.display;
      if (d.indexOf('flex') >= 0 || d.indexOf('grid') >= 0 || d === 'table' || d === 'table-row') {
        return assign(cur);
      }
      if (d !== 'inline' && d !== 'inline-block' && d !== 'contents') blockAncestor = cur;
      cur = cur.parentElement;
    }
    return assign(blockAncestor);
  };

  const items = [];
  const walker = document.createTreeWalker(body, NodeFilter.SHOW_TEXT, null);
  let node;
  while ((node = walker.nextNode())) {
    if (items.length >= MAX_ITEMS) break;
    const text = node.nodeValue;
    if (!text || !text.trim()) continue;
    const el = node.parentElement;
    if (!el || skipTags[el.tagName]) continue;
    let chrome = false;
    for (let p = el; p && p !== body; p = p.parentElement) {
      if (isChrome(p)) { chrome = true; break; }
    }
    if (chrome) continue;

    let cs;
    try { cs = getComputedStyle(el); } catch (e) { continue; }
    if (cs.display === 'none' || cs.visibility === 'hidden' || cs.opacity === '0') continue;

    const group = containerOf(el);
    let cont = false;
    try {
      const prev = prevInlineNode(node);
      if (prev) {
        const prevText = prev.textContent || '';
        cont = !!prevText.trim() && !/\s$/.test(prevText) && !/^\s/.test(text);
      }
    } catch (e) { cont = false; }

    const range = document.createRange();
    range.selectNodeContents(node);
    const rects = Array.prototype.filter.call(range.getClientRects(), visible)
      .filter(function (r) { return r.left < innerW && r.left + r.width > 0; });
    if (!rects.length) continue;

    if (rects.length === 1) {
      const t = norm(text);
      if (t) items.push({ t: t, g: group, x: rects[0].left, y: rects[0].top, w: rects[0].width, h: rects[0].height, c: cont });
      continue;
    }

    // 被自动换行拆成多行：按每行宽度占比切分文本，并尽量在空格处收边，
    // 这样同一段里嵌的链接 / 旁边的单元格都还能落回它原来那一行。
    const total = rects.reduce(function (s, r) { return s + r.width; }, 0) || 1;
    let start = 0;
    for (let i = 0; i < rects.length; i++) {
      const r = rects[i];
      const last = i === rects.length - 1;
      let end = last
        ? text.length
        : Math.min(text.length, start + Math.round(text.length * (r.width / total)));
      if (!last && end < text.length) {
        const cut = text.indexOf(' ', end - 1);
        if (cut >= 0 && cut - end < 12) end = cut; // 就近断在空格，避免切词
      }
      const t = norm(text.slice(start, end));
      if (t) items.push({ t: t, g: group, x: r.left, y: r.top, w: r.width, h: r.height, c: cont });
      start = Math.max(start, end);
    }
  }

  return JSON.stringify({ charW: charW, cols: cols, items: items });
}`

// maxMergeCell 「可以并到同一行」的单块最大宽度（列）。
//
// 一行的意义取决于它是「同类单元格」还是「正文与侧栏恰好同高」：导航项、表格单元格、
// 卡片字段都是短块，并成一行既忠实又好读；而正文段落的一行旁边往往还有侧栏、目录、
// 信息框，硬按 y 并起来会把互不相干的句子接成一句（维基百科这类页面尤其明显）。
// 所以只有**整行都是窄块**时才合并，否则各块独占一行——退化成常规取文，不会更糟。
const maxMergeCell = 60

// maxColumnGap 行内两块之间的最大补白（列）。测量侧已把视口外与超出页宽的块裁掉，
// 这里只是兜底：绝对定位、浮动元素仍可能算出离谱的 x，不能让一行被撑成巨长。
const maxColumnGap = 80

// inlineGap 判定「两块本来就连着」的像素容差（以字符宽度为单位）。
// 一个词被 <b>/<span> 拆成两个文本节点时，两块之间几乎没有空白；而页面上相邻的
// 两个栏目之间总有栏距。用像素间隙而不是列位来判断，两种情况才分得开。
const inlineGap = 0.5

// layoutPage 是 layoutExtractJS 的产物：单字符像素宽 + 按视觉行收集到的文本块。
type layoutPage struct {
	CharW float64      `json:"charW"`
	Cols  int          `json:"cols"` // 视口宽度折合多少列；0 表示不限
	Items []layoutItem `json:"items"`
}

// layoutItem 一个文本块在视口里的位置（像素，视口坐标）。
type layoutItem struct {
	Text  string  `json:"t"`
	Group int     `json:"g"` // 所属排布容器（测量侧编号，首次出现顺序即阅读顺序）
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	W     float64 `json:"w"`
	H     float64 `json:"h"`
	Cont  bool    `json:"c"` // 续接前一段（同一个词被 <b>/<span> 拆开）
}

// renderLayout 把测量结果还原成「保持视觉行」的文本。第二个返回值表示是否可用：
// 页面结构异常（空文档、脚本被禁、JSON 被截断）时返回 false，调用方退回 innerText。
func renderLayout(payload string) (string, bool) {
	var page layoutPage
	if err := json.Unmarshal([]byte(payload), &page); err != nil || len(page.Items) == 0 {
		return "", false
	}
	charW := page.CharW
	if charW <= 0 {
		charW = 8
	}

	// 顺序沿用测量时的 **DOM 顺序**，不按 y 重排。
	//
	// DOM 顺序就是作者的阅读顺序：正文的行、侧栏的项各自连续出现。若按 y 排序再按行
	// 合并，正文某一行会和同一高度上的侧栏/目录项并排，两句互不相干的话被接成一句
	// （维基百科这类「正文 + 信息框」的页面会整段读不懂）。DOM 顺序下，行内合并只发生
	// 在**紧邻的两块**之间，跨容器的东西不会撞在一起。
	items := make([]layoutItem, 0, len(page.Items))
	for _, it := range page.Items {
		if strings.TrimSpace(it.Text) == "" || isSeparatorOnly(it.Text) {
			continue
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return "", false
	}

	// 整页最左点作为列基准：这样既对齐了相对位置，又不会让每行都顶着一截缩进
	minX := items[0].X
	for _, it := range items {
		if it.X < minX {
			minX = it.X
		}
	}
	// 列位量化：同一列在不同行里的 x 会有 1~2px 的抖动（表格边框、padding 取整），
	// 直接 round(x/charW) 会在临界点把同一列算成两列。先把全页的 x 归并成「列带」，
	// 每带取中位数，再按带换算列位——同一列的所有块因此落到同一列。
	colOfX := buildColumnSnap(items, minX, charW)

	var b strings.Builder
	appendLine := func(line string) {
		if line == "" {
			return
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	for _, group := range splitByGroup(items) {
		// 容器内按「上到下、左到右」排序：容器里各块的 DOM 顺序未必是行优先
		// （整列排下来的导航就是列优先），但容器内并成一行是安全的。
		sort.SliceStable(group, func(i, j int) bool {
			if math.Abs(group[i].Y-group[j].Y) > 0.5 {
				return group[i].Y < group[j].Y
			}
			return group[i].X < group[j].X
		})
		for _, row := range rowsWithinGroup(group) {
			if isMergeableRow(row, charW, page.Cols) {
				appendLine(renderVisualRow(row, charW, colOfX, page.Cols))
				continue
			}
			// 一行里混着长块（正文行 + 侧栏项之类）：各块独占一行，顺序取文，
			// 不做行内拼接——把两句不相干的话接起来，比多占一行糟得多。
			for _, it := range row {
				appendLine(strings.TrimSpace(it.Text))
			}
		}
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return "", false
	}
	return out, true
}

// splitByGroup 按「排布容器」切分，容器之间保持首次出现顺序。
//
// 这既是阅读顺序（DOM 顺序），又天然隔离了不同容器：正文与侧栏属于不同容器，
// 即使它们在同一高度上也不会被并成一行——按 y 全局排序时正是这里出的病句。
func splitByGroup(items []layoutItem) [][]layoutItem {
	order := make([]int, 0, 8)
	byGroup := make(map[int][]layoutItem, 8)
	for _, it := range items {
		if _, ok := byGroup[it.Group]; !ok {
			order = append(order, it.Group)
		}
		byGroup[it.Group] = append(byGroup[it.Group], it)
	}
	out := make([][]layoutItem, 0, len(order))
	for _, g := range order {
		out = append(out, byGroup[g])
	}
	return out
}

// rowsWithinGroup 在同一个排布容器内把「同一视觉行」的块聚成一组，组内按 x 排序。
//
// 只用垂直重叠判据（≥ 较矮者的一半）：同一行的元素高矮常常不一（图标 16px、链接
// 20px、按钮 36px），top 永远对不齐；上下相邻两行只在行距上轻微重叠，达不到一半。
// 横向是否该并成一行留给 isMergeableRow 判断——那是两件事。
func rowsWithinGroup(items []layoutItem) [][]layoutItem {
	var rows [][]layoutItem
	var top, bottom float64
	for _, it := range items {
		if len(rows) == 0 {
			rows = append(rows, []layoutItem{it})
			top, bottom = it.Y, it.Y+it.H
			continue
		}
		overlap := math.Min(bottom, it.Y+it.H) - math.Max(top, it.Y)
		if overlap > 0 && overlap >= 0.5*math.Min(it.H, bottom-top) {
			rows[len(rows)-1] = append(rows[len(rows)-1], it)
			if it.Y+it.H > bottom {
				bottom = it.Y + it.H
			}
			if it.Y < top {
				top = it.Y
			}
			continue
		}
		rows = append(rows, []layoutItem{it})
		top, bottom = it.Y, it.Y+it.H
	}
	for _, row := range rows {
		sort.SliceStable(row, func(i, j int) bool { return row[i].X < row[j].X })
	}
	return rows
}

// isMergeableRow 这一组能否并成一行：单块当然可以；多块要求全是窄块，且总跨度
// 不超出页宽（否则是横向溢出/绝对定位的残余，并起来只会得到一长串空格）。
func isMergeableRow(row []layoutItem, charW float64, cols int) bool {
	if len(row) < 2 {
		return true
	}
	start := row[0].X
	prevEnd := math.Inf(-1)
	for _, it := range row {
		if cellWidth(it.Text) > maxMergeCell {
			return false
		}
		if it.X < prevEnd-inlineGap*charW {
			return false // 两块在横向重叠：不是并排的单元格，是叠在一起的层
		}
		prevEnd = itemRight(it, charW)
	}
	if cols > 0 && charW > 0 && prevEnd-start > float64(cols)*charW {
		return false
	}
	return true
}

// separatorChars 页面里只用来分隔栏目的符号。单独成块的这类符号没有信息量，
// 留着只会让输出多出一堆孤零零的「|」行（门户首页尤其多）。
const separatorChars = "|｜/\\·•・-–—~=+*_.,;:!?›»‹«<>()[]{}"

// isSeparatorOnly 该块是否只是分隔符号（1~2 个字符且全在 separatorChars 里）。
func isSeparatorOnly(s string) bool {
	rs := []rune(strings.TrimSpace(s))
	if len(rs) == 0 || len(rs) > 2 {
		return false
	}
	for _, r := range rs {
		if !strings.ContainsRune(separatorChars, r) {
			return false
		}
	}
	return true
}

// buildColumnSnap 把全页的 x 坐标归并成「列带」，返回 x → 列位 的换算函数。
//
// 只做 round(x/charW) 是不够的：表格的 th 与 td、带 padding 的单元格，x 会差 1~2px，
// 在 charW 边界附近就一个算成 20 列、一个算成 21 列，同一列看起来就歪了。先把相差
// 不到一个字符宽的 x 归成一带（取中位数），同一带的块自然落到同一列。
func buildColumnSnap(items []layoutItem, minX, charW float64) func(float64) int {
	if charW <= 0 {
		return func(x float64) int { return 0 }
	}
	xs := make([]float64, 0, len(items))
	for _, it := range items {
		xs = append(xs, it.X)
	}
	sort.Float64s(xs)

	type band struct {
		lo, hi, sum float64
		n           int
	}
	var bands []band
	for _, x := range xs {
		if n := len(bands); n > 0 && x-bands[n-1].hi <= 0.75*charW {
			bands[n-1].hi = x
			bands[n-1].sum += x
			bands[n-1].n++
			continue
		}
		bands = append(bands, band{lo: x, hi: x, sum: x, n: 1})
	}
	centers := make([]float64, len(bands))
	cols := make([]int, len(bands))
	for i, b := range bands {
		c := b.sum / float64(b.n)
		centers[i] = c
		cols[i] = int(math.Round((c - minX) / charW))
		if cols[i] < 0 {
			cols[i] = 0
		}
	}
	return func(x float64) int {
		// 落在哪一带就取哪一带的列位；找不到（理论上不会）退回直接换算
		i := sort.SearchFloat64s(centers, x)
		if i >= len(centers) {
			i = len(centers) - 1
		}
		if i > 0 && math.Abs(centers[i-1]-x) <= math.Abs(centers[i]-x) {
			i--
		}
		return cols[i]
	}
}

// itemRight 块的右边缘（像素）：优先用测量到的矩形宽度，缺失时按文本显示宽度估算。
func itemRight(it layoutItem, charW float64) float64 {
	if it.W > 0 {
		return it.X + it.W
	}
	return it.X + float64(cellWidth(it.Text))*charW
}

// renderVisualRow 渲染一个视觉行：行内按 x 排序，块与块之间按列位补白。
//
// 两个细节决定成败：
//   - 列位用「显示宽度」而不是字符数。汉字在屏幕上占两列，按字符数记会让后面所有
//     块的位置整体偏左（中文页面尤其明显）。
//   - 当下一块的起点不晚于当前列位时，说明两块本来就连着（`<b>`、`<span>` 把一个词
//     拆成了两个文本节点），这时直接拼接，不能插空格——否则 "PDI" 会变成 "PD I"。
func renderVisualRow(row []layoutItem, charW float64, colOfX func(float64) int, cols int) string {
	sort.SliceStable(row, func(i, j int) bool { return row[i].X < row[j].X })

	var b strings.Builder
	col := 0        // 当前已写出的列（显示宽度）
	prevEndX := 0.0 // 上一块的右边缘（像素）
	havePrev := false
	for _, it := range row {
		target := colOfX(it.X)
		// 页宽是硬边界：x 超出视口的块已经在测量侧裁掉了，这里再兜一次底
		if cols > 0 && target > cols {
			target = cols
		}
		if havePrev {
			switch {
			case it.Cont && it.X-prevEndX <= inlineGap*charW:
				// 紧贴着上一块：同一个词被拆成的两段，直接拼接
			case target > col:
				gap := target - col
				if gap > maxColumnGap {
					gap = maxColumnGap
				}
				b.WriteString(strings.Repeat(" ", gap))
				col += gap
			default:
				// 上一段的文本超出了它自己的框、把目标列吃掉了：至少隔一个空格，
				// 否则两段互不相干的内容会粘成一串
				b.WriteString(" ")
				col++
			}
		}
		b.WriteString(it.Text)
		col += cellWidth(it.Text)
		if it.W > 0 {
			prevEndX = it.X + it.W
		} else {
			// 没有矩形宽度（旧数据/测量缺失）：按自身文本的显示宽度估一个右边缘
			prevEndX = it.X + float64(cellWidth(it.Text))*charW
		}
		havePrev = true
	}
	return strings.TrimRight(b.String(), " ")
}

// cellWidth 文本的显示宽度（列）：CJK / 全角 / emoji 记 2 列，其余记 1 列。
//
// 只用于列位估算——真实字形宽度由页面字体决定，这里要的是「和 x/charW 同一口径」。
func cellWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeCellWidth(r)
	}
	return w
}

// runeCellWidth 单字符显示宽度：仅覆盖「窄 / 宽」两档，够列位对齐用。
func runeCellWidth(r rune) int {
	switch {
	// 零宽与控制类（组合符号、变体选择符等）不占列
	case r == 0x200B || r == 0x200C || r == 0x200D || r == 0xFE0E || r == 0xFE0F,
		r >= 0x0300 && r <= 0x036F:
		return 0
	// CJK 部首、假名、汉字、全角标点、谚文、全角字母数字
	case r >= 0x1100 && (r <= 0x115F || // 谚文字母
		r == 0x2329 || r == 0x232A ||
		(r >= 0x2E80 && r <= 0xA4CF) || // CJK 部首 ~ 彝文
		(r >= 0xAC00 && r <= 0xD7A3) || // 谚文音节
		(r >= 0xF900 && r <= 0xFAFF) || // CJK 兼容汉字
		(r >= 0xFE30 && r <= 0xFE4F) || // CJK 兼容形式
		(r >= 0xFF00 && r <= 0xFF60) || // 全角形式
		(r >= 0xFFE0 && r <= 0xFFE6) ||
		(r >= 0x1F300 && r <= 0x1F64F) || // emoji
		(r >= 0x1F900 && r <= 0x1F9FF) ||
		(r >= 0x20000 && r <= 0x3FFFD)): // CJK 扩展平面
		return 2
	}
	return 1
}
