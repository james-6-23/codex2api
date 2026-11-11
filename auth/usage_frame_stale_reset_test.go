package auth

import (
	"testing"
	"time"
)

// Issue #862: after the 5h quota resets, a stale 100% snapshot (reset already
// in the past) must not re-arm the premium 5h cooldown. markPremium5hRateLimited
// would otherwise fabricate a fresh 5h fallback window and keep the account
// unavailable indefinitely.
func TestUsageFrameCooldownsIgnoreStale5hWindow(t *testing.T) {
	store := NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		PlanType:            "plus",
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		UsagePercent5hValid: true,
	}
	store.AddAccount(acc)
	staleReset := time.Now().Add(-time.Minute)
	acc.SetUsageSnapshot5hAt(100, staleReset, staleReset)

	limited7d, limited5h := store.MarkUsageFrameCooldowns(acc, time.Now())
	if limited7d || limited5h {
		t.Fatal("a stale exhausted 5h window re-armed a cooldown after the window reset")
	}
	acc.mu.RLock()
	cooling := acc.Status == StatusCooldown && acc.CooldownReason == premium5hCooldownReason
	acc.mu.RUnlock()
	if cooling {
		t.Fatal("stale 5h snapshot re-armed the premium 5h cooldown")
	}
}

// Positive control: a genuinely exhausted current 5h window still fences the
// account until its reset time.
func TestUsageFrameCooldownsMarkActive5hWindow(t *testing.T) {
	store := NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		PlanType:            "plus",
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		UsagePercent5hValid: true,
	}
	store.AddAccount(acc)
	reset := time.Now().Add(30 * time.Minute)
	acc.SetUsageSnapshot5hAt(100, reset, time.Now())

	_, limited5h := store.MarkUsageFrameCooldowns(acc, time.Now())
	if !limited5h {
		t.Fatal("an exhausted current 5h window was not marked rate-limited")
	}
	acc.mu.RLock()
	cooling := acc.Status == StatusCooldown && acc.CooldownReason == premium5hCooldownReason && acc.CooldownUtil.Equal(reset)
	acc.mu.RUnlock()
	if !cooling {
		t.Fatal("premium 5h cooldown missing or not aligned with the window reset")
	}
}
