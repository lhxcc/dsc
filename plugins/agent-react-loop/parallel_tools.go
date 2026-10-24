package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"dsc-sdk"
	"dsc/proto"
)

// toolParallelism 单批只读工具的最大并发（DSC_TOOL_PARALLELISM）。
// 默认 4；0/1 关闭并行（等价旧行为）。非法值回退默认，不阻断启动。
func toolParallelism() int {
	const def = 4
	v := strings.TrimSpace(os.Getenv("DSC_TOOL_PARALLELISM"))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// parallelToolOutcome 一次预执行的调用结果：resp 与 err 至少一个非零。
type parallelToolOutcome struct {
	resp *proto.ExecuteToolResponse
	err  error
}

// prefetchReadOnlyBatch 在整批调用都满足「非本地工具 + 工具声明只读」时并发执行，
// 返回按源索引索引的结果表；任一条不满足则返回 nil，调用方回退原串行路径。
//
// 准入依据只有工具自声明的能力标签（随 ListTools 的 proto.Tool.capabilities 下发）：
// agent 与宿主分属不同进程，拿不到宿主的写能力裁决；能力未知一律按不可并行处理
// （fail-closed）。整个批次全有或全无——混合批次不做分段混排，避免审批与副作用交错。
//
// 只并发 RPC 本身：tool/call 事件、落盘与「开始调用」帧仍由调用方按源序串行完成，
// 结果也按源序落地，事件日志因此保持确定、可回放。
func (a *ReactLoopAgent) prefetchReadOnlyBatch(
	ctx context.Context,
	toolClient proto.ToolServiceClient,
	toolCalls []*proto.ToolCall,
	availableTools []*proto.Tool,
	truncated bool,
) map[int]*parallelToolOutcome {
	limit := toolParallelism()
	if limit <= 1 || len(toolCalls) < 2 || truncated {
		return nil
	}
	caps := make(map[string][]string, len(availableTools))
	for _, t := range availableTools {
		caps[t.GetName()] = t.GetCapabilities()
	}
	for _, tc := range toolCalls {
		if tc == nil || isLocalTool(tc.Name) {
			return nil
		}
		if !dsc.ToolCallReadOnly(caps[tc.Name], tc.ArgumentsJson) {
			return nil
		}
	}

	// 每个调用都携带会话标识与审批策略（与串行路径一致）。
	a.sessMu.Lock()
	sessionID := ""
	if a.sess != nil {
		sessionID = a.sess.ID()
	}
	a.sessMu.Unlock()
	policy := a.approvalPolicy

	out := make(map[int]*parallelToolOutcome, len(toolCalls))
	var mu sync.Mutex
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, tc := range toolCalls {
		wg.Add(1)
		go func(i int, tc *proto.ToolCall) {
			defer wg.Done()
			// 工具意外不得中断会话：goroutine 内 panic 无法被外层捕获，
			// 在此转为错误结果（与宿主/管道层的 recover 防线同款）。
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					out[i] = &parallelToolOutcome{err: fmt.Errorf("tool %s panicked (parallel): %v", tc.Name, r)}
					mu.Unlock()
				}
			}()
			sem <- struct{}{}
			defer func() { <-sem }()
			resp, err := toolClient.ExecuteTool(ctx, &proto.ExecuteToolRequest{
				ToolName:       tc.Name,
				ArgumentsJson:  tc.ArgumentsJson,
				ToolCallId:     tc.Id,
				ApprovalPolicy: policy,
				SessionId:      sessionID,
			})
			mu.Lock()
			out[i] = &parallelToolOutcome{resp: resp, err: err}
			mu.Unlock()
		}(i, tc)
	}
	wg.Wait()
	return out
}
