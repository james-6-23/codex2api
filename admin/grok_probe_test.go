package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func TestGrokPersistedStateNeedsRefresh(t *testing.T) {
	now := time.Now()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at", CredentialGeneration: 2}
	freshFacts := map[string]database.GrokAccountFact{}
	for _, kind := range []string{database.GrokFactUser, database.GrokFactSettings, database.GrokFactBilling, database.GrokFactAutoTopup} {
		freshFacts[kind] = database.GrokAccountFact{Kind: kind, CredentialGeneration: 2, Status: "ok", ExpiresAt: now.Add(time.Minute)}
	}
	state := &database.GrokAccountState{
		CredentialGeneration: 2,
		Facts:                freshFacts,
		Catalogs: []database.GrokModelCatalog{{Snapshot: database.GrokModelCatalogSnapshot{
			Origin: auth.GrokDefaultChatProxyBaseURL, CredentialGeneration: 2,
			Status: "ok", ExpiresAt: now.Add(time.Minute),
		}}},
	}
	if grokPersistedStateNeedsRefresh(account, state, now) {
		t.Fatal("fresh state was marked stale")
	}

	state.Catalogs[0].Snapshot.Status = "unavailable"
	state.Catalogs[0].Snapshot.ExpiresAt = now.Add(time.Hour) // stale-if-error routing window
	if !grokPersistedStateNeedsRefresh(account, state, now) {
		t.Fatal("failed catalog must be retried even while old items remain routable")
	}
	state.Catalogs[0].Snapshot.Status = "ok"
	billing := state.Facts[database.GrokFactBilling]
	billing.ExpiresAt = now
	state.Facts[database.GrokFactBilling] = billing
	if !grokPersistedStateNeedsRefresh(account, state, now) {
		t.Fatal("expired billing fact did not trigger refresh")
	}
	selection := grokPersistedStateRefreshSelection(account, state, now)
	if len(selection.FactKinds) != 1 {
		t.Fatalf("expired billing selection = %#v, want one fact", selection.FactKinds)
	}
	if _, ok := selection.FactKinds[proxy.GrokControlPlaneBilling]; !ok || selection.Catalog {
		t.Fatalf("expired billing selection = %#v catalog=%v, want billing only", selection.FactKinds, selection.Catalog)
	}
}

func TestGrokCapabilityFailureTTLPreventsRecurringAutomaticProbe(t *testing.T) {
	now := time.Now()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", CredentialGeneration: 1}
	origin := normalizeGrokProbeOrigin(auth.GrokDefaultAPIBaseURL)
	state := &database.GrokAccountState{
		CredentialGeneration: 1,
		Catalogs: []database.GrokModelCatalog{{Snapshot: database.GrokModelCatalogSnapshot{
			Origin: origin, CredentialGeneration: 1, Status: "ok", ExpiresAt: now.Add(time.Hour),
		}, Items: []database.GrokModelCatalogItem{{ModelID: "grok-test", CredentialGeneration: 1}}}},
	}
	for _, protocol := range []proxy.GrokProtocol{proxy.GrokProtocolResponses, proxy.GrokProtocolChatCompletions, proxy.GrokProtocolMessages} {
		state.Capabilities = append(state.Capabilities, database.GrokModelCapability{
			ModelID: "grok-test", Origin: origin, Protocol: string(protocol), CredentialGeneration: 1,
			Status: "unsupported", ObservedAt: now, ExpiresAt: now.Add(grokCapabilityFailureTTL),
		})
	}
	if grokGenerationNeedsCapabilityProbe(account, state, 1, now.Add(10*time.Minute)) {
		t.Fatal("negative capability observations should suppress automatic reprobe for 24 hours")
	}
	if !grokGenerationNeedsCapabilityProbe(account, state, 1, now.Add(25*time.Hour)) {
		t.Fatal("expired capability observations should become probe candidates")
	}
}

func TestGrokNextMaintenanceDueUsesExpiryAndRejectsIncompleteState(t *testing.T) {
	now := time.Now()
	origin := normalizeGrokProbeOrigin(auth.GrokDefaultAPIBaseURL)
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai-test", CredentialGeneration: 1, BaseURL: origin}
	state := &database.GrokAccountState{
		CredentialGeneration: 1,
		Catalogs: []database.GrokModelCatalog{{Snapshot: database.GrokModelCatalogSnapshot{
			Origin: origin, CredentialGeneration: 1, Status: "ok", ExpiresAt: now.Add(5 * time.Minute),
		}}},
	}

	due, err := grokNextMaintenanceDue(account, state, now)
	if err != nil || !due.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("fresh empty-catalog due = %v, err=%v, want catalog expiry", due, err)
	}
	state.Catalogs[0].Snapshot.ExpiresAt = now
	if _, err := grokNextMaintenanceDue(account, state, now); !errors.Is(err, errGrokMaintenanceProjectionIncomplete) {
		t.Fatalf("expired catalog error = %v, want incomplete projection", err)
	}
	state.Catalogs = nil
	if _, err := grokNextMaintenanceDue(account, state, now); !errors.Is(err, errGrokMaintenanceProjectionIncomplete) {
		t.Fatalf("missing catalog error = %v, want incomplete projection", err)
	}
}

func TestRefreshStaleGrokControlPlaneSelectsOnlyExpiredBillingWhenProbeDisabled(t *testing.T) {
	var callsMu sync.Mutex
	calls := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callsMu.Lock()
		calls[r.URL.Path]++
		callsMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/billing":
			_, _ = w.Write([]byte(`{"config":{"creditUsagePercent":10}}`))
		default:
			http.Error(w, `{"error":{"code":"unexpected_endpoint"}}`, http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	db := newTestAdminDB(t)
	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamGrok,
		"access_token":  "test-oauth-access-token",
		"base_url":      server.URL + "/v1",
	}
	id, err := db.InsertAccountWithUpstream(context.Background(), "grok-freshness", "xai", auth.UpstreamGrok, credentials, "")
	if err != nil {
		t.Fatalf("InsertAccountWithUpstream: %v", err)
	}
	store := auth.NewStore(db, nil, nil) // GrokProbeEnabled defaults false.
	t.Cleanup(store.Stop)
	if store.GrokProbeEnabled() {
		t.Fatal("test requires generation probe switch to remain disabled")
	}
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	account := store.FindByID(id)
	if account == nil {
		t.Fatal("loaded account is nil")
	}
	generation := account.GetCredentialGeneration()
	now := time.Now()
	for _, kind := range []string{database.GrokFactUser, database.GrokFactSettings, database.GrokFactBilling, database.GrokFactAutoTopup} {
		expires := now.Add(time.Minute)
		if kind == database.GrokFactBilling {
			expires = now.Add(-time.Second)
		}
		applied, persistErr := db.UpsertGrokAccountFact(context.Background(), database.GrokAccountFact{
			AccountID: id, Kind: kind, CredentialGeneration: generation, Status: "ok",
			Source: "test", Payload: map[string]any{}, FieldPresence: map[string]string{},
			ObservedAt: now.Add(-time.Minute), ExpiresAt: expires,
		})
		if persistErr != nil || !applied {
			t.Fatalf("UpsertGrokAccountFact(%s) applied=%v err=%v", kind, applied, persistErr)
		}
	}
	origin, _ := account.GrokCredentials()
	applied, err := db.ReplaceGrokModelCatalog(context.Background(), database.GrokModelCatalogSnapshot{
		AccountID: id, Origin: origin, CredentialGeneration: generation,
		AuthKind: auth.GrokAuthKindOAuth, Status: "ok", ObservedAt: now, ExpiresAt: now.Add(time.Minute),
	}, nil)
	if err != nil || !applied {
		t.Fatalf("ReplaceGrokModelCatalog applied=%v err=%v", applied, err)
	}

	h := &Handler{db: db, store: store}
	refreshed, failed := h.refreshStaleGrokControlPlane(context.Background(), []*auth.Account{account})
	if refreshed != 1 || failed != 0 {
		t.Fatalf("refresh counts = %d/%d, want 1/0", refreshed, failed)
	}
	callsMu.Lock()
	defer callsMu.Unlock()
	if calls["/v1/billing"] != 1 || len(calls) != 1 {
		t.Fatalf("control-plane calls = %#v, want billing only", calls)
	}
}
