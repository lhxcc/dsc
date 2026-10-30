package core

import (
	"math"
	"math/rand"
	"testing"
)

// resetCoefs 还原进程级生效系数（它是全局量，测试之间必须复位）。
func resetCoefs(t *testing.T) {
	t.Helper()
	SetActiveTokenCoefs(DefaultTokenCoefs())
	t.Cleanup(func() { SetActiveTokenCoefs(DefaultTokenCoefs()) })
}

// TestTokenCoefSpecRoundTrip 环境变量形态的往返：7 个数值 + 可选模型名。
func TestTokenCoefSpecRoundTrip(t *testing.T) {
	in := &TokenCoefSet{PerMille: [7]float64{250, 140, 620, 700, 870, 1800, 1000}, Model: "model-a"}
	spec := FormatTokenCoefSpec(in)
	out, err := ParseTokenCoefSpec(spec)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if out.Model != "model-a" {
		t.Fatalf("模型名丢失: %q", out.Model)
	}
	for i := range in.PerMille {
		if math.Abs(out.PerMille[i]-in.PerMille[i]) > 1e-9 {
			t.Fatalf("第 %d 位不一致: %v vs %v", i, out.PerMille[i], in.PerMille[i])
		}
	}
	// 坏输入一律拒绝（宁可退回默认，也不用一套明显坏的系数裁历史）
	for _, bad := range []string{"", "1,2,3", "x,2,3,4,5,6,7", "-1,2,3,4,5,6,7", "1,2,3,4,5,6,99999"} {
		if _, err := ParseTokenCoefSpec(bad); err == nil {
			t.Errorf("应拒绝 %q", bad)
		}
	}
}

// TestEstimatorUsesActiveCoefs 生效系数必须真的作用到估算上（否则整套装配是空壳）。
func TestEstimatorUsesActiveCoefs(t *testing.T) {
	resetCoefs(t)
	text := "abcdefghij0123456789" // 10 字母 + 10 数字
	base := EstimateTextTokens(text)

	// 字母权重翻倍：估算必须跟着涨
	alt := DefaultTokenCoefs()
	alt.PerMille[clsLetter] *= 2
	SetActiveTokenCoefs(alt)
	if got := EstimateTextTokens(text); got <= base {
		t.Fatalf("改系数后估算应变化：base=%d got=%d", base, got)
	}

	SetActiveTokenCoefs(DefaultTokenCoefs())
	if got := EstimateTextTokens(text); got != base {
		t.Fatalf("复位后应回到原值：%d vs %d", got, base)
	}
}

// TestFitTokenCoefficients 用带噪声的合成账本验证最小二乘能还原真值，
// 并守住门槛（样本不足 / 类别质量不足必须拒绝）。
func TestFitTokenCoefficients(t *testing.T) {
	resetCoefs(t)
	const model = "synthetic"
	truth := [7]float64{250, 140, 620, 700, 870, 0, 0}

	rng := rand.New(rand.NewSource(7))
	var samples []UsageSample
	for i := 0; i < 300; i++ {
		// 各类字符量在合理区间内随机组合，总量足够大（class mass 门槛）
		c := [7]int{
			rng.Intn(6000) + 2000, // 字母
			rng.Intn(1200) + 400,  // 空白
			rng.Intn(600) + 100,   // 数字
			rng.Intn(1500) + 300,  // 标点
			rng.Intn(800) + 200,   // CJK
			0, 0,
		}
		actual := 0.0
		for k := 0; k < 7; k++ {
			actual += float64(c[k]) * truth[k] / 1000
		}
		actual *= 1 + (rng.Float64()-0.5)*0.04 // ±2% 噪声
		samples = append(samples, UsageSample{
			Model: model, Sent: int(actual), Actual: int(actual), Msgs: 20, MsgEst: 0, Classes: c,
		})
	}

	fit, err := FitTokenCoefficients(model, samples, DefaultTokenCoefs())
	if err != nil {
		t.Fatalf("拟合应成功: %v", err)
	}
	if fit.Model != model || fit.Samples != 300 {
		t.Fatalf("拟合结果元信息不对: %+v", fit)
	}
	for k := 0; k < 5; k++ {
		got, want := fit.PerMille[k], truth[k]
		if math.Abs(got-want)/want > 0.15 {
			t.Errorf("类别 %d 拟合 %.0f，期望约 %.0f（偏差 >15%%）", k, got, want)
		}
	}
	if fit.Residual > 0.03 {
		t.Errorf("残差应很小，got %.3f", fit.Residual)
	}

	// 样本不足：拒绝
	if _, err := FitTokenCoefficients(model, samples[:50], DefaultTokenCoefs()); err == nil {
		t.Error("样本不足应拒绝")
	}
	// 类别质量不足（CJK 几乎没出现）：该列保持原值，其余照常拟合
	var thin []UsageSample
	for _, s := range samples {
		c := s.Classes
		c[clsWide] = 1
		thin = append(thin, UsageSample{Model: model, Sent: s.Sent, Actual: s.Actual, Classes: c})
	}
	cur := DefaultTokenCoefs()
	fit2, err := FitTokenCoefficients(model, thin, cur)
	if err != nil {
		t.Fatalf("缺一类样本不应整份拒绝: %v", err)
	}
	if fit2.PerMille[clsWide] != cur.PerMille[clsWide] {
		t.Errorf("证据不足的类别应保留原值，got %.0f want %.0f",
			fit2.PerMille[clsWide], cur.PerMille[clsWide])
	}
}

// TestManagerAutoFitsAndApplies 自动闭环：结算够样本 → 重拟合写盘 → 下个实例启动即采用。
func TestManagerAutoFitsAndApplies(t *testing.T) {
	resetCoefs(t)
	dir := t.TempDir()
	const model = "auto-model"

	m := NewManager(&ManagerConfig{ExecDir: dir})
	m.SetModelName(model) // 首次拿到模型名时会尝试拟合（此时没样本，失败无副作用）

	// 造样本：真实权重与默认值明显不同（字母 0.10、标点 1.00）。
	// 各类字符量**独立**随机——若都按同一个 i 线性变化，列之间完全共线，
	// 最小二乘只能解出「合起来对」，单个权重会被摊平（这正是要避免的假拟合）。
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 260; i++ {
		c := [7]int{
			rng.Intn(6000) + 18000,
			rng.Intn(1500) + 3000,
			rng.Intn(800) + 600,
			rng.Intn(2000) + 2000,
			rng.Intn(1200) + 1500,
			0, 0,
		}
		actual := 0.10*float64(c[clsLetter]) + 0.14*float64(c[clsSpace]) +
			0.62*float64(c[clsDigit]) + 1.00*float64(c[clsPunct]) + 0.87*float64(c[clsWide])
		m.recordUsageSample("sess-auto", UsageSample{
			Model: model, Sent: int(actual), Actual: int(actual), Msgs: 20, Classes: c,
		})
	}
	if _, err := m.RefitTokenCoefs(true); err != nil {
		t.Fatalf("应能拟合: %v", err)
	}

	// 下个实例：SetModelName 第一次拿到模型名 → 读盘并生效
	resetCoefs(t)
	m2 := NewManager(&ManagerConfig{ExecDir: dir})
	m2.SetModelName(model)
	active := ActiveTokenCoefs()
	if active.Model != model {
		t.Fatalf("新实例应采用拟合系数（model=%q），got %+v", active.Model, active)
	}
	if math.Abs(active.PerMille[clsLetter]-100) > 25 || math.Abs(active.PerMille[clsPunct]-1000) > 250 {
		t.Fatalf("系数未按账本校正: %v", active.PerMille)
	}
}

// TestOverflowSampleFeedsRatioNotFit provider 400 的报数是一条真实样本（sent↔actual），
// 但**没有构成计数**：它必须进比例校准，不得进系数最小二乘（全零类别列会把权重推高）。
func TestOverflowSampleFeedsRatioNotFit(t *testing.T) {
	resetCoefs(t)
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	sample := UsageSample{Model: "m400", Sent: 1000, Actual: 1170, NoClasses: true}

	m.recordUsageSample("sess", sample)
	m.calibMu.RLock()
	c := m.calib["m400"]
	m.calibMu.RUnlock()
	if c == nil || c.Samples != 1 {
		t.Fatalf("比例校准应吸收这条样本: %+v", c)
	}
	if c.Factor < 1.16 || c.Factor > 1.18 {
		t.Fatalf("比例应约 1.17，got %.3f", c.Factor)
	}

	// 系数拟合：这类样本一律跳过（这里只有它们，所以样本不足）
	if _, err := FitTokenCoefficients("m400", []UsageSample{sample}, DefaultTokenCoefs()); err == nil {
		t.Fatal("无构成计数的样本不应参与系数拟合")
	}
}
