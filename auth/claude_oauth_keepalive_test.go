package auth

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func insertClaudeTestAccount(t *testing.T, db *database.DB, name string, creds map[string]any) int64 {
	t.Helper()
	id, err := db.InsertAccountWithUpstream(context.Background(), name, UpstreamClaude, "oauth", creds, "")
	if err != nil {
		t.Fatalf("InsertAccountWithUpstream(%s): %v", name, err)
	}
	return id
}

func TestClaudeOAuthKeepaliveLazyModeOptIn(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-keepalive.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	id := insertClaudeTestAccount(t, db, "claude-expiring", map[string]any{
		"upstream_type":    UpstreamClaude,
		"claude_auth_kind": ClaudeAuthKindOAuth,
		"access_token":     "old-at",
		"refresh_token":    "old-rt",
		"expires_at":       time.Now().Add(10 * time.Minute).Format(time.RFC3339),
	})

	store := NewStore(db, cache.NewMemory(16), &database.SystemSettings{
		MaxConcurrency: 2,
		LazyMode:       true,
	})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}

	called := make(chan struct{}, 4)
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		called <- struct{}{}
		return &ClaudeTokenData{
			AccessToken:  "fresh-at",
			RefreshToken: "rotated-rt",
			ExpiresAt:    time.Now().Add(8 * time.Hour),
		}, nil
	})
	store.SetBackgroundRefreshInterval(15 * time.Millisecond)
	store.StartBackgroundRefresh()

	select {
	case <-called:
		t.Fatal("lazy mode refreshed Claude OAuth before claude_oauth_keepalive_enabled opt-in")
	case <-time.After(80 * time.Millisecond):
	}

	store.SetClaudeOAuthKeepalive(true)
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("claude_oauth_keepalive_enabled did not trigger background refresh in lazy mode")
	}

	// Give persistence and memory publication a moment to settle.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		row, err := db.GetAccountByID(ctx, id)
		if err == nil && row.GetCredential("access_token") == "fresh-at" {
			if row.GetCredential("refresh_token") != "rotated-rt" {
				t.Fatalf("persisted refresh_token = %q, want rotated-rt", row.GetCredential("refresh_token"))
			}
			if row.GetCredential(ClaudeLastRefreshAtCredentialKey) == "" {
				t.Fatal("claude_last_refresh_at was not persisted")
			}
			if row.GetCredential(ClaudeRefreshErrorCredentialKey) != "" || row.GetCredential(ClaudeRefreshNextRetryAtCredentialKey) != "" {
				t.Fatalf("stale error/retry survived success: err=%q retry=%q", row.GetCredential(ClaudeRefreshErrorCredentialKey), row.GetCredential(ClaudeRefreshNextRetryAtCredentialKey))
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("refreshed Claude credentials were not persisted")
}

func TestClaudeOAuthKeepaliveEligibilityMatrix(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:              2,
		LazyMode:                    true,
		ClaudeOAuthKeepaliveEnabled: true,
		CodexOAuthKeepaliveEnabled:  false,
	})
	t.Cleanup(store.Stop)

	expiring := time.Now().Add(15 * time.Minute)
	distant := time.Now().Add(2 * time.Hour)

	cases := []struct {
		name string
		acc  *Account
		want bool
	}{
		{
			name: "claude oauth within 30m lead",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusReady, HealthTier: HealthTierHealthy},
			want: true,
		},
		{
			name: "claude oauth distant expiry skipped",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: distant, Status: StatusReady, HealthTier: HealthTierHealthy},
			want: false,
		},
		{
			name: "claude oauth in 5h quota cooldown refreshed",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusCooldown, CooldownReason: "rate_limited_5h", CooldownUtil: time.Now().Add(time.Hour), HealthTier: HealthTierRisky},
			want: true,
		},
		{
			name: "claude oauth in 7d quota cooldown refreshed",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusCooldown, CooldownReason: "rate_limited_7d", CooldownUtil: time.Now().Add(24 * time.Hour), HealthTier: HealthTierHealthy},
			want: true,
		},
		{
			name: "claude api key skipped",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindAPIKey, AccessToken: "sk-ant-api03-x", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusReady, HealthTier: HealthTierHealthy},
			want: false,
		},
		{
			name: "claude setup token skipped",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindSetupToken, AccessToken: "setup-at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusReady, HealthTier: HealthTierHealthy},
			want: false,
		},
		{
			name: "claude oauth manually disabled skipped",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusReady, DispatchPaused: 1, HealthTier: HealthTierHealthy},
			want: false,
		},
		{
			name: "claude oauth manually disabled even if 401 latched skipped",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusCooldown, CooldownReason: "unauthorized", Disabled: 1, DispatchPaused: 1, HealthTier: HealthTierBanned},
			want: false,
		},
		{
			name: "claude oauth permanent failure skipped",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusCooldown, CooldownReason: "unauthorized", PermanentRefreshFailures: 1, HealthTier: HealthTierBanned},
			want: false,
		},
		{
			name: "claude oauth terminal error skipped",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusError, HealthTier: HealthTierRisky},
			want: false,
		},
		{
			name: "claude oauth in transient retry backoff skipped",
			acc:  &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth, AccessToken: "at", RefreshToken: "rt", ExpiresAt: expiring, Status: StatusReady, HealthTier: HealthTierHealthy, claudeRefreshNextRetryAt: time.Now().Add(5 * time.Minute)},
			want: false,
		},
		{
			name: "codex skipped when only claude keepalive enabled",
			acc:  &Account{UpstreamType: "codex", AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Minute), Status: StatusReady, HealthTier: HealthTierHealthy},
			want: false,
		},
		{
			name: "grok skipped in lazy mode",
			acc:  &Account{UpstreamType: UpstreamGrok, AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Minute), Status: StatusReady, HealthTier: HealthTierHealthy},
			want: false,
		},
		{
			name: "antigravity skipped in lazy mode",
			acc:  &Account{UpstreamType: UpstreamAntigravity, AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Minute), Status: StatusReady, HealthTier: HealthTierHealthy},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := store.shouldBackgroundRefreshForPolicy(tc.acc, true, store.GetCodexOAuthKeepalive(), store.GetClaudeOAuthKeepalive())
			if got != tc.want {
				t.Fatalf("shouldBackgroundRefreshForPolicy = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestClaudeOAuthKeepaliveRespectsQuietHours(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:              2,
		LazyMode:                    true,
		ClaudeOAuthKeepaliveEnabled: true,
		BackgroundQuietHours:        `[{"start":"00:00","end":"24:00"}]`,
	})
	t.Cleanup(store.Stop)

	acc := &Account{
		DBID: 93401, UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth,
		AccessToken: "old-at", RefreshToken: "valid-rt", Status: StatusReady,
		ExpiresAt: time.Now().Add(10 * time.Minute), HealthTier: HealthTierHealthy,
	}
	store.AddAccount(acc)

	var calls atomic.Int32
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		calls.Add(1)
		return &ClaudeTokenData{AccessToken: "fresh-at", RefreshToken: "valid-rt", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
	})

	store.parallelRefreshAll(context.Background())
	if calls.Load() != 0 {
		t.Fatalf("quiet hours did not pause Claude keepalive, calls=%d", calls.Load())
	}

	if err := store.SetBackgroundQuietHours("[]"); err != nil {
		t.Fatal(err)
	}
	store.parallelRefreshAll(context.Background())
	if calls.Load() != 1 || acc.GetAccessToken() != "fresh-at" {
		t.Fatalf("after clearing quiet hours: calls=%d token=%q", calls.Load(), acc.GetAccessToken())
	}
}

func TestClaudeOAuthKeepalivePreservesQuotaCooldown(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-cooldown.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	id := insertClaudeTestAccount(t, db, "claude-cooling", map[string]any{
		"upstream_type":    UpstreamClaude,
		"claude_auth_kind": ClaudeAuthKindOAuth,
		"access_token":     "old-at",
		"refresh_token":    "old-rt",
		"expires_at":       time.Now().Add(10 * time.Minute).Format(time.RFC3339),
	})

	store := NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:              2,
		LazyMode:                    true,
		ClaudeOAuthKeepaliveEnabled: true,
	})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	acc := store.FindByID(id)
	until := time.Now().Add(4 * time.Hour).Truncate(time.Second)
	acc.SetCooldownUntil(until, "rate_limited_5h")
	if err := db.SetCooldown(ctx, id, "rate_limited_5h", until); err != nil {
		t.Fatal(err)
	}

	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		return &ClaudeTokenData{AccessToken: "fresh-at", RefreshToken: "new-rt", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
	})

	store.parallelRefreshAll(ctx)
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("access_token") != "fresh-at" || row.GetCredential("refresh_token") != "new-rt" {
		t.Fatalf("cooling account was not refreshed: at=%q rt=%q", row.GetCredential("access_token"), row.GetCredential("refresh_token"))
	}
	reason, gotUntil := acc.GetCooldownSnapshot()
	if reason != "rate_limited_5h" || !gotUntil.Equal(until) || !row.CooldownUntil.Valid || row.CooldownReason != "rate_limited_5h" {
		t.Fatalf("quota cooldown cleared during keepalive: mem=(%q,%v) db=(%q,%v)", reason, gotUntil, row.CooldownReason, row.CooldownUntil)
	}
}

func TestClaudeOAuthRefreshTransientBackoffAndRetryAfter(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-backoff.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	secretRT := "secret-claude-rt-934"
	id := insertClaudeTestAccount(t, db, "claude-transient", map[string]any{
		"upstream_type":    UpstreamClaude,
		"claude_auth_kind": ClaudeAuthKindOAuth,
		"access_token":     "old-at",
		"refresh_token":    secretRT,
		"expires_at":       time.Now().Add(-time.Minute).Format(time.RFC3339),
	})

	store := NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:              2,
		LazyMode:                    true,
		ClaudeOAuthKeepaliveEnabled: true,
	})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	acc := store.FindByID(id)
	acc.Models = []string{"claude-sonnet-4-6"}

	var calls atomic.Int32
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		n := calls.Add(1)
		if n == 1 {
			headers := http.Header{"Retry-After": {"600"}}
			return nil, newOAuthRefreshHTTPErrorWithRetryAfter(429, []byte(`{"error":"rate_limited","token":"`+secretRT+`"}`), secretRT, parseClaudeRefreshRetryAfter(headers))
		}
		return &ClaudeTokenData{AccessToken: "recovered-at", RefreshToken: "recovered-rt", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
	})

	store.parallelRefreshAll(ctx)
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
	if acc.IsBanned() || atomic.LoadInt32(&acc.Disabled) != 0 || acc.HasActiveCooldown() || !acc.ModelCatalogEligible() {
		t.Fatalf("transient 429 latched auth failure: banned=%t disabled=%d cooldown=%t eligible=%t", acc.IsBanned(), atomic.LoadInt32(&acc.Disabled), acc.HasActiveCooldown(), acc.ModelCatalogEligible())
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	persistedErr := row.GetCredential(ClaudeRefreshErrorCredentialKey)
	persistedRetry := row.GetCredential(ClaudeRefreshNextRetryAtCredentialKey)
	if persistedErr == "" || strings.Contains(persistedErr, secretRT) {
		t.Fatalf("unexpected persisted error = %q", persistedErr)
	}
	retryAt := parseOAuthCredentialExpiry(persistedRetry)
	if retryAt.IsZero() || time.Until(retryAt) < 9*time.Minute {
		t.Fatalf("Retry-After 600s not respected: next_retry_at=%q", persistedRetry)
	}

	// Second background pass during backoff must skip.
	store.parallelRefreshAll(ctx)
	if calls.Load() != 1 {
		t.Fatalf("background refresh ran during backoff, calls=%d", calls.Load())
	}

	// On-demand 401 recovery bypasses background backoff and clears diagnostics.
	if err := store.RefreshClaudeOAuthAfterUnauthorized(ctx, acc); err != nil {
		t.Fatalf("on-demand refresh failed: %v", err)
	}
	if calls.Load() != 2 || acc.GetAccessToken() != "recovered-at" {
		t.Fatalf("calls=%d at=%q", calls.Load(), acc.GetAccessToken())
	}
	row, _ = db.GetAccountByID(ctx, id)
	if row.GetCredential(ClaudeRefreshErrorCredentialKey) != "" || row.GetCredential(ClaudeRefreshNextRetryAtCredentialKey) != "" || row.GetCredential(ClaudeLastRefreshAtCredentialKey) == "" {
		t.Fatalf("diagnostics not reset on recovery: err=%q retry=%q last=%q",
			row.GetCredential(ClaudeRefreshErrorCredentialKey),
			row.GetCredential(ClaudeRefreshNextRetryAtCredentialKey),
			row.GetCredential(ClaudeLastRefreshAtCredentialKey))
	}
}

func TestClaudeOAuthRefreshPermanentFailureRaceRecovery(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	id := insertClaudeTestAccount(t, db, "claude-race", map[string]any{
		"upstream_type":    UpstreamClaude,
		"claude_auth_kind": ClaudeAuthKindOAuth,
		"access_token":     "old-at",
		"refresh_token":    "old-rt",
		"expires_at":       time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	})

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	acc := store.FindByID(id)

	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		// Simulate another instance rotating credentials in DB while this call was in flight.
		_ = db.UpdateCredentials(ctx, id, map[string]any{
			"access_token":  "peer-fresh-at",
			"refresh_token": "peer-fresh-rt",
			"expires_at":    time.Now().Add(8 * time.Hour).Format(time.RFC3339),
		})
		return nil, newOAuthRefreshHTTPError(400, []byte(`{"error":"invalid_grant"}`), "old-rt")
	})

	if err := store.RefreshSingle(ctx, id); err != nil {
		t.Fatalf("expected concurrent race recovery, got err=%v", err)
	}
	if acc.GetAccessToken() != "peer-fresh-at" || acc.IsBanned() || acc.Status == StatusError {
		t.Fatalf("race recovery failed: at=%q banned=%t status=%v", acc.GetAccessToken(), acc.IsBanned(), acc.Status)
	}
}

func TestClaudeOAuthRefreshConcurrentBackgroundAndRequestShareLease(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	id := insertClaudeTestAccount(t, db, "claude-concurrent", map[string]any{
		"upstream_type":    UpstreamClaude,
		"claude_auth_kind": ClaudeAuthKindOAuth,
		"access_token":     "old-at",
		"refresh_token":    "shared-rt",
		"expires_at":       time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	})

	store := NewStore(db, cache.NewMemory(16), &database.SystemSettings{
		MaxConcurrency:              2,
		LazyMode:                    true,
		ClaudeOAuthKeepaliveEnabled: true,
	})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}
	acc := store.FindByID(id)

	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
		}
		return &ClaudeTokenData{AccessToken: "single-at", RefreshToken: "single-rt", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
	})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		store.parallelRefreshAll(ctx)
	}()

	<-started
	reqErrCh := make(chan error, 1)
	go func() {
		reqErrCh <- store.RefreshClaudeOAuthAfterUnauthorized(ctx, acc)
	}()
	time.Sleep(30 * time.Millisecond)
	close(release)

	wg.Wait()
	if err := <-reqErrCh; err != nil {
		t.Fatalf("concurrent request refresh failed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream refresh called %d times, want 1", calls.Load())
	}
	if acc.GetAccessToken() != "single-at" {
		t.Fatalf("access_token = %q, want single-at", acc.GetAccessToken())
	}
}

func TestClaudeOAuthRefreshPersistsDespiteCallerCancellation(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "claude-cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	id := insertClaudeTestAccount(t, db, "claude-cancel", map[string]any{
		"upstream_type":    UpstreamClaude,
		"claude_auth_kind": ClaudeAuthKindOAuth,
		"access_token":     "old-at",
		"refresh_token":    "old-rt",
		"expires_at":       time.Now().Add(5 * time.Minute).Format(time.RFC3339),
	})

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatal(err)
	}

	reqCtx, cancel := context.WithCancel(ctx)
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		cancel()
		return &ClaudeTokenData{AccessToken: "persisted-at", RefreshToken: "persisted-rt", ExpiresAt: time.Now().Add(8 * time.Hour)}, nil
	})

	if err := store.RefreshSingle(reqCtx, id); err != nil {
		t.Fatalf("RefreshSingle failed after mid-exchange cancel: %v", err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("access_token") != "persisted-at" || row.GetCredential("refresh_token") != "persisted-rt" {
		t.Fatalf("rotated credentials lost on cancel: at=%q rt=%q", row.GetCredential("access_token"), row.GetCredential("refresh_token"))
	}
}

func TestClaudeOAuthRefreshIneffectiveBackoffAndGroupCatalogRecovery(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:              2,
		LazyMode:                    true,
		ClaudeOAuthKeepaliveEnabled: true,
	})
	t.Cleanup(store.Stop)

	acc := &Account{
		DBID: 93409, UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindOAuth,
		AccessToken: "old-at", RefreshToken: "valid-rt", Status: StatusReady,
		ExpiresAt: time.Now().Add(5 * time.Minute),
		Models:    []string{"claude-sonnet-4-6"},
		GroupIDs:  []int64{42},
	}
	store.AddAccount(acc)
	store.MarkCooldownWithError(acc, time.Hour, "unauthorized", "401")
	atomic.StoreInt32(&acc.Disabled, 1)

	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*ClaudeTokenData, error) {
		// Return a short-lived AT inside the 30m lead window to test ineffective-refresh throttle.
		return &ClaudeTokenData{AccessToken: "short-at", RefreshToken: "valid-rt", ExpiresAt: time.Now().Add(10 * time.Minute)}, nil
	})

	store.parallelRefreshAll(context.Background())
	if !acc.ModelCatalogEligible() || acc.Status != StatusReady || len(acc.GroupIDs) != 1 || acc.GroupIDs[0] != 42 {
		t.Fatalf("recovery state invalid: eligible=%t status=%v groups=%v", acc.ModelCatalogEligible(), acc.Status, acc.GroupIDs)
	}
	if store.shouldBackgroundRefreshForPolicy(acc, true, false, true) {
		t.Fatal("ineffective-refresh backoff did not throttle immediate re-refresh")
	}
}
