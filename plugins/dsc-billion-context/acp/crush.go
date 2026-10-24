package acp

// crush：工具结果的**确定性**压缩（对齐 acp-kernel src/crush.ts 的第一段 absorb 门）。
//
// 位置：absorb 是两段式——先 crush（纯函数、无模型往返），不行才走 absorb prompt
// 让模型蒸馏。DSC 之前只有第二段：任何一个大工具结果都直接甩给模型，既慢又贵，
// 而且在上下文快满时，模型那一轮蒸馏本身也可能塞不进去。
//
// 三条硬约束（照抄内核的契约，缺一条就会变成"优化了但结果不可信"）：
//   - **纯且确定**：同一输入 + 同一配置 ⇒ 同一字节。前缀缓存靠这个。
//   - **fail open**：任何畸形输入（截断的 JSON、未闭合的字符串、超深嵌套）一律
//     原样放行，绝不产出比原文更糟的东西。
//   - **收益门**：压缩后必须真的小够（MinReduction），否则按未命中处理。
//
// 区别：内核的 json-fold 附带 `__acp_crush` 解码器可无损还原。DSC 这里只做
// **结构保真的因子化**（同值提升 + 相同行折叠），不宣称可逆——模型看得到计数、
// 常量和样本，足以判断要不要再 absorb。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CrushConfig 确定性压缩配置（对齐 acp-kernel config.crush）。
type CrushConfig struct {
	// Enabled 总开关（默认关）。打开前需 Absorb.Enabled 也为真：crush 只是
	// absorb 两段式的第一段，没有第二段兜底时单独压一半没有意义。
	Enabled bool `json:"enabled"`
	// MinReduction 最小收益比（默认 0.1 = 至少省 10%），达不到就按未命中处理。
	MinReduction float64 `json:"minReduction"`
	// Strategies 按策略名的开关（缺省视为启用；显式 false 关闭）。
	Strategies map[string]bool `json:"strategies,omitempty"`
	// ExcludeTools 不参与压缩的工具名（支持 * 后缀通配）。
	ExcludeTools []string `json:"excludeTools,omitempty"`
}

// CrushDecisionKind 一次 crush 评估的结论。
type CrushDecisionKind string

const (
	// CrushSkip 未命中：保持原文。
	CrushSkip CrushDecisionKind = "skip"
	// CrushCrushed 压到阈值之下：不需要再 absorb。
	CrushCrushed CrushDecisionKind = "crushed"
	// CrushDistill 压过但仍偏大：把压缩后的文本交给 absorb prompt 路径。
	CrushDistill CrushDecisionKind = "distill"
)

// CrushEvaluation crush 评估结果。
type CrushEvaluation struct {
	Kind     CrushDecisionKind
	Text     string // 落线文本：skip 为原文；其余为压缩结果
	Strategy string // 命中的策略名（skip 时为空）
	Before   int    // 原文估算 token
	After    int    // 落线文本估算 token
}

// payloadKind 内容种类：不同种类只能走各自策略，避免"日志策略去啃 JSON"这类
// 假装成功实则更差的组合。
type payloadKind string

const (
	kindJSON payloadKind = "json"
	kindCode payloadKind = "code"
	kindLog  payloadKind = "log"
	kindText payloadKind = "text"
)

// 策略名。
const (
	strategyJSONFold = "json-fold"
	strategyCodeTrim = "code-trim"
	strategyLogSel   = "log-select"
)

// classifyPayload 判断内容种类。判据刻意保守：拿不准就算 text（不压）。
func classifyPayload(s string) payloadKind {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return kindText
	}
	if c := trimmed[0]; c == '{' || c == '[' {
		var v any
		if json.Unmarshal([]byte(trimmed), &v) == nil {
			return kindJSON
		}
	}
	if looksLikeLog(trimmed) {
		return kindLog
	}
	if looksLikeCode(trimmed) {
		return kindCode
	}
	return kindText
}

// looksLikeLog 粗判日志：多行且多数行以时间戳/级别标记开头。
func looksLikeLog(s string) bool {
	lines := strings.Split(s, "\n")
	if len(lines) < 8 {
		return false
	}
	hit := 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		up := strings.ToUpper(t)
		// 级别标记可能出现在行首，也可能在时间戳/pid 之后——两者都算。
		// 只认行首会把「2026-10-25T10:00:00Z ERROR …」这类最常见格式漏掉。
		if hasLogLevelToken(up) {
			hit++
		}
	}
	return hit*2 >= len(lines)
}

// logLevelTokens 日志级别标记。
var logLevelTokens = []string{"TRACE", "DEBUG", "INFO", "WARN", "ERROR", "FATAL", "PANIC"}

// hasLogLevelToken 报告该行（已大写）是否带日志级别标记。
func hasLogLevelToken(up string) bool {
	for _, lv := range logLevelTokens {
		if strings.Contains(up, lv) {
			return true
		}
	}
	return false
}

// looksLikeCode 粗判代码：多行 + 至少一处代码特征。
//
// 门槛刻意低（一个特征即可）：代码注释剥离的收益主要来自 // 与 /* 行，只要
// 有这些就值得试；真正的安全由 codeTrim 的 fail-open 兜底（未闭合字符串/注释
// 一律原样放行），而不是靠分类器猜得准。
func looksLikeCode(s string) bool {
	if strings.Count(s, "\n") < 8 {
		return false
	}
	for _, m := range []string{"//", "/*", "func ", "def ", "class ", "import ", "package ", "=>", ");\n", ";\n"} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// CrushStrategiesFor 返回某内容种类允许的策略顺序。
func CrushStrategiesFor(kind payloadKind) []string {
	switch kind {
	case kindJSON:
		return []string{strategyJSONFold}
	case kindLog:
		return []string{strategyLogSel}
	case kindCode:
		return []string{strategyCodeTrim}
	}
	return nil
}

// EvaluateCrush 评估一条工具结果是否需要/可以确定性压缩。
//
// 门控顺序与内核一致：开关 → 候选（工具名/排除表）→ 大小门 → 上下文压力门 →
// 按种类跑策略 → 收益门。任何一步不满足即 skip（原文不动）。
func EvaluateCrush(msg CoreMessage, state *CompressionState, config Config, tokenCount int) CrushEvaluation {
	cc := config.Crush
	orig := msg.Text
	before := estimateTokensForText(orig)
	eval := CrushEvaluation{Kind: CrushSkip, Text: orig, Before: before, After: before}

	if !cc.Enabled || !config.Absorb.Enabled {
		return eval
	}
	if msg.Role != RoleTool || orig == "" {
		return eval
	}
	if !IsAbsorbCandidate(msg, config) || crushExcluded(msg.ToolName, cc.ExcludeTools) {
		return eval
	}
	minTokens := config.Absorb.MinToolTokens
	if minTokens <= 0 {
		minTokens = 1000
	}
	if before < minTokens {
		return eval
	}
	// 上下文压力门：宽裕时不做任何压缩（省下的 token 没价值，反而损失保真）
	if pct := config.Absorb.ContextThresholdPct; pct > 0 && config.ModelContextLimit > 0 {
		if float64(tokenCount) < pct*float64(config.ModelContextLimit) {
			return eval
		}
	}

	kind := classifyPayload(orig)
	for _, name := range CrushStrategiesFor(kind) {
		if enabled, ok := cc.Strategies[name]; ok && !enabled {
			continue
		}
		out, ok := runCrushStrategy(name, orig)
		if !ok || out == "" || out == orig {
			continue
		}
		after := estimateTokensForText(out)
		if after >= before {
			continue
		}
		minReduction := cc.MinReduction
		if minReduction <= 0 {
			minReduction = 0.1
		}
		if float64(before-after)/float64(before) < minReduction {
			continue // 收益不够，按未命中处理（避免为了 2% 破坏可读性）
		}
		eval.Text, eval.Strategy, eval.After = out, name, after
		if after < minTokens {
			eval.Kind = CrushCrushed
		} else {
			eval.Kind = CrushDistill
		}
		return eval
	}
	return eval
}

// runCrushStrategy 调具体策略；策略内部必须 fail open（返回 ok=false）。
func runCrushStrategy(name, text string) (out string, ok bool) {
	defer func() {
		// 策略是纯函数，不该 panic；真 panic 也绝不把请求带崩
		if recover() != nil {
			out, ok = "", false
		}
	}()
	switch name {
	case strategyJSONFold:
		return jsonFold(text)
	case strategyLogSel:
		return logSelect(text)
	case strategyCodeTrim:
		return codeTrim(text)
	}
	return "", false
}

// crushExcluded 工具名排除表（支持 * 后缀通配，如 "browser_*"）。
func crushExcluded(tool string, excludes []string) bool {
	for _, e := range excludes {
		if e == "" {
			continue
		}
		if strings.HasSuffix(e, "*") {
			if strings.HasPrefix(tool, strings.TrimSuffix(e, "*")) {
				return true
			}
			continue
		}
		if tool == e {
			return true
		}
	}
	return false
}

// ---- json-fold：同值提升 + 相同行折叠 ----

// crushEnvelope 因子化信封。__acp_crush 标明这是因子化后的结构（不是原始数据），
// 计数与样本足以判断是否需要 absorbance。
type crushEnvelope struct {
	Kind    string         `json:"__acp_crush"`
	Count   int            `json:"count,omitempty"`
	Const   map[string]any `json:"const,omitempty"`
	Row     map[string]any `json:"row,omitempty"`
	Rows    []any          `json:"rows,omitempty"`
	Omitted int            `json:"omitted,omitempty"`
}

const (
	jsonFoldRunMin   = 4  // 相同行至少 4 条才折叠（少了不值得引入信封）
	jsonFoldHoistMin = 4  // 同一字段在同值上至少 4 次才提升为常量
	jsonFoldMaxDepth = 12 // 嵌套超过该深度直接放弃（fail open）
	crushMaxInput    = 2_000_000
)

// jsonFold 对「对象数组」做无损因子化：把每行都相同的字段提升到 const，
// 把连续相同行折叠成 {count,row}。结构与数据都还在，只是换了个说法。
func jsonFold(text string) (string, bool) {
	if len(text) > crushMaxInput {
		return "", false
	}
	var v any
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &v); err != nil {
		return "", false
	}
	if depthOf(v, 0) > jsonFoldMaxDepth {
		return "", false
	}
	folded, changed := foldJSONValue(v)
	if !changed {
		return "", false
	}
	out, err := json.Marshal(folded)
	if err != nil {
		return "", false
	}
	return string(out), true
}

func depthOf(v any, d int) int {
	if d > jsonFoldMaxDepth {
		return d
	}
	max := d
	switch t := v.(type) {
	case map[string]any:
		for _, x := range t {
			if n := depthOf(x, d+1); n > max {
				max = n
			}
		}
	case []any:
		for _, x := range t {
			if n := depthOf(x, d+1); n > max {
				max = n
			}
		}
	}
	return max
}

// foldJSONValue 递归折叠；任何不确定的分支保持原样（返回 changed=false）。
func foldJSONValue(v any) (any, bool) {
	rows, ok := v.([]any)
	if !ok || len(rows) < jsonFoldRunMin {
		return v, false
	}
	// 全部是对象才做字段因子化
	objs := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		o, ok := r.(map[string]any)
		if !ok {
			return v, false
		}
		objs = append(objs, o)
	}

	// 1) 同值提升：某字段在所有行里取值完全相同
	counts := map[string]map[string]int{}
	for _, o := range objs {
		for k, val := range o {
			key := canonicalJSON(val)
			if counts[k] == nil {
				counts[k] = map[string]int{}
			}
			counts[k][key]++
		}
	}
	constFields := map[string]any{}
	for k, vals := range counts {
		if len(vals) != 1 {
			continue
		}
		if counts[k][canonicalJSON(nil)] > 0 {
			continue // 缺失值与 null 混在一起，别硬提
		}
		if vals[firstKey(vals)] < jsonFoldHoistMin || vals[firstKey(vals)] != len(objs) {
			continue
		}
		constFields[k] = objs[0][k]
	}

	// 2) 相同行折叠（在剩余字段上做，连续相同才算）
	stripped := make([]map[string]any, len(objs))
	for i, o := range objs {
		m := make(map[string]any, len(o))
		for k, val := range o {
			if _, hoisted := constFields[k]; hoisted {
				continue
			}
			m[k] = val
		}
		stripped[i] = m
	}
	var runs []crushEnvelope
	for i := 0; i < len(stripped); {
		j := i + 1
		key := canonicalJSON(stripped[i])
		for j < len(stripped) && canonicalJSON(stripped[j]) == key {
			j++
		}
		n := j - i
		if n >= jsonFoldRunMin {
			runs = append(runs, crushEnvelope{Kind: "repeat", Count: n, Row: stripped[i]})
		} else {
			for k := 0; k < n; k++ {
				runs = append(runs, crushEnvelope{Kind: "one", Row: stripped[i]})
			}
		}
		i = j
	}
	if len(constFields) == 0 && len(runs) == len(stripped) {
		return v, false // 什么都没省下
	}

	env := crushEnvelope{Kind: "rows", Count: len(objs), Const: constFields, Rows: envelopesToAny(runs)}
	return env, true
}

func envelopesToAny(rs []crushEnvelope) []any {
	out := make([]any, 0, len(rs))
	for _, r := range rs {
		if r.Kind == "repeat" {
			out = append(out, map[string]any{"__acp_crush": "repeat", "count": r.Count, "row": r.Row})
			continue
		}
		out = append(out, map[string]any{"row": r.Row})
	}
	return out
}

func firstKey(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys[0]
}

// canonicalJSON 归一化 JSON 以便比较（map 顺序无关、数字统一表示）。
func canonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// ---- log-select：保留全部错误行，其余按级筛选 ----

const (
	logKeepTailLines = 200 // 尾部保留行数（最新日志最有用）
	logMaxLineLen    = 500 // 单行超长时截断（防止一坨 base64 撑爆）
)

// logSelect 按级别筛选日志行：**ERROR/FAIL/FATAL/PANIC 行一律原样保留**（这是
// 内核强制的不变量——丢掉错误行的压缩等于把故障信息删了），WARN 去重，
// 堆栈帧折叠，尾部保留最近若干行。
func logSelect(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) <= logKeepTailLines {
		return "", false // 不够长，压了也不省
	}
	head := lines[:len(lines)-logKeepTailLines]
	tail := lines[len(lines)-logKeepTailLines:]

	var out []string
	seenWarn := map[string]bool{}
	omitted := 0
	flush := func() {
		if omitted > 0 {
			out = append(out, fmt.Sprintf("[... %d 行省略 ...]", omitted))
			omitted = 0
		}
	}
	for i := 0; i < len(head); i++ {
		line := head[i]
		if len(line) > logMaxLineLen {
			line = line[:logMaxLineLen] + "…[truncated]"
		}
		up := strings.ToUpper(line)
		isErr := strings.Contains(up, "ERROR") || strings.Contains(up, "FATAL") ||
			strings.Contains(up, "PANIC") || strings.Contains(up, "FAIL")
		if isErr {
			flush()
			out = append(out, line)
			continue
		}
		if strings.Contains(up, "WARN") {
			key := strings.TrimSpace(line)
			if seenWarn[key] {
				omitted++
				continue
			}
			seenWarn[key] = true
			flush()
			out = append(out, line)
			continue
		}
		// 堆栈帧（"    at " / "\tat " / "  File "）成组折叠
		if isStackFrame(line) {
			j := i
			for j < len(head) && isStackFrame(head[j]) {
				j++
			}
			out = append(out, fmt.Sprintf("[... %d 行堆栈帧 ...]", j-i))
			i = j - 1
			continue
		}
		omitted++
	}
	flush()
	out = append(out, tail...)
	if omitted := len(lines) - len(tail) - (len(out) - 1); omitted > 0 && omitted < len(lines)/10 {
		return "", false // 几乎没省，别动
	}
	return strings.Join(out, "\n"), true
}

func isStackFrame(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "at ") || strings.HasPrefix(t, "File \"") ||
		strings.HasPrefix(t, "0x") || strings.HasPrefix(t, "goroutine ")
}

// ---- code-trim：注释剥离（字符串字面量感知） ----

const codeKeepRatio = 0.97

// codeTrim 剥离注释行；**字符串字面量里的 // 与 /* 不当作注释**（否则会把
// URL、正则、模板内容吃坏）。未闭合字符串/注释一律放弃（fail open）。
func codeTrim(text string) (string, bool) {
	lines := strings.Split(text, "\n")
	var out []string
	removed := 0
	inBlock := false
	for _, line := range lines {
		if inBlock {
			if idx := strings.Index(line, "*/"); idx >= 0 {
				inBlock = false
				rest := strings.TrimSpace(line[idx+2:])
				if rest != "" {
					out = append(out, rest)
				}
				continue
			}
			removed++
			continue
		}
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "//") || strings.HasPrefix(t, "#") {
			removed++
			continue
		}
		if strings.HasPrefix(t, "/*") {
			if idx := strings.Index(t, "*/"); idx > 2 {
				removed++
				continue
			}
			inBlock = true
			removed++
			continue
		}
		// 行内块注释：只在没有引号时剥离，避免吃掉字符串
		if !strings.ContainsAny(line, "\"'`") {
			if idx := strings.Index(line, "/*"); idx >= 0 {
				if end := strings.Index(line[idx:], "*/"); end > 0 {
					line = strings.TrimRight(line[:idx], " \t")
					if strings.TrimSpace(line) != "" {
						out = append(out, line)
					}
					removed++
					continue
				}
			}
		}
		out = append(out, line)
	}
	if inBlock {
		return "", false // 未闭合：原样放行
	}
	if float64(removed) < float64(len(lines))*(1-codeKeepRatio) {
		return "", false // 收益不足
	}
	return strings.Join(out, "\n"), true
}

// crushMarkerPrefix 非 JSON 载荷的落线标记：下一轮据此跳过（避免二次压缩把
// 已经压过的内容再啃一遍）。JSON 不需要前缀——信封本身就是标记，且加了前缀
// 就不再是合法 JSON，模型想解析反而麻烦。
const crushMarkerPrefix = "[acp-crushed:"

// isCrushed 判断文本是否已被 crush 过。
func isCrushed(text string) bool {
	t := strings.TrimSpace(text)
	if strings.HasPrefix(t, crushMarkerPrefix) {
		return true
	}
	if strings.HasPrefix(t, "{") {
		var probe map[string]any
		if json.Unmarshal([]byte(t), &probe) == nil {
			if _, ok := probe["__acp_crush"]; ok {
				return true
			}
		}
	}
	return false
}

// ApplyCrush 对本步消息做确定性压缩（对齐 acp-kernel crush 节点）。
//
// 返回值：
//   - Messages：替换过文本的消息列表
//   - Crushed：压到阈值以下（无需再 absorb）
//   - Distilled：压过但仍偏大（交由 absorb prompt 继续）
func ApplyCrush(messages []CoreMessage, state *CompressionState, config Config, tokenCount int) ([]CoreMessage, int, int) {
	if !config.Crush.Enabled || !config.Absorb.Enabled || len(messages) == 0 {
		return messages, 0, 0
	}
	out := make([]CoreMessage, len(messages))
	crushed, distilled := 0, 0
	for i, msg := range messages {
		out[i] = msg
		if isCrushed(msg.Text) {
			continue
		}
		eval := EvaluateCrush(msg, state, config, tokenCount)
		switch eval.Kind {
		case CrushSkip:
			continue
		case CrushCrushed, CrushDistill:
			text := eval.Text
			if !strings.HasPrefix(strings.TrimSpace(text), "{") {
				text = fmt.Sprintf("%s%s · %d→%d tokens]\n%s",
					crushMarkerPrefix, eval.Strategy, eval.Before, eval.After, text)
			}
			out[i].Text = text
			if eval.Kind == CrushCrushed {
				crushed++
			} else {
				distilled++
			}
		}
	}
	return out, crushed, distilled
}
