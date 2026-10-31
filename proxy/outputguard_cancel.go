package proxy

import (
	"context"
	"io"
	"net/http"
	"sync"

	"github.com/codex2api/security/outputguard"
)

const (
	// GuardTriggeredPublicCode 是对外暴露的稳定错误码，禁止把内部触发原因
	//（repetition/channel_bytes/...）泄漏给下游。
	GuardTriggeredPublicCode = "model_output_guard_triggered"
	// GuardTriggeredPublicMessage 是脱敏后的公开终态文案。
	GuardTriggeredPublicMessage = "Upstream model output exceeded the gateway safety policy; generation was stopped."
)

// GuardTerminationCoordinator 负责输出失控防护命中后的强制终止编排：
//  1. 第一动作是在等待任何下游写锁之前主动 cancel 上游 context；
//  2. 关闭上游响应体，跳过 5s drain；
//  3. 丢弃本地流缓冲，阻止后续 Flush/Finalize 释放已判定失控的字节。
//
// sync.Once 保证并发触发时上述动作原子执行且仅执行一次，同时冻结首个
// 触发 reason，避免后到 reason 覆盖已有终态。
type GuardTerminationCoordinator struct {
	guard          *outputguard.RequestGuard
	cancelUpstream context.CancelFunc
	body           io.Closer
	once           sync.Once
	trippedOutcome streamOutcome
}

// NewGuardTerminationCoordinator 构造终止协调器。cancelUpstream 与 body 均可为 nil
// （例如上游尚未建立连接），此时对应步骤自动跳过。
func NewGuardTerminationCoordinator(guard *outputguard.RequestGuard, cancelUpstream context.CancelFunc, body io.Closer) *GuardTerminationCoordinator {
	return &GuardTerminationCoordinator{guard: guard, cancelUpstream: cancelUpstream, body: body}
}

// HandleTripped 执行一次（且仅一次）强制终止并返回冻结后的 guard 终态。
// discardBuffers 通常传入 streamFlushWriter.DiscardBuffered 等清缓冲回调；
// 回调在 once 内执行，并发调用方只会观察到最后一次写入的同一份 outcome。
func (c *GuardTerminationCoordinator) HandleTripped(reason string, discardBuffers ...func()) streamOutcome {
	if c == nil {
		return guardTerminalOutcome(reason)
	}
	c.once.Do(func() {
		// 先取消上游、关闭响应体，再做任何本地清理；下游写锁与通知不在本函数内，
		// 因而锁争用永远不会延迟上游取消与资源回收。
		if c.cancelUpstream != nil {
			c.cancelUpstream()
		}
		if c.body != nil {
			_ = c.body.Close()
		}
		for _, discard := range discardBuffers {
			if discard != nil {
				discard()
			}
		}
		frozenReason := reason
		if c.guard != nil && c.guard.IsTripped() && c.guard.TrippedReason() != "" {
			frozenReason = c.guard.TrippedReason()
		}
		c.trippedOutcome = guardTerminalOutcome(frozenReason)
	})
	return c.trippedOutcome
}

// guardTerminalOutcome 构造绝对优先的 guard 终态：502、model_output_guard、
// 不惩罚账号、本地终态、无原始 failure payload。
func guardTerminalOutcome(reason string) streamOutcome {
	return streamOutcome{
		logStatusCode:  http.StatusBadGateway,
		failureKind:    "model_output_guard",
		failureMessage: GuardTriggeredPublicMessage,
		guardTripped:   true,
		guardReason:    reason,
		penalize:       false,
		terminalLocal:  true,
	}
}
