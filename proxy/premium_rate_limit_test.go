package proxy

import (
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func newProxyPremiumTestStore() *auth.Store {
	return auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:                   4,
		TestConcurrency:                  1,
		TestModel:                        "gpt-5.4",
		BackgroundRefreshIntervalMinutes: 2,
		UsageProbeMaxAgeMinutes:          10,
		RecoveryProbeIntervalMinutes:     30,
	})
}

func TestApply429CooldownPremium5hWindowMarksRateLimited(t *testing.T) {
	store := newProxyPremiumTestStore()
	acc := &auth.Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "plus",
		Status:      auth.StatusReady,
	}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "1800")

	decision := Apply429Cooldown(store, acc, nil, resp, "gpt-5.4")

	if decision.Scope != rateLimitScopeAccount || decision.Reason != "rate_limited_5h" {
		t.Fatalf("Apply429Cooldown() = %#v, want premium 5h account cooldown", decision)
	}
	if !acc.IsPremium5hRateLimited() {
		t.Fatal("account should enter premium 5h rate_limited state")
	}
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want %q", got, "rate_limited")
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false while premium 5h limit is active")
	}
	if got := acc.GetDynamicConcurrencyLimit(); got != 1 {
		t.Fatalf("GetDynamicConcurrencyLimit() = %d, want 1", got)
	}
}

func TestApply429CooldownUnknownRateLimitSetsModelCooldown(t *testing.T) {
	store := newProxyPremiumTestStore()
	acc := &auth.Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "pro",
		Status:      auth.StatusReady,
	}

	start := time.Now()
	decision := Apply429Cooldown(store, acc, []byte(`{"error":{"type":"rate_limit_error"}}`), nil, "gpt-5.4")

	if decision.Scope != rateLimitScopeModel {
		t.Fatalf("Apply429Cooldown().Scope = %q, want model", decision.Scope)
	}
	if decision.ResetAt.Before(start.Add(4*time.Minute)) || decision.ResetAt.After(start.Add(6*time.Minute)) {
		t.Fatalf("ResetAt = %v, want about 5m from now", decision.ResetAt)
	}
	if !acc.IsModelRateLimited("gpt-5.4") {
		t.Fatal("account model should enter short cooldown")
	}
}

func TestApply429CooldownUsageLimitWithoutResetStaysAccountScoped(t *testing.T) {
	store := newProxyPremiumTestStore()
	acc := &auth.Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "pro",
		Status:      auth.StatusReady,
	}

	start := time.Now()
	decision := Apply429Cooldown(store, acc, []byte(`{"error":{"type":"usage_limit_reached"}}`), nil, "gpt-5.4")

	if decision.Scope != rateLimitScopeAccount || decision.Reason != "usage_limit" {
		t.Fatalf("Apply429Cooldown() = %#v, want account usage_limit", decision)
	}
	if decision.ResetAt.Before(start.Add(4*time.Hour)) || decision.ResetAt.After(start.Add(6*time.Hour)) {
		t.Fatalf("ResetAt = %v, want about 5h from now", decision.ResetAt)
	}
	if acc.IsModelRateLimited("gpt-5.4") {
		t.Fatal("usage_limit_reached should not be stored as a model cooldown")
	}
}

func TestSyncCodexUsageStatePremium5hOnlyHeadersMarksRateLimited(t *testing.T) {
	store := newProxyPremiumTestStore()
	acc := &auth.Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "team",
		Status:      auth.StatusReady,
	}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "900")

	result := SyncCodexUsageState(store, acc, resp)

	if result.HasUsage7d {
		t.Fatal("HasUsage7d = true, want false for 5h-only headers")
	}
	if !result.HasUsage5h {
		t.Fatal("HasUsage5h = false, want true")
	}
	if !result.Persisted5hOnly {
		t.Fatal("Persisted5hOnly = false, want true")
	}
	if !result.Premium5hRateLimited {
		t.Fatal("Premium5hRateLimited = false, want true")
	}
	if !acc.IsPremium5hRateLimited() {
		t.Fatal("account should enter premium 5h rate_limited state from headers alone")
	}
}

func TestSyncCodexUsageStateCreditAccountSkipsPremium5hWindowLimit(t *testing.T) {
	store := newProxyPremiumTestStore()
	acc := &auth.Account{
		DBID:                  1,
		AccessToken:           "token",
		PlanType:              "team",
		Status:                auth.StatusReady,
		CreditEnabled:         true,
		CreditSkipUsageWindow: true,
	}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "900")

	result := SyncCodexUsageState(store, acc, resp)

	if !result.HasUsage5h {
		t.Fatal("HasUsage5h = false, want true")
	}
	if !result.Persisted5hOnly {
		t.Fatal("Persisted5hOnly = false, want true")
	}
	if result.Premium5hRateLimited {
		t.Fatal("Premium5hRateLimited = true, want false for credit account")
	}
	if acc.IsPremium5hRateLimited() {
		t.Fatal("credit account should not enter premium 5h rate_limited state from usage-window headers")
	}
	pct5h, _, ok := acc.GetUsageSnapshot5h()
	if !ok || pct5h != 100 {
		t.Fatalf("5h snapshot = (%v, %v), want 100 with valid snapshot", pct5h, ok)
	}
}
