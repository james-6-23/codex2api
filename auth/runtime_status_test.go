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
