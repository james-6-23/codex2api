package auth

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func useAntigravityTestEndpoints(t *testing.T, baseURL string) {
	t.Helper()
	previous := DefaultAntigravityEndpoints
	DefaultAntigravityEndpoints = AntigravityEndpoints{
		TokenURL: baseURL + "/token", UserInfoURL: baseURL + "/userinfo",
		LoadProject: []string{baseURL + "/load"}, Quota: []string{baseURL + "/quota"},
		QuotaSummary: []string{baseURL + "/summary"}, AICredits: []string{baseURL + "/credits"},
	}
	t.Cleanup(func() { DefaultAntigravityEndpoints = previous })
}

func newAntigravityRefreshTestAccount(t *testing.T, credentials map[string]any) (*Store, *database.DB, *Account, int64) {
	t.Helper()
	return newAntigravityRefreshTestAccountAtPath(t, filepath.Join(t.TempDir(), "antigravity-refresh.db"), credentials)
}

func newAntigravityRefreshTestAccountAtPath(t *testing.T, dbPath string, credentials map[string]any) (*Store, *database.DB, *Account, int64) {
	t.Helper()
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if credentials == nil {
		credentials = make(map[string]any)
	}
	defaults := map[string]any{
		"upstream_type": UpstreamAntigravity,
		"access_token":  "access-old", "refresh_token": "refresh-old",
		"oauth_client_key": "test", "antigravity_client_id": "test-client", "antigravity_client_secret": "test-secret",
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"project_id": "project-old", "account_id": "subject-1",
		"email": "user@example.com", "verified_email": true,
		"models": []string{"gemini-old"}, "credential_family_id": "ag-refresh-family",
	}
	for key, value := range defaults {
		if _, ok := credentials[key]; !ok {
			credentials[key] = value
		}
	}
	accountID, err := db.InsertAccountWithCredentials(context.Background(), "antigravity", credentials, "")
	if err != nil {
		t.Fatalf("insert Antigravity account: %v", err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(context.Background(), accountID); err != nil {
		t.Fatalf("load Antigravity account: %v", err)
	}
	account := store.FindByID(accountID)
	if account == nil {
		t.Fatal("loaded Antigravity account is missing from runtime")
	}
	return store, db, account, accountID
}

func TestRefreshAccountRoutesExpiredAntigravityOAuthToDedicatedRefresh(t *testing.T) {
	var tokenCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAntigravityRefreshFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			_, _ = io.WriteString(w, `{"access_token":"lazy-access-new","refresh_token":"lazy-refresh-new","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, _, account, _ := newAntigravityRefreshTestAccount(t, map[string]any{
		"expires_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339),
	})

	if err := store.refreshAccount(context.Background(), account); err != nil {
		t.Fatalf("refreshAccount: %v", err)
	}
	account.mu.RLock()
	accessToken := account.AccessToken
	account.mu.RUnlock()
	if tokenCalls.Load() == 0 || accessToken != "lazy-access-new" {
		t.Fatalf("Antigravity lazy refresh was not routed: calls=%d access=%q", tokenCalls.Load(), accessToken)
	}
}

func writeAntigravityRefreshFixture(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/load":
		_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-new","paidTier":{"name":"Pro"}}`)
	case "/quota":
		_, _ = io.WriteString(w, `{"models":{"gemini-test":{"quotaInfo":{"remainingFraction":0.9}}}}`)
	case "/summary":
		_, _ = io.WriteString(w, `{"groups":[]}`)
	case "/credits":
		_, _ = io.WriteString(w, `{"paidTier":{"availableCredits":[]}}`)
	default:
		return false
	}
	return true
}

func TestRefreshAntigravityAccountUsesPersistedOAuthMetadataAndPublishesDBFirst(t *testing.T) {
	userinfoEntered := make(chan struct{})
	releaseUserinfo := make(chan struct{})
	var once sync.Once
	var gotClientID, gotClientSecret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAntigravityRefreshFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			gotClientID = r.Form.Get("client_id")
			gotClientSecret = r.Form.Get("client_secret")
			_, _ = io.WriteString(w, `{"access_token":"access-new","refresh_token":"refresh-new","id_token":"id-new","expires_in":3600,"scope":"scope-new"}`)
		case "/userinfo":
			once.Do(func() { close(userinfoEntered) })
			<-releaseUserinfo
			_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true,"name":"User"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	deniedAt := time.Now().UTC().Format(time.RFC3339)
	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{
		"oauth_client_key": "custom", "antigravity_client_id": "custom-client",
		"antigravity_client_secret": "custom-secret", "oauth_scope": "scope-old",
		"antigravity_permissions": fmt.Sprintf(`{"allowed":false,"reason":"old denial","updated_at":%q}`, deniedAt),
	})
	if account.IsAvailable() {
		t.Fatal("persisted Allowed=false was not restored as a runtime hard fence")
	}

	result := make(chan error, 1)
	go func() { result <- store.RefreshAntigravityAccount(context.Background(), account) }()
	select {
	case <-userinfoEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not reach userinfo after token rotation")
	}
	rowDuringSync, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	account.mu.RLock()
	runtimeAccessDuringSync := account.AccessToken
	runtimeGenerationDuringSync := account.CredentialGeneration
	account.mu.RUnlock()
	if rowDuringSync.GetCredential("access_token") != "access-old" || rowDuringSync.CredentialGeneration != 1 || runtimeAccessDuringSync != "access-old" || runtimeGenerationDuringSync != 1 {
		t.Fatalf("credential published before durable sync: row=%q/gen%d runtime=%q/gen%d", rowDuringSync.GetCredential("access_token"), rowDuringSync.CredentialGeneration, runtimeAccessDuringSync, runtimeGenerationDuringSync)
	}
	close(releaseUserinfo)
	if err := <-result; err != nil {
		t.Fatalf("RefreshAntigravityAccount: %v", err)
	}
	if gotClientID != "custom-client" || gotClientSecret != "custom-secret" {
		t.Fatalf("token client metadata = %q/%q, want persisted custom credentials", gotClientID, gotClientSecret)
	}
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 2 || row.GetCredential("access_token") != "access-new" || row.GetCredential("refresh_token") != "refresh-new" || row.GetCredential("oauth_scope") != "scope-new" {
		t.Fatalf("persisted refreshed credential = gen%d %#v", row.CredentialGeneration, row.Credentials)
	}
	account.mu.RLock()
	gotRuntimeAccess := account.AccessToken
	gotRuntimeGeneration := account.CredentialGeneration
	hardBlocked := account.AntigravityHardBlocked
	account.mu.RUnlock()
	if gotRuntimeAccess != "access-new" || gotRuntimeGeneration != row.CredentialGeneration || hardBlocked || !account.IsAvailable() {
		t.Fatalf("runtime after DB publish = access %q gen%d hard=%t available=%t", gotRuntimeAccess, gotRuntimeGeneration, hardBlocked, account.IsAvailable())
	}
}

func TestRefreshAntigravityAccountSingleflightsByCredentialFamily(t *testing.T) {
	tokenEntered := make(chan struct{})
	releaseToken := make(chan struct{})
	var enteredOnce sync.Once
	var tokenCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAntigravityRefreshFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenCalls.Add(1)
			enteredOnce.Do(func() { close(tokenEntered) })
			<-releaseToken
			_, _ = io.WriteString(w, `{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, _, account, _ := newAntigravityRefreshTestAccount(t, map[string]any{
		"antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})

	errs := make(chan error, 2)
	go func() { errs <- store.RefreshAntigravityAccount(context.Background(), account) }()
	select {
	case <-tokenEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first refresh did not reach token endpoint")
	}
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		errs <- store.RefreshAntigravityAccount(context.Background(), account)
	}()
	<-secondStarted
	time.Sleep(50 * time.Millisecond)
	close(releaseToken)
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent refresh %d: %v", i, err)
		}
	}
	if got := tokenCalls.Load(); got != 1 {
		t.Fatalf("token endpoint calls = %d, want one family-singleflight refresh", got)
	}
}

func TestRefreshAntigravityAccountDiscardsCASLoser(t *testing.T) {
	tokenEntered := make(chan struct{})
	releaseToken := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAntigravityRefreshFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			close(tokenEntered)
			<-releaseToken
			_, _ = io.WriteString(w, `{"access_token":"provider-loser","refresh_token":"provider-loser-rt","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{
		"antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})

	result := make(chan error, 1)
	go func() { result <- store.RefreshAntigravityAccount(context.Background(), account) }()
	select {
	case <-tokenEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not reach token endpoint")
	}
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	_, applied, err := db.ReplaceAccountCredentialsCAS(context.Background(), accountID, row.CredentialGeneration, "replacement-family", map[string]any{
		"upstream_type": UpstreamAntigravity, "access_token": "replacement-access", "refresh_token": "replacement-refresh",
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339), "project_id": "replacement-project",
		"account_id": "replacement-subject", "email": "replacement@example.com", "models": []string{"replacement-model"},
	})
	if err != nil || !applied {
		t.Fatalf("replace concurrent credential: applied=%t err=%v", applied, err)
	}
	close(releaseToken)
	if err := <-result; err != nil {
		t.Fatalf("CAS loser should adopt the winning durable credential: %v", err)
	}
	current, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	account.mu.RLock()
	runtimeAccess := account.AccessToken
	runtimeFamily := account.CredentialFamilyID
	runtimeGeneration := account.CredentialGeneration
	account.mu.RUnlock()
	if current.GetCredential("access_token") != "replacement-access" || strings.Contains(current.GetCredential("access_token"), "provider-loser") || runtimeAccess != "replacement-access" || runtimeFamily != "replacement-family" || runtimeGeneration != current.CredentialGeneration {
		t.Fatalf("CAS loser leaked stale result: row=%#v runtime access=%q family=%q gen=%d", current.Credentials, runtimeAccess, runtimeFamily, runtimeGeneration)
	}
}

func TestRefreshAntigravityAccountPersistsRotatedTokenOnLaterSyncFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-rotated","refresh_token":"refresh-rotated","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-old","paidTier":{"name":"Pro"}}`)
		case "/quota":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary quota failure"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{
		"antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})

	err := store.RefreshAntigravityAccount(context.Background(), account)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "quota") {
		t.Fatalf("refresh error = %v, want later quota failure", err)
	}
	row, dbErr := db.GetAccountByID(context.Background(), accountID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	account.mu.RLock()
	runtimeAccess := account.AccessToken
	runtimeRefresh := account.RefreshToken
	hardBlocked := account.AntigravityHardBlocked
	account.mu.RUnlock()
	if row.CredentialGeneration != 2 || row.GetCredential("access_token") != "access-rotated" || row.GetCredential("refresh_token") != "refresh-rotated" || runtimeAccess != "access-rotated" || runtimeRefresh != "refresh-rotated" || hardBlocked {
		t.Fatalf("rotated credential was not safely preserved: gen%d row=%#v runtime=%q/%q hard=%t", row.CredentialGeneration, row.Credentials, runtimeAccess, runtimeRefresh, hardBlocked)
	}
}

func TestRefreshAntigravityAccountQuarantinesRotatedTokenWhenIdentityCannotBeReverified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-unverified","refresh_token":"refresh-unverified","expires_in":3600}`)
		case "/userinfo":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary identity failure"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{
		"antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})

	err := store.RefreshAntigravityAccount(context.Background(), account)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "identity") {
		t.Fatalf("refresh error = %v, want identity failure", err)
	}
	row, dbErr := db.GetAccountByID(context.Background(), accountID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	account.mu.RLock()
	runtimeProject := account.AntigravityProjectID
	hardBlocked := account.AntigravityHardBlocked
	account.mu.RUnlock()
	if row.CredentialGeneration != 2 || row.GetCredential("access_token") != "access-unverified" || row.GetCredential("refresh_token") != "refresh-unverified" || row.GetCredential("account_id") != "" || row.GetCredential("project_id") != "" || len(row.GetCredentialStringSlice("models")) != 0 || !strings.HasPrefix(row.GetCredential("antigravity_sync_error"), antigravityIdentityRevalidationErrorPrefix) || runtimeProject != "" || !hardBlocked || account.IsAvailable() {
		t.Fatalf("unverified rotated identity was not quarantined: gen=%d row=%#v runtimeProject=%q hard=%t available=%t", row.CredentialGeneration, row.Credentials, runtimeProject, hardBlocked, account.IsAvailable())
	}
}

func TestRefreshAntigravityAccountQuarantinesPrincipalTransition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAntigravityRefreshFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-subject-2","refresh_token":"refresh-subject-2","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-2","email":"other@example.com","verified_email":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{
		"antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})

	err := store.RefreshAntigravityAccount(context.Background(), account)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "changed google principal") {
		t.Fatalf("refresh error = %v, want principal transition quarantine", err)
	}
	row, dbErr := db.GetAccountByID(context.Background(), accountID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	account.mu.RLock()
	runtimeProject := account.AntigravityProjectID
	hardBlocked := account.AntigravityHardBlocked
	account.mu.RUnlock()
	if row.CredentialGeneration != 2 || row.GetCredential("access_token") != "access-subject-2" || row.GetCredential("refresh_token") != "refresh-subject-2" || row.GetCredential("project_id") != "" || len(row.GetCredentialStringSlice("models")) != 0 || runtimeProject != "" || !hardBlocked || account.IsAvailable() {
		t.Fatalf("principal transition was not quarantined: gen%d row=%#v runtimeProject=%q hard=%t available=%t", row.CredentialGeneration, row.Credentials, runtimeProject, hardBlocked, account.IsAvailable())
	}
}

func TestAntigravityPersistedProviderFactsAreRuntimeHardFences(t *testing.T) {
	tests := []struct {
		name  string
		facts map[string]any
	}{
		{name: "quota forbidden", facts: map[string]any{"antigravity_quota": `{"forbidden":true,"models":[]}`}},
		{name: "permission denied", facts: map[string]any{"antigravity_permissions": `{"allowed":false,"reason":"policy denied","updated_at":"2026-08-23T00:00:00Z"}`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, account, _ := newAntigravityRefreshTestAccount(t, test.facts)
			account.mu.RLock()
			hardBlocked := account.AntigravityHardBlocked
			reason := account.AntigravityHardBlockReason
			account.mu.RUnlock()
			if !hardBlocked || reason == "" || account.IsAvailable() || account.ModelCatalogEligible() {
				t.Fatalf("persisted fact did not fence runtime: hard=%t reason=%q available=%t catalog=%t", hardBlocked, reason, account.IsAvailable(), account.ModelCatalogEligible())
			}
		})
	}
}

func TestAntigravityPersistedSyncErrorTextAloneIsNotAPermanentFence(t *testing.T) {
	_, _, account, _ := newAntigravityRefreshTestAccount(t, map[string]any{
		"antigravity_sync_error": "Antigravity token refresh failed: custom: invalid_grant",
	})
	account.mu.RLock()
	hardBlocked := account.AntigravityHardBlocked
	permanentFailures := account.PermanentRefreshFailures
	account.mu.RUnlock()
	if hardBlocked || permanentFailures != 0 || !account.IsAvailable() {
		t.Fatalf("unmarked sync_error text restored a permanent fence: hard=%t failures=%d available=%t", hardBlocked, permanentFailures, account.IsAvailable())
	}
}

func TestRefreshAntigravityAccountMarksInvalidGrantPermanent(t *testing.T) {
	var tokenCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		tokenCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)
	store, db, account, accountID := newAntigravityRefreshTestAccount(t, map[string]any{
		"oauth_client_key": "custom", "antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})

	err := store.RefreshAntigravityAccount(context.Background(), account)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "invalid_grant") {
		t.Fatalf("refresh error = %v, want invalid_grant", err)
	}
	row, dbErr := db.GetAccountByID(context.Background(), accountID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	account.mu.RLock()
	hardBlocked := account.AntigravityHardBlocked
	status := account.Status
	permanentFailures := account.PermanentRefreshFailures
	account.mu.RUnlock()
	if row.CredentialGeneration != 1 || !strings.Contains(strings.ToLower(row.GetCredential("antigravity_sync_error")), "invalid_grant") || row.GetCredential(antigravityPermanentRefreshErrorCredentialKey) != row.GetCredential("antigravity_sync_error") || !hardBlocked || status != StatusError || permanentFailures < permanentRefreshFailureTerminalLimit || account.IsAvailable() {
		t.Fatalf("permanent failure state = gen%d sync=%q hard=%t status=%v failures=%d available=%t", row.CredentialGeneration, row.GetCredential("antigravity_sync_error"), hardBlocked, status, permanentFailures, account.IsAvailable())
	}
	if tokenCalls.Load() == 0 {
		t.Fatal("invalid_grant fixture was not exercised")
	}

	reloadedStore := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(reloadedStore.Stop)
	if err := reloadedStore.LoadAccountByID(context.Background(), accountID); err != nil {
		t.Fatalf("reload permanently fenced account: %v", err)
	}
	reloaded := reloadedStore.FindByID(accountID)
	if reloaded == nil {
		t.Fatal("permanently fenced account disappeared on reload")
	}
	reloaded.mu.RLock()
	reloadedHardBlocked := reloaded.AntigravityHardBlocked
	reloadedPermanentFailures := reloaded.PermanentRefreshFailures
	reloaded.mu.RUnlock()
	if !reloadedHardBlocked || reloadedPermanentFailures < permanentRefreshFailureTerminalLimit || reloaded.IsAvailable() {
		t.Fatalf("permanent marker was not restored on reload: hard=%t failures=%d available=%t", reloadedHardBlocked, reloadedPermanentFailures, reloaded.IsAvailable())
	}
}

func TestRefreshAntigravityAccountFailsClosedWithoutProxyPoolEgress(t *testing.T) {
	store, _, account, _ := newAntigravityRefreshTestAccount(t, map[string]any{
		"antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})
	store.SetProxyPoolEnabled(true)
	err := store.RefreshAntigravityAccount(context.Background(), account)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "no usable proxy") {
		t.Fatalf("refresh error = %v, want proxy-pool fail-closed error", err)
	}
}

func TestRefreshAntigravityAccountPublishesDurableCredentialWhenCooldownCleanupFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAntigravityRefreshFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"cleanup-access-new","refresh_token":"cleanup-refresh-new","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	dbPath := filepath.Join(t.TempDir(), "antigravity-cleanup-failure.db")
	store, db, account, accountID := newAntigravityRefreshTestAccountAtPath(t, dbPath, map[string]any{
		"antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})
	if err := db.SetCooldown(context.Background(), accountID, "unauthorized", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	trigger := fmt.Sprintf(`
		CREATE TRIGGER fail_antigravity_cooldown_cleanup
		BEFORE UPDATE OF cooldown_reason ON accounts
		WHEN OLD.id = %d AND OLD.cooldown_reason = 'unauthorized' AND NEW.cooldown_reason = ''
		BEGIN
			SELECT RAISE(ABORT, 'forced cooldown cleanup failure');
		END`, accountID)
	if _, err := raw.Exec(trigger); err != nil {
		t.Fatal(err)
	}

	err = store.RefreshAntigravityAccount(context.Background(), account)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cooldown") {
		t.Fatalf("refresh error = %v, want forced cooldown cleanup failure", err)
	}
	row, dbErr := db.GetAccountByID(context.Background(), accountID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	account.mu.RLock()
	runtimeAccess := account.AccessToken
	runtimeStatus := account.Status
	runtimeCooldownReason := account.CooldownReason
	account.mu.RUnlock()
	if row.GetCredential("access_token") != "cleanup-access-new" || runtimeAccess != "cleanup-access-new" || runtimeStatus != StatusCooldown || runtimeCooldownReason != "unauthorized" || store.FindByID(accountID) != account {
		t.Fatalf("durable credential was not reconciled after cleanup failure: row=%q runtime=%q status=%v reason=%q present=%t", row.GetCredential("access_token"), runtimeAccess, runtimeStatus, runtimeCooldownReason, store.FindByID(accountID) != nil)
	}
}

func TestReloadAntigravityRuntimeRemovesStaleAccountWhenDurableReadFails(t *testing.T) {
	store, _, account, accountID := newAntigravityRefreshTestAccount(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.reloadAntigravityRuntimeOrRemove(ctx, account, accountID); err == nil {
		t.Fatal("reloadAntigravityRuntimeOrRemove error = nil, want canceled read error")
	}
	if got := store.FindByID(accountID); got != nil {
		t.Fatalf("stale runtime account survived failed durable reload: %#v", got)
	}
}

func TestRefreshAntigravityAccountRemovesRuntimeWhenRotatedCredentialCASFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if writeAntigravityRefreshFixture(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"cas-error-access","refresh_token":"cas-error-refresh","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-1","email":"user@example.com","verified_email":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useAntigravityTestEndpoints(t, server.URL)

	dbPath := filepath.Join(t.TempDir(), "antigravity-cas-failure.db")
	store, db, account, accountID := newAntigravityRefreshTestAccountAtPath(t, dbPath, map[string]any{
		"antigravity_client_id": "client", "antigravity_client_secret": "secret",
	})
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	trigger := fmt.Sprintf(`
		CREATE TRIGGER fail_antigravity_credential_cas
		BEFORE UPDATE OF credentials ON accounts
		WHEN OLD.id = %d
		BEGIN
			SELECT RAISE(ABORT, 'forced credential CAS failure');
		END`, accountID)
	if _, err := raw.Exec(trigger); err != nil {
		t.Fatal(err)
	}

	err = store.RefreshAntigravityAccount(context.Background(), account)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "persist") {
		t.Fatalf("refresh error = %v, want durable CAS failure", err)
	}
	if got := store.FindByID(accountID); got != nil {
		t.Fatalf("runtime retained old credential after rotated CAS failure: %#v", got)
	}
	row, dbErr := db.GetAccountByID(context.Background(), accountID)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if row.GetCredential("access_token") != "access-old" || row.GetCredential("refresh_token") != "refresh-old" {
		t.Fatalf("forced CAS failure unexpectedly mutated durable credential: %#v", row.Credentials)
	}
}
