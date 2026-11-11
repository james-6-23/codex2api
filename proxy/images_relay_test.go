package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// issue #820：账号池只配置 OpenAI Responses 中转（api key 接入）账号时，
// 生图不应报「无可用账号」，请求应路由到中转自己的 /responses 出站。
func TestImagesOpenAIResponsesRelayAccountSelectedAndRouted(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
	settings := DefaultRuntimeSettings()
	settings.CodexImagesMainModel = "gpt-5.4"
	settings.CodexForceWebsocket = false
	settings.CodexRequestCompression = false
	settings.CodexTelemetryEnabled = false
	ApplyRuntimeSettings(settings)

	var gotPath atomic.Value
	var gotAuth atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotAuth.Store(r.Header.Get("Authorization"))
		_, _ = io.Copy(io.Discard, r.Body)
		writeKeepaliveImageResult(w)
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, MaxRetries: 1, MaxRateLimitRetries: 0, RetryIntervalMS: 0,
	})
	t.Cleanup(store.Stop)
	// 账号池只有 api key 接入的 OpenAI Responses 中转账号（issue #820 场景）。
	account := &auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL: upstream.URL, APIKey: "relay-key", GroupIDs: []int64{10},
	}
	if !account.IsOpenAIResponsesAPI() {
		t.Fatalf("account should be classified as OpenAI Responses relay")
	}
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(contextAPIKeyID, int64(991037))
		c.Set(contextAPIKeyRow, &database.APIKeyRow{
			ID: 991037, Limits: database.APIKeyLimits{MaxConcurrency: 1},
		})
		c.Set(contextScopeBudgetGate, newScopeBudgetGate(991037, []database.APIKeyScopeLimit{
			{ScopeType: database.APIKeyScopeTypeGroup, ScopeID: 10, MaxConcurrency: 2},
		}))
		c.Next()
	})
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	body := []byte(`{"model":"gpt-image-2","prompt":"draw a test image"}`)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/images/generations", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", response.StatusCode, string(payload))
	}
	if !bytes.Contains(payload, []byte(tinyPNGBase64)) {
		t.Fatalf("expected image output in response, got: %s", string(payload))
	}
	if path, _ := gotPath.Load().(string); path != "/v1/responses" {
		t.Fatalf("expected upstream path /v1/responses, got %q", path)
	}
	if authHeader, _ := gotAuth.Load().(string); authHeader != "Bearer relay-key" {
		t.Fatalf("expected relay API key auth header, got %q", authHeader)
	}
}

func TestImageCapableAccountFilterAllowsOpenAIResponsesRelay(t *testing.T) {
	relay := &auth.Account{UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "k"}
	if !imageCapableAccountFilter(relay) {
		t.Fatalf("OpenAI Responses relay account should be image capable")
	}
	if imagePreferredAccountFilter(relay) {
		t.Fatalf("relay account without plan should stay in fallback tier")
	}
	grok := &auth.Account{UpstreamType: "grok", APIKey: "xai-key"}
	if imageCapableAccountFilter(grok) {
		t.Fatalf("Grok account should stay excluded")
	}
	codex := &auth.Account{AccessToken: "codex-token", PlanType: "plus"}
	if !imageCapableAccountFilter(codex) || !imagePreferredAccountFilter(codex) {
		t.Fatalf("Codex account should remain image capable and preferred")
	}
	if imageCapableAccountFilter(nil) {
		t.Fatalf("nil account should be rejected")
	}
}
