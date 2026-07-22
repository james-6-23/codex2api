package auth

import (
	"strings"
	"testing"
	"time"
)

func TestRuntimeStatusShowsRefreshingForRTWithoutAccessToken(t *testing.T) {
	acc := &Account{
		RefreshToken: "rt-test",
		Status:       StatusReady,
	}

	if got := acc.RuntimeStatus(); got != "refreshing" {
		t.Fatalf("RuntimeStatus() = %q, want refreshing", got)
	}
}

func TestRuntimeStatusKeepsErrorForFailedRTAccount(t *testing.T) {
	acc := &Account{
		RefreshToken: "rt-test",
		Status:       StatusError,
		ErrorMsg:     "invalid_grant",
	}

	if got := acc.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
}

func TestMarkErrorAndClearCooldownRoundTrip(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		Status:      StatusReady,
	}

	store.MarkError(acc, "batch test failed")
	if got := acc.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() after MarkError = %q, want error", got)
	}

	store.ClearCooldown(acc)
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() after ClearCooldown = %q, want active", got)
	}
}

func TestMarkCooldownWithErrorKeepsUnauthorizedStatusAndMessage(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}

	store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", "上游返回 401: token_invalidated")

	if got := acc.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	acc.Mu().RLock()
	errorMsg := acc.ErrorMsg
	cooldownReason := acc.CooldownReason
	cooldownUntil := acc.CooldownUtil
	status := acc.Status
	acc.Mu().RUnlock()
	if status != StatusCooldown {
		t.Fatalf("Status = %v, want cooldown", status)
	}
	if cooldownReason != "unauthorized" || cooldownUntil.IsZero() {
		t.Fatalf("cooldown = (%q, %s), want unauthorized with deadline", cooldownReason, cooldownUntil)
	}
	if !strings.Contains(errorMsg, "token_invalidated") {
		t.Fatalf("ErrorMsg = %q, want token_invalidated", errorMsg)
	}
}

// TestUnauthorizedCooldownDurationPolicies 验证 unauthorized 的自适应和精确时长冷却策略。
func TestUnauthorizedCooldownDurationPolicies(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		recent   bool
		duration time.Duration
		want     time.Duration
	}{
		{name: "MarkCooldown fresh uses 6h", method: "cooldown", duration: time.Hour, want: 6 * time.Hour},
		{name: "MarkCooldown recent uses 24h", method: "cooldown", recent: true, duration: time.Hour, want: 24 * time.Hour},
		{name: "MarkCooldownWithError fresh uses 6h", method: "with_error", duration: time.Hour, want: 6 * time.Hour},
		{name: "MarkCooldownWithError recent uses 24h", method: "with_error", recent: true, duration: time.Hour, want: 24 * time.Hour},
		{name: "exact method preserves arbitrary duration", method: "exact", duration: 90 * time.Minute, want: 90 * time.Minute},
		{name: "exact method preserves 24h", method: "exact", duration: 24 * time.Hour, want: 24 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore(nil, nil, nil)
			acc := &Account{
				DBID:        1,
				AccessToken: "at-test",
				Status:      StatusReady,
				HealthTier:  HealthTierHealthy,
			}
			if tt.recent {
				acc.LastUnauthorizedAt = time.Now().Add(-time.Hour)
			}

			switch tt.method {
			case "cooldown":
				store.MarkCooldown(acc, tt.duration, "unauthorized")
			case "with_error":
				store.MarkCooldownWithError(acc, tt.duration, "unauthorized", "unauthorized")
			case "exact":
				store.MarkCooldownWithErrorExactDuration(acc, tt.duration, "unauthorized", "deleted runtime")
			default:
				t.Fatalf("unknown method %q", tt.method)
			}

			reason, until := acc.GetCooldownSnapshot()
			if reason != "unauthorized" {
				t.Fatalf("cooldown reason = %q, want unauthorized", reason)
			}
			if remaining := time.Until(until); remaining < tt.want-time.Minute || remaining > tt.want {
				t.Fatalf("cooldown remaining = %s, want approximately %s", remaining, tt.want)
			}
		})
	}
}

func TestMarkUsage7dRateLimitedUsesActiveResetWindow(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "team",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(time.Hour))

	if !store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = false, want true")
	}
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false for active 7d usage limit")
	}
}

func TestMarkUsage7dRateLimitedSkipsCreditUsageWindow(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:                  1,
		AccessToken:           "at-test",
		PlanType:              "team",
		Status:                StatusReady,
		HealthTier:            HealthTierHealthy,
		CreditEnabled:         true,
		CreditSkipUsageWindow: true,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(time.Hour))

	if store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = true, want false for credit account")
	}
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active for credit account", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true for credit account")
	}
}

func TestMarkUsage7dRateLimitedSkipsExpiredResetWindow(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "team",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(-time.Minute))

	if store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = true, want false for expired reset")
	}
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active after expired reset", got)
	}
}
