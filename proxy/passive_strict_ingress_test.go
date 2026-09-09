package proxy

import (
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

func TestIndependentBackgroundIngressCannotReachUpstream(test *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(writer, `{"output":[]}`)
	}))
	defer upstream.Close()
	for _, path := range []string{"/v1/responses", "/v1/responses/compact", "/v1/chat/completions", "/v1/messages", "websocket"} {
		test.Run(path, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			handler.store.AddAccount(&auth.Account{DBID: 1706, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "test", Models: []string{"gpt-5.6-sol"}})
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
				RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: promptSessionTestFingerprint(test.Name()),
				ThreadSource: "agent_created_thread", RequestKind: "turn", SessionAccounting: newAPISessionAccountingBypass, PassiveFeature: newAPIPassiveFeatureIndependent}
			body := []byte(`{"model":"gpt-5.6-sol","input":"background","messages":[{"role":"user","content":"background"}],"max_tokens":16}`)
			if path != "websocket" {
				requestContext, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, path, body, meta)
				endpoints := map[string]func(*gin.Context){"/v1/responses": handler.Responses, "/v1/responses/compact": handler.ResponsesCompact, "/v1/chat/completions": handler.ChatCompletions, "/v1/messages": handler.Messages}
				endpoints[path](requestContext)
				if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "codex_background_root_unavailable") {
					test.Fatalf("unexpected response: %d %s", recorder.Code, recorder.Body.String())
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
				if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"background"}`)); err != nil {
					test.Fatal(err)
				}
				_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, payload, err := connection.ReadMessage()
				if err != nil {
					test.Fatal(err)
				}
				if !strings.Contains(string(payload), "codex_background_root_unavailable") {
					test.Fatalf("unexpected websocket response: %s", payload)
				}
			}
			if calls.Load() != 0 {
				test.Fatal("unassociated background reached an upstream")
			}
		})
	}
}

func TestPassiveAssociationDiagnosticsKeepOriginalAndMainRoots(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	mainFingerprint := promptSessionTestFingerprint("main")
	backgroundFingerprint := promptSessionTestFingerprint("independent")
	candidates := 1
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
		RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: mainFingerprint,
		ThreadSource: "agent_created_thread", RequestKind: "turn", PassiveFeature: newAPIPassiveFeatureRelatedInternal,
		OriginalRootFingerprint: backgroundFingerprint, RootAssociation: "same_scope_unique", RootCandidateCount: &candidates}
	body := []byte(`{"model":"gpt-5.6-sol","input":"background"}`)
	requestContext, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(requestContext, body)
	identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
	resolution := usageRequestDiagnosticState(requestContext).Resolved
	if !identity.requiresRootAccount || !identity.relatedToRoot || resolution.OriginalRootFingerprint != backgroundFingerprint || resolution.RootFingerprint != mainFingerprint {
		test.Fatalf("lost association evidence: %+v", resolution)
	}
	if resolution.RootAssociation != "same_scope_unique" || resolution.RootCandidateCount == nil || *resolution.RootCandidateCount != 1 {
		test.Fatalf("lost candidate evidence: %+v", resolution)
	}
	if gjson.GetBytes(body, "input").String() != "background" {
		test.Fatal("request body was rewritten")
	}
}
