package auth

import (
	"context"
	"testing"
	"time"
)

func TestCodexRestartPreservesUnauthorizedUntilVerified(t *testing.T) {
	ctx := context.Background()
	db := newProxyPoolReloadTestDB(t)
	deadline := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	add := func(reason string) int64 {
		t.Helper()
		id, err := db.InsertAccountWithCredentials(ctx, reason, map[string]any{
			"access_token": "test-at", "refresh_token": "test-" + reason,
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		}, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SetCooldownWithError(ctx, id, reason, deadline, "previous rejection"); err != nil {
			t.Fatal(err)
		}
		return id
	}
	unauthorized, limited := add("unauthorized"), add("rate_limited")
	terminal := add("other")
	if err := db.SetError(ctx, terminal, "deactivated_workspace"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetCooldown(ctx, terminal, "rate_limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	startStore := func() *Store {
		t.Helper()
		s := NewStore(db, nil, nil)
		t.Cleanup(s.Stop)
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	store := startStore()
	a := store.FindByID(unauthorized)
	if a == nil || a.RuntimeStatus() != "unauthorized" || a.IsAvailable() || !a.NeedsRecoveryProbe(0) || a.ErrorMsg != "previous rejection" {
		t.Fatal("expired 401 must stay quarantined and eligible for verification")
	}
	row, err := db.GetAccountByID(ctx, unauthorized)
	if err != nil || row.CooldownReason != "unauthorized" || !row.CooldownUntil.Time.Equal(deadline) {
		t.Fatal("startup erased or extended the persisted 401", err)
	}
	if !store.FindByID(limited).IsAvailable() || store.FindByID(terminal).RuntimeStatus() != "error" {
		t.Fatal("expired quota cooldown should release; terminal error must not")
	}
	calls := 0
	probe := func(ctx context.Context, account *Account) error {
		calls++
		if calls == 1 {
			store.MarkCooldownWithError(account, 24*time.Hour, "unauthorized", "still unauthorized")
		} else {
			ReportCodexAuthorizationProbeSuccess(ctx, account)
		}
		return nil // The probe reports handled rejections as well as successes this way.
	}
	store.SetUsageProbeFunc(probe)
	store.parallelRecoveryProbe(ctx)
	if calls != 1 || a.IsAvailable() || a.RuntimeStatus() != "unauthorized" {
		t.Fatal("handled 401 incorrectly restored account")
	}
	store.Stop()
	row, err = db.GetAccountByID(ctx, unauthorized)
	if err != nil || row.CooldownReason != "unauthorized" || !row.CooldownUntil.Time.After(time.Now()) || row.ErrorMessage != "still unauthorized" {
		t.Fatal("rejected recovery probe must persist a new 401 deadline and error", err)
	}
	// Let the persisted cooldown expire between processes, not just in memory.
	if err := db.SetCooldown(ctx, unauthorized, "unauthorized", deadline); err != nil {
		t.Fatal(err)
	}
	store = startStore()
	a = store.FindByID(unauthorized)
	if a == nil || a.IsAvailable() || !a.NeedsRecoveryProbe(0) {
		t.Fatal("restart after another 401 must still require verification")
	}
	store.SetUsageProbeFunc(probe)
	store.parallelRecoveryProbe(ctx)
	if calls != 2 || !a.IsAvailable() || a.RuntimeStatus() != "active" {
		reason, until := a.GetCooldownSnapshot()
		t.Fatalf("verified recovery: calls=%d status=%s available=%v reason=%q until=%v", calls, a.RuntimeStatus(), a.IsAvailable(), reason, until)
	}
	row, err = db.GetAccountByID(ctx, unauthorized)
	if err != nil || row.CooldownReason != "" || row.CooldownUntil.Valid {
		t.Fatal("verified recovery did not clear persisted cooldown", err)
	}
}

func TestCodexAuthorizationRecoveryProbeEligibility(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name, at, rt, upstream, reason string
		expiry, deadline               time.Time
		terminal, want                 bool
	}{
		{name: "AT only", at: "at", expiry: now.Add(time.Hour), reason: "unauthorized", want: true},
		{name: "AT without known expiry", at: "at", reason: "unauthorized", want: true},
		{name: "expired AT only", at: "at", expiry: now.Add(-time.Hour), reason: "unauthorized"},
		{name: "no credentials", reason: "unauthorized"},
		{name: "active deadline", at: "at", reason: "unauthorized", deadline: now.Add(time.Hour)},
		{name: "terminal", at: "at", rt: "rt", reason: "unauthorized", terminal: true},
		{name: "other cooldown AT only", at: "at", reason: "rate_limited"},
		{name: "relay 401", at: "at", rt: "rt", upstream: UpstreamOpenAIResponses, reason: "unauthorized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acc := &Account{AccessToken: tc.at, RefreshToken: tc.rt, UpstreamType: tc.upstream, ExpiresAt: tc.expiry, HealthTier: HealthTierBanned}
			if tc.upstream == UpstreamOpenAIResponses {
				acc.BaseURL, acc.APIKey = "https://relay.example.invalid", "fixture-key"
				if !acc.IsRelayStyle() {
					t.Fatal("expected a configured relay account")
				}
			}
			acc.SetCooldownUntil(tc.deadline, tc.reason)
			if tc.terminal {
				acc.PermanentRefreshFailures = permanentRefreshFailureTerminalLimit
			}
			if got := acc.NeedsRecoveryProbe(0); got != tc.want {
				t.Fatalf("NeedsRecoveryProbe = %t, want %t", got, tc.want)
			}
		})
	}
}
