package admin

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type accountLiveItem struct {
	// A nil embedded projection omits Codex credit fields for other planes.
	*AccountLiveCredits
	ActiveRequests   int64 `json:"active_requests"`
	OccupiedRequests int64 `json:"occupied_requests"`
	// DynamicConcurrencyLimit is the admission cap the scheduler currently
	// enforces (base concurrency after health-tier and quota guards).
	DynamicConcurrencyLimit int64 `json:"dynamic_concurrency_limit"`
	// BaseConcurrencyEffective is the configured cap (account override,
	// then group, then the global default) before any runtime reduction.
	BaseConcurrencyEffective int64 `json:"base_concurrency_effective"`

	// Runtime usage/status projection so the account page can reflect WS frame
	// observations without waiting for a list reload. Optional pointers keep the
	// backward-compat contract: an older backend omits the key (frontend keeps
	// the list value); a pointer that is nil serializes as null (frontend clears
	// the now-invalid window).
	Status           *string  `json:"status,omitempty"`
	UsingCredits     *bool    `json:"using_credits,omitempty"`
	PlanType         *string  `json:"plan_type,omitempty"`
	UsagePercent5h   *float64 `json:"usage_percent_5h"`
	Reset5hAt        *string  `json:"reset_5h_at"`
	UsageUpdatedAt5h *string  `json:"codex_5h_usage_updated_at"`
	UsagePercent7d   *float64 `json:"usage_percent_7d"`
	Reset7dAt        *string  `json:"reset_7d_at"`
	UsageUpdatedAt   *string  `json:"codex_usage_updated_at"`
	// Frame freshness proves the WS quota ingestion path is working for this
	// account; omitted until the first frame is applied.
	UsageFrameAt   *string `json:"codex_usage_frame_at,omitempty"`
	UsageFrameKind *string `json:"codex_usage_frame_kind,omitempty"`
}

// AccountLiveCredits uses the existing account-list credit field contract.
type AccountLiveCredits struct {
	Valid                bool    `json:"credits_valid"`
	Balance              *string `json:"credits_balance"`
	HasCredits           *bool   `json:"credits_has_credits"`
	Unlimited            *bool   `json:"credits_unlimited"`
	OverageLimitReached  *bool   `json:"credits_overage_limit_reached"`
	SpendControlReached  *bool   `json:"credits_spend_control_reached"`
	RateLimitReachedType string  `json:"credits_rate_limit_reached_type"`
}

// GetAccountLiveState returns request-local runtime counters for the visible
// account page. Scheduler counters use in-memory atomics; concurrency caps are
// the scheduler's maintained values and are not recomputed here. The usage
// projection is a single read lock per account and never rebuilds a snapshot,
// so it stays cheap at the one-second cadence even for a full page.
func (h *Handler) GetAccountLiveState(c *gin.Context) {
	ids, err := parseAccountListIDs(c.Query("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, "ids 参数无效")
		return
	}
	if len(ids) > accountListPageMax {
		writeError(c, http.StatusBadRequest, "ids 最多允许 500 个")
		return
	}

	live := make(map[int64]accountLiveItem, len(ids))
	for _, id := range ids {
		account := h.store.FindByID(id)
		if account == nil {
			continue
		}
		item := accountLiveItem{
			ActiveRequests:           account.GetActiveRequests(),
			OccupiedRequests:         account.GetOccupiedRequests(),
			DynamicConcurrencyLimit:  account.GetDynamicConcurrencyLimit(),
			BaseConcurrencyEffective: account.GetBaseConcurrencyEffective(),
		}
		// Antigravity rows use a persisted quota snapshot; runtimeStatusLocked
		// does not model their display status, so leave the row's fields alone.
		if !account.IsAntigravityAPI() {
			usage := account.GetAccountLiveUsageSnapshot()
			item.PlanType = &usage.PlanType
			item.AccountLiveCredits = &AccountLiveCredits{Valid: usage.CreditsValid}
			if usage.CreditsValid {
				item.Balance = usage.Credits.Balance
				item.HasCredits = &usage.Credits.HasCredits
				item.Unlimited = &usage.Credits.Unlimited
				item.OverageLimitReached = &usage.Credits.OverageLimitReached
				item.SpendControlReached = usage.Credits.SpendControlReached
				item.RateLimitReachedType = usage.Credits.RateLimitReachedType
			}
			status := usage.Status
			item.Status = &status
			usingCredits := usage.UsingCredits
			item.UsingCredits = &usingCredits
			if usage.UsagePercent5hValid {
				pct := usage.UsagePercent5h
				item.UsagePercent5h = &pct
			}
			item.Reset5hAt = optionalLiveTime(usage.Reset5hAt)
			item.UsageUpdatedAt5h = optionalLiveTime(usage.UsageUpdatedAt5h)
			if usage.UsagePercent7dValid {
				pct := usage.UsagePercent7d
				item.UsagePercent7d = &pct
			}
			item.Reset7dAt = optionalLiveTime(usage.Reset7dAt)
			item.UsageUpdatedAt = optionalLiveTime(usage.UsageUpdatedAt)
			if !usage.UsageFrameLastAt.IsZero() {
				item.UsageFrameAt = optionalLiveTime(usage.UsageFrameLastAt)
				kind := usage.UsageFrameLastKind
				item.UsageFrameKind = &kind
			}
		}
		live[id] = item
	}
	c.JSON(http.StatusOK, gin.H{
		"accounts":                    live,
		"session_slot_buffer_enabled": h.store.SessionSlotBufferEnabled(),
	})
}

func optionalLiveTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	formatted := t.Format(time.RFC3339)
	return &formatted
}
