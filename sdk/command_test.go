package dsc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"dsc/proto"
	"dsc/proto/metadata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestCommandsToProto 声明转换：非法名（空、含空白、含 "/"）被丢弃，
// 重复名只留第一条，Usage 缺省补 /name。
func TestCommandsToProto(t *testing.T) {
	got := commandsToProto([]Command{
		{Name: "alpha", Summary: "ok"},
		{Name: "/beta", Usage: "/beta --x"},
		{Name: "  "},
		{Name: "bad name"},
		{Name: "nested/cmd"},
		{Name: "alpha", Summary: "dup"},
	})
	if len(got) != 2 {
		t.Fatalf("应保留 2 条合法声明，实际 %d: %+v", len(got), got)
	}
	if got[0].Name != "alpha" || got[0].Usage != "/alpha" {
		t.Fatalf("首条应为 alpha 且 Usage 补 /alpha，实际 %+v", got[0])
	}
	if got[1].Name != "beta" || got[1].Usage != "/beta --x" {
		t.Fatalf("前导 / 应被去掉、自定义 Usage 保留，实际 %+v", got[1])
	}
	if commandsToProto(nil) != nil {
		t.Fatal("无声明应返回 nil")
	}
}

// TestMetadataCarriesCommands 命令声明必须随 metadata 上报，否则宿主不知道
// 有这个命令（也就不会转发）。
func TestMetadataCarriesCommands(t *testing.T) {
	srv := &metadataServer{cfg: Config{
		Name: "p", Type: TypeDsc,
		Commands: []Command{{Name: "status", Summary: "看状态"}},
	}}
	info, err := srv.GetInfo(context.Background(), &metadata.Empty{})
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if len(info.GetCommands()) != 1 || info.GetCommands()[0].GetName() != "status" {
		t.Fatalf("metadata 应带命令声明，实际 %+v", info.GetCommands())
	}
}

// TestHookRunCommandUnimplemented 没接 RunCommand 的插件返回 Unimplemented：
// 宿主据此报「命令不可用」，而不是把空串当成正常输出。
func TestHookRunCommandUnimplemented(t *testing.T) {
	srv := &hookServiceServer{hook: &Hook{}}
	_, err := srv.RunCommand(context.Background(), &proto.RunCommandRequest{Name: "x"})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("应返回 Unimplemented，实际 %v", err)
	}
}

// TestHookRunCommandResult 正常路径：返回文本；插件返回 err 时文本仍带回
// （调用方可展示半成品 + 原因）。
func TestHookRunCommandResult(t *testing.T) {
	srv := &hookServiceServer{hook: &Hook{
		RunCommand: func(ctx context.Context, name, args string) (string, error) {
			if name != "x" || args != "a b" {
				return "", errors.New("unexpected args")
			}
			return "out", nil
		},
	}}
	resp, err := srv.RunCommand(context.Background(), &proto.RunCommandRequest{Name: "x", Args: "a b"})
	if err != nil || resp.GetText() != "out" || resp.GetError() != "" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}

	srv.hook.RunCommand = func(context.Context, string, string) (string, error) {
		return "half", errors.New("boom")
	}
	resp, err = srv.RunCommand(context.Background(), &proto.RunCommandRequest{Name: "x"})
	if err != nil {
		t.Fatalf("插件错误不应变成 RPC 错误: %v", err)
	}
	if !strings.Contains(resp.GetError(), "boom") || resp.GetText() != "half" {
		t.Fatalf("文本与原因都应带回: %+v", resp)
	}
}
