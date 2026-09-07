package proxy

import (
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const maxAccountSessionObservationCacheEntries = 100000
const accountSessionUsagePeriodContextKey = "account_session_usage_period"
const promptSessionWindowExpiresAtContextKey = "prompt_session_window_expires_at"
const accountSessionObservationRefreshInterval = 5 * time.Minute

type accountSessionObservationCacheEntry struct {
	Attributed bool
	RecordedAt time.Time
}

func clearAccountSessionObservationContext(c *gin.Context) {
	if c != nil {
		c.Set(accountSessionUsagePeriodContextKey, auth.AccountSessionUsagePeriod{})
		c.Set(promptSessionWindowExpiresAtContextKey, time.Time{})
	}
}

// populateAccountSessionObservation enriches the normal usage-log path with
// verified NewAPI display metadata and marks only the first local sighting of
// an account/session pair for the persistent operational aggregate. This data
// is deliberately separate from prompt-risk scoring and enforcement.
func (h *Handler) populateAccountSessionObservation(c *gin.Context, input *database.UsageLogInput) {
	if h == nil || c == nil || input == nil {
		return
	}
	audit := h.capturePromptFilterAuditContext(c)
	input.NewAPIUserName = strings.TrimSpace(audit.NewAPIUserName)
	input.NewAPIPlatform = strings.TrimSpace(audit.NewAPIPlatform)
	input.NewAPIUserID = strings.TrimSpace(audit.NewAPIUserID)
	if requestSessionAccountingBypass(c) {
		input.SessionHash = ""
		input.RecordSessionObservation = false
		return
	}
	input.SessionHash = strings.TrimSpace(audit.RootSessionHash)
	if input.AccountID <= 0 || input.SessionHash == "" {
		return
	}
	if raw, exists := c.Get(promptSessionWindowExpiresAtContextKey); exists {
		input.SessionWindowExpiresAt, _ = raw.(time.Time)
	}
	if raw, exists := c.Get(accountSessionUsagePeriodContextKey); exists {
		if period, valid := raw.(auth.AccountSessionUsagePeriod); valid && period.AccountID == input.AccountID {
			input.SessionUsagePeriodID = period.ID
			input.SessionUsageStartedAt = period.StartedAt
		}
	}

	key := fmt.Sprintf("%d:%s", input.AccountID, input.SessionHash)
	hasVerifiedUser := input.NewAPIUserID != ""
	h.accountSessionObservationMu.Lock()
	if h.accountSessionObservations == nil {
		h.accountSessionObservations = make(map[string]accountSessionObservationCacheEntry)
	}
	previous, exists := h.accountSessionObservations[key]
	if !exists && len(h.accountSessionObservations) >= maxAccountSessionObservationCacheEntries {
		clear(h.accountSessionObservations)
	}
	now := time.Now().UTC()
	input.RecordSessionObservation = !exists || (hasVerifiedUser && !previous.Attributed) || now.Sub(previous.RecordedAt) >= accountSessionObservationRefreshInterval
	if input.RecordSessionObservation {
		h.accountSessionObservations[key] = accountSessionObservationCacheEntry{
			Attributed: previous.Attributed || hasVerifiedUser,
			RecordedAt: now,
		}
	}
	h.accountSessionObservationMu.Unlock()
	input.ObservedAt = now
}
