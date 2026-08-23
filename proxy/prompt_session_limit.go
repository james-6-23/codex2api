package proxy

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const promptSessionLimitCleanupInterval = time.Minute

type promptSessionCreationLimitStatus struct {
	Enabled       bool
	Subject       string
	SessionHash   string
	Used          int
	Limit         int
	WindowSeconds int
	RetryAfter    int
	Existing      bool
}

// checkPromptSessionCreationLimitForSelectedAccount deliberately runs only
// after scheduling has selected the concrete upstream account. The user-level
// creation limit is an extension of account session-window control: relay/API
// accounts that have not enabled session capacity must not make a passive
// review/naming request consume one of the user's windows merely because the
// downstream Codex client carried its normal Session-Id.
func (h *Handler) checkPromptSessionCreationLimitForSelectedAccount(c *gin.Context, body []byte, account *auth.Account) (promptSessionCreationLimitStatus, bool) {
	if account == nil || (c != nil && c.GetBool("prompt_intelligence_internal")) {
		return promptSessionCreationLimitStatus{}, false
	}
	enabled, _, _ := account.SessionCapacityConfig()
	if !enabled {
		return promptSessionCreationLimitStatus{}, false
	}
	return h.checkPromptSessionCreationLimit(c, h.promptFilterConfigForRequest(c), body)
}

// releaseSelectedAccountAfterPromptSessionRejection undoes only the capacity
// admission made by the current selection. An already-active session may have
// existed before this request and must remain available for its other turns.
func (h *Handler) releaseSelectedAccountAfterPromptSessionRejection(account *auth.Account, affinityKey string, priorSessionAccountID int64) {
	if h == nil || h.store == nil || account == nil {
		return
	}
	if priorSessionAccountID != account.ID() {
		h.store.RemoveAccountSession(account.ID(), affinityKey)
	}
	h.store.Release(account)
}

func (h *Handler) checkPromptSessionCreationLimit(c *gin.Context, cfg promptfilter.Config, body []byte) (promptSessionCreationLimitStatus, bool) {
	risk := cfg.Advanced.Risk
	status := promptSessionCreationLimitStatus{
		Enabled:       risk.SessionCreationLimitEnabled,
		Limit:         risk.SessionCreationLimit,
		WindowSeconds: risk.SessionCreationLimitWindowSeconds,
	}
	if h == nil || c == nil {
		return status, false
	}

	policyStatus, policyContext := h.cachedNewAPIPolicyAuditState(c)
	verifiedPerson := (policyStatus == "verified" || policyStatus == "signed_response") &&
		policyContext.MetaVerified && strings.TrimSpace(policyContext.Identity.UserID) != ""
	if verifiedPerson && h.store != nil {
		if override, ok := h.store.GetPromptSessionLimitOverride(policyContext.Platform, policyContext.Identity.UserID); ok {
			switch override.Mode {
			case database.PromptSessionLimitModeOff:
				status.Enabled = false
				status.Limit = 0
				status.WindowSeconds = 0
			case database.PromptSessionLimitModeCustom:
				status.Enabled = true
				status.Limit = override.Limit
				status.WindowSeconds = override.WindowSeconds
			}
		}
	}
	if !status.Enabled || status.Limit <= 0 || status.WindowSeconds <= 0 {
		return status, false
	}

	sessionID := ""
	if verifiedPerson {
		sessionID = strings.TrimSpace(policyContext.Meta.SessionFingerprint)
		if sessionID != "" {
			status.Subject = "newapi:" + strings.TrimSpace(policyContext.Platform) + ":" + strings.TrimSpace(policyContext.Identity.UserID)
		}
	}
	if sessionID == "" {
		sessionID = ResolveStableExplicitSessionID(c.Request.Header, body)
	}
	if status.Subject == "" {
		if keyID := requestAPIKeyID(c); keyID > 0 {
			status.Subject = fmt.Sprintf("api-key:%d", keyID)
		}
	}
	// Strong enforcement requires both a stable conversation identity and a
	// stable authenticated subject. Content/IP fallbacks deliberately do not count.
	if sessionID == "" || status.Subject == "" {
		return status, false
	}
	status.SessionHash = hashRiskIdentity(sessionID)
	now := time.Now()
	cutoff := now.Add(-time.Duration(status.WindowSeconds) * time.Second)

	h.promptSessionLimitMu.Lock()
	if h.promptSessionLimits == nil {
		h.promptSessionLimits = make(map[string]map[string]time.Time)
	}
	if h.promptSessionLastCleanup.IsZero() || now.Sub(h.promptSessionLastCleanup) >= promptSessionLimitCleanupInterval {
		for subject, subjectSessions := range h.promptSessionLimits {
			for key, createdAt := range subjectSessions {
				if createdAt.Before(cutoff) {
					delete(subjectSessions, key)
				}
			}
			if len(subjectSessions) == 0 {
				delete(h.promptSessionLimits, subject)
			}
		}
		h.promptSessionLastCleanup = now
	}
	sessions := h.promptSessionLimits[status.Subject]
	if sessions == nil {
		sessions = make(map[string]time.Time)
		h.promptSessionLimits[status.Subject] = sessions
	}
	oldest := now
	for key, createdAt := range sessions {
		if createdAt.Before(cutoff) {
			delete(sessions, key)
			continue
		}
		if createdAt.Before(oldest) {
			oldest = createdAt
		}
	}
	if len(sessions) == 0 {
		delete(h.promptSessionLimits, status.Subject)
		sessions = make(map[string]time.Time)
		h.promptSessionLimits[status.Subject] = sessions
	}
	if _, exists := sessions[status.SessionHash]; exists {
		status.Existing = true
		status.Used = len(sessions)
		h.promptSessionLimitMu.Unlock()
		writePromptSessionLimitHeaders(c, status)
		return status, false
	}
	if len(sessions) >= status.Limit {
		status.Used = len(sessions)
		status.RetryAfter = int(oldest.Add(time.Duration(status.WindowSeconds) * time.Second).Sub(now).Seconds())
		if status.RetryAfter < 1 {
			status.RetryAfter = 1
		}
		h.promptSessionLimitMu.Unlock()
		writePromptSessionLimitHeaders(c, status)
		return status, true
	}
	sessions[status.SessionHash] = now
	status.Used = len(sessions)
	h.promptSessionLimitMu.Unlock()
	writePromptSessionLimitHeaders(c, status)
	return status, false
}

func writePromptSessionLimitHeaders(c *gin.Context, status promptSessionCreationLimitStatus) {
	if c == nil || !status.Enabled {
		return
	}
	c.Header("X-Codex2API-Session-Limit", strconv.Itoa(status.Limit))
	c.Header("X-Codex2API-Session-Used", strconv.Itoa(status.Used))
	c.Header("X-Codex2API-Session-Window-Seconds", strconv.Itoa(status.WindowSeconds))
	if status.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(status.RetryAfter))
	}
}

func sendPromptSessionCreationLimitError(c *gin.Context, status promptSessionCreationLimitStatus) {
	message := promptSessionCreationLimitMessage(status)
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrorCode("session_creation_limit_exceeded"),
		message,
		api.ErrorTypeInvalidRequest,
	), http.StatusBadRequest)
}

func promptSessionCreationLimitMessage(status promptSessionCreationLimitStatus) string {
	return "当前时间内创建窗口已达到上限，请复用已有会话或稍后再试"
}
