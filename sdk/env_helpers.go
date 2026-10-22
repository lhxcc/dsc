package dsc

import (
	"os"
	"strconv"
	"strings"
)

// 通用 env helper：把「读 env + 解析 + 兜底」三步合一，消除各插件散落的
// `if v := os.Getenv(...); v != "" { if n, err := strconv.Atoi(v); err == nil && n > 0 { ... } }`
// 模式。所有 helper 容忍前后空白；解析失败或非法值回落 def，不报错（env 是
// 用户输入，按 fail-soft 处理；需要 fail-loud 的调用方自行校验返回值）。

// EnvStr 读取字符串 env；缺席返回 def。
func EnvStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// EnvWithFallback 读取「DSC_ 前缀优先、旧名回退」的字符串 env；两者都缺席返回 def。
//
// 约定：DSC_<P>_* 是正式名（避免与终端同名变量冲突），无前缀旧名保留兼容。
// 与 LoadLLMConfig 的 API_KEY/BASE_URL/MODEL 三键整组策略不同，开关类变量各自
// 独立判断——它们不构成一个组，也不存在新旧混搭互相矛盾的问题。
func EnvWithFallback(primary, legacy, def string) string {
	if v := strings.TrimSpace(os.Getenv(primary)); v != "" {
		return v
	}
	if legacy != "" {
		if v := strings.TrimSpace(os.Getenv(legacy)); v != "" {
			return v
		}
	}
	return def
}

// EnvLookupSet 返回第一个「已设置且非空白」的 env 值；都未设置返回 ("", false)。
// 与 EnvWithFallback 的差别：调用方能区分「未设置」与「显式给了值（含 0）」，
// 后者对输出上限这类语义很关键——显式 0 表示不携带，不能被兜底默认值覆盖。
func EnvLookupSet(keys ...string) (string, bool) {
	for _, k := range keys {
		v, ok := os.LookupEnv(k)
		if !ok {
			continue
		}
		if v = strings.TrimSpace(v); v != "" {
			return v, true
		}
	}
	return "", false
}

// EnvInt 读取 int env；缺席/非法返回 def。
func EnvInt(key string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return def
	}
	return v
}

// EnvInt64 读取 int64 env；缺席/非法返回 def。
func EnvInt64(key string, def int64) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(os.Getenv(key)), 10, 64)
	if err != nil {
		return def
	}
	return v
}

// EnvFloat 读取 float64 env（用于比例/阈值类配置，如压缩触发阈值 0.80）；
// 缺席/非法返回 def。
func EnvFloat(key string, def float64) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(key)), 64)
	if err != nil {
		return def
	}
	return v
}

// EnvBool 读取 bool env；空串/0/false/no/off 返回 false，其余非空返回 true。
// 显式 true/1/yes/on 与隐式非空都视为 true（与既有 envBool 语义一致）。
func EnvBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "0", "false", "no", "off":
		return false
	case "1", "true", "yes", "on":
		return true
	}
	return def
}

// EnvPositiveInt 读取正整数 env（>0）；缺席/非法/非正返回 0。
// 对齐既有插件散落的 `parsePositiveInt` 模式。
func EnvPositiveInt(key string) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// EnvPositiveInt64 读取正 int64 env（>0）；缺席/非法/非正返回 0。
// 对齐既有插件散落的 `parsePositiveInt64` 模式。
func EnvPositiveInt64(key string) int64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}
