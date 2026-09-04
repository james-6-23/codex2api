package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func newRootlessPassiveModelTestHandler(t *testing.T) *Handler {
	t.Helper()
	config := promptGuardTestConfig()
	config.Advanced.NewAPI.Enabled = true
	handler := newPromptGuardTestHandler(config)
	handler.store.SetPassiveInternalModelsEnabled(true)
	handler.store.SetCodexUnlinkedAccountFallbackEnabled(true)
	handler.store.SetCodexUnlinkedAccountFallbackSeconds(300)
	t.Cleanup(func() {
		handler.store.Stop()
		_ = handler.cache.Close()
	})
	return handler
}

func signedRootlessPassiveModelContext(t *testing.T, method, path string, body []byte, meta newAPIPolicyMeta) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	requestContext.Request = httptest.NewRequest(method, path, bytes.NewReader(body))
	requestContext.Set(contextAPIKeyID, int64(101))
	setSignedNewAPIRequestHeaders(t, requestContext.Request, body, t.Name(), newAPIIdentity{
		UserID: "42", ClientIP: "203.0.113.8",
	}, "test-platform", "integration-secret", promptSessionTestFingerprint(t.Name()))
	meta.PlatformID, meta.Profile, meta.Mode = "test-platform", "balanced", "enforce"
	meta.Provider, meta.Protocol = "openai", "responses"
	meta.TokenID, meta.InstallationID = 7, "rootless-device"
	meta.SessionFingerprint = promptSessionTestFingerprint(t.Name())
	addSignedNewAPIPolicyMeta(t, requestContext, meta, true)
	return requestContext, recorder
}

func TestRootlessPassiveModelsReuseRecentAccountWithoutModelNamesOrBindings(t *testing.T) {
	for _, rootState := range []string{newAPIPolicyRootSessionUnavailable, newAPIPolicyRootSessionResolved, "independent"} {
		for _, model := range []string{"gpt-5.6-luna", "future-internal-model-2030"} {
			t.Run(rootState+"/"+model, func(t *testing.T) {
				handler := newRootlessPassiveModelTestHandler(t)
				account := &auth.Account{DBID: 17, AccessToken: "recent", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
				handler.store.AddAccount(account)
				body := []byte(`{"model":"` + model + `","input":"background task"}`)
				meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: rootState, ThreadSource: "future_internal_kind", RequestKind: "turn"}
				if rootState == newAPIPolicyRootSessionResolved {
					meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
					meta.RootSessionFingerprint = promptSessionTestFingerprint("missing-root-binding")
				}
				if rootState == "independent" {
					meta.RootSessionState = newAPIPolicyRootSessionResolved
					meta.RootSessionRelation = newAPIPolicyRootSessionRelationRoot
					meta.RootSessionFingerprint = promptSessionTestFingerprint("independent-background-root")
					meta.SessionAccounting = newAPISessionAccountingBypass
					meta.PassiveFeature = newAPIPassiveFeatureIndependent
				}
				requestContext, _ := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, meta)
				handler.primeNewAPIPolicyContext(requestContext, body)
				identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
				if !passiveInternalRequestAuthorized(requestContext) || !identity.unlinkedFallbackOnly || identity.unlinkedFallbackScope == "" {
					t.Fatalf("rootless internal classification = %+v", identity)
				}
				key := capacityAwareSessionAffinityKey(identity, 101)
				if key != "" {
					t.Fatalf("rootless request acquired durable affinity %q", key)
				}
				filter := handler.applyPassiveInternalModelRouting(requestContext, model, identity, key, true, accountFilterForModel(model))
				if !filter(account) || account.SupportsCodexModel(model) {
					t.Fatal("test did not exercise a model whitelist exemption")
				}
				observation, err := json.Marshal(unlinkedFallbackRuntimeRecord{AccountID: account.ID(), ObservedAt: time.Now().Add(-time.Second)})
				if err != nil {
					t.Fatal(err)
				}
				if err := handler.cache.SetRuntime(t.Context(), unlinkedFallbackRuntimeNamespace, identity.unlinkedFallbackScope, observation, time.Minute); err != nil {
					t.Fatal(err)
				}
				selected, proxyURL := handler.takeUnlinkedRecentAccount(requestContext, identity, 101, nil, filter, auth.DispatchPolicyStandard)
				if selected != account {
					t.Fatalf("recent account = %+v, want account %d", selected, account.ID())
				}
				handler.bindAccountSession(requestContext, key, selected, proxyURL)
				handler.store.Release(selected)
				rootKey := sessionAffinityKey("newapi-root-session:"+meta.RootSessionFingerprint, 101)
				if _, found := handler.store.SessionAffinityAccountID(rootKey); found {
					t.Fatal("temporary scheduling manufactured a root binding")
				}
				if selected, _ := handler.takeUnlinkedRecentAccount(requestContext, identity, 101, map[int64]bool{account.ID(): true}, filter, auth.DispatchPolicyStandard); selected != nil {
					handler.store.Release(selected)
					t.Fatal("model exemption bypassed account exclusion")
				}
			})
		}
	}
}

func TestRootlessPassiveModelExemptionRequiresTrustedInternalClassification(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		source  string
		state   string
		enabled bool
		forged  bool
	}{
		{"ordinary user", "user", newAPIPolicyRootSessionUnavailable, true, false},
		{"unclassified", "", newAPIPolicyRootSessionUnavailable, true, false},
		{"switch disabled", "system", newAPIPolicyRootSessionUnavailable, false, false},
		{"conflicting root", "system", newAPIPolicyRootSessionConflict, true, false},
		{"forged metadata", "system", newAPIPolicyRootSessionUnavailable, true, true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			handler := newRootlessPassiveModelTestHandler(t)
			handler.store.SetPassiveInternalModelsEnabled(testCase.enabled)
			body := []byte(`{"model":"future-internal-model-2030","input":"background task"}`)
			requestContext, _ := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, newAPIPolicyMeta{
				RootSessionVersion: 1, RootSessionState: testCase.state, ThreadSource: testCase.source,
			})
			if testCase.forged {
				requestContext.Request.Header.Set("X-NewAPI-Policy-Meta-Signature", strings.Repeat("0", 64))
			}
			validate := handler.passiveInternalModelValidator(requestContext, body, handler.modelValidator([]string{"gpt-5.6-sol"}))
			if validate(gjson.GetBytes(body, "model"), "model") == nil {
				t.Fatal("untrusted or disabled request bypassed model validation")
			}
			handler.primeNewAPIPolicyContext(requestContext, body)
			identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
			account := &auth.Account{DBID: 17, Models: []string{"gpt-5.6-sol"}}
			filter := handler.applyPassiveInternalModelRouting(requestContext, "future-internal-model-2030", identity, capacityAwareSessionAffinityKey(identity, 101), true, accountFilterForModel("future-internal-model-2030"))
			if filter(account) {
				t.Fatal("untrusted or disabled request bypassed account model whitelist")
			}
		})
	}
}

func TestRootlessPassiveModelsReachHTTPUpstreamOutsideModelCatalog(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		if strings.HasSuffix(request.URL.Path, "/compact") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"id":"resp_rootless_compact","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			return
		}
		response := `{"id":"resp_rootless","status":"completed","output":[{"id":"msg_rootless","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
		if !gjson.GetBytes(readUpstreamRequestBody(request), "stream").Bool() {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, response)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":"+response+"}\n\n")
	}))
	defer upstream.Close()

	for _, model := range []string{"gpt-5.6-luna", "future-internal-model-2030"} {
		for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages"} {
			t.Run(model+path, func(t *testing.T) {
				handler := newRootlessPassiveModelTestHandler(t)
				handler.store.SetCodexUnlinkedAccountFallbackEnabled(false)
				handler.store.AddAccount(&auth.Account{
					DBID: 17, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL,
					APIKey: "rootless-key", Models: []string{"gpt-5.6-sol"},
				})
				body := []byte(`{"model":"` + model + `","input":"background task"}`)
				if path == "/v1/chat/completions" || path == "/v1/messages" {
					body = []byte(`{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"background task"}]}`)
				}
				requestContext, recorder := signedRootlessPassiveModelContext(t, http.MethodPost, path, body, newAPIPolicyMeta{
					RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionUnavailable, ThreadSource: "future_background_feature",
				})
				before := upstreamCalls.Load()
				switch path {
				case "/v1/responses":
					handler.Responses(requestContext)
				case "/v1/responses/compact":
					handler.ResponsesCompact(requestContext)
				case "/v1/chat/completions":
					handler.ChatCompletions(requestContext)
				case "/v1/messages":
					handler.Messages(requestContext)
				}
				if recorder.Code != http.StatusOK || upstreamCalls.Load() != before+1 {
					t.Fatalf("status=%d upstream calls=%d body=%s", recorder.Code, upstreamCalls.Load()-before, recorder.Body.String())
				}
				if !passiveInternalRequestAuthorized(requestContext) {
					t.Fatal("request did not use internal classification")
				}
			})
		}
	}
}

func TestRootlessPassiveWebSocketAcceptsNewModelWithoutCatalogEntry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamCalls atomic.Int32
	previousExecute := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExecute })
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		upstreamCalls.Add(1)
		payload := "data: " + `{"type":"response.completed","response":{"id":"resp_rootless_ws","status":"completed","output":[{"id":"msg_rootless_ws","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
	}
	handler := newRootlessPassiveModelTestHandler(t)
	handler.store.AddAccount(&auth.Account{
		DBID: 17, AccessToken: "rootless-token", AccountID: "rootless-account", Models: []string{"gpt-5.6-sol"},
	})
	router := gin.New()
	router.GET("/v1/responses", func(requestContext *gin.Context) {
		requestContext.Set(contextAPIKeyID, int64(101))
		handler.ResponsesWebSocket(requestContext)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	requestContext, _ := signedRootlessPassiveModelContext(t, http.MethodGet, "/v1/responses", nil, newAPIPolicyMeta{
		RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionUnavailable, ThreadSource: "future_background_feature",
	})
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", requestContext.Request.Header)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"future-internal-model-2030","input":"background task"}`)); err != nil {
		t.Fatal(err)
	}
	_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, payload, err := connection.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if event := gjson.GetBytes(payload, "type").String(); event != "response.completed" || upstreamCalls.Load() != 1 {
		t.Fatalf("WebSocket event=%s upstream calls=%d", payload, upstreamCalls.Load())
	}
}

func TestRootlessPassiveNewModelStillHonorsUserCyberCooldown(t *testing.T) {
	handler, _ := newPromptConversationLockTestHandler(t)
	handler.store.SetPassiveInternalModelsEnabled(true)
	handler.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "test-platform", Secret: "integration-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}})
	triggerBody := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	trigger := signedBoundNewAPIPolicyContext(t, "rootless-cooldown-trigger", newAPIIdentity{
		UserID: "42", ClientIP: "203.0.113.8",
	}, triggerBody, 101, "test-platform", "integration-secret", promptSessionTestFingerprint("rootless-cooldown-trigger"))
	setIngressRequestBodyIfAbsent(trigger, triggerBody)
	handler.logUpstreamCyberPolicy(trigger, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	body := []byte(`{"model":"future-internal-model-2030","input":"background task"}`)
	requestContext, recorder := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, newAPIPolicyMeta{
		RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionUnavailable, ThreadSource: "system",
	})
	handler.Responses(requestContext)
	if recorder.Code != http.StatusBadRequest || gjson.GetBytes(recorder.Body.Bytes(), "error.code").String() != promptUserCyberCooldownReasonCode {
		t.Fatalf("cooldown status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if metadata := policyDecisionMetadataFromHeaders(recorder.Header()); metadata.StrikeEligible {
		t.Fatal("rootless internal retry accumulated a new punishment")
	}
}

func TestRootlessPassiveModelRoutingSupportsNativeIndependentRequests(t *testing.T) {
	handler := newRootlessPassiveModelTestHandler(t)
	body := []byte(`{"model":"future-internal-model-2030","client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testRootSessionA + `","thread_source":"future_background_feature","request_kind":"turn"},"input":"background task"}`)
	requestContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	requestContext.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	requestContext.Request.Header.Set("Authorization", "Bearer direct-rootless-key")
	requestContext.Set(contextAPIKeyID, int64(101))
	identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
	if !passiveInternalRequestAuthorized(requestContext) || !identity.unlinkedFallbackOnly || !identity.bypassWindowAccounting {
		t.Fatalf("native independent classification = %+v", identity)
	}
	validate := handler.passiveInternalModelValidator(requestContext, body, handler.modelValidator([]string{"gpt-5.6-sol"}))
	if validationError := validate(gjson.GetBytes(body, "model"), "model"); validationError != nil {
		t.Fatalf("native independent model validation = %+v", validationError)
	}
}

func TestPassiveModelExemptionDoesNotAbandonAnExistingRootAccount(t *testing.T) {
	handler := newRootlessPassiveModelTestHandler(t)
	root := &auth.Account{DBID: 17, AccessToken: "root", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
	other := &auth.Account{DBID: 18, AccessToken: "other", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
	handler.store.AddAccounts([]*auth.Account{root, other})
	rootFingerprint := promptSessionTestFingerprint("existing-root-binding")
	rootKey := sessionAffinityKey("newapi-root-session:"+rootFingerprint, 101)
	handler.store.BindSessionAffinity(rootKey, root, "")
	body := []byte(`{"model":"future-internal-model-2030","input":"background task"}`)
	requestContext, _ := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, newAPIPolicyMeta{
		RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRelated,
		RootSessionFingerprint: rootFingerprint, ThreadSource: "subagent",
	})
	handler.primeNewAPIPolicyContext(requestContext, body)
	identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
	if identity.unlinkedFallbackOnly {
		t.Fatal("existing root binding was downgraded to rootless scheduling")
	}
	key := capacityAwareSessionAffinityKey(identity, 101)
	filter := handler.applyPassiveInternalModelRouting(requestContext, "future-internal-model-2030", identity, key, true, accountFilterForModel("future-internal-model-2030"))
	if !filter(root) || filter(other) {
		t.Fatal("existing root did not retain exclusive model exemption")
	}
	if selected, _ := handler.store.NextForSessionWithFilter(key, 101, map[int64]bool{root.ID(): true}, filter); selected != nil {
		handler.store.Release(selected)
		t.Fatal("excluded root account escaped to another account")
	}
}
