package auth

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestClaudeOAuthRefreshClearsUnauthorizedLatch(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &Account{
		DBID: 867, UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth,
		AccessToken: "expired-at", RefreshToken: "valid-rt", Status: StatusReady,
		ExpiresAt: time.Now().Add(time.Hour), Models: []string{"claude-sonnet-4-5"},
	}
	store.AddAccount(account)
	store.MarkCooldownWithError(account, time.Hour, "unauthorized", "upstream 401")
	atomic.StoreInt32(&account.Disabled, 1)
	if account.ModelCatalogEligible() || !account.IsBanned() || atomic.LoadInt32(&account.Disabled) == 0 {
		t.Fatal("unauthorized Claude account should be hidden from the model catalog")
	}
	if !store.shouldBackgroundRefresh(account, false) {
		t.Fatal("recoverable unauthorized Claude account was dropped from background refresh")
	}

	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		return &ClaudeTokenData{AccessToken: "fresh-at", RefreshToken: "valid-rt", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	if err := store.RefreshClaudeOAuthAfterUnauthorized(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&account.Disabled) != 0 || account.IsBanned() || account.HasActiveCooldown() || account.GetAccessToken() != "fresh-at" || !account.ModelCatalogEligible() {
		t.Fatalf("stale auth latch survived refresh: disabled=%d banned=%t cooldown=%t status=%s eligible=%t", atomic.LoadInt32(&account.Disabled), account.IsBanned(), account.HasActiveCooldown(), account.RuntimeStatus(), account.ModelCatalogEligible())
	}
	if store.shouldBackgroundRefresh(account, false) {
		t.Fatal("healthy token with a distant expiry was scheduled for another refresh")
	}
}

func TestClaudeOAuthRefreshPreservesRateLimitCooldown(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &Account{
		DBID: 868, UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth,
		AccessToken: "expired-at", RefreshToken: "valid-rt", Status: StatusReady,
		ExpiresAt: time.Now().Add(time.Minute),
	}
	store.AddAccount(account)
	store.MarkCooldownWithError(account, time.Hour, "rate_limited", "429")
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		return &ClaudeTokenData{AccessToken: "fresh-at", RefreshToken: "valid-rt", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	if err := store.RefreshClaudeOAuthAfterUnauthorized(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	reason, until := account.GetCooldownSnapshot()
	if reason != "rate_limited" || !until.After(time.Now()) || account.GetAccessToken() != "fresh-at" {
		t.Fatalf("rate-limit cooldown = (%q, %s), token=%s", reason, until, account.GetAccessToken())
	}
	if store.shouldBackgroundRefresh(account, false) {
		t.Fatal("rate-limit cooldown became a background refresh")
	}
}

func TestClaudeOAuthRefreshPermanentFailureStaysBounded(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &Account{
		DBID: 869, UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth,
		AccessToken: "expired-at", RefreshToken: "revoked-rt", Status: StatusReady,
		ExpiresAt: time.Now().Add(-time.Minute),
	}
	store.AddAccount(account)
	var calls atomic.Int32
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		calls.Add(1)
		return nil, newOAuthRefreshHTTPError(400, []byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`), "revoked-rt")
	})
	err := store.RefreshClaudeOAuthAfterUnauthorized(context.Background(), account)
	if err == nil || !IsPermanentRefreshFailure(err) || !strings.Contains(err.Error(), "invalid_grant") || strings.Contains(err.Error(), "revoked-rt") {
		t.Fatalf("permanent refresh error = %v", err)
	}
	if calls.Load() != 1 || !account.IsBanned() || account.RuntimeStatus() != "unauthorized" || account.ModelCatalogEligible() {
		t.Fatalf("calls=%d banned=%t status=%s eligible=%t", calls.Load(), account.IsBanned(), account.RuntimeStatus(), account.ModelCatalogEligible())
	}
	store.parallelRefreshAll(context.Background())
	if calls.Load() != 1 || store.shouldBackgroundRefresh(account, false) {
		t.Fatalf("revoked refresh token was retried, calls=%d", calls.Load())
	}
}

func TestClaudeOAuthRefreshTransportFailureDoesNotBan(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &Account{
		DBID: 870, UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth,
		AccessToken: "expired-at", RefreshToken: "valid-rt", Status: StatusReady,
		ExpiresAt: time.Now().Add(time.Hour), Models: []string{"claude-sonnet-4-5"},
	}
	store.AddAccount(account)
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		return nil, newOAuthRefreshHTTPError(503, []byte(`{"error":"temporarily_unavailable","error_description":"upstream timeout"}`), "valid-rt")
	})
	err := store.RefreshClaudeOAuthAfterUnauthorized(context.Background(), account)
	if err == nil || IsPermanentRefreshFailure(err) || strings.Contains(err.Error(), "valid-rt") {
		t.Fatalf("transport refresh error = %v", err)
	}
	if account.IsBanned() || atomic.LoadInt32(&account.Disabled) != 0 || account.HasActiveCooldown() || !account.ModelCatalogEligible() {
		t.Fatalf("transport failure latched auth state: banned=%t disabled=%d cooldown=%t eligible=%t", account.IsBanned(), atomic.LoadInt32(&account.Disabled), account.HasActiveCooldown(), account.ModelCatalogEligible())
	}
}

func TestClaudeOAuthRefreshClearsPersistedUnauthorizedCooldown(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-401.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithUpstream(ctx, "claude", "anthropic", "oauth", map[string]interface{}{
		"upstream_type": "claude", "claude_auth_kind": "oauth",
		"access_token": "old-at", "refresh_token": "old-rt",
		"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	account := store.FindByID(id)
	store.MarkCooldownWithError(account, time.Hour, "unauthorized", "upstream 401")
	atomic.StoreInt32(&account.Disabled, 1)
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		return &ClaudeTokenData{AccessToken: "fresh-at", RefreshToken: "old-rt", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	if err := store.RefreshClaudeOAuthAfterUnauthorized(ctx, account); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CooldownReason != "" || row.CooldownUntil.Valid || row.GetCredential("access_token") != "fresh-at" || !account.ModelCatalogEligible() {
		t.Fatalf("persisted state reason=%q until=%v token=%q eligible=%t", row.CooldownReason, row.CooldownUntil, row.GetCredential("access_token"), account.ModelCatalogEligible())
	}
}
