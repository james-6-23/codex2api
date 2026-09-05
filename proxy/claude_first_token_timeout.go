package proxy

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/auth"
)

// claudeSlowFirstTokenLogThreshold 是 Claude 路径"首字缓慢"日志的阈值。生产观测：
// effort xhigh + 大上下文的正常请求首字多在 60s 内，超过即值得留痕定位。
const claudeSlowFirstTokenLogThreshold = 60 * time.Second

// claudeFirstTokenTimeoutFor 返回本次 attempt 应使用的首字超时。Claude OAuth 账号优先用
// ClaudeCode 全局配置里的专属超时（默认 120s），配置为 0 或非 Claude 账号时跟随全局
// first_token_timeout_seconds。全局值在生产常年为 0（关闭），而 Claude 上游偶发
// message_start 之后数分钟无内容，不设超时会让并发位被僵尸请求长期占住。
func claudeFirstTokenTimeoutFor(store *auth.Store, account *auth.Account) time.Duration {
	if store != nil && account.IsClaudeOAuth() {
		if timeout := store.ClaudeFirstTokenTimeout(); timeout > 0 {
			return timeout
		}
	}
	return currentFirstTokenTimeout()
}

// claudeNativeFirstTokenOutcome 把"首字看门狗触发、且首个可见帧从未到达"的原生透传结果
// 归一成首字超时 outcome：日志与重试判定沿用 Codex 翻译路径的同一语义，而不是笼统的
// "上游流中断"。成功流与已有可见帧的流保持原结果。
func claudeNativeFirstTokenOutcome(guard *firstTokenTimeoutGuard, firstTokenMs int, outcome streamOutcome, timeout time.Duration) streamOutcome {
	if guard == nil || !guard.TimedOut() || firstTokenMs > 0 || outcome.logStatusCode == http.StatusOK {
		return outcome
	}
	return firstTokenTimeoutOutcome(timeout)
}

// activateClaudeStreamKeepalive 让 Claude OAuth 流式请求在首字前就开始向下游发 SSE 保活
// 注释（间隔沿用 continuousRetryKeepaliveInterval）。原生透传会把 message_start 等
// 首字前帧扣住等待静默重试窗口，下游在长推理期间收不到任何字节，网关/客户端会误判
// 连接已死而超时重试，放大并发占用；保活让"上游在思考"与"连接已死"可区分。
func activateClaudeStreamKeepalive(ctx context.Context, store *auth.Store, account *auth.Account, isStream bool) {
	if !isStream || store == nil || !account.IsClaudeOAuth() || !store.ClaudeStreamKeepaliveEnabled() {
		return
	}
	activateContinuousRetryKeepalive(ctx)
}

// claudeFirstTokenSlow 报告已记录的首字耗时是否超过缓慢阈值；0 表示未记录到首字。
func claudeFirstTokenSlow(firstTokenMs int) bool {
	return firstTokenMs > 0 && time.Duration(firstTokenMs)*time.Millisecond >= claudeSlowFirstTokenLogThreshold
}

// logClaudeFirstTokenLatency 给 Claude 路径的三类首字异常留痕：看门狗超时、首字缓慢、
// 首字前下游已断开。每条都带账号/模型/effort/等待时长，便于按 effort 分档定位卡顿。
func logClaudeFirstTokenLatency(account *auth.Account, model, effort string, firstTokenMs int, outcome streamOutcome, start time.Time) {
	if !account.IsClaudeOAuth() {
		return
	}
	if effort == "" {
		effort = "-"
	}
	waited := time.Since(start).Round(time.Millisecond)
	switch {
	case outcome.failureKind == "timeout" && firstTokenMs == 0:
		log.Printf("Claude 首字超时，已取消上游并释放并发位 (account=%d, model=%s, effort=%s, waited=%s, /v1/messages)", account.ID(), model, effort, waited)
	case outcome.logStatusCode == logStatusClientClosed && firstTokenMs == 0:
		log.Printf("Claude 首字前下游断开 (account=%d, model=%s, effort=%s, waited=%s, /v1/messages)", account.ID(), model, effort, waited)
	case claudeFirstTokenSlow(firstTokenMs):
		log.Printf("Claude 首字缓慢 (account=%d, model=%s, effort=%s, ttft_ms=%d, /v1/messages)", account.ID(), model, effort, firstTokenMs)
	}
}
