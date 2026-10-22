package core

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// 工具级瞬态重试（只读调用专用）：
//
//	只重试「读操作 + 连接级瞬态错误」——读操作幂等，重试不放大副作用；
//	写语义或写语义未知的调用一律不重试（重复写比失败更糟）。
//	取消、工具超时、panic 永不重试：超时是策略预算（重试会翻倍墙钟），
//	panic 已在 executeToolBody 的 recover 中转为错误。
//
// 环境变量 DSC_TOOL_RETRY 控制重试次数（默认 2，0 关闭；非法值回退默认）。

const (
	defaultToolRetryAttempts = 2
	toolRetryBackoff         = 200 * time.Millisecond
)

// toolRetryAttempts 返回只读工具瞬态错误的最大重试次数。
func toolRetryAttempts() int {
	v := strings.TrimSpace(os.Getenv("DSC_TOOL_RETRY"))
	if v == "" {
		return defaultToolRetryAttempts
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return defaultToolRetryAttempts
	}
	return n
}

// toolCallRetryable 报告本次调用是否可安全重试：仅当工具显式声明写能力、且本次
// 调用被裁决为只读时为真。未声明能力的旧工具与写入类调用一律不可重试（fail-closed）。
func toolCallRetryable(tool ToolDefinition, argsJSON string) bool {
	decl, declared := declaredWriteCapabilityDecl(tool)
	if !declared {
		return false
	}
	d := resolveWriteCallFromDecl(decl, argsJSON)
	return !d.write && !d.unknownWrite
}

// isTransientToolError 判断错误是否属于可重试的连接级瞬态错误。
// 取消与超时明确排除（调用方已放弃，或属策略预算）。
func isTransientToolError(err error) bool {
	if err == nil {
		return false
	}
	var timeoutErr *ToolTimeoutError
	if errors.As(err, &timeoutErr) {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ETIMEDOUT) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	if st, ok := status.FromError(err); ok && st.Code() == codes.Unavailable {
		return true
	}
	return false
}
