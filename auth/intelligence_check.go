package auth

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

type intelligencePausesKey struct{}

// Attach one snapshot to bulk account loads instead of querying once per account.
func (s *Store) withIntelligencePauses(ctx context.Context) (context.Context, error) {
	if s.db == nil {
		return ctx, nil
	}
	ids, err := s.db.ListIntelligenceCheckPausedAccountIDs(ctx)
	if err != nil {
		return ctx, err
	}
	paused := make(map[int64]bool, len(ids))
	for _, id := range ids {
		paused[id] = true
	}
	return context.WithValue(ctx, intelligencePausesKey{}, paused), nil
}
func (s *Store) intelligencePaused(ctx context.Context, id int64) bool {
	if paused, ok := ctx.Value(intelligencePausesKey{}).(map[int64]bool); ok {
		return paused[id]
	}
	if s.db == nil {
		return false
	}
	paused, err := s.db.IsIntelligenceCheckPaused(ctx, id)
	if err != nil {
		log.Printf("读取账号 %d 检测暂停失败: %v", id, err)
		return true
	}
	return paused
}

// RefreshIntelligenceAccount publishes committed action/group changes through
// the same snapshot path as cross-instance outbox delivery.
func (s *Store) RefreshIntelligenceAccount(ctx context.Context, id int64) error {
	return s.reloadDispatchAccountByID(ctx, id)
}

// IntelligenceCheckQuotaHold reports whether a detector request for model is
// known to be futile because the account is rate limited or out of quota, and
// when that is expected to end (zero when the recovery time is unknown).
//
// It follows the same facts that keep a fresh request away from the account:
// usage/rate-limit cooldowns, exhausted usage windows unless credits cover
// them, the source's own quota gate, and a quota cooldown on the model itself.
// Disabled or paused accounts, authentication failures and errors are not
// quota holds, and neither is the configurable auto-pause threshold, under
// which the upstream still answers.
func (a *Account) IntelligenceCheckQuotaHold(model string, policy DispatchPolicy, now time.Time) (time.Time, bool) {
	if a == nil {
		return time.Time{}, false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.Status == StatusError {
		return time.Time{}, false
	}
	var until time.Time
	held := false
	hold := func(at time.Time) {
		held = true
		if at.After(now) && at.After(until) {
			until = at
		}
	}
	if a.isAntigravityAPILocked() {
		blocked, resetAt := false, time.Time{}
		if strings.TrimSpace(model) != "" {
			blocked, _, resetAt = a.isAntigravityModelQuotaExhaustedLocked(model, now)
		}
		if !blocked {
			blocked, _, resetAt = a.IsAntigravityQuotaExhaustedLocked(now)
		}
		if blocked {
			hold(resetAt)
		}
	}
	// Spark has its own window: only a transient throttle on the shared account
	// budget, not a 5h/7d cooldown, rejects a spark request.
	if a.Status == StatusCooldown && now.Before(a.CooldownUtil) && isUsageLimitCooldownReason(a.CooldownReason) &&
		(policy != DispatchPolicySpark || a.isTransientRateLimitCooldownLocked()) {
		hold(a.CooldownUtil)
	}
	switch {
	case policy == DispatchPolicySpark:
		if a.sparkUsageExhaustedLocked(now) {
			hold(a.ResetSparkAt)
		}
	case !a.isAntigravityAPILocked() && a.usageWindowBlocksFreshDispatchLocked(now):
		var resetAt time.Time
		if a.rawPremium5hRateLimitedLocked(now) {
			resetAt = a.Reset5hAt
		}
		if (a.rawUsageExhaustedLocked() || a.rawUsageWindow7dExhaustedLocked(now)) && a.Reset7dAt.After(resetAt) {
			resetAt = a.Reset7dAt
		}
		hold(resetAt)
	}
	// The expiry of an exhausted billing fact is its freshness, not a reset time.
	if a.isGrokAPILocked() && a.GrokFactsGeneration == a.CredentialGeneration && a.GrokBillingExhausted && now.Before(a.GrokBillingExpiresAt) {
		hold(time.Time{})
	}
	if cooldown, ok := a.ModelCooldowns[normalizeModelCooldownKey(model)]; ok && cooldown.ResetAt.After(now) {
		switch cooldown.Reason {
		case "rate_limited", "rate_limited_model", "model_capacity", "quota_exhausted", "usage_limited":
			hold(cooldown.ResetAt)
		}
	}
	return until, held
}

// TryAcquireIntelligenceCheck reserves an ordinary occupied slot but ignores
// administrative/quality pauses so recovery probes and explicit manual checks
// can run; the queue already keeps automatic checks off manually disabled
// accounts. It never changes either pause or the account's
// authentication/quota status.
func (s *Store) TryAcquireIntelligenceCheck(a *Account) bool {
	if a == nil || atomic.LoadInt32(&a.Disabled) != 0 {
		return false
	}
	a.mu.RLock()
	limit := a.effectiveBaseConcurrencyLocked(s.maxConcurrency.Load())
	a.mu.RUnlock()
	for {
		occupied := a.OccupiedRequests.Load()
		if occupied >= limit || a.ActiveRequests.Load() >= limit {
			return false
		}
		if a.OccupiedRequests.CompareAndSwap(occupied, occupied+1) {
			a.ActiveRequests.Add(1)
			if atomic.LoadInt32(&a.Disabled) != 0 {
				releaseOccupiedAccountSlot(a)
				return false
			}
			s.fastSchedulerUpdate(a)
			return true
		}
	}
}
