package dsc

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"dsc/proto"

	gproto "google.golang.org/protobuf/proto"
)

// 尾部缓冲只保留最后若干字节，并且不能把多字节字符砍成半个。
func TestRecordOutputKeepsUTF8SafeTail(t *testing.T) {
	const call = "call-tail"
	defer clearActivity(call)

	// 全是 3 字节汉字，总量超过上限：截断点必然落在某个字中间，必须往后让到边界
	han := strings.Repeat("汉", 1700) // 5100 字节
	recordOutput(call, []byte(han))

	activityMu.Lock()
	tail := append([]byte(nil), calls[call].tail...)
	total := calls[call].total
	activityMu.Unlock()

	if total != int64(len(han)) {
		t.Fatalf("total = %d, want %d", total, len(han))
	}
	if len(tail) > outputTailLimit {
		t.Fatalf("尾部超过上限：%d > %d", len(tail), outputTailLimit)
	}
	if !utf8.Valid(tail) {
		t.Fatalf("尾部不是合法 UTF-8（砍断了多字节字符）")
	}
	if !strings.HasSuffix(han, string(tail)) {
		t.Fatalf("尾部不是原输出的后缀")
	}
	// 0 字节的 ASCII 前缀 + 纯汉字尾巴也要成立
	recordOutput(call, []byte(strings.Repeat("a", 300)))
	activityMu.Lock()
	mixed := string(calls[call].tail)
	activityMu.Unlock()
	if !utf8.ValidString(mixed) || !strings.Contains(mixed, "汉") {
		t.Fatalf("混合尾部异常：%q", mixed)
	}
}

// GetActivity 一趟同时给出活动时间、尾部与累计字节数；无记录时全空。
func TestGetActivityReturnsTailAndActivity(t *testing.T) {
	srv := &toolServiceServer{}
	const call = "call-get"
	defer clearActivity(call)

	if resp, err := srv.GetActivity(context.Background(), &proto.GetActivityRequest{ToolCallId: call}); err != nil {
		t.Fatalf("get: %v", err)
	} else if resp.GetLastActivityUnixMs() != 0 || len(resp.GetOutputTail()) != 0 || resp.GetOutputBytes() != 0 {
		t.Fatalf("未上报时应全空：%+v", resp)
	}

	recordOutput(call, []byte("hello\n"))
	resp, err := srv.GetActivity(context.Background(), &proto.GetActivityRequest{ToolCallId: call})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(resp.GetOutputTail()) != "hello\n" || resp.GetOutputBytes() != 6 {
		t.Fatalf("tail = %q / bytes = %d", resp.GetOutputTail(), resp.GetOutputBytes())
	}
	if resp.GetLastActivityUnixMs() == 0 {
		t.Fatal("有输出即视为活动，时间戳不该为 0")
	}

	// 清理后彻底消失：调用 id 不复用，残留只会泄漏内存
	clearActivity(call)
	resp, _ = srv.GetActivity(context.Background(), &proto.GetActivityRequest{ToolCallId: call})
	if len(resp.GetOutputTail()) != 0 || resp.GetOutputBytes() != 0 {
		t.Fatalf("清理后仍有残留：%+v", resp)
	}
}

// AppendOutput 从 ctx 取调用 id：没有 CallInfo 时静默无事（插件可能在工具外调用）。
func TestAppendOutputRequiresCallInfo(t *testing.T) {
	AppendOutput(context.Background(), []byte("ignored"))
	activityMu.Lock()
	n := len(calls)
	activityMu.Unlock()
	if n != 0 {
		t.Fatalf("无 CallInfo 不应登记任何调用，实际 %d 条", n)
	}

	ctx := WithCallInfo(context.Background(), CallInfo{SessionID: "s", ToolCallID: "call-ctx"})
	defer clearActivity("call-ctx")
	AppendOutput(ctx, []byte("from ctx"))
	activityMu.Lock()
	got := string(calls["call-ctx"].tail)
	activityMu.Unlock()
	if got != "from ctx" {
		t.Fatalf("tail = %q", got)
	}
}

// TestGetActivitySanitizesInvalidUTF8Tail 尾部经 proto string 回传给宿主：非法字节会
// 让整个 GetActivity 响应 marshal 失败，宿主的续命与 job_output 一起瞎掉。远端命令
// 吐什么字节我们控制不了，所以这里必须止损。
func TestGetActivitySanitizesInvalidUTF8Tail(t *testing.T) {
	srv := &toolServiceServer{}
	const call = "call-bad-utf8"
	defer clearActivity(call)

	recordOutput(call, []byte{0xff, 0xfe, 'o', 'k'})
	resp, err := srv.GetActivity(context.Background(), &proto.GetActivityRequest{ToolCallId: call})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !utf8.Valid(resp.GetOutputTail()) {
		t.Fatalf("尾部不是合法 UTF-8：%q", string(resp.GetOutputTail()))
	}
	// 真正要保证的是「能序列化」——这正是当初会失败的那一步。
	if _, err := gproto.Marshal(resp); err != nil {
		t.Fatalf("响应无法序列化：%v", err)
	}
	if !strings.HasSuffix(string(resp.GetOutputTail()), "ok") {
		t.Fatalf("有效部分应保留：%q", string(resp.GetOutputTail()))
	}
}
