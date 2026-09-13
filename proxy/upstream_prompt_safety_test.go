package proxy

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const promptSafetySampleError = `{"code":"invalid_prompt","type":"invalid_request_error","message":"Invalid prompt: your prompt was flagged as potentially violating our usage policy. Please try again with a different prompt: https://platform.openai.com/docs/guides/reasoning#advice-on-prompting"}`

func TestUpstreamPromptSafetyStructuredDetectionAndHardStop(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	for _, payload := range []string{
		`{"error":` + promptSafetySampleError + `}`,
		`{"type":"response.failed","response":{"error":` + promptSafetySampleError + `}}`,
		`{"response":{"status_details":{"error":` + promptSafetySampleError + `}}}`,
		promptSafetySampleError,
	} {
		body := []byte(payload)
		require.True(test, isUpstreamPromptSafetyRefusal(body))
		require.False(test, isExplicitUpstreamCyberPolicy(body))
		for _, policy := range []database.ContinuousRetryPolicy{{}, {Enabled: true, CatchAll: true}, {Enabled: true, ErrorCodes: []string{"invalid_prompt"}, StatusCodes: []int{400, 500}}} {
			for _, status := range []int{400, 429, 500, 502} {
				general, rate := 0, 0
				require.False(test, handler.shouldRetryUpstreamHTTPStatus(status, body, &general, &rate, -1, -1, policy))
				require.Zero(test, general)
				require.Zero(test, rate)
				require.False(test, continuousRetryHTTPSelected(policy, status, body))
				disposition := handler.httpSessionFailureDispositionForPolicy(status, body, false, policy)
				require.False(test, disposition.reportAccount)
				require.True(test, disposition.retainAffinity)
			}
			for _, status := range []int{0, 400, 500} {
				failure := continuousRetryTestHTTPError{status: status, body: body}
				wrapped := &Error{Code: ErrorCodeUpstreamError, Type: ErrorTypeUpstreamError, Retryable: true, Cause: failure}
				retries := 0
				require.False(test, shouldRetryRequestError(failure, &retries, -1, policy))
				require.False(test, shouldRetryRequestError(wrapped, &retries, -1, policy))
				require.Equal(test, body, upstreamPromptSafetyRequestErrorBody(wrapped))
				require.False(test, continuousRetryRequestErrorSelected(policy, failure))
				require.False(test, isRetryableRequestErrorForContext(test.Context(), failure, policy))
			}
			outcome := classifyResponseFailedOutcome(body)
			require.Equal(test, 400, outcome.logStatusCode)
			require.Equal(test, "safety_policy", outcome.failureKind)
			require.False(test, outcome.penalize)
			require.False(test, continuousRetryStreamFailureSelected(outcome, body, "response.failed", policy))
		}
	}
	for _, payload := range []string{
		`{"error":{"code":"invalid_prompt","message":"tool_input_too_large: split the request"}}`,
		`{"error":{"code":"invalid_prompt","message":"missing input"}}`,
		`{"error":{"code":"invalid_prompt"}}`,
		strings.Replace(promptSafetySampleError, "invalid_prompt", "server_error", 1),
		`{"input":` + promptSafetySampleError + `}`,
		`{"codex_error_info":"cyber_policy","error":` + promptSafetySampleError + `}`,
		strings.Replace(promptSafetySampleError, "Invalid prompt:", "Example diagnostic: Invalid prompt:", 1),
	} {
		require.False(test, isUpstreamPromptSafetyRefusal([]byte(payload)), payload)
	}
}

func TestUpstreamPromptSafetyLocksOriginalRootWithoutCYBOrUserCooldown(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	config := handler.store.GetPromptFilterConfig()
	config.Advanced.Enforcement.ConversationLockEnabled = true
	config.Advanced.Enforcement.CYBStrikeEnabled = true
	handler.store.SetPromptFilterConfig(config)
	body := promptLockForkBody(continuityTestThread, "")
	request, recorder := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(continuityTestThread, ""))
	setIngressRequestBodyIfAbsent(request, body)
	payload := []byte(`{"type":"response.failed","response":{"error":` + promptSafetySampleError + `}}`)
	frame, incident, handled := handler.attachUpstreamCyberPolicyStreamDecision(request, "/v1/responses", "gpt-6-astra", payload, upstreamCyberPolicyAttempt{StatusCode: 400, Transport: "sse"})
	require.True(test, handled)
	require.Empty(test, incident)
	diagnostic := promptSafetyDiagnostic(request)
	require.NotNil(test, diagnostic)
	require.Equal(test, "locked", diagnostic.LockResult)
	require.True(test, diagnostic.Locked)
	require.False(test, diagnostic.Strike)
	require.False(test, gjson.GetBytes(frame, "response.error.details.codex2api_safety.strike_eligible").Bool())
	require.Equal(test, "stop", gjson.GetBytes(frame, "response.error.details.codex2api_safety.retry").String())
	require.False(test, gjson.GetBytes(frame, "response.error.details.codex2api_policy").Exists())
	require.Equal(test, gjson.GetBytes(payload, "response.error.message").String(), gjson.GetBytes(frame, "response.error.message").String())
	_, delegated := newAPIUpstreamCyberPolicyDecision(request)
	require.False(test, delegated)
	require.Empty(test, recorder.Header().Get("X-Codex2API-Policy-Decision-ID"))
	handler.logUpstreamCyberPolicy(request, "/v1/responses", "gpt-6-astra", payload)
	policy, verified := handler.verifyNewAPIPolicyContext(request, handler.promptFilterConfigForRequest(request).Advanced.NewAPI, body)
	require.True(test, verified)
	identity, known := verifiedPromptConversationLockIdentity(request, policy)
	require.True(test, known)
	lock, err := handler.db.GetActivePromptConversationLockBySessionHash(test.Context(), identity.SessionHash)
	require.NoError(test, err)
	require.Equal(test, upstreamPromptSafetyReason, lock.ReasonCode)
	require.EqualValues(test, 1, lock.TriggerCount)
	_, _, err = handler.db.GetActivePromptConversationRestriction(test.Context(), "", "test-platform", "42", time.Hour, time.Hour)
	require.ErrorIs(test, err, sql.ErrNoRows)
	for _, scenario := range []struct {
		session, parent string
		blocked         bool
	}{
		{continuityTestThread, "", true}, {promptLockForkSession, continuityTestThread, true},
		{promptLockGrandchildSession, promptLockForkSession, true}, {promptLockGrandchildSession, "", true},
		{accountIdentitySampleRoot, "", false},
	} {
		body := promptLockForkBody(scenario.session, scenario.parent)
		next, output := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(scenario.session, scenario.parent))
		setIngressRequestBodyIfAbsent(next, body)
		blocked := handler.rejectLockedPromptConversation(next, handler.promptFilterConfigForRequest(next), body, body, "/v1/responses", "gpt-6-astra")
		require.Equal(test, scenario.blocked, blocked, output.Body.String())
		if blocked {
			require.Equal(test, promptSafetyLockedReason, gjson.GetBytes(output.Body.Bytes(), "error.code").String())
			require.False(test, policyDecisionMetadataFromHeaders(output.Header()).StrikeEligible)
		}
	}
	_, err = handler.db.UnlockPromptConversation(test.Context(), lock.LockKey, "reviewed")
	require.NoError(test, err)
	nextBody := promptLockForkBody(promptLockForkSession, "")
	next, _ := signedPromptLockContext(test, http.MethodPost, "/v1/responses", nextBody, promptLockForkMeta(promptLockForkSession, ""))
	_, blocked, failure := handler.requestPromptConversationLock(next, handler.promptFilterConfigForRequest(next), nextBody, nextBody, "/v1/responses", "gpt-6-astra")
	require.Nil(test, failure)
	require.False(test, blocked)
	waitPromptFilterAuditIdle(test, handler.db)
	logs, err := handler.db.ListPromptFilterLogs(test.Context(), 10)
	require.NoError(test, err)
	require.Len(test, logs, 1)
	require.Equal(test, upstreamPromptSafetyReason, logs[0].ReasonCode)
	require.False(test, logs[0].StrikeEligible)
	serialized, err := json.Marshal(usageRequestDiagnosticState(request))
	require.NoError(test, err)
	require.Equal(test, "locked", gjson.GetBytes(serialized, "prompt_safety.lock_result").String())
}

func TestUpstreamPromptSafetyDisabledUnknownIdentityAndStorageFailure(test *testing.T) {
	for _, scenario := range []string{"disabled", "no_identity", "storage_unavailable"} {
		test.Run(scenario, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			body := promptLockForkBody(continuityTestThread, "")
			request, recorder := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(continuityTestThread, ""))
			if scenario == "disabled" {
				config := handler.store.GetPromptFilterConfig()
				config.Advanced.Enforcement.ConversationLockEnabled = false
				handler.store.SetPromptFilterConfig(config)
			} else if scenario == "no_identity" {
				request, _ = gin.CreateTestContext(recorder)
				request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				body = nil
			} else {
				canceled, cancel := context.WithCancel(request.Request.Context())
				cancel()
				request.Request = request.Request.WithContext(canceled)
			}
			setIngressRequestBodyIfAbsent(request, body)
			handler.recordUpstreamPromptSafety(request, "/v1/responses", "gpt-6-astra", []byte(promptSafetySampleError))
			diagnostic := promptSafetyDiagnostic(request)
			require.NotNil(test, diagnostic)
			require.False(test, diagnostic.Locked)
			require.NotEqual(test, "locked", diagnostic.LockResult)
			require.Equal(test, "stop", diagnostic.Retry)
		})
	}
}

func TestUpstreamPromptSafetyHTTPAndHiddenWSErrorDetails(test *testing.T) {
	for _, endpoint := range []string{"/v1/responses", "/v1/responses/compact", "/v1/messages"} {
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Request = httptest.NewRequest(http.MethodPost, endpoint, nil)
		require.True(test, writeUpstreamPromptSafetyError(request, []byte(promptSafetySampleError)))
		require.Equal(test, 400, recorder.Code)
		require.Equal(test, "false", recorder.Header().Get("X-Should-Retry"))
		require.Equal(test, "invalid_prompt", gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
		require.Equal(test, "stop", gjson.GetBytes(recorder.Body.Bytes(), "error.details.retry").String())
	}
	for _, status := range []int{400, 500} {
		failure := responsesWSUpstreamAPIError(status, []byte(promptSafetySampleError))
		require.Same(test, failure, responsesWSClientUpstreamAPIError(failure, true), fmt.Sprint(status))
	}
}

func TestUpstreamPromptSafetyEndToEndHTTPAndSSE(test *testing.T) {
	for _, upstreamStream := range []bool{false, true} {
		for _, catchAll := range []bool{false, true} {
			test.Run(fmt.Sprintf("stream_%t_catchall_%t", upstreamStream, catchAll), func(test *testing.T) {
				handler, _, _, _ := failoverTestSetup(test, true)
				settings := CurrentRuntimeSettings()
				settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: catchAll, CatchAll: catchAll}
				settings.CodexForceWebsocket = false
				ApplyRuntimeSettings(settings)
				previousResin := GetResinConfig()
				test.Cleanup(func() { SetResinConfig(previousResin) })
				test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
				var attempts atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					attempts.Add(1)
					if upstreamStream {
						writer.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.failed\",\"response\":{\"error\":%s}}\n\n", promptSafetySampleError)
					} else {
						writer.Header().Set("Content-Type", "application/json")
						writer.WriteHeader(400)
						_, _ = fmt.Fprintf(writer, "{\"error\":%s}", promptSafetySampleError)
					}
				}))
				defer upstream.Close()
				SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "prompt-safety-test"})
				_, body := failoverTestRequest(test, handler)
				body, err := sjson.SetBytes(body, "stream", upstreamStream)
				require.NoError(test, err)
				request := promptSessionLimitTestContext(continuityTestThread)
				request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				request.Request.Header.Set("Authorization", "Bearer test-user-key")
				recorder := httptest.NewRecorder()
				output, _ := gin.CreateTestContext(recorder)
				request.Writer = output.Writer
				ctx, cancel := context.WithTimeout(test.Context(), 5*time.Second)
				defer cancel()
				request.Request = request.Request.WithContext(ctx)
				handler.Responses(request)
				require.EqualValues(test, 1, attempts.Load(), recorder.Body.String())
				require.Contains(test, recorder.Body.String(), "invalid_prompt")
				require.Contains(test, recorder.Body.String(), `"retry":"stop"`)
				require.NotNil(test, promptSafetyDiagnostic(request))
				require.True(test, promptSafetyDiagnostic(request).Locked, recorder.Body.String())
			})
		}
	}
}

func TestUpstreamPromptSafetyForkWebSocketInheritsLock(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	body := promptLockForkBody(continuityTestThread, "")
	first, _ := signedPromptLockContext(test, http.MethodPost, "/v1/responses", body, promptLockForkMeta(continuityTestThread, ""))
	setIngressRequestBodyIfAbsent(first, body)
	handler.recordUpstreamPromptSafety(first, "/v1/responses", "gpt-6-astra", []byte(promptSafetySampleError))
	require.True(test, promptSafetyDiagnostic(first).Locked)
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
	body = promptLockForkBody(promptLockForkSession, continuityTestThread)
	body, err = sjson.SetBytes(body, "type", "response.create")
	require.NoError(test, err)
	require.NoError(test, connection.WriteMessage(websocket.TextMessage, body))
	require.NoError(test, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, response, err := connection.ReadMessage()
	require.NoError(test, err)
	require.Equal(test, promptSafetyLockedReason, gjson.GetBytes(response, "error.code").String(), string(response))
	require.False(test, gjson.GetBytes(response, "error.details.strike_eligible").Bool())
	require.NoError(test, connection.Close())
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		test.Fatal("websocket handler did not finish")
	}
}

func TestUpstreamPromptSafetyWebSocketFirstRefusal(test *testing.T) {
	handler, _, _, _ := failoverTestSetup(test, true)
	settings := CurrentRuntimeSettings()
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(writer, "data: {\"type\":\"response.failed\",\"response\":{\"error\":%s}}\n\n", promptSafetySampleError)
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "prompt-safety-ws"})
	router := gin.New()
	finished := make(chan *database.PromptSafetyDiagnostic, 1)
	router.GET("/v1/responses", func(request *gin.Context) {
		defer func() { finished <- promptSafetyDiagnostic(request) }()
		request.Set(contextAPIKeyID, int64(7))
		handler.ResponsesWebSocket(request)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	headers := http.Header{"Authorization": []string{"Bearer test-user-key"}}
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	require.NoError(test, err)
	defer connection.Close()
	_, body := failoverTestRequest(test, handler)
	body, err = sjson.SetBytes(body, "type", "response.create")
	require.NoError(test, err)
	require.NoError(test, connection.WriteMessage(websocket.TextMessage, body))
	require.NoError(test, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, response, err := connection.ReadMessage()
	require.NoError(test, err)
	require.Equal(test, "invalid_prompt", gjson.GetBytes(response, "error.code").String(), string(response))
	require.Equal(test, "stop", gjson.GetBytes(response, "error.details.retry").String())
	require.True(test, gjson.GetBytes(response, "error.details.conversation_locked").Bool(), string(response))
	require.EqualValues(test, 1, attempts.Load())
	require.NoError(test, connection.Close())
	select {
	case diagnostic := <-finished:
		require.NotNil(test, diagnostic)
		require.True(test, diagnostic.Locked)
	case <-time.After(5 * time.Second):
		test.Fatal("websocket handler did not finish")
	}
}
