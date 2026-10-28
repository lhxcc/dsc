package main

import (
	"context"
	"testing"
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
