package dsc

import (
	"context"
	"sync"
	"time"

	"dsc/proto"
)

// 跨进程活动表：宿主的活跃续命看门狗要知道「这次工具调用最近还在动吗」，而活动
// 只在执行方（本进程）产生。ctx 跨 gRPC 不携带 value，插件里的 core.TouchActivity
// 拿到的是空句柄——所以宿主改为轮询 GetActivity 读这张表。
//
// 按 ToolCallId 索引：ExecuteTool 进入时登记一次初始活动，退出时清理（宿主每次调用
// 都生成新的 call id，不存在复用累积）。
var (
	activityMu sync.Mutex
	activityTS = map[string]int64{}
)

// recordActivity 记录一次活动（Unix 毫秒）。
func recordActivity(callID string) {
	if callID == "" {
		return
	}
	activityMu.Lock()
	activityTS[callID] = time.Now().UnixMilli()
	activityMu.Unlock()
}

// clearActivity 清理调用结束后的记录。
func clearActivity(callID string) {
	if callID == "" {
		return
	}
	activityMu.Lock()
	delete(activityTS, callID)
	activityMu.Unlock()
}

// GetActivity 供宿主轮询某次调用最近一次活动时间；0 表示尚未上报。
//
// 宿主用它替代「固定空闲预算」：有输出就续命（继续在前台等结果），持续无输出越过
// 预算才把任务转入后台。旧宿主不会调用它，旧插件返回 Unimplemented 时宿主退回固定
// 预算——两侧都不因缺对方而失败。
func (s *toolServiceServer) GetActivity(_ context.Context, req *proto.GetActivityRequest) (*proto.GetActivityResponse, error) {
	activityMu.Lock()
	ts := activityTS[req.GetToolCallId()]
	activityMu.Unlock()
	return &proto.GetActivityResponse{LastActivityUnixMs: ts}, nil
}
