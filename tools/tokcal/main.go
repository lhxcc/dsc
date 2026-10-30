// tokcal 从会话账本（<sessions>/*.usage.jsonl）重拟合 token 估算系数，并按模型输出
// 校准表与漂移诊断。
//
// 为什么需要它：`core/token_meter.go` 里那组千分权重是我们对分词器的近似。运行期只能
// 学到一个**整体比例**（token-calibration.json 里的 factor），学不到「哪一类字符估错了」。
// 账本里每帧都记了估算、实测和各类字符计数，够做一次真正的最小二乘——把「差多少」升级
// 成「差在哪种内容上」。
//
// 用法：
//
//	tokcal -dir E:/Programs/dsc-for-windows-amd64/sessions
//	tokcal -dir ... -model cn:deepseek-v4.1-flash -min 50
//
// 输出三部分：
//  1. 每个模型的样本数、当前比例中位数、离散度（p90/p10）、最近 50 帧 vs 最早 50 帧的漂移；
//  2. 用账本重拟合的千分系数，与现用系数并排对比（含拟合后的残差中位数）；
//  3. 若某模型的拟合结果与现用系数差得远，给出「是否值得改常量」的判断依据。
//
// 它只读文件、不改运行期状态：系数是要人确认后写回 `tokPerMille*` 的——自动改写估算器
// 会让「同一份历史在不同机器上算出不同窗口占用」，那比估偏更难排查。
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"

	"dsc/core"
)

// usageSample 与 core.UsageSample 的 JSON 形状一致（这里只解码需要的字段，
// 工具不依赖 core 的私有实现）。
type usageSample struct {
	Model   string `json:"model"`
	Sent    int    `json:"sent"`
	Actual  int    `json:"actual"`
	Msgs    int    `json:"msgs"`
	MsgEst  int    `json:"msg_est"`
	ToolEst int    `json:"tool_est"`
	Classes [7]int `json:"classes"`
}

func main() {
	dir := flag.String("dir", "", "会话目录（含 *.usage.jsonl）；缺省取 dsc 的 sessions 目录")
	only := flag.String("model", "", "只分析指定模型")
	minSamples := flag.Int("min", 20, "参与拟合的最少样本数")
	flag.Parse()

	sessionsDir := *dir
	if sessionsDir == "" {
		sessionsDir = defaultSessionsDir()
	}
	samples, err := loadSamples(sessionsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取账本失败: %v\n", err)
		os.Exit(1)
	}
	if len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "%s 下没有 *.usage.jsonl 样本（先跑几次真实请求）\n", sessionsDir)
		os.Exit(1)
	}

	byModel := map[string][]usageSample{}
	for _, s := range samples {
		byModel[s.Model] = append(byModel[s.Model], s)
	}
	names, current := core.EstimatorCoefficients()

	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	fmt.Printf("会话目录: %s\n样本总数: %d\n\n", sessionsDir, len(samples))
	for _, model := range models {
		if *only != "" && model != *only {
			continue
		}
		reportModel(model, byModel[model], names, current, *minSamples)
	}
}

// reportModel 打印单个模型的校准报告。
func reportModel(model string, rows []usageSample, names []string, current []int, minSamples int) {
	ratios := make([]float64, 0, len(rows))
	for _, r := range rows {
		if r.Sent > 0 && r.Actual > 0 {
			ratios = append(ratios, float64(r.Actual)/float64(r.Sent))
		}
	}
	if len(ratios) == 0 {
		return
	}
	sort.Float64s(ratios)
	median := ratios[len(ratios)/2]
	p10, p90 := ratios[len(ratios)/10], ratios[9*len(ratios)/10]

	fmt.Printf("=== %s ===\n", model)
	fmt.Printf("  样本 %d 帧；比值 中位 %.3f  p10 %.3f  p90 %.3f  离散度 %.2f\n",
		len(ratios), median, p10, p90, safeDiv(p90, p10))

	if len(ratios) >= 100 {
		first, last := medianRange(ratios[:50]), medianRange(ratios[len(ratios)-50:])
		drift := last/first - 1
		flag := ""
		if drift > 0.10 || drift < -0.10 {
			flag = "  ← 漂移显著（内容构成变了，或分词器/模板变了）"
		}
		fmt.Printf("  漂移：最早 50 帧 %.3f → 最近 50 帧 %.3f（%+.1f%%）%s\n",
			first, last, drift*100, flag)
	}

	if len(rows) < minSamples {
		fmt.Printf("  样本不足 %d 帧，跳过系数重拟合\n\n", minSamples)
		return
	}

	// 最小二乘：actual ≈ Σ w_i·类_i + b·msgs + c·toolEst
	// 类别权重是主项；msgs 与 toolEst 单独作为列，避免把消息结构开销摊进字符权重里。
	ncol := len(names) + 2
	X := make([][]float64, 0, len(rows))
	y := make([]float64, 0, len(rows))
	for _, r := range rows {
		if r.Actual <= 0 || r.Sent <= 0 {
			continue
		}
		row := make([]float64, ncol)
		for i := 0; i < len(names); i++ {
			row[i] = float64(r.Classes[i])
		}
		row[len(names)] = float64(r.Msgs)
		row[len(names)+1] = float64(r.ToolEst)
		X = append(X, row)
		y = append(y, float64(r.Actual))
	}
	coef := solveLMS(X, y)
	if coef == nil {
		fmt.Printf("  拟合失败（矩阵退化），原样保留现用系数\n\n")
		return
	}

	// 还原成「每千字符」口径（类权重 × 1000），便于与常量对比
	fmt.Printf("  %-8s %10s %10s %8s\n", "class", "现用/千", "拟合/千", "变化")
	maxDelta := 0.0
	unstable := false
	for i, name := range names {
		fit := coef[i] * 1000
		delta := fit/float64(current[i]) - 1
		if abs(delta) > abs(maxDelta) {
			maxDelta = delta
		}
		// 系数落在物理上不可能的区间（负值、几千/字符），或者类别样本太少，
		// 都说明这一列不可信——多数时候是「哪一类几乎没出现」或「两列共线」。
		thin := 0
		for _, r := range rows {
			thin += r.Classes[i]
		}
		if fit < 0 || fit > 4000 || thin < 500 {
			unstable = true
			fmt.Printf("  %-8s %10d %10.0f %+7.0f%%  ← 不可信（该列字符数 %d，或与其他列共线）\n",
				name, current[i], fit, delta*100, thin)
			continue
		}
		fmt.Printf("  %-8s %10d %10.0f %+7.0f%%\n", name, current[i], fit, delta*100)
	}
	fmt.Printf("  每消息 %.2f tok/条（现用 4/6）；工具目录系数 %.3f\n",
		coef[len(names)], coef[len(names)+1])

	// 残差：拟合值 vs 实测
	res := make([]float64, 0, len(X))
	for i := range X {
		pred := 0.0
		for j := range X[i] {
			pred += coef[j] * X[i][j]
		}
		if pred > 0 {
			res = append(res, pred/y[i])
		}
	}
	sort.Float64s(res)
	if len(res) > 0 {
		fmt.Printf("  拟合后 pred/actual：中位 %.3f  p10 %.3f  p90 %.3f\n",
			res[len(res)/2], res[len(res)/10], res[9*len(res)/10])
	}
	switch {
	case unstable:
		fmt.Printf("  提示：存在不可信的列（负值/超界/样本过少）。这通常是共线（例如中文与全角标点总同现）\n" +
			"        或该类别几乎没出现；**不要**据此改常量，先把样本凑足或按内容类型分开取样。\n")
	case abs(maxDelta) > 0.25:
		fmt.Printf("  提示：最大类别偏差 %+.0f%%，可考虑把该模型为主的部署改用拟合系数\n", maxDelta*100)
	}
	fmt.Println()
}

// loadSamples 读取目录下所有 *.usage.jsonl。
func loadSamples(dir string) ([]usageSample, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []usageSample
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".usage.jsonl") {
			continue
		}
		f, err := os.Open(core.PJoin(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var s usageSample
			if json.Unmarshal(sc.Bytes(), &s) == nil && s.Model != "" && s.Sent > 0 && s.Actual > 0 {
				out = append(out, s)
			}
		}
		f.Close()
	}
	return out, nil
}

// defaultSessionsDir 取 dsc 的会话目录（与宿主一致：<ExecDir>/sessions）。
func defaultSessionsDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "sessions"
	}
	return core.PJoin(core.PDir(exe), "sessions")
}

// solveLMS 解最小二乘（正规方程 + 轻量岭 + 列归一化），返回 nil 表示退化。
//
// 列归一化不是优化，是必需品：类别计数在 1e4 量级、消息数在 1e2 量级，直接解正规方程
// 时条件数极大，高斯消元会放大舍入误差，系数出现「负几千」这类显然错误的值。
func solveLMS(X [][]float64, y []float64) []float64 {
	if len(X) == 0 {
		return nil
	}
	n := len(X[0])
	// 每列按 L2 范数归一到单位长度；解完再折算回去。
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
	const ridge = 1e-3
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
	// 高斯消元（部分主元）
	for i := 0; i < n; i++ {
		p := i
		for r := i + 1; r < n; r++ {
			if abs(A[r][i]) > abs(A[p][i]) {
				p = r
			}
		}
		A[i], A[p] = A[p], A[i]
		v[i], v[p] = v[p], v[i]
		if abs(A[i][i]) < 1e-12 {
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

func medianRange(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	return v[len(v)/2]
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
