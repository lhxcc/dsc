package main

import (
	"testing"

	"dsc/proto"
)

func hasToolNamed(tools []*proto.Tool, name string) bool {
	for _, t := range tools {
		if t.GetName() == name {
			return true
		}
	}
	return false
}

// TestFilterContextSearchVisibility 会话检索工具只在「历史装不进窗口」时出现。
//
// 整个历史都在眼前时，检索没有增量价值，白占目录与提示前缀；一旦滑窗开始裁历史，
// 被裁掉的那部分就没有别的入口了。
func TestFilterContextSearchVisibility(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 1000
	a.historyInjection = 0
	tools := []*proto.Tool{
		{Name: contextSearchToolName, Description: "search history", ParametersJson: `{"type":"object"}`},
		{Name: "shell", Description: "run", ParametersJson: `{"type":"object"}`},
	}

	// 历史装得下 → 不暴露
	if got := a.filterContextSearch(tools, 100); hasToolNamed(got, contextSearchToolName) {
		t.Fatal("历史装得进窗口时不该暴露 context_search")
	}
	// 装不下 → 暴露
	if got := a.filterContextSearch(tools, 5000); !hasToolNamed(got, contextSearchToolName) {
		t.Fatal("滑窗开始裁历史时应暴露 context_search")
	}
	if !a.contextSearchSeen {
		t.Fatal("一旦暴露应记住，避免工具目录每步抖动")
	}
	// 之后估算回落也保持常开：工具目录是提示缓存前缀的一部分，增删会让整段前缀失效
	if got := a.filterContextSearch(tools, 100); !hasToolNamed(got, contextSearchToolName) {
		t.Fatal("已暴露的工具应保持常开（不要随估算回落消失）")
	}
}

// TestFilterContextSearchHonoursInjectionOff 显式关闭历史注入时不暴露，也不记状态：
// 那是用户明确表示不要让模型翻旧账，给个检索工具等于绕过这个决定。
func TestFilterContextSearchHonoursInjectionOff(t *testing.T) {
	a := newTestAgent(t)
	a.contextWindow = 1000
	a.historyInjection = -1
	tools := []*proto.Tool{
		{Name: contextSearchToolName, ParametersJson: `{"type":"object"}`},
		{Name: "shell", ParametersJson: `{"type":"object"}`},
	}
	if got := a.filterContextSearch(tools, 5000); hasToolNamed(got, contextSearchToolName) {
		t.Fatal("关闭历史注入时不该暴露 context_search")
	}
	if a.contextSearchSeen {
		t.Fatal("关闭注入时不该记住可见性（否则重新开启后会误暴露）")
	}
}

// TestDropToolByNameKeepsOthers 过滤只去掉目标工具，其余顺序不变；名字不存在时原样返回。
func TestDropToolByNameKeepsOthers(t *testing.T) {
	tools := []*proto.Tool{{Name: "a"}, {Name: "context_search"}, {Name: "c"}}
	got := dropToolByName(tools, contextSearchToolName)
	if len(got) != 2 || got[0].GetName() != "a" || got[1].GetName() != "c" {
		t.Fatalf("应只去掉 context_search，其余保持顺序，got %+v", got)
	}
	// 未命中时返回同一份，不做无谓分配
	if again := dropToolByName(got, "nope"); len(again) != 2 {
		t.Fatalf("未命中应原样返回，got %+v", again)
	}
	// 原切片不被修改（可能与其他快照共享底层数组）
	if len(tools) != 3 || tools[1].GetName() != contextSearchToolName {
		t.Fatalf("不应修改入参切片，got %+v", tools)
	}
}
