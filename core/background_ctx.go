package core

import "context"

// 后台任务执行域标记。
//
// 为什么需要它：活跃续命看门狗（WithIdleDeadline）在**宿主**侧的 tool/execute 槽
// 安装，而它的活动信号（TouchActivity）要在**执行方**重置计时器。ctx 跨 gRPC 传给
// 插件工具时，取消信号会传播、但 ctx 里的活动句柄不会——插件里的 dsc.TouchActivity
// 拿到的是空句柄，等于 no-op。于是看门狗只在固定 idle 后必然触发：对长命令来说，
// 「续命」从来没生效过，到点就是一发取消。
//
// 现在长命令在宽限期后转入后台任务继续跑。后台任务不该再被「无输出」判死：它的
// 语义是「已经交出去了，模型可以干别的」，继续用空闲预算杀它既有害（几十秒没输出的
// 构建/测试很常见）也自相矛盾（前台等待时它明明是被允许跑的）。故后台执行域打标记，
// 装看门狗的一律跳过；终止只走 job_kill 或进程退出。
type backgroundJobKey struct{}

// WithBackgroundJob 标记该 ctx 属于宿主后台任务（宽限期超时后继续执行的那条）。
func WithBackgroundJob(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundJobKey{}, true)
}

// IsBackgroundJob 报告该 ctx 是否属于后台任务。
func IsBackgroundJob(ctx context.Context) bool {
	v, _ := ctx.Value(backgroundJobKey{}).(bool)
	return v
}
