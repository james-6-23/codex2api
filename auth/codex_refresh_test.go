package auth

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func codexRefreshFixture(t *testing.T, handler http.HandlerFunc) (*Store, *database.DB, int64, string) {
	t.Helper()
	provider := httptest.NewServer(handler)
	t.Cleanup(provider.Close)
	oldDecorator := ResinRequestDecorator
	ResinRequestDecorator = func(string, string) string { return provider.URL }
	t.Cleanup(func() { ResinRequestDecorator = oldDecorator })
	path := filepath.Join(t.TempDir(), "codex-refresh.db")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAccountWithCredentials(context.Background(), "refresh-test", map[string]any{
		"refresh_token": "old-rt", "access_token": "old-at", "plan_type": "plus",
		"expires_at": time.Now().Add(time.Minute).Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, cache.NewMemory(16), &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return store, db, id, path
}

func writeCodexRefreshedTokens(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600}`))
}

func TestCodexRefreshBackgroundPreservesQuotaCooldown(t *testing.T) {
	var calls atomic.Int32
	previous := CodexOAuthIdentityHeaders
	CodexOAuthIdentityHeaders = func(*Account) (http.Header, error) {
		return http.Header{"User-Agent": {"codex2api/test"}, "Originator": {"codex2api"}, "X-Oai-Attestation": {"excluded"}}, nil
	}
	t.Cleanup(func() { CodexOAuthIdentityHeaders = previous })
	store, db, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if "codex2api/test" != req.Header.Get("User-Agent") || "codex2api" != req.Header.Get("Originator") || "" != req.Header.Get("X-Oai-Attestation") {
			t.Error("OAuth refresh did not apply only the selected client identity")
		}
		writeCodexRefreshedTokens(w)
	})
	account := store.FindByID(id)
	until := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	account.SetCooldownUntil(until, "rate_limited_7d")
	if err := db.SetCooldown(context.Background(), id, "rate_limited_7d", until); err != nil {
		t.Fatal(err)
	}
	store.parallelRefreshAll(context.Background())
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || row.GetCredential("refresh_token") != "new-rt" {
		t.Fatal("cooling account was not refreshed")
	}
	if !row.CooldownUntil.Valid || !row.CooldownUntil.Time.Equal(until) || row.CooldownReason != "rate_limited_7d" || !account.HasActiveCooldown() {
		t.Fatal("token refresh cleared quota cooldown")
	}
	if row.GetCredential("codex_last_refresh_at") == "" {
		t.Fatal("last refresh was not recorded")
	}
}

func TestCodexRefreshPreservesCooldownSetDuringExchange(t *testing.T) {
	var store *Store
	var db *database.DB
	var id int64
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	store, db, id, _ = codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		store.FindByID(id).SetCooldownUntil(until, "rate_limited_5h")
		if err := db.SetCooldown(context.Background(), id, "rate_limited_5h", until); err != nil {
			t.Error(err)
		}
		writeCodexRefreshedTokens(w)
	})
	if err := store.RefreshSingle(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !store.FindByID(id).HasActiveCooldown() || row.CooldownReason != "rate_limited_5h" {
		t.Fatal("concurrent cooldown was lost")
	}
}

func TestCodexRefreshSurvivesCallerCancellation(t *testing.T) {
	consumed, resume := make(chan struct{}), make(chan struct{})
	store, db, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		close(consumed)
		<-resume
		writeCodexRefreshedTokens(w)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- store.RefreshSingle(ctx, id) }()
	select {
	case <-consumed:
	case <-time.After(5 * time.Second):
		close(resume)
		t.Fatal("OAuth was not reached")
	}
	cancel()
	close(resume)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish")
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("refresh_token") != "new-rt" {
		t.Fatal("caller cancellation lost rotated RT")
	}
}

func TestCodexRefreshDatabaseFailureFencesRestartAndDoesNotPublish(t *testing.T) {
	var calls atomic.Int32
	store, db, id, path := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); writeCodexRefreshedTokens(w) })
	injector, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err := injector.Exec(`CREATE TRIGGER fail_codex_save BEFORE UPDATE OF credentials ON accounts BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshSingle(context.Background(), id); err == nil {
		t.Fatal("persistence failure reported success")
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	account := store.FindByID(id)
	if row.GetCredential("refresh_token") != "old-rt" || account.GetAccessToken() != "old-at" {
		t.Fatal("uncommitted credential was published")
	}
	if _, err := injector.Exec(`DROP TRIGGER fail_codex_save`); err != nil {
		t.Fatal(err)
	}
	restarted := NewStore(db, cache.NewMemory(16), &database.SystemSettings{MaxConcurrency: 2})
	defer restarted.Stop()
	if err := restarted.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := restarted.RefreshSingle(context.Background(), id); err == nil || !strings.Contains(err.Error(), "未确认") {
		t.Fatalf("lost durable fence: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("consumed old RT %d times", calls.Load())
	}
}

func TestCodexRefreshRetriesPersistenceWithoutRepeatingOAuth(t *testing.T) {
	var calls atomic.Int32
	store, db, id, path := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); writeCodexRefreshedTokens(w) })
	// The trigger is dropped while the store may hold the WAL write lock. A
	// raw connection has no busy timeout and fails with SQLITE_BUSY instead
	// of waiting, which leaves the trigger in place past the retry budget.
	injector, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer injector.Close()
	if _, err := injector.Exec(`CREATE TRIGGER transient_codex_save BEFORE UPDATE OF credentials ON accounts BEGIN SELECT RAISE(ABORT, 'transient write failure'); END`); err != nil {
		t.Fatal(err)
	}
	restored := make(chan error, 1)
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, err := injector.Exec(`DROP TRIGGER transient_codex_save`)
		restored <- err
	}()
	if err := store.RefreshSingle(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := <-restored; err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || row.GetCredential("refresh_token") != "new-rt" {
		t.Fatal("retry repeated OAuth or lost new RT")
	}
}

func TestCodexRefreshUpdatesDisabledSiblingRoute(t *testing.T) {
	store, db, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) { writeCodexRefreshedTokens(w) })
	peer, err := db.InsertAccountWithCredentials(context.Background(), "workspace", map[string]any{
		"refresh_token": "old-rt", "access_token": "old-at", "custom_headers": map[string]string{"Chatgpt-Account-Id": "other-workspace"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately never load the sibling into Store.
	if err := db.SetAccountEnabled(context.Background(), peer, false); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshSingle(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), peer)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("refresh_token") != "new-rt" || row.GetCredentialStringMap("custom_headers")["Chatgpt-Account-Id"] != "other-workspace" {
		t.Fatal("sibling credential or route lost")
	}
}

func TestCodexRefreshDoesNotOverwriteAdministrativeReplacement(t *testing.T) {
	var db *database.DB
	var id int64
	store, databaseHandle, accountID, _ := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		if err := db.UpdateCredentials(context.Background(), id, map[string]any{"refresh_token": "replacement-rt", "access_token": "replacement-at", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)}); err != nil {
			t.Error(err)
		}
		writeCodexRefreshedTokens(w)
	})
	db, id = databaseHandle, accountID
	if err := store.RefreshSingle(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("refresh_token") != "replacement-rt" || store.FindByID(id).GetAccessToken() != "replacement-at" {
		t.Fatal("stale OAuth result overwrote replacement")
	}
}

func TestCodexRefreshAmbiguousResponseIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	store, _, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"refresh_token":"new-rt"}`))
	})
	for range 2 {
		if err := store.RefreshSingle(context.Background(), id); err == nil {
			t.Fatal("uncertain response reported success")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("ambiguous exchange repeated %d times", calls.Load())
	}
}

func TestCodexRefreshExpiredTokenIsPermanent(t *testing.T) {
	var calls atomic.Int32
	store, _, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_expired"}}`))
	})
	err := store.RefreshSingle(context.Background(), id)
	if err == nil || !strings.Contains(err.Error(), "重新登录") || calls.Load() != 1 || store.FindByID(id).RuntimeStatus() != "unauthorized" {
		t.Fatalf("expired RT misclassified: %v, calls %d", err, calls.Load())
	}
}

func TestCodexRefreshBackgroundEligibility(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{})
	defer store.Stop()
	claude := &Account{RefreshToken: "rt", ExpiresAt: time.Now(), UpstreamType: UpstreamClaude, Disabled: 1, HealthTier: HealthTierHealthy}
	if !store.shouldBackgroundRefresh(claude, false) {
		t.Fatal("Codex change altered legacy Claude refresh policy")
	}
	for _, tc := range []struct {
		name             string
		disabled, paused int32
		want             bool
	}{
		{"grok enabled", 0, 0, true}, {"grok disabled", 1, 0, false}, {"grok paused", 0, 1, false},
	} {
		grok := &Account{RefreshToken: "rt", ExpiresAt: time.Now(), UpstreamType: UpstreamGrok, HealthTier: HealthTierHealthy, Disabled: tc.disabled, DispatchPaused: tc.paused}
		if got := store.shouldBackgroundRefresh(grok, false); got != tc.want {
			t.Fatalf("%s: eligible=%t, want %t", tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		name, upstream, reason string
		disabled, paused       int32
		want                   bool
	}{
		{"ready", "", "", 0, 0, true}, {"quota paused", "", "rate_limited_7d", 0, 1, true},
		{"disabled", "", "rate_limited_7d", 1, 0, false}, {"manual pause", "", "", 0, 1, false},
		{"unauthorized", "", "unauthorized", 0, 0, false}, {"claude", "claude", "", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{RefreshToken: "rt", ExpiresAt: time.Now(), HealthTier: HealthTierHealthy, UpstreamType: tc.upstream, Disabled: tc.disabled, DispatchPaused: tc.paused}
			if tc.reason != "" {
				account.SetCooldownWithReason(time.Hour, tc.reason)
			}
			if got := store.shouldBackgroundRefresh(account, true); got != tc.want {
				t.Fatalf("eligible=%t, want %t", got, tc.want)
			}
		})
	}
}

func TestCodexRefreshConcurrentStoresShareForcedExchange(t *testing.T) {
	var calls atomic.Int32
	consumed, resume := make(chan struct{}), make(chan struct{}, 1)
	defer func() {
		select {
		case resume <- struct{}{}:
		default:
		}
	}()
	first, db, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			close(consumed)
			<-resume
		}
		writeCodexRefreshedTokens(w)
	})
	secondCache := first.tokenCache
	if addr := os.Getenv("CODEX2API_TEST_REDIS_ADDR"); addr != "" {
		redisA, err := cache.NewRedis(addr, "", 15)
		if err != nil {
			t.Fatal(err)
		}
		defer redisA.Close()
		redisB, err := cache.NewRedis(addr, "", 15)
		if err != nil {
			t.Fatal(err)
		}
		defer redisB.Close()
		first.tokenCache, secondCache = redisA, redisB
		t.Log("verifying independent Redis clients")
	}
	second := NewStore(db, secondCache, &database.SystemSettings{MaxConcurrency: 2})
	defer second.Stop()
	if err := second.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	go func() { results <- first.RefreshSingle(context.Background(), id) }()
	select {
	case <-consumed:
	case <-time.After(5 * time.Second):
		t.Fatal("first exchange did not start")
	}
	go func() { results <- second.RefreshSingle(context.Background(), id) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		second.oauthRefreshLocksMu.Lock()
		waiting := len(second.oauthRefreshLocks) > 0
		second.oauthRefreshLocksMu.Unlock()
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second store did not wait for lease")
		}
		time.Sleep(time.Millisecond)
	}
	resume <- struct{}{}
	for range 2 {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("refresh did not finish")
		}
	}
	if calls.Load() != 1 || second.FindByID(id).GetAccessToken() != "new-at" {
		t.Fatal("forced refresh did not share rotated credentials")
	}
}

func TestCodexRefreshReloadsUpdatedSessionFallback(t *testing.T) {
	store, db, id, _ := codexRefreshFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"refresh_token_expired"}}`))
			return
		}
		cookie, err := r.Cookie("__Secure-next-auth.session-token")
		if err != nil || cookie.Value != "updated-session" {
			t.Error("fallback used stale session token")
		}
		_, _ = w.Write([]byte(`{"accessToken":"session-at"}`))
	})
	account := store.FindByID(id)
	account.mu.Lock()
	account.SessionToken = "stale-session"
	account.mu.Unlock()
	if err := db.UpdateCredentials(context.Background(), id, map[string]any{"session_token": "updated-session"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RefreshSingle(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("session_token") != "updated-session" || row.GetCredential("codex_refresh_error") == "" {
		t.Fatal("fallback lost new credentials or hid failed RT")
	}
}

func TestCodexRefreshLazyKeepaliveIsOptIn(t *testing.T) {
	called := make(chan struct{}, 2)
	store, _, _, _ := codexRefreshFixture(t, func(w http.ResponseWriter, _ *http.Request) { called <- struct{}{}; writeCodexRefreshedTokens(w) })
	store.SetLazyMode(true)
	store.SetBackgroundRefreshInterval(10 * time.Millisecond)
	store.StartBackgroundRefresh()
	select {
	case <-called:
		t.Fatal("lazy mode refreshed without opt-in")
	case <-time.After(80 * time.Millisecond):
	}
	store.SetCodexOAuthKeepalive(true)
	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Fatal("opt-in did not start background refresh")
	}
	store.Stop()
}
