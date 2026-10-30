package main

import (
	"strings"
	"testing"
)

// TestArgsSyntaxProblemPointsAtTheBreak 校验给模型的定位信息：不是「不合法」，而是
// 「断在第几个字节、断点前后写的是什么」。
//
// 这条消息会作为工具结果留在上下文里，所以两件事都要是硬的：位置对得上（模型能据此判断
// 漏了哪个括号），以及长度有界（不能把整段参数再复制一遍，此后每轮重放都付费）。
func TestArgsSyntaxProblemPointsAtTheBreak(t *testing.T) {
	// 现场那份：缺 </parameter>，参数断在末尾。
	broken := `{"command":"cd F:/X && sed -i '968d' a.xml <parameter=description> Delete line 968 </tool_call>`
	got := argsSyntaxProblem(broken)
	if !strings.Contains(got, "突然结束") {
		t.Errorf("断在末尾应明说「突然结束」（漏写闭合标签的典型形状）; got: %s", got)
	}
	if !strings.Contains(got, "Delete line 968") {
		t.Errorf("应给出断点附近的内容，模型才知道停在哪; got: %s", got)
	}
	if len(got) > 240 {
		t.Errorf("描述过长 (%d 字节)：它会永久留在上下文里; got: %s", len(got), got)
	}

	// 中途出错（多了一个逗号）：应报具体字节位置，而不是笼统的「不合法」。
	mid := `{"a":1,}`
	gotMid := argsSyntaxProblem(mid)
	if !strings.Contains(gotMid, "无法继续解析") || !strings.Contains(gotMid, "第 8 个字节") {
		t.Errorf("中途出错应给出字节位置; got: %s", gotMid)
	}

	// 空参数与合法参数各自有话说，且都不能 panic。
	if s := argsSyntaxProblem(""); s == "" {
		t.Error("空参数也要有说明（否则回执里是个空洞）")
	}
	if s := argsSyntaxProblem(`{"a":1}`); s == "" {
		t.Error("合法参数走到这里也该有说明")
	}
}

// TestMalformedArgsReasonMentionsTruncationOnlyWhenItHappened 区分两种成因。
//
// 该给模型的建议不同：截断是它自己的输出被限额切断（继续写），标签漏写是格式问题
// （重发时补括号）。合成一句话会让它以为重试就行，于是把同一段残句再发一遍。
func TestMalformedArgsReasonMentionsTruncationOnlyWhenItHappened(t *testing.T) {
	if s := truncationCause(true, "max_tokens"); !strings.Contains(s, "截断") {
		t.Errorf("截断时应点明; got: %s", s)
	}
	if s := truncationCause(false, "tool_calls"); strings.Contains(s, "被截断") {
		t.Errorf("未截断时不得说成截断; got: %s", s)
	}
}
