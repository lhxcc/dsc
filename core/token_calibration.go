package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"dsc/session"
)

// 分词器校准：把「每一次真实请求」变成可复核的样本，并据此维护一张按模型的校准表。
//
// 为什么需要它：估算公式是我们自己的近似（见 token_meter.go 的分类权重），而 provider
// 的计数是真值。两者的比值 long-run 上是稳定的（分詞器固定、内容分布大致稳定），但会
//
//   - 换模型时整体移动（不同分词器）；
//   - 缓慢漂移（内容构成变化、provider 侧模板调整、模型被替换）；
//   - 在升级后一次性跳变（例如估算系数改了）。
//
// 以前这个比值只活在**宿主内存**里（sessionUsageAnchor），重启即失忆、跨会话不共享，
// 于是每次冷启动的第一个请求都只能按 1 处理——那恰好是历史最厚、最容易撞墙的时刻。
//
// 这里把它落成两份磁盘产物：
//
//  1. `<sessions>/<会话键>.usage.jsonl`：逐请求的账本（估算、实测、构成计数）。
//     只追加、人类可读，既是审计依据，也是「重拟合」的原始数据。
//  2. `<sessions>/token-calibration.json`：按模型聚合的校准表（比值、样本数、离散度、
//     漂移）。启动时读它，冷启动第一帧就有据可依，不必从 1 开始赌。

// UsageSample 一次已结算请求的校准样本。
type UsageSample struct {
	TS      int64  `json:"ts"`
	Session string `json:"session,omitempty"`
	Model   string `json:"model"`
	// Sent 是最终发出表面（消息 + 工具目录）的启发式估算；Actual 是 provider 报的
	// prompt 侧用量。两者之比就是这一帧的偏差。
	Sent   int `json:"sent"`
	Actual int `json:"actual"`
	// 构成拆分：便于以后按内容类别重拟合系数（不是只有整体比例）。
	Msgs    int    `json:"msgs"`
	MsgEst  int    `json:"msg_est"`
	ToolEst int    `json:"tool_est"`
	Classes [7]int `json:"classes"`
}

// modelCalibration 单个模型的校准条目。
type modelCalibration struct {
	Model   string  `json:"model"`
	Factor  float64 `json:"factor"`  // EWMA(actual/sent)，缺失即视为 1
	Last    float64 `json:"last"`    // 最近一帧的比值
	Samples int     `json:"samples"` // 累计样本数
	// Spread 是最近窗口内 p90/p10：比值**离散度**。它变大意味着内容构成在变，
	// 单靠一个比例已经描述不准——那是该重拟合系数的信号。
	Spread    float64 `json:"spread"`
	UpdatedAt int64   `json:"updated_at"`
}

// calibrationFile 是 token-calibration.json 的结构。带 schema：不符即整份丢弃重建
// （AGENTS.md §4：不做逐字段迁移）。
type calibrationFile struct {
	Schema int                          `json:"schema"`
	Models map[string]*modelCalibration `json:"models"`
}

const (
	calibrationSchema     = 1
	calibrationFileName   = "token-calibration.json"
	usageLedgerSuffix     = ".usage.jsonl"
	calibrationWindow     = 32   // 计算离散度的滑动窗口
	calibrationMinSamples = 3    // 少于此数不用该模型的比值
	calibrationAlpha      = 0.20 // EWMA 权重（新样本）
	calibrationDriftWarn  = 0.15 // 比值相对基线偏移超过此值就算漂移
	calibrationMaxRatio   = 8.0  // 单帧比值上限（防错误文本里的数字污染）
	calibrationLedgerCap  = 5000 // 账本超过这么多行就压缩保留最近一半
	usageLedgerMaxBytes   = 2 << 20
)

// calibrationPath 返回校准表路径（与会话日志同目录：同一份运行数据一起备份/清理）。
func (m *Manager) calibrationPath() string {
	return PJoin(m.sessionDir(), calibrationFileName)
}

// loadCalibration 读取校准表；文件缺失/损坏/版本不符一律当空表（不迁移、不猜测）。
func (m *Manager) loadCalibration() {
	path := m.calibrationPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var f calibrationFile
	if err := json.Unmarshal(data, &f); err != nil || f.Schema != calibrationSchema || f.Models == nil {
		m.logger.Warn("token calibration file ignored (schema mismatch or corrupt)",
			"path", path, "schema", f.Schema)
		return
	}
	m.calibMu.Lock()
	m.calib = f.Models
	m.calibMu.Unlock()
	m.logger.Info("token calibration loaded", "path", path, "models", len(f.Models))
}

// saveCalibrationLocked 原子写校准表（临时文件 + rename）。调用方须持有 calibMu。
func (m *Manager) saveCalibrationLocked() {
	path := m.calibrationPath()
	if err := os.MkdirAll(PDir(path), 0o755); err != nil {
		return
	}
	f := calibrationFile{Schema: calibrationSchema, Models: m.calib}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// calibrationFor 返回某模型当前的校准比例；样本不足或未记录时返回 0（调用方按 1 处理）。
//
// 这是**冷启动**的依据：重启后内存锚点没了，但昨天学到的比例还在，第一个请求就不必
// 退回 1 去赌。
func (m *Manager) calibrationFor(model string) float64 {
	if m == nil || model == "" {
		return 0
	}
	m.calibMu.RLock()
	defer m.calibMu.RUnlock()
	c := m.calib[model]
	if c == nil || c.Samples < calibrationMinSamples || c.Factor <= 0 {
		return 0
	}
	return c.Factor
}

// observeRatio 用一帧真实比值更新校准表（EWMA + 离散度 + 漂移检测），并落盘。
//
// 漂移检测的口径：**新样本与本模型当前比例**持续偏离（连续 3 帧 > 15%）才算漂移。
// 单帧偏离不算——一次异常内容（例如整段 base64）本来就会让比值跳一下。
func (m *Manager) observeRatio(sessionID, model string, sample UsageSample) {
	if m == nil || model == "" || sample.Sent <= 0 || sample.Actual <= 0 {
		return
	}
	ratio := float64(sample.Actual) / float64(sample.Sent)
	if ratio < 1.0/calibrationMaxRatio || ratio > calibrationMaxRatio {
		return // 明显是解析错/异常文本，不污染校准
	}

	m.calibMu.Lock()
	if m.calib == nil {
		m.calib = make(map[string]*modelCalibration)
	}
	c := m.calib[model]
	if c == nil {
		c = &modelCalibration{Model: model}
		m.calib[model] = c
	}
	before := c.Factor
	if c.Samples == 0 {
		c.Factor = ratio
	} else {
		c.Factor = c.Factor*(1-calibrationAlpha) + ratio*calibrationAlpha
	}
	c.Last = ratio
	c.Samples++
	c.UpdatedAt = time.Now().Unix()
	m.calibRecent[model] = appendRatio(m.calibRecent[model], ratio)
	if sp := spreadOf(m.calibRecent[model]); sp > 0 {
		c.Spread = sp
	}

	drift := 0.0
	if before > 0 {
		drift = c.Factor/before - 1
	}
	if drift > calibrationDriftWarn || drift < -calibrationDriftWarn {
		m.calibDriftHits[model]++
	} else {
		m.calibDriftHits[model] = 0
	}
	hits := m.calibDriftHits[model]
	spread := c.Spread
	samples := c.Samples
	factor := c.Factor
	m.saveCalibrationLocked()
	m.calibMu.Unlock()

	if hits >= 3 {
		m.logger.Warn("tokenizer calibration drifting",
			"model", model, "factor", fmt.Sprintf("%.3f", factor),
			"previous", fmt.Sprintf("%.3f", before), "samples", samples,
			"spread", fmt.Sprintf("%.2f", spread),
			"hint", "内容构成或分词器在变：整体比例仍可用，但离散度变大时该重拟合估算系数")
	}
}

// appendRatio 把比值追加进滑动窗口（超出窗口丢最旧）。
func appendRatio(rs []float64, r float64) []float64 {
	rs = append(rs, r)
	if len(rs) > calibrationWindow {
		rs = rs[len(rs)-calibrationWindow:]
	}
	return rs
}

// spreadOf 返回窗口内 p90/p10（样本不足或退化时返回 0）。
func spreadOf(rs []float64) float64 {
	if len(rs) < 4 {
		return 0
	}
	sorted := append([]float64(nil), rs...)
	sort.Float64s(sorted)
	p10 := sorted[len(sorted)/10]
	p90 := sorted[9*len(sorted)/10]
	if p10 <= 0 {
		return 0
	}
	return p90 / p10
}

// usageLedgerPath 返回某会话的账本路径。
func (m *Manager) usageLedgerPath(sessionID string) string {
	key := sessionKeyOf(sessionID)
	return PJoin(m.sessionDir(), key+usageLedgerSuffix)
}

// sessionKeyOf 把会话 id 映射成文件名安全的键（与 session.Store 同一套 id 规则：
// id 本身就是安全单组件名，ValidID 拒绝路径穿越）。
func sessionKeyOf(id string) string {
	if session.ValidID(id) {
		return id
	}
	return "unknown"
}

// appendUsageSample 把一帧样本追加到会话账本（失败只记日志，绝不影响请求）。
func (m *Manager) appendUsageSample(sessionID string, sample UsageSample) {
	path := m.usageLedgerPath(sessionID)
	if err := os.MkdirAll(PDir(path), 0o755); err != nil {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size() > usageLedgerMaxBytes {
		compactUsageLedger(path)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	line, err := json.Marshal(sample)
	if err != nil {
		return
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		m.logger.Debug("usage ledger append failed", "path", path, "error", err)
	}
}

// compactUsageLedger 账本过大时保留最近一半（丢最旧）。
func compactUsageLedger(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	f.Close()
	if len(lines) <= calibrationLedgerCap {
		return
	}
	keep := lines[len(lines)-calibrationLedgerCap/2:]
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return
	}
	w := bufio.NewWriter(out)
	for _, ln := range keep {
		fmt.Fprintln(w, ln)
	}
	w.Flush()
	out.Close()
	_ = os.Rename(tmp, path)
}

// recordUsageSample 记录一帧校准样本：写账本 + 更新按模型的校准表。
//
// 调用点在「请求已结算、拿到 provider 用量」这一处（llm_router），因此 sample 里的
// sent 必须是**最终发出表面**的估算——不是 agent 组装的那份、更不是压缩前的。
func (m *Manager) recordUsageSample(sessionID string, sample UsageSample) {
	if m == nil || sample.Actual <= 0 || sample.Sent <= 0 {
		return
	}
	sample.TS = time.Now().Unix()
	sample.Session = sessionID
	m.appendUsageSample(sessionID, sample)
	m.observeRatio(sessionID, sample.Model, sample)
}

// warmCalibrationFromLedgers 在没有任何校准表时，从历史账本重建一份。
//
// 只在首次运行（或校准表被删）时发生：逐个模型取账本里的全部样本，算中位数比值。
// 这样即使校准表丢了，也不必从 1 重新学。
func (m *Manager) warmCalibrationFromLedgers() {
	if m == nil {
		return
	}
	m.calibMu.RLock()
	have := len(m.calib) > 0
	m.calibMu.RUnlock()
	if have {
		return
	}
	dir := m.sessionDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	ratios := make(map[string][]float64)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), usageLedgerSuffix) {
			continue
		}
		f, err := os.Open(PJoin(dir, e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			var s UsageSample
			if json.Unmarshal(sc.Bytes(), &s) != nil || s.Sent <= 0 || s.Actual <= 0 || s.Model == "" {
				continue
			}
			r := float64(s.Actual) / float64(s.Sent)
			if r < 1.0/calibrationMaxRatio || r > calibrationMaxRatio {
				continue
			}
			ratios[s.Model] = append(ratios[s.Model], r)
		}
		f.Close()
	}
	if len(ratios) == 0 {
		return
	}
	m.calibMu.Lock()
	if m.calib == nil {
		m.calib = make(map[string]*modelCalibration)
	}
	for model, rs := range ratios {
		sort.Float64s(rs)
		median := rs[len(rs)/2]
		m.calib[model] = &modelCalibration{
			Model: model, Factor: median, Last: rs[len(rs)-1],
			Samples: len(rs), Spread: spreadOf(rs), UpdatedAt: time.Now().Unix(),
		}
	}
	m.saveCalibrationLocked()
	m.calibMu.Unlock()
	m.logger.Info("token calibration rebuilt from usage ledgers", "models", len(ratios))
}
