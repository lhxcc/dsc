package billion

import (
	"encoding/json"
	"regexp"
	"strings"
)

// compress 参数解析（对齐 acp-kernel parseCompressArgs 的宽松语义）。
//
// 为什么需要宽松：compress 的 summary 往往是大段自然语言，模型输出的 args 常带
// 代码围栏（```json ... ```）、尾逗号，或在长输出被截断时留下未闭合的数组。
// 严格 json.Unmarshal 会把这些一律判为失败，压缩随之失败，上下文继续膨胀——
// 这正是 DESIGN §6 把 parseCompressArgs 列为核心职责的原因。

// compressRangeArgs 单个 range 的宽松字段集合（兼容多种字段拼写）。
type compressRangeArgs struct {
	StartID  string `json:"startId"`
	StartRef string `json:"startRef"`
	Start    string `json:"start"`
	EndID    string `json:"endId"`
	EndRef   string `json:"endRef"`
	End      string `json:"end"`
	Summary  string `json:"summary"`
	Topic    string `json:"topic"`
}

func (r compressRangeArgs) normalize() (PruneRange, bool) {
	start := firstNonEmptyStr(r.StartID, r.StartRef, r.Start)
	end := firstNonEmptyStr(r.EndID, r.EndRef, r.End)
	if start == "" || end == "" || strings.TrimSpace(r.Summary) == "" {
		return PruneRange{}, false
	}
	return PruneRange{StartRef: start, EndRef: end, Summary: r.Summary, Topic: r.Topic}, true
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ParseCompressArgs 解析 compress 调用的 args 文本，返回范围集。
//
// 容错顺序：
//  1. 定位首个 '{'（跳过前置说明文字/围栏），扫描出第一个**平衡**的 JSON 对象；
//  2. 去掉尾逗号（Go 的 encoding/json 不接受）；
//  3. 解析失败或对象被截断时，逐个回收 content 数组里**完整**的 range 对象，
//     丢弃残缺的尾部——保证"写了一半的压缩请求"仍能压掉已完整表达的部分；
//  4. 字段名兼容 startId/startRef/start 与 endId/endRef/end。
func ParseCompressArgs(text string) []PruneRange {
	obj, balanced := balancedJSONObject(text)
	if obj == "" {
		return nil
	}
	cleaned := stripTrailingCommas(obj)

	var req struct {
		Content []compressRangeArgs `json:"content"`
	}
	if balanced {
		if err := json.Unmarshal([]byte(cleaned), &req); err == nil {
			if out := normalizeCompressRanges(req.Content); len(out) > 0 {
				return out
			}
			// 解析成功但没有任何有效 range：不是截断问题，直接返回空让调用方报错
			return nil
		}
	}
	// 截断或结构异常：回收完整对象
	return salvageCompressRanges(cleaned)
}

func normalizeCompressRanges(in []compressRangeArgs) []PruneRange {
	out := make([]PruneRange, 0, len(in))
	for _, r := range in {
		if pr, ok := r.normalize(); ok {
			out = append(out, pr)
		}
	}
	return out
}

// balancedJSONObject 从首个 '{' 起扫描第一个平衡的 JSON 对象（感知字符串与转义）。
// 返回对象文本与是否平衡（false 表示输出被截断）。
func balancedJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return "", false
	}
	depth := 0
	inStr, esc := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
			// 字符串内的括号不参与配对
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return s[start:], false
}

var trailingCommaRE = regexp.MustCompile(`,(\s*[}\]])`)

// stripTrailingCommas 去掉对象/数组最后一个元素后的逗号（"a":1,} → "a":1}）。
func stripTrailingCommas(s string) string {
	prev := ""
	for prev != s {
		prev = s
		s = trailingCommaRE.ReplaceAllString(s, "$1")
	}
	return s
}

// salvageCompressRanges 从（可能被截断的）content 数组里回收每个完整的 range 对象。
func salvageCompressRanges(s string) []PruneRange {
	idx := strings.Index(s, `"content"`)
	if idx < 0 {
		return nil
	}
	arr := s[idx:]
	open := strings.IndexByte(arr, '[')
	if open < 0 {
		return nil
	}
	arr = arr[open+1:]

	var out []PruneRange
	depth, startAt := 0, -1
	inStr, esc := false, false
	for i := 0; i < len(arr); i++ {
		c := arr[i]
		switch {
		case esc:
			esc = false
		case c == '\\' && inStr:
			esc = true
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '{':
			if depth == 0 {
				startAt = i
			}
			depth++
		case c == '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && startAt >= 0 {
				var one compressRangeArgs
				if err := json.Unmarshal([]byte(stripTrailingCommas(arr[startAt:i+1])), &one); err == nil {
					if pr, ok := one.normalize(); ok {
						out = append(out, pr)
					}
				}
				startAt = -1
			}
		}
	}
	return out
}
