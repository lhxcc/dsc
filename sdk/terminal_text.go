package dsc

import "dsc/core"

// CollapseCarriageReturns 归一「就地刷新」式输出（进度条 \r 重绘、CRLF 行尾），
// 只保留每条物理行最终可见的那一段。唯一实现见 core.CollapseCarriageReturns。
//
// 工具在把 stdout/stderr 交给模型前应当调用它：否则模型会收到成百上千段进度帧
// （实测一条结果 2415 字符里 2112 是再也看不到的中间帧），既浪费 token 又干扰判断。
func CollapseCarriageReturns(s string) string { return core.CollapseCarriageReturns(s) }
