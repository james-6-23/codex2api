package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestSessionModelMismatchReturnsGuidanceWithoutChangingAccount(test *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages", "websocket"} {
		test.Run(path, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			bound := &auth.Account{DBID: 17, AccessToken: "bound", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityIdleTTLSeconds: 3600}
			other := &auth.Account{DBID: 18, AccessToken: "other", Status: auth.StatusReady, Models: []string{"gpt-5.6-terra"}}
			handler.store.AddAccounts([]*auth.Account{bound, other})
			fingerprint := promptSessionTestFingerprint(test.Name())
			rootKey := sessionAffinityKey("newapi-root-session:"+fingerprint, 101)
			handler.store.BindSessionAffinity(rootKey, bound, "")
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: fingerprint, ThreadSource: "user", RequestKind: "turn"}
			body := []byte(`{"type":"response.create","model":"gpt-5.6-terra","input":"switch model","messages":[{"role":"user","content":"switch model"}],"max_tokens":16}`)
			if path == "websocket" {
				router := gin.New()
				router.GET("/v1/responses", func(requestContext *gin.Context) {
					requestContext.Set(contextAPIKeyID, int64(101))
					handler.ResponsesWebSocket(requestContext)
				})
				server := httptest.NewServer(router)
				defer server.Close()
				requestContext, _ := signedRootlessPassiveModelContext(test, http.MethodGet, "/v1/responses", nil, meta)
				connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", requestContext.Request.Header)
				if err != nil {
					test.Fatal(err)
				}
				defer connection.Close()
				if err := connection.WriteMessage(websocket.TextMessage, body); err != nil {
					test.Fatal(err)
				}
				_ = connection.SetReadDeadline(time.Now().Add(3 * time.Second))
				_, response, err := connection.ReadMessage()
				if err != nil || gjson.GetBytes(response, "error.code").String() != "session_model_unavailable" {
					test.Fatalf("unexpected model switch event=%s error=%v", response, err)
				}
			} else {
				requestContext, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, path, body, meta)
				endpoint := map[string]func(*gin.Context){"/v1/responses": handler.Responses, "/v1/responses/compact": handler.ResponsesCompact, "/v1/chat/completions": handler.ChatCompletions, "/v1/messages": handler.Messages}[path]
				endpoint(requestContext)
				if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), sessionModelUnavailableMessage) {
					test.Fatalf("unexpected model switch response=%d %s", recorder.Code, recorder.Body.String())
				}
			}
			if accountID, found := handler.store.AccountSessionAccountID(rootKey, time.Now()); !found || accountID != bound.ID() {
				test.Fatalf("model switch changed the window owner: %d %v", accountID, found)
			}
			if accountID, found := handler.store.SessionAffinityAccountID(rootKey); !found || accountID != bound.ID() {
				test.Fatal("model switch removed the original affinity")
			}
		})
	}
}

func TestSessionModelSupportUsesMappingsNotCooldowns(test *testing.T) {
	relay := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://example.invalid", APIKey: "test", Models: []string{"upstream-model"}, ModelMapping: `{"client-model":"upstream-model"}`}
	for _, compact := range []bool{false, true} {
		if !sessionModelSupportFilter("client-model", "client-model", compact)(relay) {
			test.Fatal("an allowed model mapping was treated as missing")
		}
		if sessionModelSupportFilter("missing", "missing", compact)(relay) {
			test.Fatal("a model missing from the bound account was allowed")
		}
	}
	bound := &auth.Account{DBID: 2, AccessToken: "bound", Status: auth.StatusCooldown, CooldownUtil: time.Now().Add(time.Hour), Models: []string{"model"}}
	if !sessionModelSupportFilter("model", "model", false)(bound) {
		test.Fatal("account cooldown was misreported as unsupported model")
	}
}

func TestSessionModelAffinityPreservesPassiveModelExemption(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	bound := &auth.Account{DBID: 17, AccessToken: "bound", Models: []string{"gpt-5.6-sol"}}
	handler.store.AddAccount(bound)
	fingerprint := promptSessionTestFingerprint(test.Name())
	rootKey := sessionAffinityKey("newapi-root-session:"+fingerprint, 101)
	handler.store.BindSessionAffinity(rootKey, bound, "")
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: fingerprint, ThreadSource: "thread_title", RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal}
	body := []byte(`{"model":"gpt-5.6-terra","input":"title"}`)
	requestContext, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(requestContext, body)
	identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
	beginDispatchSelection(requestContext)
	key := capacityAwareSessionAffinityKey(identity, 101)
	if modelError := handler.configureSessionModelAffinity(requestContext, identity, key, "gpt-5.6-terra", "gpt-5.6-terra", false); modelError != nil {
		test.Fatal("authorized title lost its passive model exemption", modelError)
	}
	filter := handler.applyPassiveInternalModelRouting(requestContext, "gpt-5.6-terra", identity, key, true, accountFilterForModel("gpt-5.6-terra"))
	if !filter(bound) {
		test.Fatal("title cannot reuse its exact root account")
	}
	handler.store.SetPassiveInternalModelsEnabled(false)
	if modelError := handler.configureSessionModelAffinity(requestContext, identity, key, "gpt-5.6-terra", "gpt-5.6-terra", false); modelError == nil {
		test.Fatal("disabled passive model exemption was ignored")
	}
}
