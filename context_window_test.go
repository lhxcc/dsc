package main

import "testing"

// TestResolveContextWindow 窗口三个来源的优先级：**实测容量是上限，配置只能往下调**。
//
// 现场：云端把模型宣传成 1M，用户在 128K 后面补个 0 写成 1310720；或本地 llama.cpp
// 实际 n_ctx=131072 而配置写了 1310720。放任不管，每轮都要靠 provider 的 4xx 教一遍。
func TestResolveContextWindow(t *testing.T) {
	cases := []struct {
		name               string
		configured, probed int
		want               int
		wantSource         string
		wantClamp          bool
	}{
		{"云端无探测：按配置", 1000000, 0, 1000000, "config", false},
		{"配置超过服务端容量：按服务端", 1310720, 131072, 131072, "probe-clamped", true},
		{"配置低于服务端容量：尊重配置", 32768, 131072, 32768, "config", false},
		{"未配置：取探测值", 0, 262144, 262144, "probe", false},
		{"都没有：默认 128K", 0, 0, defaultContextWindow, "default", false},
	}
	for _, c := range cases {
		got, source, clamped := resolveContextWindow(c.configured, c.probed, defaultContextWindow)
		if got != c.want || source != c.wantSource || clamped != c.wantClamp {
			t.Errorf("%s: resolveContextWindow(%d,%d) = (%d,%q,%v), want (%d,%q,%v)",
				c.name, c.configured, c.probed, got, source, clamped, c.want, c.wantSource, c.wantClamp)
		}
	}
}
