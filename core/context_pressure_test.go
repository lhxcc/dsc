package core

import (
	"testing"

	"dsc/proto"
)

// projectPressure 是「本轮压力」的唯一合成点（对齐 DSH tokenMeter.measure 的
// baseline + surfaceDelta）。四条分支必须各自钉住，否则压缩会在错误的时机触发：
//  1. 信封不可比（工具目录变了 / 旧记录无指纹）→ 纯估算；
//  2. 可比且用量 >= 当次估算（请求未被改写）→ 用量 + 表面差量；
//  3. 可比但用量 < 当次估算（那次请求被压缩/改写）→ 退回估算；
//  4. 窗口变了 → 不可比。
func TestProjectPressure(t *testing.T) {
	const cur = 923391 // 当前表面：消息 913354 + 工具目录 10037

	// 1) 信封不可比
	for _, a := range []sessionUsageAnchor{
		{promptTokens: 40347, sentEstimate: 37790, toolsHash: ""},    // 旧记录无指纹
		{promptTokens: 40347, sentEstimate: 37790, toolsHash: "old"}, // 工具目录变了
	} {
		got, matched := projectPressure(a, cur, "new", "", 1048576)
		if matched {
			t.Fatalf("信封不同却判定可比: %+v", a)
		}
		if got != cur {
			t.Fatalf("不可比时应退回纯估算 %d, got %d", cur, got)
		}
	}

	// 2) 可比且用量不低于当次估算 → 实测底数 + 差量
	a := sessionUsageAnchor{promptTokens: 500000, sentEstimate: 480000, toolsHash: "h", contextWindow: 1048576}
	got, matched := projectPressure(a, cur, "h", "m", 1048576)
	if !matched {
		t.Fatal("同信封应判定可比")
	}
	if want := 500000 + (cur - 480000); got != want {
		t.Fatalf("投影 = %d, want %d（实测 500000 + 差量 %d）", got, want, cur-480000)
	}

	// 3) 可比但用量远低于当次估算（上一次被压缩后端改写，如 42k vs 900k）→ 不可当底数
	b := sessionUsageAnchor{promptTokens: 40347, sentEstimate: 900000, toolsHash: "h", contextWindow: 1048576}
	got, matched = projectPressure(b, cur, "h", "m", 1048576)
	if !matched {
		t.Fatal("信封相同应判定可比（即使不采用用量）")
	}
	if got != cur {
		t.Fatalf("用量低于该信封的启发式价时应退回估算 %d, got %d", cur, got)
	}

	// 4) 窗口变了（换模型）→ 不可比
	c := sessionUsageAnchor{promptTokens: 500000, sentEstimate: 480000, toolsHash: "h", contextWindow: 131072}
	if got, matched = projectPressure(c, cur, "h", "m", 1048576); matched || got != cur {
		t.Fatalf("窗口不同应不可比: got=%d matched=%v", got, matched)
	}

	// 5) 换模型（窗口/工具目录相同）→ 不可比
	e := sessionUsageAnchor{promptTokens: 500000, sentEstimate: 480000, toolsHash: "h", model: "m1", contextWindow: 1048576}
	if got, matched = projectPressure(e, cur, "h", "m2", 1048576); matched || got != cur {
		t.Fatalf("换模型应不可比: got=%d matched=%v", got, matched)
	}
	// 锚点未记录模型（旧日志）→ 不参与比较，仍按信封其余字段判定
	e2 := sessionUsageAnchor{promptTokens: 500000, sentEstimate: 480000, toolsHash: "h", contextWindow: 1048576}
	if _, matched = projectPressure(e2, cur, "h", "m2", 1048576); !matched {
		t.Fatal("锚点无模型信息时不应仅因此判定不可比")
	}

	// 6) 表面缩小 → 允许负差量，但不低于 0
	d := sessionUsageAnchor{promptTokens: 1000, sentEstimate: 2000, toolsHash: "h"}
	if got, _ = projectPressure(d, 0, "h", "", 0); got != 0 {
		t.Fatalf("负差量应被夹到 0, got %d", got)
	}
}

// ToolsFingerprint 决定「能否沿用上次用量」：同一工具集必须稳定（顺序无关），
// 任何字段变化必须改变指纹，否则会在工具描述悄悄变化后继续用过期锚点。
func TestToolsFingerprint(t *testing.T) {
	a := &proto.Tool{Name: "read_file", Description: "read", ParametersJson: `{"type":"object"}`}
	b := &proto.Tool{Name: "shell", Description: "run", ParametersJson: `{"type":"object"}`}

	if ToolsFingerprint(nil) != "" {
		t.Fatal("空工具目录应返回空指纹（视为无信封信息）")
	}
	f1 := ToolsFingerprint([]*proto.Tool{a, b})
	f2 := ToolsFingerprint([]*proto.Tool{b, a}) // 顺序不同
	if f1 == "" || f1 != f2 {
		t.Fatalf("指纹应与顺序无关: %q vs %q", f1, f2)
	}

	changedDesc := &proto.Tool{Name: "read_file", Description: "read a file", ParametersJson: `{"type":"object"}`}
	if ToolsFingerprint([]*proto.Tool{changedDesc, b}) == f1 {
		t.Fatal("描述变化必须改变指纹（否则会沿用过期锚点）")
	}
	changedSchema := &proto.Tool{Name: "read_file", Description: "read", ParametersJson: `{"type":"object","properties":{}}`}
	if ToolsFingerprint([]*proto.Tool{changedSchema, b}) == f1 {
		t.Fatal("参数 schema 变化必须改变指纹")
	}
}
