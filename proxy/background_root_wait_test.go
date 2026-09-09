package proxy

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestBackgroundRootWaitBudgetAndRouting(test *testing.T) {
	runtimeCache := cache.NewMemory(1)
	test.Cleanup(func() { _ = runtimeCache.Close() })
	for _, source := range []string{"thread_title", "ambient_suggestions", "agent_created_thread", "guardian_review", "memory_consolidation", "subagent", "thread_description", "thread_summary"} {
		for _, scenario := range []string{"late root", "timeout", "shared budget", "already bound"} {
			test.Run(source+"/"+scenario, func(test *testing.T) {
				synctest.Test(test, func(test *testing.T) {
					config := promptGuardTestConfig()
					config.Advanced.NewAPI.Enabled = true
					store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
					test.Cleanup(store.Stop)
					store.SetPromptFilterConfig(config)
					store.SetPassiveInternalModelsEnabled(true)
					store.SetCodexUnlinkedAccountFallbackEnabled(true)
					store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
						APIKeyID: 101, PlatformCode: "test-platform", Secret: "integration-secret", Enabled: true,
						PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit,
					}})
					handler := NewHandler(store, nil, nil, nil)
					handler.SetRuntimeCache(runtimeCache)
					root := &auth.Account{DBID: 17, AccessToken: "root", Status: auth.StatusReady, Models: []string{"main-model"}}
					other := &auth.Account{DBID: 18, AccessToken: "other", Status: auth.StatusReady, Models: []string{"title-model"}}
					handler.store.AddAccounts([]*auth.Account{root, other})
					fingerprint := promptSessionTestFingerprint(test.Name())
					meta := newAPIPolicyMeta{
						RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
						RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: fingerprint,
						ThreadSource: source, RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal,
					}
					if scenario == "shared budget" {
						remaining := int64(7000)
						meta.RootAccountWaitMillis = &remaining
					}
					body := []byte(`{"model":"title-model","input":"background task"}`)
					requestContext, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
					handler.primeNewAPIPolicyContext(requestContext, body)
					identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
					if !identity.requiresRootAccount || identity.unlinkedFallbackOnly {
						test.Fatal("background request may fall back to unrelated accounts")
					}
					rootKey := sessionAffinityKey(identity.affinityID, 101)
					if scenario == "already bound" {
						handler.store.BindSessionAffinity(rootKey, root, "")
					}
					result := make(chan *api.APIError, 1)
					started := time.Now()
					go func() { result <- handler.waitForBackgroundRootAccount(requestContext, identity) }()
					synctest.Wait()
					if scenario == "late root" {
						time.Sleep(10 * time.Second)
						handler.store.BindSessionAffinity(rootKey, root, "")
					}
					waitError := <-result
					wantDuration := map[string]time.Duration{"late root": 10 * time.Second, "timeout": 60 * time.Second, "shared budget": 7 * time.Second}[scenario]
					if elapsed := time.Since(started); elapsed != wantDuration {
						test.Fatalf("wait=%v, want %v", elapsed, wantDuration)
					}
					if scenario == "timeout" || scenario == "shared budget" {
						if waitError == nil || api.HTTPStatusCode(waitError.Code) != http.StatusBadRequest || waitError.Code != api.ErrCodeRootAccountWaitTimeout {
							test.Fatalf("expected non-retryable timeout: %v", waitError)
						}
						return
					}
					if waitError != nil {
						test.Fatal(waitError)
					}
					filter := handler.applyPassiveInternalModelRouting(requestContext, "title-model", identity, capacityAwareSessionAffinityKey(identity, 101), true, accountFilterForModel("title-model"))
					if !filter(root) || filter(other) {
						test.Fatal("background model selected another account instead of its main root")
					}
				})
			})
		}
	}
}

func TestBackgroundRootWaitDoesNotChangeUserRequests(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	for _, source := range []string{"user", ""} {
		meta := newAPIPolicyMeta{
			RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
			RootSessionRelation:    newAPIPolicyRootSessionRelationRelated,
			RootSessionFingerprint: promptSessionTestFingerprint(source), ThreadSource: source, RequestKind: "turn",
		}
		body := []byte(`{"model":"test","input":"task"}`)
		requestContext, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
		handler.primeNewAPIPolicyContext(requestContext, body)
		identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
		if identity.requiresRootAccount || handler.waitForBackgroundRootAccount(requestContext, identity) != nil {
			test.Fatalf("unexpected root wait for %s", source)
		}
	}
}

func TestBackgroundRootTimeoutPrecedesAPIKeyConcurrency(test *testing.T) {
	for _, source := range []string{"thread_title", "ambient_suggestions", "agent_created_thread", "guardian_review", "memory_consolidation"} {
		for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
			test.Run(source+path, func(test *testing.T) {
				handler := newRootlessPassiveModelTestHandler(test)
				remaining := int64(0)
				meta := newAPIPolicyMeta{
					RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
					RootSessionRelation:    newAPIPolicyRootSessionRelationRelated,
					RootSessionFingerprint: promptSessionTestFingerprint(test.Name()), ThreadSource: source,
					RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal, RootAccountWaitMillis: &remaining,
				}
				body := []byte(`{"model":"gpt-5.6-sol","input":"background","messages":[{"role":"user","content":"background"}],"max_tokens":16}`)
				requestContext, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, path, body, meta)
				requestContext.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 101, Limits: database.APIKeyLimits{MaxConcurrency: 1}})
				release, _, acquired := handler.apiKeyConcurrencyLimiter().acquire(101, 2)
				if !acquired {
					test.Fatal("failed to reserve existing concurrency")
				}
				defer release()
				releaseSecond, _, acquired := handler.apiKeyConcurrencyLimiter().acquire(101, 2)
				if !acquired {
					test.Fatal("failed to fill existing concurrency")
				}
				defer releaseSecond()
				endpoint := map[string]func(*gin.Context){
					"/v1/responses": handler.Responses, "/v1/responses/compact": handler.ResponsesCompact,
					"/v1/chat/completions": handler.ChatCompletions, "/v1/messages": handler.Messages,
				}[path]
				endpoint(requestContext)
				if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "60-second wait limit") {
					test.Fatalf("root timeout was not enforced before concurrency: %d %s", recorder.Code, recorder.Body.String())
				}
			})
		}
	}
}
