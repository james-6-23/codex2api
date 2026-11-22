package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/config"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestResponsesHTTPRequestCountry(t *testing.T) {
	received := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case received <- readUpstreamRequestBody(r):
		default:
			t.Error("unexpected extra upstream request")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, wsContextTestSSE("resp_country_http"))
	}))
	t.Cleanup(upstream.Close)
	previousResin := GetResinConfig()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})
	t.Cleanup(func() { SetResinConfig(previousResin) })

	previousSettings := CurrentRuntimeSettings()
	settings := previousSettings
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	store := newContinuationCodexStore()
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)

	for _, policy := range []struct{ name, rules, missing string }{
		{"passthrough", "{}", ""},
		{"default_JP", `{"default":[{"params":{"request_country":"JP"}}]}`, `"JP"`},
	} {
		t.Run(policy.name, func(t *testing.T) {
			withPayloadRules(t, policy.rules)
			for _, tc := range []struct{ name, field, want string }{
				{"omitted", "", policy.missing},
				{"unknown", `,"request_country":null`, "null"},
				{"known", `,"request_country":"US"`, `"US"`},
			} {
				t.Run(tc.name, func(t *testing.T) {
					body := `{"model":"gpt-5.5","input":"hello"` + tc.field + `}`
					req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/responses", strings.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
					resp := httptest.NewRecorder()
					router.ServeHTTP(resp, req)
					if resp.Code != http.StatusOK {
						t.Fatalf("Responses returned %d: %s", resp.Code, resp.Body.String())
					}
					select {
					case body := <-received:
						got := gjson.GetBytes(body, "request_country")
						if got.Exists() != (tc.want != "") || got.Raw != tc.want {
							t.Fatalf("upstream request_country = %q, want %q", got.Raw, tc.want)
						}
					default:
						t.Fatal("no upstream HTTP request")
					}
				})
			}
		})
	}
}
