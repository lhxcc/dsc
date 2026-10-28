package main

import (
	"context"
	"testing"

	"dsc/proto"
)

// TestEmptyPreStepPayloadDoesNothing 轻载荷（无 messages_json）必须什么都不做，
// 尤其不能清掉 lastMsgs——那是 request-error 紧急压缩唯一的材料来源。
//
// 宿主只把消息列表发给持有压缩岗位的插件；本插件没当值时每步都会收到不带消息的
// 信封。若在写缓存之后才判空，缓存会被逐 step 清空，等到它真接手（改配置/重启）时
// 第一次溢出就只能空压。
func TestEmptyPreStepPayloadDoesNothing(t *testing.T) {
	s := newCompactionBasicServer()
	s.window = 1_000_000

	kept := []*proto.Message{{Role: "user", Content: "材料"}}
	s.mu.Lock()
	s.lastMsgs = kept
	s.lastSession = "s1"
	s.mu.Unlock()

	out, err := s.handlePreStep(context.Background(), `{"session":"s1","token_count":900000,"projected_tokens":900000}`)
	if err != nil {
		t.Fatalf("空载荷不应报错: %v", err)
	}
	if out != "" {
		t.Fatalf("空载荷不应产出改写，got %.80s", out)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lastMsgs) != len(kept) {
		t.Fatalf("lastMsgs 被清空了：%d 条（期望仍有 %d 条）", len(s.lastMsgs), len(kept))
	}
	if s.lastSession != "s1" {
		t.Fatalf("lastSession 不应被改写: %q", s.lastSession)
	}
}
