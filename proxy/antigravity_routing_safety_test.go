package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestAntigravityIsExcludedFromCodexOnlyTransports(t *testing.T) {
	account := &auth.Account{
		DBID:                 77,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-access-token",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
		Models:               []string{"gemini-2.5-flash"},
	}

	compactFilter := accountFilterForCompactResponsesModelWithOriginal("gemini-2.5-flash", "gemini-2.5-flash", false)
	if compactFilter(account) {
		t.Fatal("Responses Compact admitted an Antigravity account")
	}
}

// Chat/Messages translate their inbound body into a Responses payload before
// dispatch, which is exactly what the Antigravity adapter consumes. Admission
// must therefore follow the same catalog gate as /v1/responses; a blanket
// transport exclusion left every advertised Antigravity model unroutable on
// those endpoints (issue #595).
func TestAntigravityIsAdmittedOnChatAndMessagesTransports(t *testing.T) {
	account := &auth.Account{
		DBID:                 81,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-access-token",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
		Models:               auth.AntigravityDefaultModelIDs(),
	}

	for _, model := range AntigravityPublishedModelIDs(auth.AntigravityDefaultModelIDs()) {
		if !accountFilterForResponsesModelWithOriginal(model, model, false)(account) {
			t.Fatalf("Chat transport filter rejected Antigravity model %q", model)
		}
		if !accountFilterForResponsesModel(model, false)(account) {
			t.Fatalf("Messages transport filter rejected Antigravity model %q", model)
		}
	}

	if accountFilterForResponsesModel("gpt-5.1-codex", false)(account) {
		t.Fatal("Antigravity account was admitted for a model it cannot serve")
	}
}

func TestOfficialCodexExecutorsRejectAntigravityBeforeNetwork(t *testing.T) {
	account := &auth.Account{
		DBID:                 78,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "must-never-leave-process",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
	}

	assertNoAccount := func(name string, err error) {
		t.Helper()
		var apiErr *Error
		if !errors.As(err, &apiErr) || apiErr.Code != ErrorCodeNoAvailableAccount {
			t.Fatalf("%s error = %#v, want no_available_account", name, err)
		}
	}

	_, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gemini-2.5-flash","input":"hello"}`), "", "http://127.0.0.1:1", "sk-local", nil, http.Header{}, false)
	assertNoAccount("ExecuteRequest", err)

	_, err = ExecuteCompactRequest(context.Background(), account, []byte(`{"model":"gemini-2.5-flash","input":"hello"}`), "", "http://127.0.0.1:1", "sk-local", nil, http.Header{})
	assertNoAccount("ExecuteCompactRequest", err)
}

func TestAntigravityAPIKeyDispatchIsExperimentalAndFailClosed(t *testing.T) {
	account := &auth.Account{
		DBID:         79,
		UpstreamType: auth.UpstreamAntigravity,
		APIKey:       "google-api-key",
		Models:       []string{"gemini-3.6-flash-low"},
	}

	t.Setenv(auth.AntigravityExperimentalInteractionsEnv, "")
	if account.AntigravityDispatchEnabled() {
		t.Fatal("API-key Interactions dispatch enabled without explicit opt-in")
	}
	if antigravityChannelAccountFilter("gemini-3.6-flash-low")(account) {
		t.Fatal("disabled API-key Interactions account entered scheduling")
	}

	t.Setenv(auth.AntigravityExperimentalInteractionsEnv, "true")
	if !account.AntigravityDispatchEnabled() {
		t.Fatal("API-key Interactions dispatch stayed disabled after explicit opt-in")
	}
	if !antigravityChannelAccountFilter("gemini-3.6-flash-low")(account) {
		t.Fatal("explicitly enabled API-key Interactions account was not schedulable")
	}
}

func TestAntigravityDefaultModelsAreListedAndAccepted(t *testing.T) {
	account := &auth.Account{
		DBID:                 80,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-access-token",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(account)
	handler := &Handler{store: store}

	listed := make(map[string]bool)
	for _, id := range handler.supportedModelIDs(context.Background()) {
		listed[id] = true
	}
	for _, model := range AntigravityPublishedModelIDs(auth.AntigravityDefaultModelIDs()) {
		if !listed[model] {
			t.Fatalf("supported model list omitted Antigravity default %q", model)
		}
		if !accountFilterForResponsesModel(model, true)(account) {
			t.Fatalf("Responses validator rejected listed Antigravity default %q", model)
		}
	}
}

func TestModelSupportedByAccountMappingIgnoresAntigravityAliases(t *testing.T) {
	t.Run("antigravity aliases are not request models", func(t *testing.T) {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
		store.AddAccount(&auth.Account{
			DBID:                 801,
			UpstreamType:         auth.UpstreamAntigravity,
			AccessToken:          "google-access-token",
			RefreshToken:         "google-refresh-token",
			AntigravityProjectID: "google-project",
			Models:               []string{"gemini-3.7-flash-tiered"},
			ModelMapping:         `{"legacy-flash":"gemini-3.7-flash-medium"}`,
		})
		handler := &Handler{store: store}

		if handler.modelSupportedByAccountMapping("legacy-flash") {
			t.Fatal("Antigravity account model_mapping expanded the public request catalog")
		}
	})

	t.Run("ordinary relay aliases remain supported", func(t *testing.T) {
		store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
		store.AddAccount(&auth.Account{
			DBID:         802,
			UpstreamType: auth.UpstreamOpenAIResponses,
			BaseURL:      "https://relay.example",
			APIKey:       "relay-key",
			Models:       []string{"relay-target"},
			ModelMapping: `{"relay-alias":"relay-target"}`,
		})
		handler := &Handler{store: store}

		if !handler.modelSupportedByAccountMapping("relay-alias") {
			t.Fatal("ordinary OpenAI Responses relay alias was unexpectedly disabled")
		}
	})
}

func TestAntigravityChannelAcceptsOnlyPublicModelsBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer upstream.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{upstream.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	account := &auth.Account{
		DBID:                 803,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-access-token",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
		Models:               []string{"gemini-3.7-flash-tiered"},
		ModelMapping:         `{"account-flash":"gemini-3.7-flash-medium"}`,
		HealthTier:           auth.HealthTierHealthy,
		Status:               auth.StatusReady,
	}
	store.AddAccount(account)
	store.SetCodexModelMapping(`{"global-flash":"gemini-3.7-flash-medium"}`)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	request := func(model string) *httptest.ResponseRecorder {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+model+`","input":"hi","stream":false}`))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Set(contextAPIKeyRow, &database.APIKeyRow{
			ID: 91,
			Limits: database.APIKeyLimits{
				UpstreamChannel: database.UpstreamChannelAntigravity,
			},
		})
		handler.Responses(c)
		return recorder
	}

	for _, model := range []string{
		"account-flash",
		"global-flash",
		"gemini-3.7-flash-tiered",
		"gemini-2.5-flash",
	} {
		recorder := request(model)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("model %q status=%d body=%s, want 400 before dispatch", model, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "not supported") {
			t.Fatalf("model %q body=%s, want unsupported-model validation error", model, recorder.Body.String())
		}
	}
	if hits := upstreamHits.Load(); hits != 0 {
		t.Fatalf("invalid Antigravity model names reached upstream %d times", hits)
	}

	publicModel := "gemini-3.7-flash-medium"
	if !antigravityChannelAccountFilter(publicModel)(account) {
		t.Fatal("public Antigravity model was not resolved to its synchronized wire backing")
	}
	if antigravityChannelAccountFilter("gemini-3.7-flash-tiered")(account) {
		t.Fatal("raw Antigravity wire model was admitted as a public request model")
	}
	wrongBacking := &auth.Account{
		UpstreamType: auth.UpstreamAntigravity,
		AccessToken:  "google-access-token",
		Models:       []string{publicModel},
	}
	if antigravityChannelAccountFilter(publicModel)(wrongBacking) {
		t.Fatal("public model name in the raw catalog bypassed public-to-wire capability checking")
	}
}

func TestAntigravityImageModelsAreNotAdvertisedByTextResponsesBridge(t *testing.T) {
	account := &auth.Account{
		DBID:                 81,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-access-token",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
		Models:               []string{"gemini-3.6-flash-low", "gemini-3.1-flash-image"},
		ModelMapping:         `{"image-alias":"gemini-3.1-flash-image"}`,
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(account)
	handler := &Handler{store: store}

	for _, id := range handler.supportedModelIDs(context.Background()) {
		if id == "gemini-3.1-flash-image" || id == "image-alias" {
			t.Fatal("image-only model leaked into the Responses text catalog")
		}
	}
	if antigravityChannelAccountFilter("gemini-3.1-flash-image")(account) {
		t.Fatal("image-only model was admitted to the text Responses route")
	}
	if relayAccountSupportsModel(account, "gemini-3.1-flash-image") {
		t.Fatal("generic relay admission accepted an Antigravity image model")
	}
	if !antigravityChannelAccountFilter("gemini-3.6-flash-low")(account) {
		t.Fatal("text model was rejected by the Antigravity Responses route")
	}
}

func TestMixedCodexManifestIncludesAntigravityModels(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "codex-token", PlanType: "plus"})
	store.AddAccount(&auth.Account{
		DBID:                 2,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-token",
		RefreshToken:         "google-refresh",
		AntigravityProjectID: "google-project",
		Models:               []string{"gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-3.6-flash-high"},
		ModelMapping:         `{"flash-alias":"gemini-3.6-flash-low"}`,
	})
	handler := &Handler{store: store}
	extras := handler.extraRelayManifestModels(context.Background(), &database.APIKeyRow{ID: 1})
	gotModels := map[string]bool{}
	for _, model := range extras {
		if model.OwnedBy == "google" {
			gotModels[model.ID] = true
		}
		if model.ID == "flash-alias" {
			t.Fatal("Antigravity alias leaked into the native manifest")
		}
	}
	if !gotModels["gemini-3.6-flash-low"] || !gotModels["gemini-3.6-flash-medium"] || !gotModels["gemini-3.6-flash-high"] {
		t.Fatalf("mixed manifest extras omitted Antigravity public model: %#v", extras)
	}
}

func TestAntigravityResponsesHandlerPreservesRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted","type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{upstream.URL, upstream.URL, upstream.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 1, MaxRateLimitRetries: 1})
	store.AddAccount(&auth.Account{
		DBID: 85, UpstreamType: auth.UpstreamAntigravity,
		AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project",
		Models: []string{"gemini-3.6-flash-low"}, HealthTier: auth.HealthTierHealthy, Status: auth.StatusReady,
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gemini-3.6-flash-low","input":"hello","stream":false}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s, want 429", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "17" {
		t.Fatalf("Retry-After = %q, want upstream value 17", got)
	}
}

func TestAntigravityAdapterConvertsStructuredTextOutput(t *testing.T) {
	payload, err := responsesToGeminiInternal([]byte(`{
		"model":"gemini-3.6-flash-low",
		"input":"hello",
		"stream":false,
		"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}}
	}`), "google-project", "gemini-3.6-flash-low")
	if err != nil {
		t.Fatal(err)
	}
	request, _ := payload["request"].(map[string]any)
	generation, _ := request["generationConfig"].(map[string]any)
	if generation["responseMimeType"] != "application/json" {
		t.Fatalf("generation config = %#v", generation)
	}
	schema, _ := generation["responseSchema"].(map[string]any)
	if schema["type"] != "OBJECT" {
		t.Fatalf("response schema = %#v", schema)
	}
}

func TestAntigravityIncompleteStreamIsTerminalWithoutAccountPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"partial\"}]},\"finishReason\":\"MAX_TOKENS\"}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":3,\"totalTokenCount\":5}}\n\n")
	}))
	defer upstream.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{upstream.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{
		DBID: 87, UpstreamType: auth.UpstreamAntigravity,
		AccessToken: "google-token", RefreshToken: "google-refresh", AntigravityProjectID: "google-project",
		Models: []string{"gemini-3.7-flash-tiered"}, HealthTier: auth.HealthTierHealthy, Status: auth.StatusReady,
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 1})
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gemini-3.7-flash-medium","input":"hello","stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"type":"response.incomplete"`) {
		t.Fatalf("status=%d body=%s, want response.incomplete", recorder.Code, body)
	}
	if strings.Contains(body, "upstream_stream_break") || strings.Contains(body, "event: response.failed") {
		t.Fatalf("incomplete response was converted into a stream failure: %s", body)
	}
	if !account.IsAvailable() || !account.ModelCatalogEligible() {
		t.Fatal("MAX_TOKENS incomplete response penalized the Antigravity account")
	}
}
