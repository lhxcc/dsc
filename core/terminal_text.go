package core

import "strings"

// CollapseCarriageReturns 把「就地刷新」式输出归一成终端里人眼最终看到的样子。
//
// 两类 CR 语义都要照顾：
//   - 进度条重绘：git/npm/pip/docker 用 \r 回到行首逐帧覆盖同一行，中间帧人眼看不到，
//     只有最后一帧可见（实测一条 `git worktree add` 结果 2415 字符里有 64 个 CR，
//     真正有意义的只有 5 行、约 300 字符）；
//   - CRLF 行尾（Windows 文本）：\r\n 等价于 \n，归一后与 Unix 侧行为一致。
//
// 规则：按 \n 切物理行，每行按 \r 切段，丢弃空段（含 CRLF 的尾随空段），保留**最后
// 一个非空段**；无 CR 的行原样返回（早退，零开销）。纯字符串处理、无平台分支，
// 因此在七端行为一致。
//
// 取舍（有意为之）：折叠后看不出「该文件用 CRLF 行尾」——需要行尾信息时用
// `file`/`xxd`/`git diff` 查；换来的是模型不再收到上千段进度噪声。
func CollapseCarriageReturns(s string) string {
	if !strings.ContainsRune(s, '\r') {
		return s
	}
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		if !strings.ContainsRune(ln, '\r') {
			continue
		}
		segs := strings.Split(ln, "\r")
		last := ""
		for j := len(segs) - 1; j >= 0; j-- {
			if strings.TrimSpace(segs[j]) != "" {
				last = segs[j]
				break
			}
		}
		lines[i] = last
	}
	return strings.Join(lines, "\n")
}
