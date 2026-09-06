package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestBioPolicyStructuredDetectionAndResponses(t *testing.T) {
	for _, payload := range []string{
		`{"error":{"code":"bio_policy","type":"invalid_request_error"}}`,
		`{"codex_error_info":" BIO_POLICY "}`,
		`{"error":{"codex_error_info":"bio_policy"}}`,
		`{"error":{"type":"bio_policy"}}`,
		`{"type":"response.failed","response":{"error":{"code":"bio_policy"}}}`,
		`{"type":"response.failed","response":{"status_details":{"error":{"code":"bio_policy"}}}}`,
	} {
		body := []byte(payload)
		if code := upstreamCyberPolicyCode(responseFailedErrorBody(body)); code != "bio_policy" || !isExplicitUpstreamSafetyPolicy(body) {
			t.Fatalf("BIO detection = %q for %s", code, payload)
		}
		outcome := classifyResponseFailedOutcome(body)
		if outcome.logStatusCode != http.StatusBadRequest || outcome.penalize || outcome.failureMessage != upstreamBioPolicyUserMessage {
			t.Fatalf("BIO outcome = %+v", outcome)
		}
		if message := responsesWSUpstreamAPIError(http.StatusBadRequest, body).Message; message != upstreamBioPolicyUserMessage {
			t.Fatalf("BIO WS message = %q", message)
		}
		recorder := httptest.NewRecorder()
		requestContext, _ := gin.CreateTestContext(recorder)
		(&Handler{}).sendUpstreamError(requestContext, http.StatusBadRequest, body)
		if recorder.Code != http.StatusBadRequest || gjson.GetBytes(recorder.Body.Bytes(), "error.message").String() != upstreamBioPolicyUserMessage {
			t.Fatalf("BIO HTTP response = %d %s", recorder.Code, recorder.Body.String())
		}
	}
	for _, payload := range []string{
		`{"error":{"code":"bio_polic"}}`,
		`{"error":{"code":"prefix_bio_policy"}}`,
		`{"error":{"code":"bio_policy_extra"}}`,
		`{"error":{"type":"invalid_request_error","message":"bio_policy biological risk"}}`,
	} {
		if isExplicitUpstreamCyberPolicy([]byte(payload)) {
			t.Fatalf("non-explicit error matched BIO: %s", payload)
		}
	}
}

func TestBioPolicyNeverRetries(t *testing.T) {
	body := []byte(`{"error":{"code":"bio_policy"}}`)
	stream := []byte(`{"type":"response.failed","response":{"status_code":500,"error":{"code":"bio_policy"}}}`)
	for _, policy := range []database.ContinuousRetryPolicy{{}, {Enabled: true, CatchAll: true}} {
		for _, limit := range []int{1, -1} {
			general, rate := 0, 0
			if shouldRetryHTTPStatus(500, body, &general, &rate, limit, limit, policy) {
				t.Fatal("BIO HTTP error selected for retry")
			}
			outcome := classifyResponseFailedOutcome(stream)
			outcome.penalize = true
			if continuousRetryStreamFailureSelected(outcome, stream, "response.failed", policy) ||
				shouldTransparentRetryStreamEventWithBudgets(outcome, "response.failed", &general, &rate, limit, limit, false, nil, nil, policy) {
				t.Fatal("BIO stream error selected for retry")
			}
			for _, requestErr := range []error{
				continuousRetryTestHTTPError{status: 400, body: body},
				&Error{Code: "bio_policy", Message: "blocked", Type: ErrorTypeUpstreamError, Retryable: true},
			} {
				if isRetryableRequestErrorForContext(context.Background(), requestErr, policy) || shouldRetryRequestError(requestErr, &general, limit, policy) {
					t.Fatal("BIO handshake/statusless error selected for retry")
				}
			}
		}
	}
}

func TestBioPolicySignedLockCooldownAndAudit(t *testing.T) {
	for _, strikeEnabled := range []bool{false, true} {
		handler, db := newPromptConversationLockTestHandler(t)
		config := handler.store.GetPromptFilterConfig()
		config.Advanced.Enforcement.CYBStrikeEnabled = strikeEnabled
		handler.store.SetPromptFilterConfig(config)
		body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
		identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
		fingerprint := promptSessionTestFingerprint("bio-thread")
		first, recorder := signedBoundPromptConversationContextWithRecorder(t, "bio-first", identity, body, fingerprint)
		setIngressRequestBodyIfAbsent(first, body)
		payload := []byte(`{"type":"response.failed","response":{"error":{"code":"bio_policy"}}}`)
		frame, incidentID, logged := handler.attachUpstreamCyberPolicyStreamDecision(first, "/v1/responses", "gpt-5.5", payload, upstreamCyberPolicyAttempt{Transport: "sse", StatusCode: 400})
		metadata, delegated := newAPIUpstreamCyberPolicyDecision(first)
		if !logged || incidentID == "" || !delegated || metadata.ReasonCode != newAPIUpstreamCyberPolicyReasonCode || metadata.StrikeEligible != strikeEnabled || !metadata.ConversationLocked {
			t.Fatalf("BIO policy metadata = %+v logged=%t", metadata, logged)
		}
		if gjson.GetBytes(frame, "response.error.message").String() != upstreamBioPolicyLockedUserMessage ||
			gjson.GetBytes(frame, "response.error.code").String() != "bio_policy" ||
			gjson.GetBytes(frame, "response.error.details.codex2api_policy.response_signature").String() != metadata.Signature {
			t.Fatalf("BIO SSE frame = %s", frame)
		}
		if newAPIPolicyDecisionAPIError(metadata).Message != upstreamBioPolicyLockedUserMessage {
			t.Fatal("signed WS envelope lost BIO message")
		}
		handler.sendUpstreamError(first, 400, responseFailedErrorBody(payload))
		if gjson.GetBytes(recorder.Body.Bytes(), "error.message").String() != upstreamBioPolicyLockedUserMessage {
			t.Fatalf("BIO locked HTTP message = %s", recorder.Body.String())
		}
		waitPromptFilterAuditIdle(t, db)
		incident, err := db.GetPromptPolicyIncident(t.Context(), incidentID)
		if err != nil || incident.UpstreamErrorCode != "bio_policy" {
			t.Fatalf("BIO audit = %+v err=%v", incident, err)
		}
		policyContext, verified := handler.verifyNewAPIPolicyContext(first, handler.promptFilterConfigForRequest(first).Advanced.NewAPI, body)
		lockIdentity, hasIdentity := verifiedPromptConversationLockIdentity(first, policyContext)
		if !verified || !hasIdentity {
			t.Fatal("BIO signed conversation identity was not verified")
		}
		for _, cached := range []bool{true, false} {
			if !cached {
				if err := handler.cache.DeleteRuntime(t.Context(), database.PromptConversationLockCacheNamespace, lockIdentity.LockKey); err != nil {
					t.Fatal(err)
				}
			}
			for _, sameThread := range []bool{true, false} {
				repeatFingerprint := ""
				wantReason := promptUserCyberCooldownReasonCode
				if sameThread {
					repeatFingerprint = fingerprint
					wantReason = promptConversationLockedReasonCode
				}
				repeat, repeatRecorder := signedBoundPromptConversationContextWithRecorder(t, fmt.Sprintf("bio-repeat-%t-%s", cached, wantReason), identity, body, repeatFingerprint)
				setIngressRequestBodyIfAbsent(repeat, body)
				if !handler.inspectPromptFilterOpenAI(repeat, body, "/v1/responses", "gpt-5.5") {
					t.Fatalf("BIO locked request was forwarded: cached=%t sameThread=%t body=%s", cached, sameThread, repeatRecorder.Body.String())
				}
				message := gjson.GetBytes(repeatRecorder.Body.Bytes(), "error.message").String()
				if !strings.Contains(message, "生物安全") || strings.Contains(message, "CYB") || gjson.GetBytes(repeatRecorder.Body.Bytes(), "error.code").String() != wantReason {
					t.Fatalf("BIO repeat response = %s", repeatRecorder.Body.String())
				}
				if policyDecisionMetadataFromHeaders(repeat.Writer.Header()).StrikeEligible {
					t.Fatal("BIO repeated restriction counted another strike")
				}
			}
		}
		lock, err := db.GetActivePromptConversationLock(t.Context(), lockIdentity.LockKey)
		if err != nil || lock.ReasonCode != promptUpstreamBioPolicyReasonCode || lock.TriggerCount != 1 {
			t.Fatalf("BIO persisted lock = %+v err=%v", lock, err)
		}
		other, _ := signedBoundPromptConversationContextWithRecorder(t, "bio-other-user", newAPIIdentity{UserID: "43", ClientIP: "203.0.113.9"}, body, fingerprint)
		setIngressRequestBodyIfAbsent(other, body)
		if handler.inspectPromptFilterOpenAI(other, body, "/v1/responses", "gpt-5.5") {
			t.Fatal("BIO lock leaked to another user")
		}
		clearNewAPIUpstreamCyberPolicyDecision(first)
		if upstreamCyberPolicyResponseMessage(first, []byte(`{"error":{"code":"cyber_policy"}}`)) != upstreamCyberPolicyUserMessage {
			t.Fatal("BIO response state leaked into the next logical turn")
		}
	}
}

func TestBioPolicyUnsignedLockAndDisabledLock(t *testing.T) {
	for _, lockEnabled := range []bool{false, true} {
		for _, session := range []string{"", "bio-session"} {
			handler, _ := newPromptConversationLockTestHandler(t)
			config := handler.store.GetPromptFilterConfig()
			config.Advanced.Enforcement.ConversationLockEnabled = lockEnabled
			handler.store.SetPromptFilterConfig(config)
			body := promptRequestBody(t, "ordinary test request")
			newRequest := func(keyID int64) *gin.Context {
				requestContext, _ := gin.CreateTestContext(httptest.NewRecorder())
				requestContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				requestContext.Request.RemoteAddr = "198.51.100.9:1234"
				requestContext.Request.Header.Set("Session-Id", session)
				requestContext.Set(contextAPIKeyID, keyID)
				setRawRequestBody(requestContext, body)
				setIngressRequestBodyIfAbsent(requestContext, body)
				return requestContext
			}
			first := newRequest(101)
			handler.logUpstreamCyberPolicy(first, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"bio_policy"}}`))
			if _, delegated := newAPIUpstreamCyberPolicyDecision(first); delegated {
				t.Fatal("unsigned BIO emitted a signed punishment")
			}
			if message := upstreamCyberPolicyResponseMessage(first); message != upstreamPolicyUserMessage("bio_policy", lockEnabled && session != "") {
				t.Fatalf("unsigned BIO message = %q", message)
			}
			repeat := newRequest(101)
			lock, found := handler.activePromptConversationLock(repeat, config, body, "/v1/responses", "gpt-5.5")
			if found != lockEnabled {
				t.Fatalf("unsigned BIO lock = %t want %t", found, lockEnabled)
			}
			if found && !strings.Contains(promptCyberRestrictionDecision(lock, config).Message, "生物安全") {
				t.Fatal("unsigned persisted BIO lost its category")
			}
			if _, found := handler.activePromptConversationLock(newRequest(102), config, body, "/v1/responses", "gpt-5.5"); found {
				t.Fatal("unsigned BIO lock leaked across API keys")
			}
			clearNewAPIUpstreamCyberPolicyDecision(first)
			if upstreamCyberPolicyResponseMessage(first) != upstreamCyberPolicyUserMessage {
				t.Fatal("unsigned BIO response state survived turn reset")
			}
		}
	}
}
