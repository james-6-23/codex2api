package auth

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
)

func TestAntigravityAccountParticipatesInScheduling(t *testing.T) {
	account := &Account{
		DBID:                 42,
		UpstreamType:         UpstreamAntigravity,
		AccessToken:          "access-token",
		RefreshToken:         "refresh-token",
		AntigravityProjectID: "project-id",
		HealthTier:           HealthTierHealthy,
	}
	if !account.IsAntigravityAPI() {
		t.Fatal("IsAntigravityAPI() = false")
	}
	if !account.IsAvailable() {
		t.Fatal("IsAvailable() = false; synced Antigravity account should be schedulable")
	}
	if !account.ModelCatalogEligible() {
		t.Fatal("ModelCatalogEligible() = false; synced Antigravity models should be discoverable")
	}
	store := &Store{accounts: []*Account{account}}
	if !store.accountLazySelectable(account) {
		t.Fatal("accountLazySelectable() = false; synced Antigravity account should be lazily selectable")
	}
	if err := store.RefreshSingle(context.Background(), account.DBID); err == nil || !strings.Contains(err.Error(), "专用配额刷新") {
		t.Fatalf("RefreshSingle() error = %v", err)
	}
}

func TestAntigravityModelCatalogHonorsAdministrativeFences(t *testing.T) {
	base := func() *Account {
		return &Account{
			DBID:                 42,
			UpstreamType:         UpstreamAntigravity,
			AccessToken:          "access-token",
			AntigravityProjectID: "project-id",
			HealthTier:           HealthTierHealthy,
		}
	}

	paused := base()
	atomic.StoreInt32(&paused.DispatchPaused, 1)
	if paused.ModelCatalogEligible() {
		t.Fatal("dispatch-paused Antigravity account must not publish models")
	}

	failed := base()
	failed.Status = StatusError
	if failed.ModelCatalogEligible() {
		t.Fatal("errored Antigravity account must not publish models")
	}

	blocked := base()
	blocked.AntigravityHardBlocked = true
	if blocked.ModelCatalogEligible() {
		t.Fatal("hard-blocked Antigravity account must not publish models")
	}
}

func TestAntigravityAPIKeyAccountClassificationAndModels(t *testing.T) {
	account := &Account{
		DBID:         43,
		UpstreamType: UpstreamAntigravity,
		APIKey:       "google-api-key",
		HealthTier:   HealthTierHealthy,
	}
	if !account.IsAntigravityAPI() {
		t.Fatal("IsAntigravityAPI() = false for API-key account")
	}
	if got := account.AntigravityAuthKind(); got != AntigravityAuthKindAPIKey {
		t.Fatalf("AntigravityAuthKind() = %q, want api_key", got)
	}
	if got := account.AntigravityAPIKey(); got != "google-api-key" {
		t.Fatalf("AntigravityAPIKey() = %q", got)
	}
	if !account.IsAvailable() {
		t.Fatal("API-key Antigravity account should be dispatchable without OAuth project metadata")
	}
	for _, model := range AntigravityDefaultModelIDs() {
		if !account.AntigravitySupportsModel(model) {
			t.Fatalf("default model %q is not supported", model)
		}
	}
	account.Models = []string{"gemini-custom"}
	if !account.AntigravitySupportsModel("GEMINI-CUSTOM") || account.AntigravitySupportsModel("gemini-2.5-flash") {
		t.Fatalf("explicit model catalog was not enforced: %v", account.AntigravityModels())
	}
}

func TestAntigravityAPIKeyDoesNotUseOAuthUnauthorizedRecovery(t *testing.T) {
	account := &Account{
		UpstreamType:             UpstreamAntigravity,
		APIKey:                   "google-api-key",
		Status:                   StatusCooldown,
		CooldownReason:           "unauthorized",
		CooldownUtil:             time.Now().Add(time.Hour),
		HealthTier:               HealthTierBanned,
		PermanentRefreshFailures: 0,
	}
	account.mu.RLock()
	recoverable := account.antigravityUnauthorizedRecoveryLocked(time.Now())
	account.mu.RUnlock()
	if recoverable {
		t.Fatal("API-key account entered OAuth unauthorized recovery")
	}
	if account.IsAvailable() {
		t.Fatal("banned API-key account should remain fenced")
	}
}

func TestUnauthorizedAntigravityAccountRemainsDispatchable(t *testing.T) {
	account := &Account{
		DBID:                 7,
		UpstreamType:         UpstreamAntigravity,
		AccessToken:          "google-access",
		RefreshToken:         "google-refresh",
		AntigravityProjectID: "project-id",
		Models:               []string{"gemini-2.5-flash"},
		Status:               StatusCooldown,
		CooldownReason:       "unauthorized",
		CooldownUtil:         time.Now().Add(23 * time.Hour),
		HealthTier:           HealthTierBanned,
	}
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()
	store := &Store{
		accounts:       []*Account{account},
		maxConcurrency: 2,
		tokenCache:     tokenCache,
	}
	store.SetFastSchedulerEnabled(true)
	store.setCachedAccountCooldown(account.DBID, "unauthorized", account.CooldownUtil)

	if !account.IsAvailable() {
		t.Fatal("IsAvailable() = false for credentialed Antigravity account under unauthorized cooldown")
	}
	if store.accountHasCachedCooldown(account) {
		t.Fatal("accountHasCachedCooldown() fenced an Antigravity account")
	}
	if !store.hasDispatchCandidateWithFilter(0, nil, func(acc *Account) bool {
		return acc.SupportsOpenAIResponsesModel("gemini-2.5-flash")
	}) {
		t.Fatal("hasDispatchCandidateWithFilter() = false; unauthorized cooldown must not empty the Antigravity pool")
	}

	_, _, _, limit := account.schedulerSnapshot(2)
	if limit <= 0 {
		t.Fatalf("schedulerSnapshot limit = %d, want > 0", limit)
	}
	_, _, snapLimit, _, available := account.fastSchedulerSnapshot(2, time.Now())
	if !available || snapLimit <= 0 {
		t.Fatalf("fastSchedulerSnapshot available=%v limit=%d, want dispatchable", available, snapLimit)
	}

	got := store.NextExcludingWithFilter(0, nil, func(acc *Account) bool {
		return acc.IsAntigravityAPI() && acc.SupportsOpenAIResponsesModel("gemini-2.5-flash")
	})
	if got == nil {
		t.Fatal("NextExcludingWithFilter() = nil; Antigravity request never reaches v1internal")
	}
	store.Release(got)

	waited, _ := store.WaitForSessionAvailableWithFilter(context.Background(), "", 200*time.Millisecond, 0, nil, func(acc *Account) bool {
		return acc.SupportsOpenAIResponsesModel("gemini-2.5-flash")
	})
	if waited == nil {
		t.Fatal("WaitForSessionAvailableWithFilter() = nil; this is the live 503 path")
	}
	store.Release(waited)
}

func TestAntigravityRateLimitCooldownRemainsFenced(t *testing.T) {
	account := &Account{
		DBID:                 9,
		UpstreamType:         UpstreamAntigravity,
		AccessToken:          "google-access",
		RefreshToken:         "google-refresh",
		AntigravityProjectID: "project-id",
		Models:               []string{"gemini-2.5-flash"},
		Status:               StatusCooldown,
		CooldownReason:       "rate_limited",
		CooldownUtil:         time.Now().Add(time.Hour),
		HealthTier:           HealthTierRisky,
	}
	tokenCache := cache.NewMemory(4)
	defer tokenCache.Close()
	store := &Store{
		accounts:       []*Account{account},
		maxConcurrency: 2,
		tokenCache:     tokenCache,
	}
	store.SetFastSchedulerEnabled(true)
	store.setCachedAccountCooldown(account.DBID, "rate_limited", account.CooldownUtil)

	if account.IsAvailable() {
		t.Fatal("rate-limited Antigravity account bypassed its active cooldown")
	}
	if !store.accountHasCachedCooldown(account) {
		t.Fatal("rate-limited Antigravity account ignored its cached cooldown")
	}
	_, _, limit, _, available := account.fastSchedulerSnapshot(2, time.Now())
	if available {
		t.Fatalf("fast scheduler snapshot available=%v limit=%d, want fenced", available, limit)
	}
	got := store.NextExcludingWithFilter(0, nil, func(acc *Account) bool {
		return acc.IsAntigravityAPI() && acc.AntigravitySupportsModel("gemini-2.5-flash")
	})
	if got != nil {
		store.Release(got)
		t.Fatal("scheduler selected an Antigravity account during rate-limit cooldown")
	}

	store.SetLazyMode(true)
	if store.accountLazySelectable(account) {
		t.Fatal("lazy scheduler treated a rate-limited Antigravity account as selectable")
	}
	if got := store.AvailableCount(); got != 0 {
		t.Fatalf("AvailableCount() = %d, want 0 for the cooled account", got)
	}
	healthy := &Account{
		DBID:                 10,
		UpstreamType:         UpstreamAntigravity,
		AccessToken:          "healthy-access",
		RefreshToken:         "healthy-refresh",
		ExpiresAt:            time.Now().Add(time.Hour),
		AntigravityProjectID: "healthy-project",
		Models:               []string{"gemini-2.5-flash"},
		HealthTier:           HealthTierHealthy,
	}
	account.SchedulerPriority = 100
	store.AddAccount(healthy)
	got = store.NextExcludingWithFilter(0, nil, func(acc *Account) bool {
		return acc.IsAntigravityAPI() && acc.AntigravitySupportsModel("gemini-2.5-flash")
	})
	if got == nil || got.DBID != healthy.DBID {
		if got != nil {
			store.Release(got)
		}
		t.Fatalf("lazy scheduler did not bypass cooled high-priority account: got=%v", got)
	}
	store.Release(got)
}

func TestAntigravityDispatchRequiresProjectAndAccessToken(t *testing.T) {
	account := &Account{UpstreamType: UpstreamAntigravity, RefreshToken: "refresh-only", AntigravityProjectID: "project-id"}
	if account.IsAvailable() {
		t.Fatal("refresh-token-only Antigravity account should not be dispatchable")
	}
	account.AccessToken = "access-token"
	if !account.IsAvailable() {
		t.Fatal("project + access token should be dispatchable")
	}
	account.AntigravityProjectID = ""
	if account.IsAvailable() {
		t.Fatal("Antigravity account without project should not be dispatchable")
	}
}

func TestAntigravityHardFencesAreNotBypassed(t *testing.T) {
	account := &Account{UpstreamType: UpstreamAntigravity, AccessToken: "at", AntigravityProjectID: "project-id", Status: StatusError}
	if account.IsAvailable() {
		t.Fatal("StatusError must remain a hard fence")
	}
	account.Status = StatusReady
	atomic.StoreInt32(&account.DispatchPaused, 1)
	if account.IsAvailable() {
		t.Fatal("DispatchPaused must remain a hard fence")
	}
	atomic.StoreInt32(&account.DispatchPaused, 0)
	account.HealthTier = HealthTierBanned
	if account.IsAvailable() {
		t.Fatal("terminal ban must remain a hard fence")
	}
}

func TestRefreshAntigravityAccountClearsUnauthorizedCooldown(t *testing.T) {
	account := &Account{
		DBID:           8,
		UpstreamType:   UpstreamAntigravity,
		AccessToken:    "stale-access",
		RefreshToken:   "google-refresh",
		Status:         StatusCooldown,
		CooldownReason: "unauthorized",
		CooldownUtil:   time.Now().Add(time.Hour),
		HealthTier:     HealthTierBanned,
	}
	atomic.StoreInt32(&account.Disabled, 1)
	store := &Store{accounts: []*Account{account}, maxConcurrency: 2}
	store.ClearCooldown(account)
	if atomic.LoadInt32(&account.Disabled) != 0 {
		t.Fatal("ClearCooldown left Disabled set")
	}
	if account.IsBanned() {
		t.Fatal("ClearCooldown left HealthTier banned")
	}
	if account.HasActiveCooldown() {
		t.Fatal("ClearCooldown left an active cooldown")
	}
}

func TestAntigravityQuotaSnapshotReadsLegacyGroupsAndWritesCanonicalField(t *testing.T) {
	var snapshot AntigravityQuotaSnapshot
	if err := json.Unmarshal([]byte(`{"models":[],"groups":[{"display_name":"Weekly","buckets":[]}],"updated_at":"2026-08-16T00:00:00Z"}`), &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Groups) != 1 || snapshot.Groups[0].DisplayName != "Weekly" {
		t.Fatalf("legacy groups = %+v", snapshot.Groups)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"quota_groups"`) || strings.Contains(string(encoded), `"groups"`) {
		t.Fatalf("canonical quota JSON = %s", encoded)
	}
}
