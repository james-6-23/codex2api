package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

// ProbeUsageSnapshot 主动发送最小探针请求刷新账号用量
func (h *Handler) ProbeUsageSnapshot(ctx context.Context, account *auth.Account) error {
	if account == nil {
		return nil
	}

	account.Mu().RLock()
	hasToken := account.AccessToken != ""
	account.Mu().RUnlock()
	if !hasToken {
		return nil
	}

	payload := buildTestPayload(h.store.GetTestModel())
	resp, err := proxy.ExecuteRequest(ctx, account, payload, "", "", "", nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if usagePct, ok := proxy.ParseCodexUsageHeaders(resp, account); ok {
		h.store.PersistUsageSnapshot(account, usagePct)
	}

	_, _ = io.Copy(io.Discard, resp.Body)

	switch resp.StatusCode {
	case http.StatusOK:
		h.store.ReportRequestSuccess(account, 0)
		h.store.ClearCooldown(account)
		return nil
	case http.StatusUnauthorized:
		h.store.ReportRequestFailure(account, "client", 0)
		h.store.MarkCooldown(account, 24*time.Hour, "unauthorized")
		return nil
	case http.StatusTooManyRequests:
		h.store.ReportRequestFailure(account, "client", 0)
		h.store.MarkCooldown(account, 5*time.Minute, "rate_limited")
		return nil
	default:
		if resp.StatusCode >= 500 {
			h.store.ReportRequestFailure(account, "server", 0)
		} else if resp.StatusCode >= 400 {
			h.store.ReportRequestFailure(account, "client", 0)
		}
		return fmt.Errorf("探针返回状态 %d", resp.StatusCode)
	}
}
