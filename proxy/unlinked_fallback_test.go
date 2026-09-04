package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestUnlinkedFallbackPrefersRecentAccountWithoutCreatingAffinity(t *testing.T) {
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.6-sol", CodexUnlinkedAccountFallbackEnabled: true, CodexUnlinkedAccountFallbackSeconds: 300})
	account := &auth.Account{DBID: 17, AccessToken: "access", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy, BaseConcurrencyEffective: 2, DynamicConcurrencyLimit: 2}
	store.AddAccount(account)
	handler := &Handler{store: store, cache: tc}

	started := time.Now()
	scope := "scope-test"
	record, _ := json.Marshal(unlinkedFallbackRuntimeRecord{AccountID: account.ID(), ObservedAt: started.Add(-time.Second)})
	if err := tc.SetRuntime(context.Background(), unlinkedFallbackRuntimeNamespace, scope, record, 5*time.Minute); err != nil {
		t.Fatalf("SetRuntime: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req
	ctx.Set(unlinkedFallbackContextKey, unlinkedFallbackContext{Scope: scope, RequestStarted: started})
	selected, _ := handler.takeUnlinkedRecentAccount(ctx, requestSessionIdentity{unlinkedFallbackOnly: true, unlinkedFallbackScope: scope}, 0, nil, nil, auth.DispatchPolicyStandard)
	if selected == nil || selected.ID() != account.ID() {
		t.Fatalf("selected account = %#v, want %d", selected, account.ID())
	}
	store.Release(selected)
	if key := capacityAwareSessionAffinityKey(requestSessionIdentity{unlinkedFallbackOnly: true}, 0); key != "" {
		t.Fatalf("unlinked request affinity key = %q, want empty request-local key", key)
	}
}

func TestUnlinkedFallbackVerifiedNewAPIScopeIgnoresDownstreamChannelCredential(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Authorization", "Bearer channel-a")
	policy := verifiedNewAPIPolicyContext{
		Identity:     newAPIIdentity{UserID: "42"},
		Platform:     "newapi",
		MetaVerified: true,
		Meta: newAPIPolicyMeta{
			TokenID:        7,
			InstallationID: "device-a",
		},
	}
	first := unlinkedFallbackScopeForRequest(ctx, nil, policy, true)
	if first == "" {
		t.Fatal("expected a verified NewAPI fallback scope")
	}

	ctx.Request.Header.Set("Authorization", "Bearer channel-b")
	second := unlinkedFallbackScopeForRequest(ctx, nil, policy, true)
	if second != first {
		t.Fatalf("scope changed with downstream channel credential: first=%q second=%q", first, second)
	}

	policy.Meta.InstallationID = "device-b"
	if got := unlinkedFallbackScopeForRequest(ctx, nil, policy, true); got == first {
		t.Fatal("installation ID did not isolate the fallback scope")
	}
}

func TestUnlinkedFallbackRejectsObservationAfterRequestStart(t *testing.T) {
	tc := cache.NewMemory(1)
	defer tc.Close()
	store := auth.NewStore(nil, tc, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.6-sol", CodexUnlinkedAccountFallbackEnabled: true})
	account := &auth.Account{DBID: 18, AccessToken: "access", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy, BaseConcurrencyEffective: 1, DynamicConcurrencyLimit: 1}
	store.AddAccount(account)
	handler := &Handler{store: store, cache: tc}
	started := time.Now()
	scope := "scope-future"
	record, _ := json.Marshal(unlinkedFallbackRuntimeRecord{AccountID: account.ID(), ObservedAt: started.Add(time.Second)})
	if err := tc.SetRuntime(context.Background(), unlinkedFallbackRuntimeNamespace, scope, record, time.Minute); err != nil {
		t.Fatalf("SetRuntime: %v", err)
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set(unlinkedFallbackContextKey, unlinkedFallbackContext{Scope: scope, RequestStarted: started})
	if selected, _ := handler.takeUnlinkedRecentAccount(ctx, requestSessionIdentity{unlinkedFallbackOnly: true, unlinkedFallbackScope: scope}, 0, nil, nil, auth.DispatchPolicyStandard); selected != nil {
		store.Release(selected)
		t.Fatal("future observation was accepted")
	}
}
