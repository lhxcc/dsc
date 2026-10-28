package main

import (
	"context"
	"strings"
	"testing"

	"dsc/proto"
)

// TestEmptyPreStepPayloadDoesNothing 轻载荷（无 messages_json）必须什么都不做。
//
// 宿主只把消息列表发给持有压缩岗位的插件；本插件没当值时每步都会收到不带消息的信封。
// 此时既不能产出改写（空列表没有可压缩的东西），也不能碰压缩记录——那等于让一个不在
// 值班的插件改写自己的状态。
func TestEmptyPreStepPayloadDoesNothing(t *testing.T) {
	s := newCompactionBasicServer()
	s.window = 1_000_000

	out, err := s.handlePreStep(context.Background(), `{"session":"s1","token_count":900000,"projected_tokens":900000}`)
	if err != nil {
		t.Fatalf("空载荷不应报错: %v", err)
	}
	if out != "" {
		t.Fatalf("空载荷不应产出改写，got %.80s", out)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.states) != 0 {
		t.Fatalf("空载荷不应建立压缩记录，实际 %d 个会话", len(s.states))
	}
}

// TestTruncateSummaryDropsAncientHead 退化摘要不得把「被压缩区间的最早一句」搬进摘要。
//
// 一份积累几十 MB 的会话，区间第一句往往是几十轮之前的话题。原样保留会误导模型把它
// 当成当前任务的起点（表现为回答一个早就结束的问题）。摘要的职责是标示「这里被截断」，
// 而不是假装复述原文；与当前工作衔接的是区间末尾。
func TestTruncateSummaryDropsAncientHead(t *testing.T) {
	oldest := "用户最初的需求：帮我搭一个完全无关的演示项目"
	region := []*proto.Message{
		{Role: "user", Content: oldest},
		{Role: "assistant", Content: "好的"},
		{Role: "user", Content: "第二步"},
		{Role: "assistant", Content: "继续"},
		{Role: "user", Content: "现在正在做的事：修 compression 的角色问题"},
	}
	got := truncateSummary(region)
	if strings.Contains(got, "完全无关的演示项目") {
		t.Fatalf("不应保留区间最早的原文（会误导）：%q", got)
	}
	if !strings.Contains(got, "已省略 5 条") {
		t.Fatalf("应说明省略了多少：%q", got)
	}
	if !strings.Contains(got, "正在做的事") {
		t.Fatalf("区间末尾应与当前工作衔接：%q", got)
	}

	// 区间很短时首尾都给（信息量小，一句计数不如两行原文）
	short := truncateSummary([]*proto.Message{{Role: "user", Content: "唯一一条"}})
	if !strings.Contains(short, "唯一一条") {
		t.Fatalf("单条区间应保留原文：%q", short)
	}
}
