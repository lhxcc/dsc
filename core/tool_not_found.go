package core

import (
	"fmt"
	"sort"
	"strings"
)

// 「工具不存在」错误的构造：附上最相近的可用工具名。
//
// 为什么要附候选：模型偶尔会调用一个根本没注册的工具名（典型是从别处学来的
// 「工具检索」类名字，或在超长会话里记混了工具表）。旧文案只有
// 「tool not found: xxx」，模型拿不到任何纠正线索，往往换一个同样不存在的名字
// 继续试——同一个错误在一步之内反复出现的常见来源。
//
// 这里按编辑距离给出最近的几个真实工具名，让模型自己就能改对；没有足够接近的
// 时候保持原来的一句话，不硬塞一堆无关名字污染错误信息。
func (m *Manager) toolNotFoundError(name string) error {
	sugg := closestToolNames(name, m.toolRegistry.List(), 3)
	if len(sugg) == 0 {
		return fmt.Errorf("tool not found: %s", name)
	}
	return fmt.Errorf("tool not found: %s (no tool by that name is registered; closest available tools: %s)",
		name, strings.Join(sugg, ", "))
}

// closestToolNames 返回与 name 最相近的至多 k 个候选工具名（按编辑距离升序）。
//
// 距离超过 len(name) 视为「毫无关系」→ 返回空（此时给候选只会误导）。
func closestToolNames(name string, candidates []string, k int) []string {
	type scored struct {
		name string
		dist int
	}
	all := make([]scored, 0, len(candidates))
	for _, c := range candidates {
		if c == name {
			continue // 精确命中的工具不可能走到这里，防御性跳过
		}
		all = append(all, scored{name: c, dist: editDistance(name, c)})
	}
	if len(all) == 0 {
		return nil
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].dist != all[j].dist {
			return all[i].dist < all[j].dist
		}
		return all[i].name < all[j].name
	})
	// 「可能记混」的界限：名字长度的一半（至少 3）以内才算候选。
	// 太松会把毫无关系的工具塞进错误里，反而干扰模型判断。
	limit := len(name) / 2
	if limit < 3 {
		limit = 3
	}
	if all[0].dist > limit {
		return nil
	}
	if len(all) > k {
		all = all[:k]
	}
	out := make([]string, 0, len(all))
	for _, s := range all {
		if s.dist > limit {
			break
		}
		out = append(out, s.name)
	}
	return out
}

// editDistance 是最小编辑距离（插入/删除/替换各计 1）。
// 工具名很短（几十字符内），朴素动态规划足够。
func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = minInt(prev[j]+1, minInt(cur[j-1]+1, prev[j-1]+cost))
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
