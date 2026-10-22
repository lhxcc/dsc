package main

import (
	"context"
	"testing"
)

// 背景：agent 侧 user-questions 通道此前是惰性 Dial（模型首次提问才连），而 go-plugin
// broker 的通告有约 5 秒窗口，实际使用时早已过期 → Dial 失败 → 模型只看到
// 「no user-questions channel is available」，TUI 根本不弹层。
// 修复后必须「注入即连接」（与 RegisterServices 对聚合 LLM/Tool 的处理同规）。

// TestSetUserQuestionsServiceEagerlyConnects 注入非 0 id 时应立即尝试连接：
// broker 不可用时注入必须返回错误（旧实现只记 id、永远返回 nil，正是静默失败来源）。
func TestSetUserQuestionsServiceEagerlyConnects(t *testing.T) {
	a := &ReactLoopAgent{}
	err := a.SetUserQuestionsService(context.Background(), 9)
	if err == nil {
		t.Fatal("broker 不可用时注入应报错——证明注入即连接，而非仅记录 id")
	}
	if a.uqClient != nil {
		t.Fatal("连接失败时不应留下客户端")
	}
}

// TestSetUserQuestionsServiceZeroClearsState 注入 0（无通道）时清空状态且不报错。
func TestSetUserQuestionsServiceZeroClearsState(t *testing.T) {
	a := &ReactLoopAgent{uqServiceID: 7}
	if err := a.SetUserQuestionsService(context.Background(), 0); err != nil {
		t.Fatalf("注入 0 不应报错: %v", err)
	}
	if a.uqServiceID != 0 || a.uqClient != nil || a.uqConn != nil {
		t.Fatalf("注入 0 应清空状态: id=%d client=%v conn=%v", a.uqServiceID, a.uqClient, a.uqConn)
	}
}

// TestDialUserQuestionsGuards dialUserQuestions 在未注入 / broker 缺失时返回错误而非 panic。
func TestDialUserQuestionsGuards(t *testing.T) {
	a := &ReactLoopAgent{}
	if _, err := a.dialUserQuestions(); err == nil {
		t.Fatal("未注入 serviceID 时应返回错误")
	}

	a.uqServiceID = 7
	if _, err := a.dialUserQuestions(); err == nil {
		t.Fatal("broker 为 nil 时应返回错误（不得 nil 解引用）")
	}
}
