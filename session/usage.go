package session

import "dsc/proto"

// LastPromptUsage 从会话事件里倒查最近一次**成功且带 prompt 用量**的 LLM 调用，
// 返回它的 prompt token 数与该次完整用量。
//
// 为什么要它：进程重启后的第一条请求没有服务端上报的锚点，只能退回启发式估算。
// 中文/代码场景的启发式与真实值偏差可达 2~3 倍，于是「已用」显示、窗口裁剪、
// 压缩压力都会以错误基数起步。以会话日志里最后一次真实用量作为锚点，首发即可
// 接着「上次真实值 + 增量」计算。
//
// 判定条件与 llm/attempt 事件语义一致，且**这是全仓唯一实现**：
//   - Error 为空：只认成功结算（失败调用没有可用的 prompt 规模）；
//   - Usage 非 nil 且 PromptTokens > 0：provider 未报用量时不算锚点。
//
// 宿主（core）与 agent 插件（经 sdk）都必须调用这里，不得各自再写一份倒查循环。
func LastPromptUsage(sess *Session) (int, *proto.Usage, bool) {
	if sess == nil {
		return 0, nil, false
	}
	evs := sess.Events()
	for i := len(evs) - 1; i >= 0; i-- {
		d, ok := evs[i].Data.(*LLMAttemptData)
		if !ok || d.Error != "" || d.Usage == nil || d.Usage.PromptTokens <= 0 {
			continue
		}
		return int(d.Usage.PromptTokens), d.Usage, true
	}
	return 0, nil, false
}
