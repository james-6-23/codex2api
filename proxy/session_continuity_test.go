package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const continuityTestThread = "01a03bb0-9da5-7772-a16a-f38258dd30c4"

func continuityTestRequest(number uint64, kind string) (*gin.Context, []byte) {
	body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-turn-metadata":{"thread_id":"%s","session_id":"%s","window_id":"%s:%d","thread_source":"user","request_kind":"%s"}}}`, continuityTestThread, continuityTestThread, continuityTestThread, number, kind))
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	beginDispatchSelection(request)
	usageRequestDiagnosticState(request).Resolved = &usageRequestResolution{ThreadSource: "user", RequestKind: kind, Stable: true}
	return request, body
}

func TestSessionContinuityObservationAndEnforcement(test *testing.T) {
	for _, mode := range []string{"off", "observe", "enforce"} {
		test.Run(mode, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			config := handler.store.GetPromptFilterConfig()
			config.Advanced.Risk.SessionContinuityMode = mode
			handler.store.SetPromptFilterConfig(config)
			request, body := continuityTestRequest(71, "turn")
			apiErr := handler.prepareSessionContinuity(request, requestSessionIdentity{stableIdentity: true}, "unbound::api-key:101", body)
			if mode == "enforce" {
				require.NotNil(test, apiErr)
				require.Equal(test, "codex_session_continuity_unbound_nonzero", string(apiErr.Code))
			} else {
				require.Nil(test, apiErr)
			}
			diagnostic := usageRequestDiagnosticState(request).Continuity
			require.Equal(test, mode != "off", diagnostic.WouldBlock)
			require.Equal(test, uint64(71), *diagnostic.Current)
		})
	}
}

func TestSessionContinuityCompactionAndRestartKeepOwner(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	account := &auth.Account{DBID: 1695, AccessToken: "test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
	handler.store.AddAccount(account)
	key := "root::api-key:101"
	handler.store.BindSessionAffinity(key, account, "")
	identity := requestSessionIdentity{stableIdentity: true}
	for index, step := range []struct {
		number uint64
		kind   string
		result string
	}{{71, "turn", "baseline"}, {71, "compaction", "same_window"}, {72, "turn", "window_advanced"}} {
		request, body := continuityTestRequest(step.number, step.kind)
		identity.relatedToRoot, identity.ownsRootBinding = step.kind == "compaction", step.kind == "compaction"
		require.Nil(test, handler.prepareSessionContinuity(request, identity, key, body))
		require.Equal(test, step.result, usageRequestDiagnosticState(request).Continuity.Result)
		require.False(test, usageRequestDiagnosticState(request).Continuity.WouldBlock)
		require.Equal(test, account.ID(), selectionTraceForRequest(request).PinnedAccount())
		require.Nil(test, handler.commitSessionContinuity(request, account))
		if index == 1 {
			record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.True(test, found)
			require.Equal(test, uint64(71), record.Number)
		}
	}
	handler.continuityRecords = nil
	handler.store.UnbindSessionAffinity(key, account.ID())
	request, body := continuityTestRequest(72, "turn")
	require.Nil(test, handler.prepareSessionContinuity(request, identity, key, body))
	require.Equal(test, "persistent_binding", usageRequestDiagnosticState(request).Continuity.OwnerSource)
	require.Equal(test, account.ID(), selectionTraceForRequest(request).PinnedAccount())
	other := &auth.Account{DBID: 1696}
	require.NotNil(test, handler.commitSessionContinuity(request, other))
}

func TestSessionContinuityModelSwitchAfterIdleKeepsPersistentOwner(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		idle      time.Duration
		mode      string
		supported bool
	}{
		{name: "two_hours_unsupported_model", idle: 2 * time.Hour, mode: "observe"},
		{name: "one_year_supported_model_with_checks_off", idle: 365 * 24 * time.Hour, mode: "off", supported: true},
		{name: "one_year_unsupported_model", idle: 365 * 24 * time.Hour, mode: "enforce"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			config := handler.store.GetPromptFilterConfig()
			config.Advanced.Risk.SessionContinuityMode = scenario.mode
			handler.store.SetPromptFilterConfig(config)
			owner := &auth.Account{DBID: 1589, AccessToken: "owner-test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
			if scenario.supported {
				owner.Models = append(owner.Models, "gpt-5.6-terra")
			}
			other := &auth.Account{DBID: 1695, AccessToken: "other-test", Status: auth.StatusReady, Models: []string{"gpt-5.6-terra"}}
			handler.store.AddAccounts([]*auth.Account{owner, other})
			key := sessionAffinityKey("newapi-root-session:"+promptSessionTestFingerprint(test.Name()), 101)
			stored := database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now().Add(-scenario.idle)}
			_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), stored)
			require.NoError(test, err)
			_, live := handler.store.LiveSessionAccountID(key, time.Now())
			require.False(test, live)

			request, body := continuityTestRequest(0, "turn")
			body = bytes.Replace(body, []byte("gpt-5.6-sol"), []byte("gpt-5.6-terra"), 1)
			apiErr := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-terra", "gpt-5.6-terra", false, body)
			if scenario.supported {
				require.Nil(test, apiErr)
			} else {
				require.NotNil(test, apiErr)
				require.Equal(test, api.ErrCodeSessionModelUnavailable, apiErr.Code)
				require.Equal(test, http.StatusBadRequest, api.HTTPStatusCode(apiErr.Code))
				require.Equal(test, sessionModelUnavailableMessage, apiErr.Message)
			}
			trace := selectionTraceForRequest(request)
			require.Equal(test, owner.ID(), trace.PinnedAccount())
			require.Equal(test, "persistent_binding", usageRequestDiagnosticState(request).Continuity.OwnerSource)
			selected, _, _ := handler.store.NextForSessionWithDispatchGuard(key, 101, nil, nil, auth.DispatchPolicyStandard, trace)
			if scenario.supported {
				require.Same(test, owner, selected)
				handler.store.Release(selected)
			} else {
				require.Nil(test, selected)
			}
			require.Nil(test, handler.store.TakePreferredAccountWithDispatch(other.ID(), 101, nil, nil, auth.DispatchPolicyStandard, trace))
			record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.True(test, found)
			require.Equal(test, owner.ID(), record.AccountID)
			require.Equal(test, uint64(0), record.Number)
		})
	}
}

func TestSessionContinuityWindowRulesAndMetadata(test *testing.T) {
	previous := database.SessionContinuityRecord{AccountID: 1695, ThreadID: continuityTestThread, Number: 71, NumberKnown: true}
	for _, item := range []struct {
		number  uint64
		reason  string
		blocked bool
	}{{71, "same_window", false}, {72, "window_advanced", false}, {74, "window_gap", true}, {70, "window_regressed", false}} {
		reason, blocked := evaluateSessionContinuity(previous, true, continuityTestThread, item.number, 1695)
		require.Equal(test, item.reason, reason)
		require.Equal(test, item.blocked, blocked)
	}
	headers := http.Header{"Thread-Id": {"01a084f2-7593-7372-a60c-f648ce2eb337"}, "X-Codex-Window-Id": {"01a084f2-7593-7372-a60c-f648ce2eb337:1"}}
	_, body := continuityTestRequest(72, "turn")
	thread, number, known, invalid := parseContinuityWindow(headers, body, true)
	require.Equal(test, continuityTestThread, thread)
	require.Equal(test, uint64(72), number)
	require.True(test, known)
	require.Empty(test, invalid)
	_, _, known, invalid = parseContinuityWindow(nil, []byte(`{"client_metadata":{}}`), true)
	require.False(test, known)
	require.Equal(test, "window_missing", invalid)
	conflict := []byte(fmt.Sprintf(`{"client_metadata":{"x-codex-turn-metadata":{"thread_id":"%s","window_id":"%s:72","window_number":71}}}`, continuityTestThread, continuityTestThread))
	_, _, _, invalid = parseContinuityWindow(nil, conflict, true)
	require.Equal(test, "number_conflict", invalid)
}

func TestSessionContinuityPersistentCommitIsMonotonicAndAtomic(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	key := "monotonic"
	initial := database.SessionContinuityRecord{AccountID: 1695, ThreadID: continuityTestThread, Number: 72, NumberKnown: true, LastSeen: time.Now()}
	_, err := handler.db.CommitSessionContinuity(context.Background(), key, initial)
	require.NoError(test, err)
	older := initial
	older.Number, older.LastSeen = 71, initial.LastSeen.Add(-time.Second)
	committed, err := handler.db.CommitSessionContinuity(context.Background(), key, older)
	require.NoError(test, err)
	require.Equal(test, uint64(72), committed.Number)
	require.Equal(test, initial.LastSeen.UnixMilli(), committed.LastSeen.UnixMilli())
	other := initial
	other.AccountID = 1696
	_, err = handler.db.CommitSessionContinuity(context.Background(), key, other)
	require.ErrorIs(test, err, database.ErrSessionOwnerConflict)
}

func TestSessionContinuityMissingWindowPreservesOwnerWithoutInventingZero(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	account := &auth.Account{DBID: 1695, AccessToken: "test", Status: auth.StatusReady}
	handler.store.AddAccount(account)
	request, _ := continuityTestRequest(71, "turn")
	key := "missing-frame::api-key:101"
	identity := requestSessionIdentity{stableIdentity: true}
	require.Nil(test, handler.prepareSessionContinuity(request, identity, key, []byte(`{"client_metadata":{}}`)))
	require.Nil(test, handler.commitSessionContinuity(request, account))
	record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
	require.NoError(test, err)
	require.True(test, found)
	require.Equal(test, account.ID(), record.AccountID)
	require.False(test, record.NumberKnown)
	handler.continuityRecords = nil
	request, body := continuityTestRequest(71, "turn")
	require.Nil(test, handler.prepareSessionContinuity(request, identity, key, body))
	require.Equal(test, "baseline", usageRequestDiagnosticState(request).Continuity.Result)
	require.Equal(test, account.ID(), selectionTraceForRequest(request).PinnedAccount())
	require.Nil(test, handler.commitSessionContinuity(request, account))
	record, found, err = handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
	require.NoError(test, err)
	require.True(test, found)
	require.True(test, record.NumberKnown)
	require.Equal(test, uint64(71), record.Number)
}

func TestSessionContinuityEnforceStopsUnboundContextBeforeHTTPOrWebSocketDispatch(test *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "websocket"} {
		test.Run(endpoint, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			config := handler.store.GetPromptFilterConfig()
			config.Advanced.Risk.SessionContinuityMode = "enforce"
			handler.store.SetPromptFilterConfig(config)
			_, body := continuityTestRequest(11, "turn")
			body = []byte(strings.Replace(string(body), `"model":`, `"type":"response.create","input":"continue","messages":[{"role":"user","content":"continue"}],"model":`, 1))
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: newAPIRootSessionFingerprint("test-platform", "42", continuityTestThread), ThreadSource: "user", RequestKind: "turn"}
			if endpoint != "websocket" {
				request, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, endpoint, body, meta)
				map[string]func(*gin.Context){"/v1/responses": handler.Responses, "/v1/responses/compact": handler.ResponsesCompact, "/v1/chat/completions": handler.ChatCompletions}[endpoint](request)
				require.Equal(test, http.StatusBadRequest, recorder.Code, recorder.Body.String())
				expectedCode := "codex_session_continuity_unbound_nonzero"
				if endpoint == "/v1/responses/compact" {
					expectedCode = "codex_session_continuity_unbound_compaction"
				}
				require.Equal(test, expectedCode, gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
				return
			}
			router := gin.New()
			router.GET("/v1/responses", func(request *gin.Context) {
				request.Set(contextAPIKeyID, int64(101))
				handler.ResponsesWebSocket(request)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			request, _ := signedRootlessPassiveModelContext(test, http.MethodGet, "/v1/responses", nil, meta)
			request.Request.Header.Set(codexWindowIDHeader, continuityTestThread+":0")
			connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", request.Request.Header)
			require.NoError(test, err)
			defer connection.Close()
			require.NoError(test, connection.WriteMessage(websocket.TextMessage, body))
			require.NoError(test, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, response, err := connection.ReadMessage()
			require.NoError(test, err)
			require.Equal(test, "codex_session_continuity_unbound_nonzero", gjson.GetBytes(response, "error.code").String(), string(response))
		})
	}
}

func TestSessionContinuityCapacityGuidanceKeepsOriginalAccount(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	account := &auth.Account{DBID: 1695, AccessToken: "test", Status: auth.StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 2, SessionCapacityReserved: 1, SessionCapacityIdleTTLSeconds: 60}
	handler.store.AddAccount(account)
	require.True(test, handler.store.AdmitAccountSession(account, "other-window", time.Now()))
	request, _ := continuityTestRequest(71, "turn")
	trace := selectionTraceForRequest(request)
	trace.PinAccount(account.ID())
	apiErr := handler.pinnedSessionCapacityError(request, "original-root")
	require.NotNil(test, apiErr)
	require.Equal(test, "codex_sticky_expansion_required", string(apiErr.Code))
	require.Equal(test, "当前窗口绑定账号的普通会话容量已满，请在「窗口管理」开启扩容并确认此窗口的扩容费用后重试。", apiErr.Message)
	require.Equal(test, http.StatusBadRequest, api.HTTPStatusCode(apiErr.Code))
	trace.SetExpandedWindow(true)
	require.Nil(test, handler.pinnedSessionCapacityError(request, "original-root"))
	require.True(test, handler.store.AdmitAccountSession(account, "paid-other-window", time.Now(), trace))
	apiErr = handler.pinnedSessionCapacityError(request, "original-root")
	require.NotNil(test, apiErr)
	require.Equal(test, "codex_sticky_account_capacity_full", string(apiErr.Code))
	require.Equal(test, "当前窗口绑定账号的普通及扩容会话容量均已满，请新开对话或切换其他窗口使用。", apiErr.Message)
	require.Equal(test, http.StatusBadRequest, api.HTTPStatusCode(apiErr.Code))
	require.Equal(test, account.ID(), trace.PinnedAccount())
}
