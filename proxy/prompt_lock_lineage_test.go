package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const promptLockForkSession = "01a09592-7b94-7242-829c-28e2be8a0d58"
const promptLockGrandchildSession = "01a095b5-6879-7271-b621-10c71cbef64f"

func signedPromptLockContext(test *testing.T, method, path string, body []byte, meta newAPIPolicyMeta) (*gin.Context, *httptest.ResponseRecorder) {
	test.Helper()
	recorder := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(recorder)
	request.Request = httptest.NewRequest(method, path, strings.NewReader(string(body)))
	request.Set(contextAPIKeyID, int64(101))
	meta.PlatformID, meta.Profile, meta.Mode = "test-platform", "balanced", "enforce"
	meta.Provider, meta.Protocol = "openai", "responses"
	meta.SessionFingerprint = promptSessionTestFingerprint(meta.RootSessionID)
	setSignedNewAPIRequestHeaders(test, request.Request, body, uuid.NewString(), newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "test-platform", "integration-secret", meta.SessionFingerprint)
	addSignedNewAPIPolicyMeta(test, request, meta, true)
	return request, recorder
}

func promptLockForkBody(session, parent string) []byte {
	_, body := continuityTestRequest(0, "turn")
	body = []byte(strings.ReplaceAll(string(body), continuityTestThread, session))
	if parent != "" {
		body = []byte(strings.Replace(string(body), `"thread_source":"user"`, `"forked_from_thread_id":"`+parent+`","thread_source":"user"`, 1))
	}
	return body
}

func promptLockForkMeta(session, parent string) newAPIPolicyMeta {
	meta := sessionOperationsTestMeta(session)
	meta.ChannelID = 22
	if parent != "" {
		meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
		meta.ForkedFromSessionFingerprint = newAPIRootSessionFingerprint("test-platform", "42", parent)
	}
	return meta
}

func seedLegacyPromptParentLock(test *testing.T, handler *Handler) string {
	test.Helper()
	config := handler.store.GetPromptFilterConfig()
	config.Advanced.Enforcement.ConversationLockEnabled = true
	config.Advanced.Enforcement.ConversationLockTTLHours = 24
	config.Advanced.Enforcement.UserCyberCooldownMinutes = 30
	handler.store.SetPromptFilterConfig(config)
	meta := promptLockForkMeta(continuityTestThread, "")
	mac := hmac.New(sha256.New, []byte("integration-secret"))
	_, _ = mac.Write([]byte(strings.Join([]string{"policy-session-v1", "test-platform", "42", continuityTestThread}, "\n")))
	meta.SessionFingerprint = hex.EncodeToString(mac.Sum(nil))[:32]
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	identity, valid := verifiedPromptConversationLockIdentity(request, verifiedNewAPIPolicyContext{
		Platform: "test-platform", Identity: newAPIIdentity{UserID: "42"}, MetaVerified: true, Meta: meta,
	})
	require.True(test, valid)
	_, _, err := handler.db.LockPromptConversation(test.Context(), database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind, Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		DecisionID: "legacy-parent-cyb", ReasonCode: newAPIUpstreamCyberPolicyReasonCode, LockedAt: time.Now().Add(-2 * time.Hour),
	})
	require.NoError(test, err)
	return identity.LockKey
}

func TestPromptLockLineageLegacyCYBForkGrandchildAndUnlock(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	lockKey := seedLegacyPromptParentLock(test, handler)
	for _, scenario := range []struct{ session, parent string }{
		{promptLockForkSession, continuityTestThread},
		{promptLockForkSession, ""},
		{promptLockGrandchildSession, promptLockForkSession},
	} {
		body := promptLockForkBody(scenario.session, scenario.parent)
		original := string(body)
		request, recorder := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(scenario.session, scenario.parent))
		setIngressRequestBodyIfAbsent(request, body)
		config := handler.promptFilterConfigForRequest(request)
		policy, verified := handler.verifyNewAPIPolicyContext(request, config.Advanced.NewAPI, body)
		require.True(test, verified)
		require.True(test, policy.MetaVerified)
		require.True(test, config.Advanced.Enforcement.ConversationLockEnabled)
		require.True(test, handler.rejectLockedPromptConversation(request, config, body, body, "/v1/responses", "gpt-5.6-sol"), "session=%s parent=%s root=%+v", scenario.session, scenario.parent, handler.resolveRequestRootSessionIdentityForContext(request, body))
		require.Equal(test, http.StatusBadRequest, recorder.Code)
		require.Equal(test, promptConversationLockedReasonCode, gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(), recorder.Body.String())
		require.False(test, policyDecisionMetadataFromHeaders(recorder.Header()).StrikeEligible)
		require.Equal(test, original, string(body))
	}
	lock, err := handler.db.GetActivePromptConversationLock(test.Context(), lockKey)
	require.NoError(test, err)
	require.EqualValues(test, 1, lock.TriggerCount)
	body := promptLockForkBody(accountIdentitySampleRoot, "")
	request, _ := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(accountIdentitySampleRoot, ""))
	setIngressRequestBodyIfAbsent(request, body)
	require.False(test, handler.rejectLockedPromptConversation(request, handler.promptFilterConfigForRequest(request), body, body, "/v1/responses", "gpt-5.6-sol"))
	_, err = handler.db.UnlockPromptConversation(test.Context(), lockKey, "test unlock")
	require.NoError(test, err)
	body = promptLockForkBody(promptLockGrandchildSession, "")
	request, _ = signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(promptLockGrandchildSession, ""))
	setIngressRequestBodyIfAbsent(request, body)
	require.False(test, handler.rejectLockedPromptConversation(request, handler.promptFilterConfigForRequest(request), body, body, "/v1/responses", "gpt-5.6-sol"))
}

func TestPromptLockLineageManualForkWithoutRepeatedParentAndUnlock(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	parentHash := hashRiskIdentity(newAPIRootSessionFingerprint("test-platform", "42", continuityTestThread))
	_, err := handler.db.LockPromptUserWindow(test.Context(), "test-platform", "42", parentHash, time.Now(), time.Hour)
	require.NoError(test, err)
	for _, parent := range []string{continuityTestThread, ""} {
		body := promptLockForkBody(promptLockForkSession, parent)
		request, recorder := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(promptLockForkSession, parent))
		setIngressRequestBodyIfAbsent(request, body)
		require.True(test, handler.rejectLockedPromptConversation(request, handler.promptFilterConfigForRequest(request), body, body, "/v1/responses", "gpt-5.6-sol"), "parent=%s root=%+v", parent, handler.resolveRequestRootSessionIdentityForContext(request, body))
		require.Equal(test, string(promptManualWindowLockedCode), gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(), recorder.Body.String())
		require.Empty(test, recorder.Header().Get("X-Codex2API-Policy-Decision-ID"))
	}
	require.NoError(test, handler.db.UnlockPromptUserWindow(test.Context(), "test-platform", "42", parentHash, time.Now()))
	body := promptLockForkBody(promptLockForkSession, "")
	request, _ := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(promptLockForkSession, ""))
	require.Nil(test, handler.promptManualWindowLockError(request, handler.promptFilterConfigForRequest(request), body, body))
}

func TestPromptLockLineageUnsignedForkIsAPIKeyScoped(test *testing.T) {
	handler, db := newPromptConversationLockTestHandler(test)
	identity, valid := promptConversationLockFallbackIdentityForSession(7, continuityTestThread)
	require.True(test, valid)
	_, _, err := db.LockPromptConversation(test.Context(), database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind, Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		DecisionID: "unsigned-parent", ReasonCode: newAPIUpstreamCyberPolicyReasonCode, LockedAt: time.Now(),
	})
	require.NoError(test, err)
	for _, keyID := range []int64{7, 8} {
		body := promptLockForkBody(promptLockForkSession, continuityTestThread)
		request := promptSessionLimitTestContext(promptLockForkSession)
		request.Set(contextAPIKeyID, keyID)
		setIngressRequestBodyIfAbsent(request, body)
		_, locked, failure := handler.requestPromptConversationLock(request, handler.promptFilterConfigForRequest(request), body, body, "/v1/responses", "gpt-5.6-sol")
		require.Nil(test, failure)
		require.Equal(test, keyID == 7, locked)
	}
}

func TestPromptLockLineageForkWebSocketStopsBeforeDispatch(test *testing.T) {
	for _, kind := range []string{"cyber", "window"} {
		test.Run(kind, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			code := promptConversationLockedReasonCode
			if kind == "cyber" {
				seedLegacyPromptParentLock(test, handler)
			} else {
				code = string(promptManualWindowLockedCode)
				_, err := handler.db.LockPromptUserWindow(test.Context(), "test-platform", "42", hashRiskIdentity(newAPIRootSessionFingerprint("test-platform", "42", continuityTestThread)), time.Now(), time.Hour)
				require.NoError(test, err)
			}
			router := gin.New()
			finished := make(chan struct{})
			router.GET("/v1/responses", func(request *gin.Context) {
				defer close(finished)
				request.Set(contextAPIKeyID, int64(101))
				handler.ResponsesWebSocket(request)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			request, _ := signedPromptLockContext(test, http.MethodGet, "/v1/responses", nil, promptLockForkMeta(promptLockForkSession, continuityTestThread))
			connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", request.Request.Header)
			require.NoError(test, err)
			defer connection.Close()
			body := promptLockForkBody(promptLockForkSession, continuityTestThread)
			body = []byte(strings.Replace(string(body), `"model":`, `"type":"response.create","input":"hello","model":`, 1))
			require.NoError(test, connection.WriteMessage(websocket.TextMessage, body))
			require.NoError(test, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
			_, response, err := connection.ReadMessage()
			require.NoError(test, err)
			require.Equal(test, code, gjson.GetBytes(response, "error.code").String(), string(response))
			require.NoError(test, connection.Close())
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				test.Fatal("websocket handler did not stop")
			}
		})
	}
}

func TestPromptLockLineageMissingLeafFingerprintAndDatabaseFailure(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	seedLegacyPromptParentLock(test, handler)
	body := promptLockForkBody(promptLockForkSession, continuityTestThread)
	request, _ := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(promptLockForkSession, continuityTestThread))
	config := handler.promptFilterConfigForRequest(request)
	policy, verified := handler.verifyNewAPIPolicyContext(request, config.Advanced.NewAPI, body)
	require.True(test, verified)
	policy.Meta.SessionFingerprint = ""
	request.Set(newAPIPolicyMetaContextKey, policy)
	_, locked, failure := handler.requestPromptConversationLock(request, config, body, body, "/v1/responses", "gpt-5.6-sol")
	require.Nil(test, failure)
	require.True(test, locked)
	canceled, cancel := context.WithCancel(request.Request.Context())
	cancel()
	request.Request = request.Request.WithContext(canceled)
	_, locked, failure = handler.requestPromptConversationLock(request, config, body, body, "/v1/responses", "gpt-5.6-sol")
	require.False(test, locked)
	require.NotNil(test, failure)
	require.Equal(test, api.ErrCodeServiceUnavailable, failure.Code)
}
