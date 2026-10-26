// Copyright IBM Corp. 2016, 2025
// SPDX-License-Identifier: MPL-2.0

package core

const (
	// EnvUnixSocketDir specifies the directory that _plugins_ should create unix
	// sockets in. Does not affect client behavior.
	EnvUnixSocketDir = "PLUGIN_UNIX_SOCKET_DIR"

	// EnvUnixSocketGroup specifies the owning, writable group to set for Unix
	// sockets created by _plugins_. Does not affect client behavior.
	EnvUnixSocketGroup = "PLUGIN_UNIX_SOCKET_GROUP"

	envMultiplexGRPC = "PLUGIN_MULTIPLEX_GRPC"

	// MaxGRPCMessageSize 插件 gRPC 收发消息上限（64 MiB）。
	//
	// gRPC 默认**收包**上限只有 4 MiB，而 agent/pre-step 事件载荷是**整份消息列表**
	// 的 JSON：长会话（数十万 token）轻易超过 4 MiB。一旦超限，事件根本送不进插件，
	// 宿主按「hook vetoed」回退成**原始全量请求**，压缩后端等于没跑——provider 随即
	// 返回 400（prompt is too long）。实测一份 913K token 的派生历史 marshal 后
	// 4,195,685 字节，正好卡在 4 MiB 线上，一条工具结果就能把它顶过去。
	//
	// 收发两侧都要放大：插件侧 server 收 pre-step 事件、插件返回改写结果，
	// 以及 broker 上承载的聚合 LLM/Tool 服务收到同一份消息列表。
	MaxGRPCMessageSize = 64 << 20
)
