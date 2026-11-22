package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestValidateCodexRelayResponsesCompatibility(t *testing.T) {
	tests := []struct {
		name, fields, rejected string
	}{
		{"discarded metadata", `"metadata":{"tenant":"demo"}`, "metadata"},
		{"discarded context policy", `"context_management":{"compact_threshold":1000}`, "context_management"},
		{"discarded verbosity", `"verbosity":"low"`, "verbosity"},
		{"async background", `"background":true`, "background"},
		{"sync background", `"background":false`, ""},
		{"nullable background", `"background":null`, ""},
		{"image background extension", `"background":"transparent"`, ""},
		{"conversation", `"conversation":"conv_demo"`, "conversation"},
		{"nullable conversation", `"conversation":null`, ""},
		{"server storage", `"store":true`, "store"},
		{"stateless", `"store":false`, ""},
		{"service tier priority", `"service_tier":"priority"`, ""},
		{"service tier alias", `"service_tier":"fast"`, ""},
		{"service tier default", `"service_tier":"default"`, "service_tier"},
		{"service tier flex", `"service_tier":"flex"`, "service_tier"},
		{"valid effort alias", `"reasoning_effort":"high"`, ""},
		{"clamped effort", `"reasoning":{"effort":"unknown"}`, "reasoning.effort"},
		{"clamped effort alias", `"reasoning_effort":"unknown"`, "reasoning_effort"},
		{"translated format", `"response_format":{"type":"json_object"}`, ""},
		{"untranslated format", `"response_format":{"type":"unknown"}`, "response_format"},
		{"nullable metadata", `"metadata":null`, ""},
		{"native text verbosity", `"text":{"verbosity":"low"}`, ""},
		{"preserved tools", `"tools":[],"reasoning":{"effort":"high"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCodexRelayResponsesCompatibility([]byte(`{"model":"gpt-6-astra","input":"hi",` + tc.fields + `}`))
			if tc.rejected == "" {
				if err != nil {
					t.Fatalf("unexpected rejection: %#v", err)
				}
				return
			}
			if err == nil || err.Code != api.ErrCodeUnsupportedFeature {
				t.Fatalf("error = %#v", err)
			}
			details, ok := err.Details.(map[string]any)
			if !ok {
				t.Fatalf("details = %#v", err.Details)
			}
			fields, ok := details["fields"].([]map[string]string)
			if !ok || len(fields) != 1 || fields[0]["field"] != tc.rejected {
				t.Fatalf("fields = %#v", fields)
			}
		})
	}
}

const strictCompatibilitySSE = "data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_strict\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

// Exercise the actual handler, selected account and executor against a loopback
// upstream: channel labels cannot decide whether native fields may be retained.
func TestStrictResponsesCompatibilityHTTPDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("CODEX_STRICT_RESPONSES_COMPAT", "")
	previous := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, endpoint := range []string{"responses", "compact"} {
		for _, channel := range []string{database.UpstreamChannelAuto, database.UpstreamChannelCodex} {
			for _, native := range []bool{true, false} {
				for _, strict := range []bool{false, true} {
					name := endpoint + "/" + channel + "/"
					if native {
						name += "native"
					} else {
						name += "relay"
					}
					if strict {
						name += "/strict"
					} else {
						name += "/default"
					}
					t.Run(name, func(t *testing.T) {
						var calls atomic.Int32
						var seen []byte
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							seen = readUpstreamRequestBody(r)
							if strings.HasSuffix(r.URL.Path, "/compact") {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, `{"id":"resp_compact","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
							} else {
								w.Header().Set("Content-Type", "text/event-stream")
								_, _ = io.WriteString(w, strictCompatibilitySSE)
							}
						}))
						defer upstream.Close()
						previousResin := resinCfg.Load()
						SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
						defer resinCfg.Store(previousResin)
						store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
						defer store.Stop()
						account := &auth.Account{DBID: 1, AccessToken: "local-test", PlanType: "pro", Models: []string{"gpt-5.5"}}
						if !native {
							account.UpstreamType = auth.UpstreamOpenAIResponses
							account.BaseURL, account.APIKey, account.PlanType = upstream.URL, "local-test", "api"
						}
						store.AddAccount(account)
						h := NewHandler(store, nil, nil, nil)
						body := []byte(`{"model":"gpt-5.5","input":"hi","stream":true,"metadata":{"tenant":"demo"},"background":false}`)
						handler := h.Responses
						if endpoint == "compact" {
							handler = h.ResponsesCompact
						}
						response := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
							c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{UpstreamChannel: channel}})
							if strict {
								c.Request.Header.Set(strictResponsesCompatibilityHeader, "true")
							}
						}, handler, body)
						if native && strict {
							if response.Code != http.StatusBadRequest || gjson.GetBytes(response.Body.Bytes(), "error.code").String() != string(api.ErrCodeUnsupportedFeature) || calls.Load() != 0 {
								t.Fatalf("native strict response=%d %s calls=%d", response.Code, response.Body.String(), calls.Load())
							}
						} else {
							if response.Code != http.StatusOK || calls.Load() != 1 {
								t.Fatalf("response=%d %s calls=%d", response.Code, response.Body.String(), calls.Load())
							}
							if got := gjson.GetBytes(seen, "metadata.tenant"); got.Exists() == native {
								t.Fatalf("wrong metadata preservation: native=%v body=%s", native, seen)
							}
							if !native && gjson.GetBytes(seen, "metadata.tenant").String() != "demo" {
								t.Fatalf("relay lost metadata: %s", seen)
							}
							if native && gjson.GetBytes(seen, "background").Exists() {
								t.Fatalf("native synchronous background not normalized: %s", seen)
							}
							gjson.GetBytes(seen, "tools").ForEach(func(_, tool gjson.Result) bool {
								if tool.Get("background").Exists() {
									t.Errorf("boolean background misrouted into image tool: %s", tool.Raw)
								}
								return true
							})
						}
						if account.ActiveRequests.Load() != 0 {
							t.Fatalf("account lease leaked: %d", account.ActiveRequests.Load())
						}
					})
				}
			}
		}
	}
}

func TestStrictResponsesCompatibilityPreservesErrorAfterSSECommit(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Header("Content-Type", "text/event-stream")
	_, _ = c.Writer.WriteString(": keepalive\n\n")
	sendResponsesCompatibilityError(c, validateCodexRelayResponsesCompatibility([]byte(`{"metadata":{"id":1}}`)))
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"response.failed"`) || !strings.Contains(body, `"code":"unsupported_feature"`) || !strings.Contains(body, `"field":"metadata"`) {
		t.Fatalf("committed SSE lost structured compatibility error: %s", body)
	}
}

func TestStrictResponsesCompatibilityWebSocketRelayPreservesFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetOpenAIResponsesWebsocketPool()
	t.Cleanup(resetOpenAIResponsesWebsocketPool)
	seen := make(chan []byte, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		seen <- payload
		_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_strict_ws","status":"completed","output":[]}}`))
		// Leave the pooled upstream connection usable until the downstream closes.
		_, _, _ = conn.ReadMessage()
	}))
	defer upstream.Close()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "local-only", PlanType: "api", Models: []string{"gpt-5.5"}, ResponsesUpstreamTransport: auth.OpenAIResponsesTransportWebsocket}
	store.AddAccount(account)
	h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	h.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()
	headers := http.Header{strictResponsesCompatibilityHeader: []string{"true"}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	payload := `{"type":"response.create","model":"gpt-5.5","input":"hi","store":true,"metadata":{"tenant":"demo"},"max_output_tokens":100,"background":false,"text":{"verbosity":"low"}}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(event, "type").String() != "response.completed" {
		t.Fatalf("event=%s", event)
	}
	select {
	case sent := <-seen:
		var want, got map[string]any
		_ = json.Unmarshal([]byte(payload), &want)
		_ = json.Unmarshal(sent, &got)
		for _, field := range []string{"store", "metadata", "max_output_tokens", "background", "text"} {
			w, _ := json.Marshal(want[field])
			g, _ := json.Marshal(got[field])
			if string(w) != string(g) {
				t.Errorf("relay changed %s: got %s want %s body=%s", field, g, w, sent)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("relay upstream did not receive request")
	}
	conn.Close()
	resetOpenAIResponsesWebsocketPool()
}

func TestStrictResponsesCompatibilityWebSocketNativeSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previous })
	for _, tc := range []struct {
		name, fields string
		allowed      bool
	}{
		{"metadata", `"metadata":{"tenant":"demo"}`, false},
		{"async", `"background":true`, false},
		{"sync", `"background":false`, true},
		{"nullable background", `"background":null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			WebsocketExecuteFunc = func(_ context.Context, _ *auth.Account, body []byte, _, _, _ string, _ *DeviceProfileConfig, _ http.Header, _ string) (*http.Response, error) {
				calls.Add(1)
				if gjson.GetBytes(body, "background").Exists() {
					t.Errorf("native synchronous background should be omitted: %s", body)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(strictCompatibilitySSE))}, nil
			}
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
			defer store.Stop()
			account := &auth.Account{DBID: 1, AccessToken: "local-only", PlanType: "pro"}
			store.AddAccount(account)
			h := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
			router := gin.New()
			h.RegisterRoutes(router)
			server := httptest.NewServer(router)
			defer server.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{strictResponsesCompatibilityHeader: []string{"true"}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.5","input":"hi",`+tc.fields+`}`)); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			for {
				_, event, err := conn.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				typ := gjson.GetBytes(event, "type").String()
				if tc.allowed && typ == "response.completed" {
					if calls.Load() != 1 {
						t.Fatalf("upstream calls=%d", calls.Load())
					}
					break
				}
				if !tc.allowed {
					if typ != "error" || gjson.GetBytes(event, "error.code").String() != string(api.ErrCodeUnsupportedFeature) || calls.Load() != 0 {
						t.Fatalf("event=%s upstream calls=%d", event, calls.Load())
					}
					if account.ActiveRequests.Load() != 0 {
						t.Fatal("strict rejection leaked selected account")
					}
					break
				}
			}
		})
	}
}
