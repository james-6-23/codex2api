package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func signedPassiveInternalPromptContext(t *testing.T, method, path string, body []byte, fingerprint string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	requestContext.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	requestContext.Set(contextAPIKeyID, int64(101))
	setSignedNewAPIRequestHeaders(t, requestContext.Request, body, t.Name(), newAPIIdentity{
		UserID: "42", ClientIP: "203.0.113.8",
	}, "gateway-a", "gateway-a-secret", fingerprint)
	addSignedNewAPIPolicyMetaWithSecret(t, requestContext, newAPIPolicyMeta{
		PlatformID: "gateway-a", Profile: "balanced", Mode: "enforce",
		Provider: "openai", Protocol: "responses", SessionFingerprint: fingerprint,
		RootSessionVersion:     1,
		RootSessionState:       newAPIPolicyRootSessionResolved,
		RootSessionRelation:    newAPIPolicyRootSessionRelationRelated,
		RootSessionFingerprint: promptSessionTestFingerprint("restricted-passive-root"),
		ThreadSource:           "subagent",
		RequestKind:            "turn",
		SubagentKind:           "guardian",
	}, true, "gateway-a-secret")
	return requestContext, recorder
}

func TestPassiveInternalHTTPCyberRestrictionsPrecedeAccountSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newPromptConversationLockTestHandler(t)
	handler.store.SetPassiveInternalModelsEnabled(true)
	triggerBody := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	triggerFingerprint := promptSessionTestFingerprint("restricted-passive-trigger")
	trigger := signedBoundNewAPIPolicyContext(t, "passive-restriction-trigger", newAPIIdentity{
		UserID: "42", ClientIP: "203.0.113.8",
	}, triggerBody, 101, "gateway-a", "gateway-a-secret", triggerFingerprint)
	setIngressRequestBodyIfAbsent(trigger, triggerBody)
	handler.logUpstreamCyberPolicy(trigger, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	config := handler.store.GetPromptFilterConfig()
	before, _, err := db.GetActivePromptConversationRestriction(t.Context(), "", "gateway-a", "42", promptConversationLockTTL(config), promptUserCyberCooldownTTL(config))
	if err != nil {
		t.Fatal(err)
	}

	for _, restriction := range []struct {
		name        string
		fingerprint string
		reason      string
		scope       string
	}{
		{"user_cooldown", promptSessionTestFingerprint("restricted-passive-child"), promptUserCyberCooldownReasonCode, database.PromptConversationRestrictionScopeUserCooldown},
		{"conversation_lock", triggerFingerprint, promptConversationLockedReasonCode, database.PromptConversationRestrictionScopeConversation},
	} {
		t.Run(restriction.name, func(t *testing.T) {
			for _, endpoint := range []struct {
				path   string
				body   string
				handle func(*gin.Context)
			}{
				{"/v1/responses", `{"model":"gpt-5.5","input":"ordinary request"}`, handler.Responses},
				{"/v1/responses/compact", `{"model":"gpt-5.5","input":"ordinary request"}`, handler.ResponsesCompact},
				{"/v1/chat/completions", `{"model":"gpt-5.5","messages":[{"role":"user","content":"ordinary request"}]}`, handler.ChatCompletions},
				{"/v1/messages", `{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"ordinary request"}]}`, handler.Messages},
			} {
				t.Run(endpoint.path, func(t *testing.T) {
					requestContext, recorder := signedPassiveInternalPromptContext(t, http.MethodPost, endpoint.path, []byte(endpoint.body), restriction.fingerprint)
					endpoint.handle(requestContext)
					if recorder.Code != http.StatusBadRequest {
						t.Fatalf("restricted internal request status=%d body=%s, want 400 before account selection", recorder.Code, recorder.Body.String())
					}
					metadata := policyDecisionMetadataFromHeaders(recorder.Header())
					if metadata.ReasonCode != restriction.reason || metadata.StrikeEligible {
						t.Fatalf("restriction metadata = %+v", metadata)
					}
					if got := gjson.GetBytes(recorder.Body.Bytes(), "error.details.restriction_scope").String(); got != restriction.scope {
						t.Fatalf("restriction scope=%q body=%s", got, recorder.Body.String())
					}
					if recorder.Header().Get("Retry-After") == "" {
						t.Fatal("restriction response omitted Retry-After")
					}
					if endpoint.path != "/v1/responses/compact" && !passiveInternalRequestAuthorized(requestContext) {
						t.Fatal("request did not exercise the passive internal path")
					}
				})
			}
		})
	}

	after, _, err := db.GetActivePromptConversationRestriction(t.Context(), "", "gateway-a", "42", promptConversationLockTTL(config), promptUserCyberCooldownTTL(config))
	if err != nil || after.TriggerCount != before.TriggerCount || !after.LockedAt.Equal(before.LockedAt) {
		t.Fatalf("restricted retries changed original punishment: before=%+v after=%+v err=%v", before, after, err)
	}
}

func TestPassiveInternalWebSocketHonorsCyberRestrictions(t *testing.T) {
	handler, _ := newPromptConversationLockTestHandler(t)
	handler.store.SetPassiveInternalModelsEnabled(true)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	triggerFingerprint := promptSessionTestFingerprint("passive-ws-trigger")
	trigger := signedBoundNewAPIPolicyContext(t, "passive-ws-trigger", newAPIIdentity{
		UserID: "42", ClientIP: "203.0.113.8",
	}, body, 101, "gateway-a", "gateway-a-secret", triggerFingerprint)
	setIngressRequestBodyIfAbsent(trigger, body)
	handler.logUpstreamCyberPolicy(trigger, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))

	for _, testCase := range []struct {
		name        string
		fingerprint string
		reason      string
	}{
		{"user_cooldown", promptSessionTestFingerprint("passive-ws-child"), promptUserCyberCooldownReasonCode},
		{"conversation_lock", triggerFingerprint, promptConversationLockedReasonCode},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			requestContext, recorder := signedPassiveInternalPromptContext(t, http.MethodGet, "/v1/responses", nil, testCase.fingerprint)
			handler.primeNewAPIPolicyContext(requestContext, nil)
			handler.resolveRequestSessionIdentityForContext(requestContext, body)
			if !passiveInternalRequestAuthorized(requestContext) {
				t.Fatal("request did not exercise the passive internal path")
			}
			blocked, delegated := handler.inspectPromptFilterOpenAIForWebSocket(requestContext, nil, body, "/v1/responses", "gpt-5.5", "passive-ws-event")
			if !blocked || !delegated {
				t.Fatalf("restricted internal WebSocket request blocked=%v delegated=%v", blocked, delegated)
			}
			metadata := policyDecisionMetadataFromHeaders(recorder.Header())
			if metadata.ReasonCode != testCase.reason || metadata.StrikeEligible {
				t.Fatalf("restriction metadata = %+v", metadata)
			}
		})
	}
}

func TestPassiveInternalStillSkipsPromptContentInspection(t *testing.T) {
	handler, _ := newPromptConversationLockTestHandler(t)
	handler.store.SetPassiveInternalModelsEnabled(true)
	body := promptRequestBody(t, blatantIntentBlockedByLocalRegex)
	for _, protocol := range []string{"openai", "anthropic", "websocket"} {
		t.Run(protocol, func(t *testing.T) {
			method, signedBody := http.MethodPost, body
			if protocol == "websocket" {
				method, signedBody = http.MethodGet, nil
			}
			requestContext, _ := signedPassiveInternalPromptContext(t, method, "/v1/responses", signedBody, promptSessionTestFingerprint("unrestricted-passive-child"))
			handler.primeNewAPIPolicyContext(requestContext, signedBody)
			handler.resolveRequestSessionIdentityForContext(requestContext, body)
			if !passiveInternalRequestAuthorized(requestContext) {
				t.Fatal("request did not exercise the passive internal path")
			}
			var blocked bool
			switch protocol {
			case "openai":
				blocked = handler.inspectPromptFilterOpenAI(requestContext, body, "/v1/responses", "gpt-5.5")
			case "anthropic":
				blocked = handler.inspectPromptFilterAnthropic(requestContext, body, "/v1/messages", "gpt-5.5")
			case "websocket":
				blocked, _ = handler.inspectPromptFilterOpenAIForWebSocket(requestContext, nil, body, "/v1/responses", "gpt-5.5", "passive-clean-event")
			}
			if blocked {
				t.Fatal("unrestricted internal transcript was reclassified as a fresh user prompt")
			}
		})
	}
}
