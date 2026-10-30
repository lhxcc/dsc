package core

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// 估算系数的「生效集合」。
//
// 为什么需要这一层：`tokPerMille*` 是对某个分词器的实测拟合，换模型、换 provider 模板、
// 内容构成长期偏移后就不再准确。校准表（token-calibration.json）里的 factor 只能整体
// 缩放，修不了「哪一类字符估错了」；账本里有每帧的构成计数，够做真正的最小二乘。
//
// 但系数是**进程内全局**的：宿主判超窗、插件算压缩预算都用同一套常量。若各进程各用
// 各的（宿主热更新了、插件还是旧的），就会出现「按 A 判、按 B 学」——正是反复踩过的坑。
// 所以：
//
//  1. 系数在**进程启动时**确定一次（宿主从文件或账本拟合，插件从环境变量读宿主注入的
//     同一份），运行期不变；
//  2. 自动重拟合只在**写文件**这一层发生，下个进程启动才生效——日志会说清楚；
//  3. 拟合有硬门槛（样本量、各类字符质量、残差必须优于现用系数），过不了就保持原样。
//
// 这样「谁来定系数」就不再需要人：数据够了自动定，定完自动用，用坏了有门槛拦。

// TokenCoefSet 一套生效中的千分权重。
type TokenCoefSet struct {
	// PerMille 各类字符的千分权重，下标与 SurfaceStats.Classes 一致。
	PerMille [7]float64 `json:"per_mille"`
	// Model 拟合所依据的模型（空 = 内置默认值）。
	Model string `json:"model,omitempty"`
	// Samples 参与拟合的帧数。
	Samples int `json:"samples,omitempty"`
	// Residual 拟合后的 pred/actual 中位数绝对偏差（0.05 = 5%）。
	Residual float64 `json:"residual,omitempty"`
	FittedAt int64   `json:"fitted_at,omitempty"`
}

// DefaultTokenCoefs 内置默认系数（在 deepseek 系分词器上用真实样本拟合而来，见 token_meter.go）。
func DefaultTokenCoefs() *TokenCoefSet {
	return &TokenCoefSet{PerMille: [7]float64{
		tokPerMilleLetter, tokPerMilleSpace, tokPerMilleDigit,
		tokPerMillePunct, tokPerMilleWide, tokPerMilleEmoji, tokPerMilleSymbol,
	}}
}

// activeTokenCoefs 是进程级生效系数（默认 = 内置）。
var activeTokenCoefs atomic.Pointer[TokenCoefSet]

// ActiveTokenCoefs 返回当前进程生效的估算系数（只读，调用方不得修改）。
func ActiveTokenCoefs() *TokenCoefSet {
	if c := activeTokenCoefs.Load(); c != nil {
		return c
	}
	return DefaultTokenCoefs()
}

// SetActiveTokenCoefs 设置进程级生效系数（宿主启动装配、插件读环境变量时调用）。
//
// 只在启动阶段调用：运行期改动会让「同一会话前后两次估算」用两把尺子。
func SetActiveTokenCoefs(c *TokenCoefSet) {
	if c == nil || !validCoefs(c.PerMille) {
		return
	}
	cp := *c
	activeTokenCoefs.Store(&cp)
}

// tokenCoefEnvKey 是宿主注入、插件读取的系数环境变量。
//
// 走环境变量而不是让每个进程各自读文件：插件进程启动时宿主可能还没落盘新的拟合结果，
// 两边读到的可能是不同版本；由宿主一次算好注入，天然一致。
const tokenCoefEnvKey = "DSC_TOKEN_COEF"

// FormatTokenCoefSpec 把系数序列化成 DSC_TOKEN_COEF 的值（逗号分隔，7 位小数以内）。
func FormatTokenCoefSpec(c *TokenCoefSet) string {
	parts := make([]string, 0, len(c.PerMille)+1)
	for _, v := range c.PerMille {
		parts = append(parts, strconv.FormatFloat(v, 'f', -1, 64))
	}
	if c.Model != "" {
		parts = append(parts, c.Model)
	}
	return strings.Join(parts, ",")
}

// ParseTokenCoefSpec 解析 DSC_TOKEN_COEF 的值：7 个数值 + 可选模型名。
//
// 宽进严出：数值个数不对、非有限值、越界（负数或 > 5 token/字符）一律返回错误，
// 由调用方退回默认值——宁可少一点精度，也不要用一套明显坏掉的系数去裁历史。
func ParseTokenCoefSpec(spec string) (*TokenCoefSet, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, fmt.Errorf("empty token coefficient spec")
	}
	fields := strings.Split(spec, ",")
	if len(fields) < 7 {
		return nil, fmt.Errorf("token coefficient spec needs 7 numbers, got %d", len(fields))
	}
	var c TokenCoefSet
	for i := 0; i < 7; i++ {
		v, err := strconv.ParseFloat(strings.TrimSpace(fields[i]), 64)
		if err != nil {
			return nil, fmt.Errorf("token coefficient #%d: %w", i, err)
		}
		c.PerMille[i] = v
	}
	if !validCoefs(c.PerMille) {
		return nil, fmt.Errorf("token coefficients out of range")
	}
	if len(fields) > 7 {
		c.Model = strings.TrimSpace(strings.Join(fields[7:], ","))
	}
	return &c, nil
}

// validCoefs 判定一套系数是否在物理可能区间内。
//
// 五类「常见内容」（字母/空白/数字/标点/CJK）下限 0.02：它们几乎必然出现，权重落到 0
// 只会把文本估成 0 token。emoji 与「其余符号」允许为 0——很多账本里根本没有这类字符，
// 拟合不出来时保持在原值即可（见 FitTokenCoefficients 的保底逻辑）。
func validCoefs(p [7]float64) bool {
	for i, v := range p {
		if math.IsNaN(v) || math.IsInf(v, 0) || v > 5000 {
			return false
		}
		if i == clsEmoji || i == clsSymbol {
			if v < 0 {
				return false
			}
			continue
		}
		if v < 20 {
			return false
		}
	}
	return true
}

// LoadTokenCoefFromEnv 让插件进程采用宿主注入的系数（SDK 在进程启动时调用）。
//
// 没有注入（独立运行、老宿主）就保持内置默认：不是错误路径，不必告警。
func LoadTokenCoefFromEnv() {
	spec := os.Getenv(tokenCoefEnvKey)
	if spec == "" {
		return
	}
	c, err := ParseTokenCoefSpec(spec)
	if err != nil {
		return
	}
	SetActiveTokenCoefs(c)
}

// ---- 拟合与落盘 ----

// tokenCoefFileName 与校准表同目录：同一份运行数据一起备份/清理。
const (
	tokenCoefFileName = "token-coefficients.json"
	tokenCoefSchema   = 1
	// 拟合门槛：样本太少时最小二乘会被个别异常帧带跑。
	tokenCoefMinSamples = 200
	// 每个类别至少累计这么多字符才认可它自己的权重；否则该列不可信（共线或没出现）。
	tokenCoefMinClassMass = 5000
)

// tokenCoefFile 是 token-coefficients.json 的结构。带 schema：不符即丢弃重建。
type tokenCoefFile struct {
	Schema      int                      `json:"schema"`
	ActiveModel string                   `json:"active_model,omitempty"`
	Models      map[string]*TokenCoefSet `json:"models"`
}

// tokenCoefPath 返回系数文件路径。
func (m *Manager) tokenCoefPath() string {
	return PJoin(m.sessionDir(), tokenCoefFileName)
}

// loadTokenCoefs 读系数文件：优先当前模型，其次文件里记的 active，最后放弃（用默认）。
//
// 这三个候选顺序对应三种现实：正常（本模型有拟合）、刚换模型（还没有本模型的）、
// 文件里只有别的模型（借来先用，与偏差系数的借用语义一致）。
func (m *Manager) loadTokenCoefs() *TokenCoefSet {
	data, err := os.ReadFile(m.tokenCoefPath())
	if err != nil {
		return nil
	}
	var f tokenCoefFile
	if err := json.Unmarshal(data, &f); err != nil || f.Schema != tokenCoefSchema {
		m.logger.Warn("token coefficient file ignored (schema mismatch or corrupt)", "path", m.tokenCoefPath())
		return nil
	}
	candidates := []string{m.modelName, f.ActiveModel}
	for _, name := range candidates {
		if name == "" {
			continue
		}
		if c, ok := f.Models[name]; ok && c != nil && validCoefs(c.PerMille) {
			return c
		}
	}
	// 借最近拟合过的一份（换模型时先用着，比默认值更接近现实）
	var best *TokenCoefSet
	for _, c := range f.Models {
		if c == nil || !validCoefs(c.PerMille) {
			continue
		}
		if best == nil || c.FittedAt > best.FittedAt {
			best = c
		}
	}
	return best
}

// SaveTokenCoefs 原子写系数文件（保留其它模型的历史拟合结果）。
//
// 与 core 内其它路径一致：MkdirAll + 临时文件 + rename，避免半截文件被下个进程读到。
func SaveTokenCoefs(dir string, c *TokenCoefSet) error {
	if c == nil || !validCoefs(c.PerMille) {
		return fmt.Errorf("invalid token coefficients")
	}
	path := PJoin(dir, tokenCoefFileName)
	f := tokenCoefFile{Schema: tokenCoefSchema, ActiveModel: c.Model, Models: map[string]*TokenCoefSet{}}
	if data, err := os.ReadFile(path); err == nil {
		var old tokenCoefFile
		if json.Unmarshal(data, &old) == nil && old.Schema == tokenCoefSchema && old.Models != nil {
			f.Models = old.Models
		}
	}
	key := c.Model
	if key == "" {
		key = "default"
	}
	f.Models[key] = c
	f.ActiveModel = key
	if err := os.MkdirAll(PDir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveTokenCoefs 是 SaveTokenCoefs 的 Manager 包装（失败只记日志：校准不该阻断请求）。
func (m *Manager) saveTokenCoefs(c *TokenCoefSet) {
	if err := SaveTokenCoefs(m.sessionDir(), c); err != nil {
		m.logger.Warn("token coefficient save failed", "error", err)
	}
}

// LoadUsageSamples 读取目录下所有 *.usage.jsonl 的校准样本（供重拟合/离线工具共用）。
func LoadUsageSamples(dir string) ([]UsageSample, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []UsageSample
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), usageLedgerSuffix) {
			continue
		}
		samples, err := readUsageLedger(PJoin(dir, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, samples...)
	}
	return out, nil
}

// readUsageLedger 读单个账本文件。
func readUsageLedger(path string) ([]UsageSample, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []UsageSample
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var s UsageSample
		if json.Unmarshal([]byte(line), &s) == nil && s.Model != "" && s.Sent > 0 && s.Actual > 0 {
			out = append(out, s)
		}
	}
	return out, nil
}

// FitTokenCoefficients 用账本样本拟合一套系数（只针对一个模型）。
//
// 模型：actual ≈ Σ w_i·class_i（i = 7 类），带列归一化的最小二乘 + 轻量岭。
// 列归一化是必需的——类别计数在 1e4 量级、常数项在 1e0 量级，不归一化时正规方程
// 条件数极大，会解出「负几千」这种物理上不可能的权重。
//
// 拒绝条件（返回 error，调用方保持现用系数）：
//   - 样本不足；
//   - 主类别（字母/空白/数字/标点/宽字符）里有累计质量不足的——那一列的权重不可信；
//   - 拟合残差没有优于现用系数（改了半天还更差，没有理由改）；
//   - 解出的权重越界。
func FitTokenCoefficients(model string, samples []UsageSample, current *TokenCoefSet) (*TokenCoefSet, error) {
	rows := make([]UsageSample, 0, len(samples))
	for _, s := range samples {
		if s.Model == model && s.Sent > 0 && s.Actual > 0 {
			rows = append(rows, s)
		}
	}
	if len(rows) < tokenCoefMinSamples {
		return nil, fmt.Errorf("样本不足（%d < %d）", len(rows), tokenCoefMinSamples)
	}

	var mass [7]int
	for _, r := range rows {
		for i := 0; i < 7; i++ {
			mass[i] += r.Classes[i]
		}
	}
	// 各类字符总量：够了的才信自己的拟合值，不够的保留现用权重。
	//
	// 不因此整份拒绝——emoji、生僻符号在很多账本里压根没有，若「有一类没数据就拒绝」，
	// 就永远拟合不出任何东西。缺证据时保持原值，是校准应有的保守。
	keep := [7]bool{}
	for i := 0; i < 7; i++ {
		keep[i] = mass[i] < tokenCoefMinClassMass
	}

	n := 7
	X := make([][]float64, 0, len(rows))
	y := make([]float64, 0, len(rows))
	for _, r := range rows {
		row := make([]float64, n)
		for i := 0; i < n; i++ {
			row[i] = float64(r.Classes[i])
		}
		X = append(X, row)
		y = append(y, float64(r.Actual))
	}
	coef := solveNormalEquations(X, y)
	if coef == nil {
		return nil, fmt.Errorf("正规方程退化，拟合失败")
	}

	base := DefaultTokenCoefs()
	if current != nil {
		base = current
	}
	fit := &TokenCoefSet{Model: model, Samples: len(rows), FittedAt: time.Now().Unix()}
	for i := 0; i < n; i++ {
		if keep[i] {
			fit.PerMille[i] = base.PerMille[i]
			continue
		}
		fit.PerMille[i] = coef[i] * 1000
	}
	if !validCoefs(fit.PerMille) {
		return nil, fmt.Errorf("拟合结果越界（%v）", fit.PerMille)
	}

	fitErr := residualMedian(X, y, fit.PerMille)
	curErr := math.MaxFloat64
	if current != nil {
		curErr = residualMedian(X, y, current.PerMille)
	}
	if fitErr > curErr {
		return nil, fmt.Errorf("拟合残差 %.3f 不优于现用 %.3f", fitErr, curErr)
	}
	fit.Residual = fitErr
	return fit, nil
}

// residualMedian 返回 pred/actual 与 1 的中位绝对偏差。
func residualMedian(X [][]float64, y []float64, perMille [7]float64) float64 {
	if len(X) == 0 {
		return math.MaxFloat64
	}
	devs := make([]float64, 0, len(X))
	for i, row := range X {
		pred := 0.0
		for j := 0; j < 7; j++ {
			pred += row[j] * perMille[j] / 1000
		}
		if pred <= 0 || y[i] <= 0 {
			continue
		}
		devs = append(devs, math.Abs(pred/y[i]-1))
	}
	if len(devs) == 0 {
		return math.MaxFloat64
	}
	sort.Float64s(devs)
	return devs[len(devs)/2]
}

// solveNormalEquations 解带列归一化与轻量岭的最小二乘。
func solveNormalEquations(X [][]float64, y []float64) []float64 {
	if len(X) == 0 {
		return nil
	}
	n := len(X[0])
	scale := make([]float64, n)
	for j := 0; j < n; j++ {
		sum := 0.0
		for r := range X {
			sum += X[r][j] * X[r][j]
		}
		scale[j] = math.Sqrt(sum)
		if scale[j] < 1e-9 {
			scale[j] = 1
		}
	}
	A := make([][]float64, n)
	v := make([]float64, n)
	for i := 0; i < n; i++ {
		A[i] = make([]float64, n)
	}
	const ridge = 1e-6
	for r := range X {
		for i := 0; i < n; i++ {
			xi := X[r][i] / scale[i]
			v[i] += xi * y[r]
			for j := 0; j < n; j++ {
				A[i][j] += xi * (X[r][j] / scale[j])
			}
		}
	}
	for i := 0; i < n; i++ {
		A[i][i] += ridge
	}
	for i := 0; i < n; i++ {
		p := i
		for r := i + 1; r < n; r++ {
			if math.Abs(A[r][i]) > math.Abs(A[p][i]) {
				p = r
			}
		}
		A[i], A[p] = A[p], A[i]
		v[i], v[p] = v[p], v[i]
		if math.Abs(A[i][i]) < 1e-12 {
			return nil
		}
		for r := i + 1; r < n; r++ {
			f := A[r][i] / A[i][i]
			for c := i; c < n; c++ {
				A[r][c] -= f * A[i][c]
			}
			v[r] -= f * v[i]
		}
	}
	x := make([]float64, n)
	for i := n - 1; i >= 0; i-- {
		s := v[i]
		for c := i + 1; c < n; c++ {
			s -= A[i][c] * x[c]
		}
		x[i] = s / A[i][i]
	}
	for j := 0; j < n; j++ {
		x[j] /= scale[j]
	}
	return x
}

// RefitTokenCoefs 用账本重拟合并在门槛通过时落盘（自动重拟合的入口）。
//
// 不改变**当前进程**的生效系数：宿主与已启动的插件必须用同一套（见文件头注释）。
// 新系数从下一个进程开始生效，日志会说明。
func (m *Manager) RefitTokenCoefs(force bool) (*TokenCoefSet, error) {
	if m == nil {
		return nil, fmt.Errorf("nil manager")
	}
	model := m.modelName
	if model == "" {
		return nil, fmt.Errorf("模型名未知，跳过拟合")
	}
	samples, err := LoadUsageSamples(m.sessionDir())
	if err != nil {
		return nil, err
	}
	fit, err := FitTokenCoefficients(model, samples, ActiveTokenCoefs())
	if err != nil {
		if force {
			m.logger.Info("token coefficient refit skipped", "model", model, "reason", err.Error())
		}
		return nil, err
	}
	m.saveTokenCoefs(fit)
	m.logger.Info("token coefficients refitted (applies to the next start)",
		"model", model, "samples", fit.Samples,
		"residual", fmt.Sprintf("%.3f", fit.Residual),
		"per_mille", fmt.Sprintf("%.0f", fit.PerMille),
		"path", m.tokenCoefPath())
	return fit, nil
}
