package core

import (
	"context"
	"time"
)

// outputProbe 可选接口：工具能报告「本次调用最近产生了什么输出」。
//
// 后台任务跑在**插件进程**里，宿主拿不到它的管道，能问的只有执行方本身。实现了
// 这个接口的工具（目前是 shell 这类「输出会流、但整段要到结束才回」的工具）把自己的
// 输出顺手登记进 GetActivity 的跨进程表，宿主据此回答「跑到一半在说什么」。
//
// 为什么按接口探测而不是按工具名特判：能不能流是插件自己决定的（它得在写自己输出
// 缓冲时调 AppendOutput）。宿主只认「谁实现了」——旧插件、别的工具一个字都不用改。
type outputProbe interface {
	OutputTail(ctx context.Context, callID string) (tail string, totalBytes int64, ok bool)
}

// probeTimeout 单次探测的超时。
//
// 这是在 job_output 的读路径上调用的 gRPC：插件若卡住（大锁、GC、进程半死），
// 没有上限就会把「读一下状态」变成挂死。宁可这一轮拿不到尾部，也不能让模型卡住。
const probeTimeout = 5 * time.Second

// jobProbes 为后台任务装配两条观察通道（尾部输出 / 最近活动）。
//
// 拿不到就返回 nil：注册表把这些当「生产方不提供」，任务退化成原来的 final-output
// 行为（落定前没有可读输出）。两侧因此互不强依赖。
func (m *Manager) jobProbes(toolName, callID string) (
	tail func() (string, int64),
	activity func() (time.Time, bool),
) {
	if callID == "" || m == nil || m.toolRegistry == nil {
		return nil, nil
	}
	tool, ok := m.toolRegistry.Get(toolName)
	if !ok {
		return nil, nil
	}
	if p, ok := tool.(outputProbe); ok {
		tail = func() (string, int64) {
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			defer cancel()
			out, total, ok := p.OutputTail(ctx, callID)
			if !ok {
				return "", 0
			}
			return out, total
		}
	}
	if p, ok := tool.(activityProbe); ok {
		activity = func() (time.Time, bool) {
			ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
			defer cancel()
			return p.LastActivity(ctx, callID)
		}
	}
	return tail, activity
}
