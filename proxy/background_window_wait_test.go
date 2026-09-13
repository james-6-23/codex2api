package proxy

import (
	"context"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBackgroundWindowWaitBudgetAndOwnership(test *testing.T) {
	for _, scenario := range []string{"late_window", "timeout", "canceled", "owner_changed", "aba", "missing_owner", "changed_after_ready"} {
		test.Run(scenario, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			synctest.Test(test, func(test *testing.T) {
				account := &auth.Account{DBID: 17, AccessToken: "root", Status: auth.StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 2}
				handler.store.AddAccount(account)
				remaining := int64(7000)
				meta := newAPIPolicyMeta{
					RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
					RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: promptSessionTestFingerprint(test.Name()),
					ThreadSource: "memory_consolidation", RequestKind: "memory", PassiveFeature: newAPIPassiveFeatureRelatedInternal, RootAccountWaitMillis: &remaining,
				}
				body := []byte(`{"model":"gpt-5.6-sol","input":"memory task"}`)
				request, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
				request.Set(ingressRequestBodyContextKey, body)
				handler.primeNewAPIPolicyContext(request, body)
				identity := handler.resolveRequestSessionIdentityForContext(request, body)
				key := sessionAffinityKey(identity.affinityID, 101)
				entry := sessionContinuityCacheEntry{Record: database.SessionContinuityRecord{AccountID: account.ID(), LastSeen: time.Now()}}
				handler.cacheSessionContinuity(hashRiskIdentity(key), entry)
				handler.store.BindSessionAffinity(key, account, "")
				require.True(test, handler.store.RemoveAccountSession(account.ID(), key))
				ctx, cancel := context.WithCancel(request.Request.Context())
				defer cancel()
				request.Request = request.Request.WithContext(ctx)
				state := usageRequestDiagnosticState(request)
				state.StartedAt = time.Now().Add(-2 * time.Second)
				result := make(chan *api.APIError, 1)
				started := time.Now()
				go func() { result <- handler.waitForBackgroundRootAccount(request, identity) }()
				synctest.Wait()
				require.Empty(test, result)
				if scenario == "canceled" {
					cancel()
				} else if scenario != "timeout" {
					time.Sleep(1250 * time.Millisecond)
					switch scenario {
					case "owner_changed":
						entry.Record.AccountID = 18
					case "aba":
						entry.Record.FailoverCount = 2
					case "missing_owner":
						entry.Record.AccountID = 0
					}
					handler.cacheSessionContinuity(hashRiskIdentity(key), entry)
					require.True(test, handler.store.AdmitAccountSession(account, key, time.Now()))
				}
				failure := <-result
				diagnostic := state.BackgroundWindowWait
				require.NotNil(test, diagnostic)
				require.Equal(test, account.ID(), diagnostic.AccountID)
				require.Equal(test, time.Since(started).Milliseconds(), state.RootAccountWaitMillis)
				switch scenario {
				case "late_window", "changed_after_ready":
					require.Nil(test, failure)
					require.Equal(test, "ready", diagnostic.Result)
					require.EqualValues(test, 1250, diagnostic.DurationMs)
					require.NotNil(test, backgroundAccountMatchFromContext(request.Request.Context()))
					if scenario == "changed_after_ready" {
						entry.Record.AccountID = 18
						handler.cacheSessionContinuity(hashRiskIdentity(key), entry)
						require.NotNil(test, handler.restoreMigratedSessionOwner(request, auth.ProtectedRelatedSessionAffinityKey(key), body))
					}
				case "timeout":
					require.NotNil(test, failure)
					require.Equal(test, api.ErrCodeRootAccountWaitTimeout, failure.Code)
					require.Equal(test, "timeout", diagnostic.Result)
					require.EqualValues(test, 5000, diagnostic.DurationMs)
				case "canceled":
					require.NotNil(test, failure)
					require.Equal(test, "canceled", diagnostic.Result)
				default:
					require.NotNil(test, failure)
					require.NotEqual(test, "ready", diagnostic.Result)
				}
			})
		})
	}
}

func TestBackgroundWindowTimeoutPrecedesAPIKeyConcurrency(test *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
		test.Run(path, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			synctest.Test(test, func(test *testing.T) {
				remaining := int64(1000)
				fingerprint := promptSessionTestFingerprint(test.Name())
				meta := newAPIPolicyMeta{
					RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
					RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: fingerprint,
					ThreadSource: "memory_consolidation", RequestKind: "memory", PassiveFeature: newAPIPassiveFeatureRelatedInternal, RootAccountWaitMillis: &remaining,
				}
				body := []byte(`{"model":"gpt-5.6-sol","input":"background","messages":[{"role":"user","content":"background"}],"max_tokens":16}`)
				request, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, path, body, meta)
				request.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 101, Limits: database.APIKeyLimits{MaxConcurrency: 1}})
				account := &auth.Account{DBID: 17, AccessToken: "root", Status: auth.StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 2}
				handler.store.AddAccount(account)
				key := sessionAffinityKey("newapi-root-session:"+fingerprint, 101)
				handler.store.BindSessionAffinity(key, account, "")
				require.True(test, handler.store.RemoveAccountSession(account.ID(), key))
				for range 2 {
					release, _, acquired := handler.apiKeyConcurrencyLimiter().acquire(101, 2)
					require.True(test, acquired)
					defer release()
				}
				endpoint := map[string]func(*gin.Context){
					"/v1/responses": handler.Responses, "/v1/responses/compact": handler.ResponsesCompact,
					"/v1/chat/completions": handler.ChatCompletions, "/v1/messages": handler.Messages,
				}[path]
				endpoint(request)
				require.Equal(test, http.StatusBadRequest, recorder.Code)
				require.Contains(test, recorder.Body.String(), string(api.ErrCodeRootAccountWaitTimeout))
				require.Equal(test, "timeout", usageRequestDiagnosticState(request).BackgroundWindowWait.Result)
			})
		})
	}
}

func TestBackgroundWindowWaitRestoresPersistentOwnerAndLogsTimeout(test *testing.T) {
	handler, owner, _, key := failoverTestSetup(test, true)
	owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 2
	handler.store.UnbindSessionAffinity(key, owner.ID())
	request, _ := failoverTestRequest(test, handler)
	request.Set(contextAPIKeyID, int64(101))
	state := usageRequestDiagnosticState(request)
	state.StartedAt = time.Now().Add(-backgroundRootAccountWaitTimeout + 150*time.Millisecond)
	identity := requestSessionIdentity{stableIdentity: true, relatedToRoot: true, requiresRootAccount: true, affinityID: "failover-root"}
	finish := handler.beginServiceErrorAudit(request)
	failure := handler.waitForBackgroundRootAccount(request, identity)
	require.NotNil(test, failure)
	require.Equal(test, api.ErrCodeRootAccountWaitTimeout, failure.Code)
	require.Equal(test, "found", state.RootAccountLookup)
	require.Equal(test, "timeout", state.BackgroundWindowWait.Result)
	api.SendError(request, failure)
	finish()
	page := serviceErrorTestPage(test, handler)
	require.Len(test, page.Items, 1)
	require.NotNil(test, page.Items[0].BackgroundWindowWait)
	require.Equal(test, owner.ID(), page.Items[0].BackgroundWindowWait.AccountID)
	require.Equal(test, "timeout", page.Items[0].BackgroundWindowWait.Result)
}

func TestBackgroundWindowWaitPinsPersistentOwnerBeforeRegistering(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	request, _ := failoverTestRequest(test, handler)
	entry, found, err := handler.readSessionContinuity(test.Context(), hashRiskIdentity(key))
	require.NoError(test, err)
	require.True(test, found)
	_, _, err = handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{
		RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), Reason: "account_disabled", At: time.Now(),
	})
	require.NoError(test, err)
	failure := handler.waitForBackgroundActiveWindow(test.Context(), request, key, owner.ID(), &entry.Record)
	require.NotNil(test, failure)
	require.Equal(test, api.ErrCodeBackgroundRootUnavailable, failure.Code)
	require.Equal(test, "owner_changed", usageRequestDiagnosticState(request).BackgroundWindowWait.Result)
	require.Nil(test, backgroundAccountMatchFromContext(request.Request.Context()))
}
