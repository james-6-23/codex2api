package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func failoverTestSetup(test *testing.T, enabled bool) (*Handler, *auth.Account, *auth.Account, string) {
	test.Helper()
	previous := CurrentRuntimeSettings()
	test.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := DefaultRuntimeSettings()
	settings.CodexSessionFailoverEnabled = enabled
	ApplyRuntimeSettings(settings)
	handler := newWindowAuthorizationHandler(test)
	owner := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "owner-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
	target := &auth.Account{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "target-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
	handler.store.AddAccount(owner)
	handler.store.AddAccount(target)
	key := "failover-root::api-key:101"
	handler.store.BindSessionAffinity(key, owner, "")
	_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now()})
	require.NoError(test, err)
	return handler, owner, target, key
}

func failoverTestRequest(test *testing.T, handler *Handler) (*gin.Context, []byte) {
	test.Helper()
	request, body := continuityTestRequest(0, "turn")
	var err error
	body, err = sjson.SetBytes(body, "input", "Full plaintext conversation")
	require.NoError(test, err)
	request.Request.Header.Set("Authorization", "Bearer test-user-key")
	handler.bindCodexIdentityClaims(request)
	request.Set(ingressRequestBodyContextKey, body)
	return request, body
}

func TestSessionAccountFailoverDisabledAndUnavailableReasons(test *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, reason := range []string{"disabled", "paused", "quota", "auto_pause", "unauthorized", "healthy", "server_cooldown"} {
			test.Run(reason+map[bool]string{false: "_off", true: "_on"}[enabled], func(test *testing.T) {
				handler, owner, target, key := failoverTestSetup(test, enabled)
				switch reason {
				case "disabled":
					atomic.StoreInt32(&owner.Disabled, 1)
				case "paused":
					atomic.StoreInt32(&owner.DispatchPaused, 1)
				case "quota":
					owner.UsagePercent7d, owner.UsagePercent7dValid, owner.PlanType, owner.Reset7dAt = 100, true, "free", time.Now().Add(time.Hour)
				case "auto_pause":
					handler.store.SetGlobalAutoPauseThresholds(0, .8)
					owner.UsagePercent7d, owner.UsagePercent7dValid, owner.Reset7dAt = 85, true, time.Now().Add(time.Hour)
				case "unauthorized":
					owner.Status, owner.CooldownReason, owner.CooldownUtil = auth.StatusCooldown, "unauthorized", time.Now().Add(time.Hour)
				case "server_cooldown":
					owner.Status, owner.CooldownReason, owner.CooldownUtil = auth.StatusCooldown, "server_error", time.Now().Add(time.Minute)
				}
				request, body := failoverTestRequest(test, handler)
				require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
				expectSwitch := enabled && reason != "healthy" && reason != "server_cooldown"
				require.Equal(test, expectSwitch, handled)
				if expectSwitch {
					require.Same(test, target, selected)
					handler.store.Release(selected)
					require.Equal(test, "switched", usageRequestDiagnosticState(request).Continuity.AccountFailover.Result)
				} else {
					require.Nil(test, selected)
				}
				record, _, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
				require.NoError(test, err)
				if expectSwitch {
					require.Equal(test, target.ID(), record.AccountID)
				} else {
					require.Equal(test, owner.ID(), record.AccountID)
				}
			})
		}
	}
}

func TestSessionAccountFailoverOpaqueRequestsAndFiltering(test *testing.T) {
	for _, scenario := range []string{"previous", "encrypted", "turn_state", "tool_output", "filter", "same_account", "blacklisted", "expired_grant"} {
		test.Run(scenario, func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, true)
			atomic.StoreInt32(&owner.Disabled, 1)
			request, body := failoverTestRequest(test, handler)
			var filter auth.AccountFilter
			switch scenario {
			case "previous":
				body, _ = sjson.SetBytes(body, "previous_response_id", "old-response")
			case "encrypted":
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","encrypted_content":"old-encrypted","id":"opaque"}]`))
			case "turn_state":
				request.Request.Header.Set("X-Codex-Turn-State", "old-state")
			case "tool_output":
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"function_call_output","call_id":"old-call","output":"result"}]`))
			case "filter":
				filter = func(*auth.Account) bool { return false }
			case "same_account":
				target.AccountID = owner.AccountID
			case "blacklisted":
				operation := database.SessionErrorIdentity{Key: sessionOperationKey("test", "test", "user", continuityTestThread), UserID: "user", SessionID: continuityTestThread}
				request.Set(sessionOperationsContextKey, operation)
				require.True(test, handler.db.EnqueueSessionError(database.SessionErrorEvent{Identity: operation, CreatedAt: time.Now(), Code: "server_is_overloaded", Message: "overloaded", AccountID: owner.ID()}))
				require.Eventually(test, func() bool {
					rows, err := handler.db.ListSessionErrors(context.Background(), database.SessionErrorQuery{Limit: 10})
					return err == nil && len(rows.Items) > 0
				}, 3*time.Second, 10*time.Millisecond)
				require.NoError(test, handler.db.SetSessionBlacklist(context.Background(), []string{operation.Key}, true))
			case "expired_grant":
				grant := signedWindowGrant{Platform: "test", UserID: "user", Grant: database.UserWindowGrant{ID: "grant", Root: "root", OwnerAccountID: owner.ID(), OwnerKey: key, Confirmed: true, ExpiresAt: time.Now().Add(-time.Minute)}}
				request.Set(windowGrantContextKey, &grant)
				require.NoError(test, handler.db.UpdateUserWindowAdmissions(context.Background(), cache.PromptSessionLimitSubject(grant.Platform, grant.UserID), func(state *database.UserWindowAdmissionState) error {
					state.Windows[grant.Grant.Root] = &grant.Grant
					return nil
				}))
			}
			prepareError := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
			if prepareError == nil {
				selected, _, _ := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, filter, auth.DispatchPolicyStandard)
				if scenario == "previous" || scenario == "turn_state" {
					require.Same(test, target, selected)
					handler.store.Release(selected)
					return
				}
				require.Nil(test, selected)
			}
			record, _, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.Equal(test, owner.ID(), record.AccountID)
		})
	}
}

func TestSessionAccountFailoverDispatchAndRestore(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		session := request.Header.Get("Session-Id")
		require.NotEqual(test, continuityTestThread, session)
		require.Equal(test, session, request.Header.Get("Thread-Id"))
		require.Equal(test, session, gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata.session_id").String())
		require.Equal(test, target.AccountID, request.Header.Get("Chatgpt-Account-Id"))
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"new-response"}`)
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "failover-test"})
	atomic.StoreInt32(&owner.Disabled, 1)
	request, body := failoverTestRequest(test, handler)
	require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	selected, _, _ := handler.nextRetryAccountForSessionWithDispatchGuard(request.Request.Context(), key, 0, newRetryAccountExclusions(), nil, auth.DispatchPolicyStandard)
	require.Same(test, target, selected)
	defer handler.store.Release(selected)
	require.Nil(test, handler.commitSessionContinuity(request, selected))
	response, err := ExecuteRequest(request.Request.Context(), selected, body, "cache", "", "test-user-key", nil, request.Request.Header, false)
	require.NoError(test, err)
	require.NoError(test, response.Body.Close())
	settings := CurrentRuntimeSettings()
	settings.CodexSessionFailoverEnabled = false
	ApplyRuntimeSettings(settings)
	handler.continuityRecords = nil
	handler.store.UnbindSessionAffinity(key, target.ID())
	handler.store.BindSessionAffinity(key, owner, "")
	restored, raw := failoverTestRequest(test, handler)
	require.Nil(test, handler.configureSessionModelAffinity(restored, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, raw))
	require.Equal(test, target.ID(), selectionTraceForRequest(restored).PinnedAccount())
	require.Equal(test, "restored", usageRequestDiagnosticState(restored).Continuity.AccountFailover.Result)
	raw, _ = sjson.SetBytes(raw, "previous_response_id", "old-response")
	record, _, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
	require.NoError(test, err)
	require.Nil(test, handler.validateMigratedSessionContext(restored, raw, record))
	cleaned, _, err := PrepareSessionRestartOutbound(restored.Request.Context(), target, raw, restored.Request.Header)
	require.NoError(test, err)
	require.False(test, gjson.GetBytes(cleaned, "previous_response_id").Exists())
}

func TestSessionAccountFailoverHTTPIngress(test *testing.T) {
	runSessionAccountFailoverIngress(test, false)
}

func TestSessionAccountFailoverCompactIngress(test *testing.T) {
	runSessionAccountFailoverIngress(test, true)
}

func runSessionAccountFailoverIngress(test *testing.T, compact bool) {
	handler, owner, target, _ := failoverTestSetup(test, true)
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	seen := make(chan http.Header, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(request.Body)
		headers := request.Header.Clone()
		if request.Header.Get("Chatgpt-Account-Id") == target.AccountID {
			require.NotContains(test, string(body), "old-restart-")
			require.Contains(test, string(body), "current plaintext")
			require.Empty(test, request.Header.Get("X-Codex-Turn-State"))
		}
		headers.Set("test-body-session", gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata.session_id").String())
		seen <- headers
		if strings.HasSuffix(request.URL.Path, "/responses/compact") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"id":"compact-response","object":"response.compaction","output":[{"type":"compaction","id":"new-compaction","encrypted_content":"new-test-encrypted"}]}`)
			return
		}
		stickyFailureSuccess(writer)
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "failover-ingress"})
	atomic.StoreInt32(&target.Disabled, 1)
	var lastSession string
	for _, expected := range []*auth.Account{owner, target} {
		if expected == target {
			atomic.StoreInt32(&owner.Disabled, 1)
			atomic.StoreInt32(&target.Disabled, 0)
		}
		_, body := failoverTestRequest(test, handler)
		body, _ = sjson.SetBytes(body, "stream", true)
		if expected == target {
			body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","encrypted_content":"gAAAAold-restart-reasoning"},{"type":"compaction","encrypted_content":"gAAAAold-restart-compaction"},{"role":"user","content":[{"type":"input_file","file_id":"old-restart-file"},{"type":"input_text","text":"current plaintext"}]}]`))
			body, _ = sjson.SetBytes(body, "previous_response_id", "old-restart-response")
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "old-restart-turn-state")
		}
		path := "/v1/responses"
		if expected == target && compact {
			path = "/v1/responses/compact"
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.request_kind", "compaction")
			body, _ = sjson.SetBytes(body, "stream", false)
		}
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Request.Header.Set("Authorization", "Bearer test-user-key")
		request.Request.Header.Set("Content-Type", "application/json")
		ctx, cancel := context.WithTimeout(request.Request.Context(), 5*time.Second)
		request.Request = request.Request.WithContext(ctx)
		if expected == target && compact {
			handler.ResponsesCompact(request)
		} else {
			handler.Responses(request)
		}
		cancel()
		require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
		if expected == target && compact {
			require.Contains(test, recorder.Body.String(), "new-compaction")
		} else {
			require.Contains(test, recorder.Body.String(), "response.completed")
		}
		require.NotEmpty(test, seen)
		headers := <-seen
		require.Equal(test, expected.AccountID, headers.Get("Chatgpt-Account-Id"))
		require.Equal(test, headers.Get("Session-Id"), headers.Get("test-body-session"))
		require.NotEqual(test, lastSession, headers.Get("Session-Id"))
		lastSession = headers.Get("Session-Id")
	}
}

func TestSessionAccountFailoverRestoresRelatedOwnerAndBlocksOldReferences(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	atomic.StoreInt32(&owner.Disabled, 1)
	request, body := failoverTestRequest(test, handler)
	require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
	require.True(test, handled)
	require.Same(test, target, selected)
	handler.store.Release(selected)
	for _, opaque := range []bool{false, true} {
		related, raw := failoverTestRequest(test, handler)
		usageRequestDiagnosticState(related).Resolved.ThreadSource = ""
		if opaque {
			raw, _ = sjson.SetRawBytes(raw, "input", []byte(`[{"type":"item_reference","id":"old-item"}]`))
		}
		identity := requestSessionIdentity{stableIdentity: true, relatedToRoot: true, requiresRootAccount: !opaque, affinityID: "failover-root"}
		relatedKey := auth.RelatedSessionAffinityKey(key)
		beginDispatchSelection(related)
		err := handler.configureSessionModelAffinity(related, identity, relatedKey, "gpt-5.6-sol", "gpt-5.6-sol", false, raw)
		if opaque {
			require.NotNil(test, err)
			continue
		}
		require.Nil(test, err)
		require.Zero(test, selectionTraceForRequest(related).PinnedAccount())
		filter := handler.applyPassiveInternalModelRouting(related, "gpt-5.6-sol", identity, relatedKey, false, nil)
		require.True(test, filter(target))
		require.False(test, filter(owner))
	}
}

func TestSessionAccountFailoverProtectedBackgroundRequiresLiveRoot(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	target.SessionCapacityEnabled, target.SessionCapacityMax, target.SessionCapacityReserved = true, 2, 1
	atomic.StoreInt32(&owner.Disabled, 1)
	request, body := failoverTestRequest(test, handler)
	require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	selected, _, _ := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
	require.Same(test, target, selected)
	handler.store.Release(selected)
	handler.store.SetMaxConcurrency(1)
	held := handler.store.TakePreferredAccountWithDispatch(target.ID(), 0, nil, nil, auth.DispatchPolicyStandard)
	require.Same(test, target, held)
	identity := requestSessionIdentity{stableIdentity: true, relatedToRoot: true, requiresRootAccount: true, affinityID: "failover-root"}
	relatedKey := auth.ProtectedRelatedSessionAffinityKey(key)
	related, raw := failoverTestRequest(test, handler)
	require.Nil(test, handler.configureSessionModelAffinity(related, identity, relatedKey, "gpt-5.6-sol", "gpt-5.6-sol", false, raw))
	filter := handler.applyPassiveInternalModelRouting(related, "gpt-5.6-sol", identity, relatedKey, false, nil)
	background, _, _ := handler.store.NextForSessionWithDispatchGuard(relatedKey, 0, nil, filter, auth.DispatchPolicyStandard, selectionTraceForRequest(related))
	require.Same(test, target, background)
	handler.store.Release(background)
	handler.store.Release(held)
	handler.store.RemoveAccountSession(target.ID(), key)
	related, raw = failoverTestRequest(test, handler)
	require.NotNil(test, handler.configureSessionModelAffinity(related, identity, relatedKey, "gpt-5.6-sol", "gpt-5.6-sol", false, raw))
}

func TestSessionAccountFailoverGrantWithoutTicketAndExpandedCapacity(test *testing.T) {
	for _, expanded := range []bool{false, true} {
		test.Run(map[bool]string{false: "omitted_ordinary_ticket", true: "incompatible_expanded_target"}[expanded], func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, true)
			atomic.StoreInt32(&owner.Disabled, 1)
			_, body := failoverTestRequest(test, handler)
			fingerprint := promptSessionTestFingerprint("failover-grant")
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: fingerprint, ThreadSource: "user", RequestKind: "turn"}
			request, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
			handler.primeNewAPIPolicyContext(request, body)
			handler.bindCodexIdentityClaims(request)
			request.Request.Header.Set("Authorization", "Bearer test-user-key")
			usageRequestDiagnosticState(request).Resolved = &usageRequestResolution{ThreadSource: "user", RequestKind: "turn", Stable: true}
			subject := cache.PromptSessionLimitSubject("test-platform", "42")
			grant := database.UserWindowGrant{ID: "ordinary-grant", Root: hashRiskIdentity(fingerprint), OwnerAccountID: owner.ID(), OwnerKey: key, Confirmed: true, Expanded: expanded, Multiplier: 1, ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now()}
			if expanded {
				grant.Multiplier = 1.5
				grant.ExtraLimit = 2
				request.Set(windowGrantContextKey, &signedWindowGrant{Platform: "test-platform", UserID: "42", Grant: grant})
			}
			require.NoError(test, handler.db.UpdateUserWindowAdmissions(context.Background(), subject, func(state *database.UserWindowAdmissionState) error { state.Windows[grant.Root] = &grant; return nil }))
			beginDispatchSelection(request)
			require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(test, handled)
			state, err := handler.db.ReadUserWindowAdmissions(context.Background(), subject)
			require.NoError(test, err)
			if expanded {
				require.Nil(test, selected)
				require.Equal(test, owner.ID(), state.Windows[grant.Root].OwnerAccountID)
			} else {
				require.Same(test, target, selected)
				handler.store.Release(selected)
				require.Equal(test, target.ID(), state.Windows[grant.Root].OwnerAccountID)
				require.True(test, grant.ExpiresAt.Equal(state.Windows[grant.Root].ExpiresAt))
			}
		})
	}
}

func TestSessionAccountFailoverFreshWSFrameAndSpark(test *testing.T) {
	handler, owner, _, key := failoverTestSetup(test, true)
	request, body := failoverTestRequest(test, handler)
	request.Request.Header.Set("Connection", "Upgrade")
	request.Request.Header.Set("Upgrade", "websocket")
	request.Request.Header.Set("X-Codex-Turn-State", "old-upgrade-state")
	require.Empty(test, sessionFailoverContextBlock(sessionFailoverRequestHeaders(request), body))
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "frame-state")
	require.Equal(test, "connection_turn_state", sessionFailoverContextBlock(sessionFailoverRequestHeaders(request), body))
	request, body = failoverTestRequest(test, handler)
	owner.Status, owner.CooldownReason, owner.CooldownUtil = auth.StatusCooldown, "rate_limited", time.Now().Add(time.Hour)
	require.True(test, owner.SparkDispatchEligible())
	require.Nil(test, handler.prepareSessionContinuity(request, requestSessionIdentity{stableIdentity: true}, key, body))
	pending, err := handler.prepareSessionAccountFailover(request, key, body, auth.DispatchPolicySpark)
	require.Nil(test, err)
	require.False(test, pending)
	owner.UsagePercentSpark, owner.UsagePercentSparkValid, owner.ResetSparkAt = 100, true, time.Now().Add(time.Hour)
	require.Equal(test, "account_spark_usage_exhausted", sessionAccountFailoverReason(owner, auth.DispatchPolicySpark))
}

func TestSessionAccountFailoverFlatWSMetadataNeverInheritsUpgradeSnapshot(test *testing.T) {
	upgraded := http.Header{}
	upgraded.Set("User-Agent", "test-client")
	upgraded.Set("Session-Id", accountIdentitySampleRoot)
	upgraded.Set("X-Codex-Turn-State", "old-state")
	upgraded.Set("X-Codex-Context-Window-Id", accountIdentitySampleContext)
	upgraded.Set("X-OpenAI-Memgen-Request", "true")
	upgraded.Set("X-Codex-Parent-Thread-Id", accountIdentitySampleRoot)
	upgraded.Set("X-Codex-Turn-Metadata", `{"session_id":"old-root","window_id":"old-root:0"}`)
	body := []byte(`{"input":"full context","client_metadata":{"session_id":"` + continuityTestThread + `","thread_id":"` + continuityTestThread + `","x-codex-window-id":"` + continuityTestThread + `:1"}}`)
	current := codexWebsocketCurrentFrameHeaders(upgraded, body)
	require.Equal(test, continuityTestThread, current.Get("Session-Id"))
	require.Equal(test, continuityTestThread+":1", current.Get("X-Codex-Window-Id"))
	require.Equal(test, "test-client", current.Get("User-Agent"))
	for _, name := range []string{"X-Codex-Turn-Metadata", "X-Codex-Turn-State", "X-OpenAI-Memgen-Request", "X-Codex-Parent-Thread-Id"} {
		require.Empty(test, current.Get(name), name)
	}
	require.Equal(test, "old-state", upgraded.Get("X-Codex-Turn-State"))
	projected, _ := ApplyCodexAnalyticsMetadata(body, current)
	metadata := diagnosticMetadataObject(gjson.GetBytes(projected, "client_metadata.x-codex-turn-metadata"))
	require.False(test, metadata.Get("context_window_id").Exists())
	require.Equal(test, continuityTestThread+":1", metadata.Get("window_id").String())
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "current-state")
	require.Equal(test, "current-state", codexWebsocketCurrentFrameHeaders(upgraded, body).Get("X-Codex-Turn-State"))
}
