package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type unavailableRootHistoryCache struct {
	cache.TokenCache
}

func (backend unavailableRootHistoryCache) GetRuntime(ctx context.Context, namespace, key string) (json.RawMessage, bool, error) {
	if namespace == unlinkedFallbackRuntimeNamespace {
		return nil, false, errors.New("root history unavailable")
	}
	return backend.TokenCache.GetRuntime(ctx, namespace, key)
}

func (backend unavailableRootHistoryCache) SetRuntime(ctx context.Context, namespace, key string, value json.RawMessage, ttl time.Duration) error {
	if namespace == unlinkedFallbackRuntimeNamespace {
		return errors.New("root history unavailable")
	}
	return backend.TokenCache.SetRuntime(ctx, namespace, key, value, ttl)
}

func TestBoundRootDispatchIgnoresTemporalHistory(test *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		for _, source := range []string{"user", "subagent"} {
			for _, history := range []string{"fresh_other_account", "expired", "missing", "unavailable"} {
				test.Run(transport+"/"+source+"/"+history, func(test *testing.T) {
					handler := newRootlessPassiveModelTestHandler(test)
					var selectedAccount atomic.Int64
					response := `{"id":"resp_bound_root","status":"completed","output":[{"id":"msg_bound_root","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1}}`
					upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
						if request.Header.Get("Authorization") == "Bearer root-key" {
							selectedAccount.Store(17)
						} else {
							selectedAccount.Store(18)
						}
						writer.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(writer, response)
					}))
					test.Cleanup(upstream.Close)
					root := &auth.Account{DBID: 17, AccessToken: "root-token", AccountID: "root-account", Models: []string{"gpt-5.6-sol"}}
					other := &auth.Account{DBID: 18, AccessToken: "other-token", AccountID: "other-account", Models: []string{"gpt-5.6-sol"}}
					if transport == "http" {
						root.UpstreamType, root.BaseURL, root.APIKey = auth.UpstreamOpenAIResponses, upstream.URL, "root-key"
						other.UpstreamType, other.BaseURL, other.APIKey = auth.UpstreamOpenAIResponses, upstream.URL, "other-key"
					} else {
						previousExecute := WebsocketExecuteFunc
						test.Cleanup(func() { WebsocketExecuteFunc = previousExecute })
						WebsocketExecuteFunc = func(_ context.Context, account *auth.Account, _ []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
							selectedAccount.Store(account.ID())
							payload := "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
							return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload))}, nil
						}
					}
					handler.store.AddAccounts([]*auth.Account{root, other})
					rootFingerprint := promptSessionTestFingerprint(test.Name())
					rootKey := sessionAffinityKey("newapi-root-session:"+rootFingerprint, 101)
					handler.store.BindSessionAffinity(rootKey, root, "")
					meta := newAPIPolicyMeta{
						RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
						RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: rootFingerprint,
						ThreadSource: source, RequestKind: "turn",
					}
					if source == "subagent" {
						meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
					}
					method := http.MethodPost
					body := []byte(`{"model":"gpt-5.6-sol","input":"continue"}`)
					if transport == "websocket" {
						method, body = http.MethodGet, nil
					}
					requestContext, recorder := signedRootlessPassiveModelContext(test, method, "/v1/responses", body, meta)
					historyScope := unlinkedFallbackScopeForRequest(requestContext, body, verifiedNewAPIPolicyContext{
						Identity: newAPIIdentity{UserID: "42"}, Platform: "test-platform", MetaVerified: true,
						Meta: newAPIPolicyMeta{TokenID: 7, InstallationID: "rootless-device"},
					}, true)
					if transport == "http" {
						handler.primeNewAPIPolicyContext(requestContext, body)
						identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
						if identity.unlinkedFallbackOnly || identity.unlinkedFallbackScope != historyScope || historyScope == "" {
							test.Fatalf("bound root incorrectly classified: %+v", identity)
						}
					}
					if history == "unavailable" {
						handler.cache = unavailableRootHistoryCache{TokenCache: handler.cache}
					} else if history != "missing" {
						observedAt := time.Now().Add(-time.Minute)
						if history == "expired" {
							observedAt = time.Now().Add(-2 * time.Hour)
						}
						payload, err := json.Marshal(unlinkedFallbackRuntimeRecord{AccountID: other.ID(), ObservedAt: observedAt})
						if err != nil {
							test.Fatal(err)
						}
						if err := handler.cache.SetRuntime(test.Context(), unlinkedFallbackRuntimeNamespace, historyScope, payload, time.Hour); err != nil {
							test.Fatal(err)
						}
					}
					if transport == "http" {
						handler.Responses(requestContext)
						if recorder.Code != http.StatusOK {
							test.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
						}
					} else {
						router := gin.New()
						router.GET("/v1/responses", func(frameContext *gin.Context) {
							frameContext.Set(contextAPIKeyID, int64(101))
							handler.ResponsesWebSocket(frameContext)
						})
						server := httptest.NewServer(router)
						test.Cleanup(server.Close)
						connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", requestContext.Request.Header)
						if err != nil {
							test.Fatal(err)
						}
						test.Cleanup(func() { _ = connection.Close() })
						if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"continue"}`)); err != nil {
							test.Fatal(err)
						}
						_ = connection.SetReadDeadline(time.Now().Add(5 * time.Second))
						_, payload, err := connection.ReadMessage()
						if err != nil {
							test.Fatal(err)
						}
						if gjson.GetBytes(payload, "type").String() != "response.completed" {
							test.Fatalf("unexpected event: %s", payload)
						}
					}
					if selectedAccount.Load() != root.ID() {
						test.Fatalf("request used account %d instead of bound root %d", selectedAccount.Load(), root.ID())
					}
					if bound, found := handler.store.SessionAffinityAccountID(rootKey); !found || bound != root.ID() {
						test.Fatalf("root binding changed: account=%d found=%t", bound, found)
					}
				})
			}
		}
	}
}
