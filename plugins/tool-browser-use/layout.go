package main

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
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
  const visible = (r) => r && r.width >= 1 && r.height >= 1;
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

  const items = [];
  const walker = document.createTreeWalker(body, NodeFilter.SHOW_TEXT, null);
  let node;
  while ((node = walker.nextNode())) {
    if (items.length >= MAX_ITEMS) break;
    const text = node.nodeValue;
    if (!text || !text.trim()) continue;
    const el = node.parentElement;
    if (!el || skipTags[el.tagName]) continue;

    let cs;
    try { cs = getComputedStyle(el); } catch (e) { continue; }
    if (cs.display === 'none' || cs.visibility === 'hidden' || cs.opacity === '0') continue;

    const range = document.createRange();
    range.selectNodeContents(node);
    const rects = Array.prototype.filter.call(range.getClientRects(), visible);
    if (!rects.length) continue;

    if (rects.length === 1) {
      const t = norm(text);
      if (t) items.push({ t: t, x: rects[0].left, y: rects[0].top, h: rects[0].height });
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
      if (t) items.push({ t: t, x: r.left, y: r.top, h: r.height });
      start = Math.max(start, end);
    }
  }

  return JSON.stringify({ charW: charW, items: items });
}`

// maxColumnGap 行内两块之间的最大补白（列）。版面里的绝对定位、浮动侧栏会拉出
// 上百列的空档，全按原样补会把文本行撑成巨长一行；这里封顶，只保留「有间隔」这个信息。
const maxColumnGap = 40

// layoutPage 是 layoutExtractJS 的产物：单字符像素宽 + 按视觉行收集到的文本块。
type layoutPage struct {
	CharW float64      `json:"charW"`
	Items []layoutItem `json:"items"`
}

// layoutItem 一个文本块在视口里的位置（像素，视口坐标）。
type layoutItem struct {
	Text string  `json:"t"`
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	H    float64 `json:"h"`
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

	items := append([]layoutItem(nil), page.Items...)
	// 先把空白块丢掉，避免它们占出一堆空行
	compact := items[:0]
	for _, it := range items {
		if it.Text != "" {
			compact = append(compact, it)
		}
	}
	items = compact
	if len(items) == 0 {
		return "", false
	}

	// 从上到下、从左到右
	sort.SliceStable(items, func(i, j int) bool {
		if math.Abs(items[i].Y-items[j].Y) > 0.5 {
			return items[i].Y < items[j].Y
		}
		return items[i].X < items[j].X
	})

	// 整页最左点作为列基准：这样既对齐了相对位置，又不会让每行都顶着一截缩进
	minX := items[0].X
	for _, it := range items {
		if it.X < minX {
			minX = it.X
		}
	}

	var b strings.Builder
	for _, row := range groupVisualRows(items) {
		line := renderVisualRow(row, charW, minX)
		if line == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		return "", false
	}
	return out, true
}

// groupVisualRows 把垂直方向重叠过半的文本块聚成同一视觉行。
//
// 判据用「重叠高度 ≥ 较矮者的一半」而不是「top 相等」：同一行的元素高度常常不一
// （图标 16px、链接 20px、按钮 36px），top 永远对不齐；而上下相邻的两行通常只在
// 行距上轻微重叠，达不到一半。
func groupVisualRows(items []layoutItem) [][]layoutItem {
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
	return rows
}

// renderVisualRow 渲染一个视觉行：行内按 x 排序，块与块之间按列位补白。
func renderVisualRow(row []layoutItem, charW, minX float64) string {
	sort.SliceStable(row, func(i, j int) bool { return row[i].X < row[j].X })

	var b strings.Builder
	col := 0 // 当前已写出的列（按字符计）
	for _, it := range row {
		target := int(math.Round((it.X - minX) / charW))
		if target < 0 {
			target = 0
		}
		if b.Len() > 0 {
			gap := target - col
			if gap < 1 {
				gap = 1 // 至少一个空格：两块贴在一起会让文本粘成一串
			}
			if gap > maxColumnGap {
				gap = maxColumnGap
			}
			b.WriteString(strings.Repeat(" ", gap))
			col += gap
		}
		b.WriteString(it.Text)
		col += utf8.RuneCountInString(it.Text)
	}
	return strings.TrimRight(b.String(), " ")
}
