package proxy

import (
	"net/http"

	"github.com/codex2api/auth"
)

// sessionFailureDisposition separates retry routing from affinity lifetime.
// The old implementation coupled both decisions by unbinding before it knew
// whether a retry would happen, which made a client-side retry migrate the
// whole conversation even when the configured retry budget was zero.
type sessionFailureDisposition struct {
	retrySameAccount bool
	retainAffinity   bool
	pinAffinity      bool
	reportAccount    bool
	permanentAccount bool
}

func isPermanentAccountHTTPFailure(statusCode int, body []byte) bool {
	switch statusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusUpgradeRequired:
		return true
	case http.StatusBadRequest:
		return isCodexModelUnsupportedError(body)
	}
	return IsDeactivatedWorkspaceError(body) || IsUsageLimitReachedError(body)
}

func isRequestScopedHTTPFailure(statusCode int, body []byte) bool {
	if statusCode < 400 || statusCode >= 500 || statusCode == http.StatusTooManyRequests {
		return false
	}
	return !isPermanentAccountHTTPFailure(statusCode, body)
}

func (h *Handler) httpSessionFailureDisposition(statusCode int, body []byte, shouldRetry bool) sessionFailureDisposition {
	permanent := isPermanentAccountHTTPFailure(statusCode, body)
	requestScoped := isRequestScopedHTTPFailure(statusCode, body)
	sticky := h.stickyTransportRetryEnabled()
	retrySame := sticky && shouldRetry && !permanent
	retain := requestScoped || (sticky && !permanent)
	return sessionFailureDisposition{
		retrySameAccount: retrySame,
		retainAffinity:   retain,
		pinAffinity:      sticky && !permanent && !requestScoped,
		reportAccount:    !requestScoped && !retrySame,
		permanentAccount: permanent,
	}
}

func (h *Handler) streamSessionFailureDisposition(outcome streamOutcome, payload []byte, shouldRetry bool) sessionFailureDisposition {
	body := payload
	if len(body) == 0 && outcome.failureMessage != "" {
		body = []byte(outcome.failureMessage)
	}
	return h.httpSessionFailureDisposition(outcome.logStatusCode, body, shouldRetry)
}

func (h *Handler) transportSessionFailureDisposition(shouldRetry bool, allowSticky bool) sessionFailureDisposition {
	sticky := allowSticky && h.stickyTransportRetryEnabled()
	return sessionFailureDisposition{
		retrySameAccount: sticky && shouldRetry,
		retainAffinity:   sticky,
		pinAffinity:      sticky,
		reportAccount:    !sticky || !shouldRetry,
	}
}

func (h *Handler) applySessionFailureAffinity(affinityKey string, account *auth.Account, disposition sessionFailureDisposition) {
	if h == nil || h.store == nil || account == nil {
		return
	}
	if disposition.retainAffinity {
		if disposition.pinAffinity {
			h.store.PinSessionAffinityAfterTransientFailure(affinityKey, account.ID())
		}
		return
	}
	h.store.UnbindSessionAffinity(affinityKey, account.ID())
}
