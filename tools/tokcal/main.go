// tokcal 从会话账本（<sessions>/*.usage.jsonl）诊断并按模型重拟合 token 估算系数。
//
// 运行期已经会自动拟合（每 500 帧一次，见 core/token_coef.go），这个工具的价值在于
// **看得见**：每个模型的样本量、比值分布、漂移、拟合结果与门槛拒绝的原因。
//
// 用法：
//
//	tokcal -dir E:/Programs/dsc-for-windows-amd64/sessions
//	tokcal -dir ... -model cn:deepseek-v4.1-flash -write   # 立即写入，下次启动生效
//
// 注意：-write 只写文件，不改当前进程；宿主/插件在下次启动时读到同一份。
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"

	"dsc/core"
)

func main() {
	dir := flag.String("dir", "", "会话目录（含 *.usage.jsonl）；缺省取可执行文件旁的 sessions")
	only := flag.String("model", "", "只分析指定模型")
	write := flag.Bool("write", false, "拟合通过时写入 token-coefficients.json（下次启动生效）")
	flag.Parse()

	sessionsDir := *dir
	if sessionsDir == "" {
		sessionsDir = defaultSessionsDir()
	}
	samples, err := core.LoadUsageSamples(sessionsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取账本失败: %v\n", err)
		os.Exit(1)
	}
	if len(samples) == 0 {
		fmt.Fprintf(os.Stderr, "%s 下没有 *.usage.jsonl 样本（先跑几次真实请求）\n", sessionsDir)
		os.Exit(1)
	}

	byModel := map[string][]core.UsageSample{}
	for _, s := range samples {
		byModel[s.Model] = append(byModel[s.Model], s)
	}
	models := make([]string, 0, len(byModel))
	for m := range byModel {
		models = append(models, m)
	}
	sort.Strings(models)

	names, active := core.EstimatorCoefficients()
	fmt.Printf("会话目录: %s\n样本总数: %d\n", sessionsDir, len(samples))
	fmt.Printf("生效系数: ")
	for i, n := range names {
		fmt.Printf("%s=%.0f ", n, active[i])
	}
	fmt.Println()
	if c := core.ActiveTokenCoefs(); c.Model != "" {
		fmt.Printf("（来自模型 %s 的拟合：%d 帧，残差 %.3f）\n\n", c.Model, c.Samples, c.Residual)
	} else {
		fmt.Printf("（内置默认）\n\n")
	}

	for _, model := range models {
		if *only != "" && model != *only {
			continue
		}
		reportModel(model, byModel[model], names, active, sessionsDir, *write)
	}
}

// reportModel 打印单个模型的分布、漂移与拟合结论。
func reportModel(model string, rows []core.UsageSample, names []string, active []float64, dir string, write bool) {
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

	fmt.Printf("=== %s ===\n", model)
	fmt.Printf("  样本 %d 帧；比值 中位 %.3f  p10 %.3f  p90 %.3f\n",
		len(ratios), ratios[len(ratios)/2], ratios[len(ratios)/10], ratios[9*len(ratios)/10])
	if len(ratios) >= 100 {
		first := ratios[50]
		last := ratios[len(ratios)-50]
		drift := last/first - 1
		mark := ""
		if drift > 0.10 || drift < -0.10 {
			mark = "  ← 漂移显著（内容构成或分词器在变）"
		}
		fmt.Printf("  漂移：最早 50 帧 %.3f → 最近 50 帧 %.3f（%+.1f%%）%s\n", first, last, drift*100, mark)
	}

	fit, err := core.FitTokenCoefficients(model, rows, core.ActiveTokenCoefs())
	if err != nil {
		fmt.Printf("  未重拟合：%v\n\n", err)
		return
	}
	fmt.Printf("  拟合（%d 帧，残差 %.3f）：\n", fit.Samples, fit.Residual)
	for i, n := range names {
		delta := fit.PerMille[i]/active[i] - 1
		fmt.Printf("    %-7s 现用 %5.0f  拟合 %5.0f  %+6.0f%%\n", n, active[i], fit.PerMille[i], delta*100)
	}
	if write {
		if err := core.SaveTokenCoefs(dir, fit); err != nil {
			fmt.Printf("  写入失败：%v\n\n", err)
			return
		}
		fmt.Printf("  已写入 %s/token-coefficients.json（下次启动生效）\n\n", dir)
		return
	}
	fmt.Printf("  （加 -write 写入；运行期也会每 500 帧自动尝试）\n\n")
}

// defaultSessionsDir 取可执行文件旁的 sessions（与宿主同一规则）。
func defaultSessionsDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "sessions"
	}
	return core.PJoin(core.PDir(exe), "sessions")
}
