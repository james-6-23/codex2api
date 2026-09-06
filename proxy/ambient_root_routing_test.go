package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestAmbientSuggestionAccountRoutingRequiresBoundRoot(test *testing.T) {
	for _, scenario := range []string{"bound", "missing", "independent", "unavailable", "bypass disabled"} {
		test.Run(scenario, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			root := &auth.Account{DBID: 17, AccessToken: "root", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
			other := &auth.Account{DBID: 18, AccessToken: "other", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
			handler.store.AddAccounts([]*auth.Account{root, other})
			rootFingerprint := promptSessionTestFingerprint(test.Name())
			rootKey := sessionAffinityKey("newapi-root-session:"+rootFingerprint, 101)
			if scenario == "bound" || scenario == "bypass disabled" {
				handler.store.BindSessionAffinity(rootKey, root, "")
			}
			handler.store.SetPassiveInternalModelsEnabled(scenario != "bypass disabled")
			meta := newAPIPolicyMeta{
				RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
				RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: rootFingerprint,
				ThreadSource: "ambient_suggestions", RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal,
			}
			if scenario == "independent" {
				meta.RootSessionRelation = newAPIPolicyRootSessionRelationRoot
				meta.PassiveFeature = newAPIPassiveFeatureIndependent
				meta.SessionAccounting = newAPISessionAccountingBypass
			}
			if scenario == "unavailable" {
				meta.RootSessionState = newAPIPolicyRootSessionUnavailable
				meta.RootSessionRelation, meta.RootSessionFingerprint, meta.PassiveFeature = "", "", ""
			}
			body := []byte(`{"model":"gpt-5.6-sol","input":"background task"}`)
			requestContext, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
			handler.primeNewAPIPolicyContext(requestContext, body)
			identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
			if !identity.requiresRootAccount || identity.unlinkedFallbackOnly {
				test.Fatalf("ambient request allowed independent scheduling: %+v", identity)
			}
			key := capacityAwareSessionAffinityKey(identity, 101)
			filter := handler.applyPassiveInternalModelRouting(requestContext, "gpt-5.6-sol", identity, key, true, accountFilterForModel("gpt-5.6-sol"))
			wantRoot := scenario == "bound" || scenario == "bypass disabled"
			if filter(root) != wantRoot || filter(other) {
				test.Fatal("ambient request did not enforce its exact root account")
			}
			futureFilter := handler.applyPassiveInternalModelRouting(requestContext, "future-internal-model", identity, key, true, accountFilterForModel("future-internal-model"))
			if futureFilter(root) != (scenario == "bound") || futureFilter(other) {
				test.Fatal("model exemption changed account binding or ignored its switch")
			}
			if selected, _ := handler.store.NextForSessionWithFilter(key, 101, map[int64]bool{root.ID(): true}, filter); selected != nil {
				handler.store.Release(selected)
				test.Fatal("unavailable root fell back to another account")
			}
		})
	}
}

func TestAmbientSuggestionMissingRootNeverReachesUpstream(test *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstreamCalls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"resp_unexpected","output":[]}`)
	}))
	test.Cleanup(upstream.Close)
	previousWebsocketExecute := WebsocketExecuteFunc
	test.Cleanup(func() { WebsocketExecuteFunc = previousWebsocketExecute })
	WebsocketExecuteFunc = func(context.Context, *auth.Account, []byte, string, string, string, *DeviceProfileConfig, http.Header, string) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	}
	for _, transport := range []string{"http", "websocket"} {
		test.Run(transport, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			other := &auth.Account{DBID: 18, AccessToken: "other", AccountID: "other", Models: []string{"gpt-5.6-sol"}}
			if transport == "http" {
				other.UpstreamType, other.BaseURL, other.APIKey = auth.UpstreamOpenAIResponses, upstream.URL, "other-key"
			}
			handler.store.AddAccount(other)
			meta := newAPIPolicyMeta{
				RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
				RootSessionRelation:    newAPIPolicyRootSessionRelationRelated,
				RootSessionFingerprint: promptSessionTestFingerprint(test.Name()), ThreadSource: "ambient_suggestions",
				RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal,
			}
			if transport == "http" {
				body := []byte(`{"model":"gpt-5.6-sol","input":"background task"}`)
				requestContext, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
				handler.Responses(requestContext)
				if recorder.Code < http.StatusBadRequest {
					test.Fatalf("missing root request succeeded: %s", recorder.Body.String())
				}
			} else {
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
				if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"background task"}`)); err != nil {
					test.Fatal(err)
				}
				_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, payload, err := connection.ReadMessage()
				if err != nil || gjson.GetBytes(payload, "type").String() != "error" {
					test.Fatalf("missing root event=%s error=%v", payload, err)
				}
			}
			if upstreamCalls.Load() != 0 {
				test.Fatal("ambient request reached an unrelated upstream account")
			}
		})
	}
}
