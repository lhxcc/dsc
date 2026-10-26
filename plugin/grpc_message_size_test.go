// Copyright IBM Corp. 2016, 2025
// SPDX-License-Identifier: MPL-2.0

package core

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// 回归测试：插件侧 gRPC server 必须能接收远超 gRPC 默认 4 MiB 上限的消息。
//
// 背景（真实故障）：宿主的 agent/pre-step 事件载荷是**整份消息列表**的 JSON，
// 长会话轻易超过 4 MiB（实测 913K token 的派生历史 marshal 后 4,195,685 字节）。
// 一旦插件 server 拒收，宿主按「hook vetoed」回退成原始全量请求，压缩后端等于
// 没跑，provider 直接 400（prompt is too long）。这里用 6 MiB 钉住该上限，
// 防止 MaxGRPCMessageSize 被误删/调小。

// largeMessageServiceDesc 只有一个回声方法，用 wrapperspb.BytesValue 承载任意
// 长度 payload（避免为测试新增 .proto）。
var largeMessageServiceDesc = grpc.ServiceDesc{
	ServiceName: "test.LargeMessage",
	HandlerType: (*interface{})(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "Echo",
			Handler: func(_ interface{}, _ context.Context, dec func(interface{}) error, _ grpc.UnaryServerInterceptor) (interface{}, error) {
				in := new(wrapperspb.BytesValue)
				if err := dec(in); err != nil {
					return nil, err
				}
				return in, nil
			},
		},
	},
	Metadata: "grpc_message_size_test",
}

// largeMessagePlugin 把上面这个服务挂到被测的 GRPCServer 上。
type largeMessagePlugin struct{ Plugin }

func (p *largeMessagePlugin) GRPCServer(_ *GRPCBroker, s *grpc.Server) error {
	s.RegisterService(&largeMessageServiceDesc, struct{}{})
	return nil
}

func (p *largeMessagePlugin) GRPCClient(context.Context, *GRPCBroker, *grpc.ClientConn) (interface{}, error) {
	return nil, nil
}

func TestGRPCServerAcceptsMessagesAboveDefaultLimit(t *testing.T) {
	client, _ := TestPluginGRPCConn(t, false, map[string]Plugin{"large": &largeMessagePlugin{}})
	defer func() { _ = client.Close() }()

	// 6 MiB > gRPC 默认 4 MiB 收包上限。
	payload := &wrapperspb.BytesValue{Value: make([]byte, 6<<20)}
	var out wrapperspb.BytesValue
	if err := client.Conn.Invoke(context.Background(), "/test.LargeMessage/Echo", payload, &out); err != nil {
		t.Fatalf("插件侧 gRPC server 应能接收 >4MiB 的消息（agent/pre-step 事件就是这种量级）: %v", err)
	}
	if len(out.Value) != len(payload.Value) {
		t.Fatalf("往返长度不一致: got %d, want %d", len(out.Value), len(payload.Value))
	}
}
