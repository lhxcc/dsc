package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"dsc/proto/metadata"
)

// fakeVisionProvider 固定上报视觉能力的 LLMProvider（仅用于 GetInfo 能力测试）。
type fakeVisionProvider struct {
	vision bool
}

func (f *fakeVisionProvider) Chat(context.Context, []Message, []Tool, int) (*ChatResponse, error) {
	return nil, nil
}
func (f *fakeVisionProvider) ChatStream(context.Context, []Message, []Tool) (<-chan *ChatStreamResponse, error) {
	return nil, nil
}
func (f *fakeVisionProvider) Name(context.Context) string    { return "fake" }
func (f *fakeVisionProvider) Version(context.Context) string { return "1.0.0" }
func (f *fakeVisionProvider) HealthCheck(context.Context) error {
	return nil
}
func (f *fakeVisionProvider) VisionEnabled() bool { return f.vision }

// TestLLMMetadataCapabilities 校验 llmMetadataServer.GetInfo 把视觉能力经 capabilities
// 上报给宿主（TUI 据此提示「图片未随消息发送：当前模型不支持图像输入」）。
func TestLLMMetadataCapabilities(t *testing.T) {
	on := &llmMetadataServer{impl: &fakeVisionProvider{vision: true}}
	info, err := on.GetInfo(context.Background(), &metadata.Empty{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.Capabilities["supports_images"] != "true" {
		t.Fatalf("vision=true 时应上报 supports_images=true, got %q", info.Capabilities["supports_images"])
	}

	off := &llmMetadataServer{impl: &fakeVisionProvider{vision: false}}
	infoOff, err := off.GetInfo(context.Background(), &metadata.Empty{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if infoOff.Capabilities["supports_images"] != "false" {
		t.Fatalf("vision=false 时应上报 supports_images=false, got %q", infoOff.Capabilities["supports_images"])
	}

	// 所有 LLM 插件默认提供 "llm" 能力，供 agent 经 Requires 声明依赖
	if info.Capabilities[CapabilityLLM] != "true" {
		t.Fatalf("LLM 插件应上报 %s=true, got %q", CapabilityLLM, info.Capabilities[CapabilityLLM])
	}
	if infoOff.Capabilities[CapabilityLLM] != "true" {
		t.Fatalf("LLM 插件（vision=false）也应上报 %s=true, got %q", CapabilityLLM, infoOff.Capabilities[CapabilityLLM])
	}
}

// A call whose arguments do not parse must be REPORTED, not run with an empty argument set.
//
// Reported from a 2080 Ti box.  The model left one </parameter> out, the engine's parser folded the rest of the call
// into the first value, and the assembled arguments ended inside a string.  This file used to unmarshal that into
// map[string]interface{}{} and hand the tool an empty command - a tool acting on a guess.
func TestUnparseableArgsIsReported(t *testing.T) {
	// The shape the service produced for that report.
	broken := `{"command":"cd F:/X && sed -i '968d' a.xml <parameter=description> Delete line 968 </tool_call>`

	var args map[string]interface{}
	err := json.Unmarshal([]byte(broken), &args)
	if err == nil {
		t.Fatal("the specimen must not parse; the test is meaningless otherwise")
	}

	got := unparseableArgs("exec_command", broken, err)
	if got == nil {
		t.Fatal("a call with unparseable arguments must produce an error")
	}
	msg := got.Error()
	for _, want := range []string{"exec_command", "not valid JSON", "</parameter>", "sed -i"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message should carry %q so a reader can tell model from parser; got: %s", want, msg)
		}
	}

	// A well-formed call is unaffected.
	if e := unparseableArgs("x", `{"a":1}`, nil); e == nil {
		t.Log("nil cause still produces a message (never called that way, but it must not panic)")
	}

	// The message is bounded: a huge argument string must not become a huge log line.  The wording around it is
	// fixed, so what is checked is that the ARGUMENTS were clipped, not the total length.
	long := strings.Repeat("a", 5000)
	m := unparseableArgs("t", long, err).Error()
	if strings.Contains(m, strings.Repeat("a", 201)) {
		t.Errorf("the argument dump should be clipped to 200 characters, got %d chars total", len(m))
	}
	if !strings.Contains(m, "...") {
		t.Errorf("a clipped dump should say so with an ellipsis: %s", m)
	}
}
