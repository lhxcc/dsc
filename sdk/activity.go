package dsc

import (
	"context"
	"sync"
	"time"

	"dsc/proto"
)

// 跨进程活动表：宿主的活跃续命看门狗要知道「这次工具调用最近还在动吗」，后台任务
// 还要知道「它跑到一半说了什么」。两个问题都只能在执行方（本进程）回答，而
// ctx value 过不了 gRPC——所以宿主反过来轮询 GetActivity 读这张表。
//
// 按 ToolCallId 索引：ExecuteTool 进入时登记一次初始活动，退出时清理（宿主每次调用
// 都生成新的 call id，不存在复用累积）。
var (
	activityMu sync.Mutex
	calls      = map[string]*callActivity{}
)

// outputTailLimit 每个调用保留的输出尾部上限。
//
// 4 KB 是折中：够看清一条命令「现在在刷什么」（编译错误、服务启动日志、进度条尾巴），
// 又不至于让每次 job_output 都把几 MB 日志搬一遍。完整的输出仍然走工具结果那条路。
const outputTailLimit = 4096

// callActivity 一次调用的活动状态。
type callActivity struct {
	ts    int64  // 最近活动时间（Unix 毫秒）
	tail  []byte // 最近输出（至多 outputTailLimit 字节）
	total int64  // 累计产生字节数
}

// recordActivity 记录一次活动（Unix 毫秒）。
func recordActivity(callID string) {
	if callID == "" {
		return
	}
	activityMu.Lock()
	entry := calls[callID]
	if entry == nil {
		entry = &callActivity{}
		calls[callID] = entry
	}
	entry.ts = time.Now().UnixMilli()
	activityMu.Unlock()
}

// recordOutput 记录一段输出：既算活动，也进尾部缓冲。
func recordOutput(callID string, p []byte) {
	if callID == "" || len(p) == 0 {
		return
	}
	activityMu.Lock()
	entry := calls[callID]
	if entry == nil {
		entry = &callActivity{}
		calls[callID] = entry
	}
	entry.ts = time.Now().UnixMilli()
	entry.total += int64(len(p))
	entry.tail = append(entry.tail, p...)
	if len(entry.tail) > outputTailLimit {
		entry.tail = trimTail(entry.tail, outputTailLimit)
	}
	activityMu.Unlock()
}

// trimTail 只保留最后 limit 字节，并从 UTF-8 字符边界起算。
//
// 直接切字节约等于随机砍半个汉字：宿主把它当字符串渲染就会出现替换字符，
// 或者让 proto 的 string 字段 marshal 失败。这里丢掉开头的续字节（10xxxxxx），
// 保证尾部本身是合法 UTF-8 的后缀。
func trimTail(buf []byte, limit int) []byte {
	if len(buf) <= limit {
		return buf
	}
	start := len(buf) - limit
	for start < len(buf) && buf[start]&0xC0 == 0x80 { // 续字节
		start++
	}
	out := make([]byte, len(buf)-start)
	copy(out, buf[start:])
	return out
}

// clearActivity 清理调用结束后的记录。
func clearActivity(callID string) {
	if callID == "" {
		return
	}
	activityMu.Lock()
	delete(calls, callID)
	activityMu.Unlock()
}

// GetActivity 供宿主轮询某次调用的活动时间与最近输出；0/空表示尚未上报。
//
// 宿主用它替代「固定空闲预算」：有输出就续命（继续在前台等结果），持续无输出越过
// 预算才把任务转入后台。后台任务则用 output_tail 回答「现在在说什么」。旧宿主不会
// 读新字段；旧插件返回 Unimplemented 时宿主退回固定预算——两侧都不因缺对方而失败。
func (s *toolServiceServer) GetActivity(_ context.Context, req *proto.GetActivityRequest) (*proto.GetActivityResponse, error) {
	activityMu.Lock()
	entry := calls[req.GetToolCallId()]
	var (
		ts    int64
		tail  []byte
		total int64
	)
	if entry != nil {
		ts, total = entry.ts, entry.total
		if len(entry.tail) > 0 {
			tail = make([]byte, len(entry.tail))
			copy(tail, entry.tail)
		}
	}
	activityMu.Unlock()
	return &proto.GetActivityResponse{
		LastActivityUnixMs: ts,
		OutputTail:         tail,
		OutputBytes:        total,
	}, nil
}
