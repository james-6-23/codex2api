package proxy

import (
	"context"
	"fmt"
	"io"

	"github.com/codex2api/auth"
)

// readAllLimitedWithContinuousRetryKeepalive 将读取上限与请求级保活组合，
// 用于非流式成功响应及原生协议 JSON 聚合，避免读体阶段长时间没有下游活动。
func readAllLimitedWithContinuousRetryKeepalive(ctx context.Context, reader io.Reader, limit int64) ([]byte, error) {
	if limit < 0 {
		return nil, fmt.Errorf("response body limit must be non-negative")
	}
	body, err := readAllWithContinuousRetryKeepalive(ctx, io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return body, nil
}

// activateAnthropicMessagesKeepalive 在选号后应用 Messages 的 Claude 开关，
// relay 和 Codex 路径始终启用保活。
func (h *Handler) activateAnthropicMessagesKeepalive(ctx context.Context, account *auth.Account, stream bool) {
	var store *auth.Store
	if h != nil {
		store = h.store
	}
	if stream && (account == nil || account.IsClaudeOAuth()) {
		setContinuousRetryKeepaliveActive(ctx, store != nil && store.ClaudeStreamKeepaliveEnabled())
		return
	}
	setContinuousRetryKeepaliveActive(ctx, true)
}

// setContinuousRetryKeepaliveActive 支持重试换号时在 relay 与 Claude OAuth
// 的不同保活策略之间切换。
func setContinuousRetryKeepaliveActive(ctx context.Context, active bool) {
	keepalive := continuousRetryKeepaliveForContext(ctx)
	if keepalive == nil {
		return
	}
	if controller, ok := keepalive.(interface{ SetActive(bool) }); ok {
		controller.SetActive(active)
		return
	}
	if active {
		keepalive.Activate()
	}
}
