package proxy

import (
	"context"
	"log"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
)

type retryAccountExclusions struct {
	hard map[int64]bool
	soft map[int64]bool
}

// websocketHTTPFallbackState carries the already-acquired account lease across
// a one-time WebSocket -> HTTP transport downgrade. A close 1009 is a transport
// limitation, not a reason to release the account and run the scheduler again.
type websocketHTTPFallbackState struct {
	forcedHTTP bool
	account    *auth.Account
	proxyURL   string
	wsElapsed  time.Duration
	source     string
	fallbackID string
	startedAt  time.Time
}

func (s *websocketHTTPFallbackState) Retain(account *auth.Account, proxyURL string, wsElapsed time.Duration, source string) {
	if s == nil || account == nil {
		return
	}
	s.forcedHTTP = true
	s.account = account
	s.proxyURL = proxyURL
	s.wsElapsed = wsElapsed
	s.source = source
	if s.startedAt.IsZero() {
		s.startedAt = time.Now().Add(-wsElapsed)
	}
	if s.fallbackID == "" {
		s.fallbackID = uuid.NewString()
	}
}

func (s *websocketHTTPFallbackState) Take() (*auth.Account, string, bool) {
	if s == nil || s.account == nil {
		return nil, "", false
	}
	account := s.account
	proxyURL := s.proxyURL
	s.account = nil
	s.proxyURL = ""
	return account, proxyURL, true
}

func (s *websocketHTTPFallbackState) ForceHTTP() bool {
	return s != nil && s.forcedHTTP
}

func (s *websocketHTTPFallbackState) WSElapsed() time.Duration {
	if s == nil {
		return 0
	}
	return s.wsElapsed
}

func (s *websocketHTTPFallbackState) ID() string {
	if s == nil {
		return ""
	}
	return s.fallbackID
}

func (s *websocketHTTPFallbackState) Source() string {
	if s == nil || s.source == "" {
		return "peer_close"
	}
	return s.source
}

func (s *websocketHTTPFallbackState) LogHTTPAttemptCompletion(endpoint string, accountID int64, attemptIndex, httpElapsedMs, httpFirstEventMs, statusCode int) {
	if s == nil || !s.forcedHTTP {
		return
	}
	wsElapsedMs := s.wsElapsed.Milliseconds()
	totalElapsedMs := wsElapsedMs + int64(httpElapsedMs)
	if !s.startedAt.IsZero() {
		totalElapsedMs = time.Since(s.startedAt).Milliseconds()
	}
	totalFirstEventMs := int64(0)
	if httpFirstEventMs > 0 {
		postFirstEventMs := int64(httpElapsedMs - httpFirstEventMs)
		if postFirstEventMs < 0 {
			postFirstEventMs = 0
		}
		totalFirstEventMs = totalElapsedMs - postFirstEventMs
		if totalFirstEventMs < 0 {
			totalFirstEventMs = 0
		}
	}
	log.Printf("WebSocket 1009 HTTP 降级尝试结束 (fallback_id=%s, source=%s, attempt=%d, account=%d, endpoint=%s, status=%d, ws_elapsed_ms=%d, http_elapsed_ms=%d, http_first_event_ms=%d, total_first_event_ms=%d, total_elapsed_ms=%d)",
		s.fallbackID, s.Source(), attemptIndex, accountID, endpoint, statusCode, wsElapsedMs, httpElapsedMs, httpFirstEventMs,
		totalFirstEventMs, totalElapsedMs)
}

func newRetryAccountExclusions() *retryAccountExclusions {
	return &retryAccountExclusions{
		hard: make(map[int64]bool),
		soft: make(map[int64]bool),
	}
}

func (r *retryAccountExclusions) MarkHard(accountID int64) {
	if r == nil || accountID == 0 {
		return
	}
	r.hard[accountID] = true
	delete(r.soft, accountID)
}

func (r *retryAccountExclusions) MarkSoftFirstTokenTimeout(accountID int64) {
	if r == nil || accountID == 0 {
		return
	}
	if r.hard[accountID] {
		return
	}
	r.soft[accountID] = true
}

func (r *retryAccountExclusions) ResetSoft() bool {
	if r == nil || len(r.soft) == 0 {
		return false
	}
	r.soft = make(map[int64]bool)
	return true
}

func (r *retryAccountExclusions) ForSelection() map[int64]bool {
	if r == nil || (len(r.hard) == 0 && len(r.soft) == 0) {
		return nil
	}
	exclude := make(map[int64]bool, len(r.hard)+len(r.soft))
	for id := range r.hard {
		exclude[id] = true
	}
	for id := range r.soft {
		exclude[id] = true
	}
	return exclude
}

func (h *Handler) nextRetryAccountForSession(ctx context.Context, affinityKey string, apiKeyID int64, exclusions *retryAccountExclusions, filter auth.AccountFilter) (*auth.Account, string) {
	if h == nil || h.store == nil {
		return nil, ""
	}
	for {
		exclude := exclusions.ForSelection()
		account, stickyProxyURL := h.nextAccountForSessionWithFilter(affinityKey, apiKeyID, exclude, filter)
		if account != nil {
			return account, stickyProxyURL
		}
		account, stickyProxyURL = h.store.WaitForSessionAvailableWithFilter(ctx, affinityKey, 30*time.Second, apiKeyID, exclude, filter)
		if account != nil {
			return account, stickyProxyURL
		}
		if !exclusions.ResetSoft() {
			return nil, ""
		}
		log.Printf("首字超时账号池已试完，清空本次请求软排除并进入下一轮重试")
	}
}

func isFirstTokenTimeoutOutcome(outcome streamOutcome) bool {
	return outcome.failureKind == "timeout"
}
