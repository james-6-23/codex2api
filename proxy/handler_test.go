package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestSupportedModelsIncludeLatestRequestedModels(t *testing.T) {
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex-spark", "gpt-5.2", "gpt-image-2", "gpt-image-2-2k", "gpt-image-2-4k"} {
		if !slices.Contains(SupportedModels, model) {
			t.Fatalf("SupportedModels missing %q", model)
		}
	}
}

func TestSupportedModelsExcludeBelowGPT52(t *testing.T) {
	for _, model := range []string{
		"gpt-5", "gpt-5-codex", "gpt-5-codex-mini",
		"gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-mini", "gpt-5.1-codex-max",
		"gpt-5.2-codex",
	} {
		if slices.Contains(SupportedModels, model) {
			t.Fatalf("SupportedModels should not include %q", model)
		}
	}
}

func TestListModelsIncludesLatestRequestedModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	handler := &Handler{}

	handler.ListModels(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	ids := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		ids = append(ids, model.ID)
	}
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex-spark", "gpt-5.2", "gpt-image-2"} {
		if !slices.Contains(ids, model) {
			t.Fatalf("/v1/models missing %q in %v", model, ids)
		}
	}
	for _, model := range []string{"gpt-image-2-2k", "gpt-image-2-4k"} {
		if !slices.Contains(ids, model) {
			t.Fatalf("/v1/models missing image alias %q in %v", model, ids)
		}
	}

	for _, model := range []string{"gpt-5", "gpt-5.1", "gpt-5.2-codex"} {
		if slices.Contains(ids, model) {
			t.Fatalf("/v1/models should not include %q in %v", model, ids)
		}
	}
}

func TestImageModelIsImageEndpointOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	sendImageOnlyModelError(ctx, "gpt-image-2")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(recorder.Body.String(), "/v1/images/generations") {
		t.Fatalf("error body should point to images endpoints: %s", recorder.Body.String())
	}
}

func TestRegisterRoutesIncludesCodexDirectResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	handler := &Handler{}

	handler.RegisterRoutes(router)

	postRoutes := make(map[string]bool)
	getRoutes := make(map[string]bool)
	for _, route := range router.Routes() {
		if route.Method == http.MethodPost {
			postRoutes[route.Path] = true
		}
		if route.Method == http.MethodGet {
			getRoutes[route.Path] = true
		}
	}

	for _, path := range []string{
		"/backend-api/codex/responses",
		"/backend-api/codex/responses/*subpath",
	} {
		if !postRoutes[path] {
			t.Fatalf("expected POST route %s to be registered; routes=%v", path, postRoutes)
		}
	}
	for _, path := range []string{
		"/v1/responses",
		"/responses",
		"/backend-api/codex/responses",
	} {
		if !getRoutes[path] {
			t.Fatalf("expected GET route %s to be registered; routes=%v", path, getRoutes)
		}
	}
}

func TestNormalizeResponsesWebSocketClientPayload(t *testing.T) {
	t.Run("defaults response create type", func(t *testing.T) {
		got, model, apiErr := normalizeResponsesWebSocketClientPayload([]byte(`{"model":"gpt-5.4","input":"hi"}`))
		if apiErr != nil {
			t.Fatalf("unexpected error: %v", apiErr)
		}
		if model != "gpt-5.4" {
			t.Fatalf("model = %q, want gpt-5.4", model)
		}
		if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.create" {
			t.Fatalf("type = %q, want response.create; body=%s", eventType, got)
		}
	})

	t.Run("rejects append", func(t *testing.T) {
		_, _, apiErr := normalizeResponsesWebSocketClientPayload([]byte(`{"type":"response.append","model":"gpt-5.4"}`))
		if apiErr == nil || !strings.Contains(apiErr.Message, "response.append") {
			t.Fatalf("error = %#v, want response.append rejection", apiErr)
		}
	})

	t.Run("rejects message previous response id", func(t *testing.T) {
		_, _, apiErr := normalizeResponsesWebSocketClientPayload([]byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"msg_123"}`))
		if apiErr == nil || !strings.Contains(apiErr.Message, "response.id") {
			t.Fatalf("error = %#v, want previous_response_id rejection", apiErr)
		}
	})
}

func TestResponsesWebSocketForwardsResponsesEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
	})

	bodyCh := make(chan []byte, 2)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header) (*http.Response, error) {
		bodyCh <- append([]byte(nil), requestBody...)
		sse := "" +
			`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"service_tier":"default"}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","previous_response_id":"resp_prev","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case gotBody := <-bodyCh:
		if gjson.GetBytes(gotBody, "type").String() != "response.create" {
			t.Fatalf("upstream type missing: %s", gotBody)
		}
		if prev := gjson.GetBytes(gotBody, "previous_response_id").String(); prev != "resp_prev" {
			t.Fatalf("previous_response_id = %q, want resp_prev; body=%s", prev, gotBody)
		}
		if store := gjson.GetBytes(gotBody, "store"); store.Exists() {
			t.Fatalf("websocket ingress should not force store=false, got %s; body=%s", store.Raw, gotBody)
		}
		if !gjson.GetBytes(gotBody, "stream").Bool() {
			t.Fatalf("upstream stream should be true: %s", gotBody)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read first event: %v", err)
	}
	if eventType := gjson.GetBytes(first, "type").String(); eventType != "response.output_text.delta" {
		t.Fatalf("first event type = %q body=%s", eventType, first)
	}
	_, second, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read terminal event: %v", err)
	}
	if eventType := gjson.GetBytes(second, "type").String(); eventType != "response.completed" {
		t.Fatalf("terminal event type = %q body=%s", eventType, second)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"again"}`)); err != nil {
		t.Fatalf("write second request: %v", err)
	}
	select {
	case <-bodyCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second upstream request")
	}
}

func assertNoAvailableAccountResponse(t *testing.T, body []byte) {
	t.Helper()

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, string(body))
	}
	if payload.Error.Message == "" {
		t.Fatalf("message is empty; body=%s", string(body))
	}
	if payload.Error.Type != ErrorTypeServerError {
		t.Fatalf("type = %q, want %q", payload.Error.Type, ErrorTypeServerError)
	}
	if payload.Error.Code != ErrorCodeNoAvailableAccount {
		t.Fatalf("code = %q, want %q", payload.Error.Code, ErrorCodeNoAvailableAccount)
	}
}

func TestUsageLogErrorMessageExtractsStructuredError(t *testing.T) {
	body := []byte(`{"error":{"code":"rate_limit_exceeded","type":"server_error","message":"Too many requests"}}`)

	got := usageLogErrorMessage(http.StatusTooManyRequests, body)

	if got != "rate_limit_exceeded · server_error · Too many requests" {
		t.Fatalf("usageLogErrorMessage() = %q", got)
	}
}

func TestResponsesEndpointsAllowCompactionInputType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction","summary":"previous context was compacted"}
		]
	}`)

	tests := []struct {
		name    string
		path    string
		handler gin.HandlerFunc
	}{
		{name: "responses", path: "/v1/responses", handler: handler.Responses},
		{name: "responses compact", path: "/v1/responses/compact", handler: handler.ResponsesCompact},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = req

			test.handler(ginCtx)

			if recorder.Code == http.StatusBadRequest && strings.Contains(recorder.Body.String(), "invalid_input_type") {
				t.Fatalf("compaction input type was rejected by local validation: %s", recorder.Body.String())
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d after validation passes; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
			}
			assertNoAvailableAccountResponse(t, recorder.Body.Bytes())
		})
	}
}

func TestPopulateCompactUsageMetaFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("responses compact endpoint", func(t *testing.T) {
		input := &database.UsageLogInput{Endpoint: "/v1/responses/compact"}

		populateCompactUsageMetaFromRequest(nil, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for /v1/responses/compact")
		}
	})

	t.Run("compaction input item", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"message","role":"user","content":"hello"},
				{"type":"compaction","summary":"previous context was compacted"}
			]
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for compaction input item")
		}
	})

	t.Run("normal responses request", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hello"}]}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if input.Compact {
			t.Fatal("Compact = true, want false for normal responses input")
		}
	})
}

func TestResponsesEndpointsAllowGPT55MaxOutputTokens128K(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{"model":"gpt-5.5","input":"hello","max_output_tokens":128000}`)

	tests := []struct {
		name    string
		path    string
		handler gin.HandlerFunc
	}{
		{name: "responses", path: "/v1/responses", handler: handler.Responses},
		{name: "responses compact", path: "/v1/responses/compact", handler: handler.ResponsesCompact},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = req

			test.handler(ginCtx)

			if recorder.Code == http.StatusBadRequest && strings.Contains(recorder.Body.String(), "max_output_tokens") {
				t.Fatalf("gpt-5.5 128k max_output_tokens was rejected by local validation: %s", recorder.Body.String())
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d after validation passes; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
			}
			assertNoAvailableAccountResponse(t, recorder.Body.Bytes())
		})
	}
}

func TestResponsesNoAvailableAccountFailsFastWithoutCancelledContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = req

	start := time.Now()
	handler.Responses(ginCtx)
	elapsed := time.Since(start)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	assertNoAvailableAccountResponse(t, recorder.Body.Bytes())
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Responses took %s with no dispatch candidates; want fast failure", elapsed)
	}
}

func TestResponsesEnforcesAPIKeyModelAllowlistBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = req
	ginCtx.Set(contextAPIKeyRow, &database.APIKeyRow{
		ID:   42,
		Name: "limited",
		Limits: database.APIKeyLimits{
			ModelAllow: []string{"gpt-5.5", "gpt-5.4-mini"},
		},
	})

	handler.Responses(ginCtx)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); !strings.Contains(got, "gpt-5.4") || !strings.Contains(got, "not allowed") {
		t.Fatalf("error.message = %q, want model allowlist rejection; body=%s", got, recorder.Body.String())
	}
}

func TestExtractResponseImageGenerationOutputDedupes(t *testing.T) {
	event := []byte(`{"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","result":"` + tinyPNGBase64 + `","output_format":"png"}}`)
	seen := make(map[string]struct{})

	raw, ok := extractResponseImageGenerationOutput(event, seen)
	if !ok {
		t.Fatal("expected image_generation_call output to be extracted")
	}

	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("decode extracted image item: %v", err)
	}
	if item["result"] != tinyPNGBase64 {
		t.Fatalf("result = %v, want tiny PNG", item["result"])
	}
	if item["bytes"] != float64(tinyPNGByteSize(t)) || item["width"] != float64(1) || item["height"] != float64(1) {
		t.Fatalf("image stats = bytes:%v width:%v height:%v", item["bytes"], item["width"], item["height"])
	}

	if _, ok := extractResponseImageGenerationOutput(event, seen); ok {
		t.Fatal("expected duplicate image_generation_call output to be ignored")
	}
}

func TestRestoreMissingResponseOutputsUsesOutputItemDone(t *testing.T) {
	response := []byte(`{"id":"resp_1","object":"response","output":[]}`)
	outputItems := []json.RawMessage{
		json.RawMessage(`{"id":"rs_1","type":"reasoning","encrypted_content":"opaque","summary":[]}`),
		json.RawMessage(`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"{\"age\":30,\"name\":\"John\"}"}]}`),
	}

	got := restoreMissingResponseOutputs(response, outputItems)

	output := gjson.GetBytes(got, "output")
	if !output.IsArray() || len(output.Array()) != 2 {
		t.Fatalf("output count = %d, want 2; body=%s", len(output.Array()), got)
	}
	if typ := output.Array()[0].Get("type").String(); typ != "reasoning" {
		t.Fatalf("first output type = %q, want reasoning; body=%s", typ, got)
	}
	if text := output.Array()[1].Get("content.0.text").String(); text != `{"age":30,"name":"John"}` {
		t.Fatalf("message text = %q, want structured JSON; body=%s", text, got)
	}
}

func TestRestoreMissingResponseOutputsPreservesCompletedOutput(t *testing.T) {
	response := []byte(`{"id":"resp_1","object":"response","output":[{"id":"msg_existing","type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	outputItems := []json.RawMessage{
		json.RawMessage(`{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"fallback"}]}`),
	}

	got := restoreMissingResponseOutputs(response, outputItems)

	if string(got) != string(response) {
		t.Fatalf("non-empty completed output should be preserved, got %s", got)
	}
}

func TestAppendMissingResponseImageOutputsAddsOutputItemDone(t *testing.T) {
	response := []byte(`{"id":"resp_1"}`)
	imageOutputs := []json.RawMessage{
		json.RawMessage(`{"id":"ig_1","type":"image_generation_call","result":"` + tinyPNGBase64 + `","output_format":"png"}`),
	}

	got := appendMissingResponseImageOutputs(response, imageOutputs)

	var payload struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("decode merged response: %v", err)
	}
	if len(payload.Output) != 1 {
		t.Fatalf("output count = %d, want 1; body=%s", len(payload.Output), got)
	}
	if payload.Output[0]["type"] != "image_generation_call" || payload.Output[0]["result"] != tinyPNGBase64 {
		t.Fatalf("unexpected output item: %#v", payload.Output[0])
	}
	if payload.Output[0]["bytes"] != float64(tinyPNGByteSize(t)) || payload.Output[0]["width"] != float64(1) || payload.Output[0]["height"] != float64(1) {
		t.Fatalf("image stats = bytes:%v width:%v height:%v", payload.Output[0]["bytes"], payload.Output[0]["width"], payload.Output[0]["height"])
	}

	gotAgain := appendMissingResponseImageOutputs(got, imageOutputs)
	if err := json.Unmarshal(gotAgain, &payload); err != nil {
		t.Fatalf("decode merged response again: %v", err)
	}
	if len(payload.Output) != 1 {
		t.Fatalf("duplicate output count = %d, want 1; body=%s", len(payload.Output), gotAgain)
	}
}

func TestAppendMissingResponseImageOutputsAnnotatesExistingOutput(t *testing.T) {
	response := []byte(`{"id":"resp_1","output":[{"id":"ig_1","type":"image_generation_call","result":"` + tinyPNGBase64 + `","output_format":"png"}]}`)

	got := appendMissingResponseImageOutputs(response, nil)

	var payload struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("decode annotated response: %v", err)
	}
	if len(payload.Output) != 1 {
		t.Fatalf("output count = %d, want 1; body=%s", len(payload.Output), got)
	}
	if payload.Output[0]["bytes"] != float64(tinyPNGByteSize(t)) || payload.Output[0]["width"] != float64(1) || payload.Output[0]["height"] != float64(1) {
		t.Fatalf("image stats = bytes:%v width:%v height:%v", payload.Output[0]["bytes"], payload.Output[0]["width"], payload.Output[0]["height"])
	}
}

func TestAccountFilterForSparkRequiresPro(t *testing.T) {
	filter := accountFilterForModel("gpt-5.3-codex-spark")
	if filter == nil {
		t.Fatal("expected filter for spark model")
	}
	if !filter(&auth.Account{PlanType: "pro"}) {
		t.Fatal("spark filter should allow pro accounts")
	}
	if !filter(&auth.Account{PlanType: "prolite"}) {
		t.Fatal("spark filter should treat prolite as pro")
	}
	if filter(&auth.Account{PlanType: "plus"}) {
		t.Fatal("spark filter should reject non-pro accounts")
	}
	normalFilter := accountFilterForModel("gpt-5.3-codex")
	if normalFilter == nil || !normalFilter(&auth.Account{PlanType: "plus"}) {
		t.Fatal("non-spark model filter should allow available accounts")
	}
	directOpenAIAccount := &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.openai.com",
		APIKey:       "sk-test",
		Models:       []string{"gpt-4.1"},
	}
	if normalFilter(directOpenAIAccount) {
		t.Fatal("codex account filter should reject direct OpenAI Responses accounts")
	}
	responsesFilter := accountFilterForResponsesModel("gpt-4.1", false)
	if !responsesFilter(directOpenAIAccount) {
		t.Fatal("responses filter should allow direct OpenAI account for configured model")
	}
	if responsesFilter(&auth.Account{AccessToken: "codex-at", PlanType: "plus"}) {
		t.Fatal("responses filter should reject codex accounts for direct-only models")
	}
	if !accountFilterForResponsesModel("gpt-4.1", true)(&auth.Account{AccessToken: "codex-at", PlanType: "plus"}) {
		t.Fatal("responses filter should allow codex accounts when model is in Codex catalog")
	}
	if accountFilterForResponsesModel("gpt-4.2", false)(directOpenAIAccount) {
		t.Fatal("responses filter should reject direct OpenAI account for unconfigured model")
	}
	cooled := &auth.Account{PlanType: "pro"}
	cooled.SetModelCooldownUntil("gpt-5.3-codex-spark", "model_capacity", time.Now().Add(time.Minute))
	if filter(cooled) {
		t.Fatal("filter should reject model-cooled accounts")
	}
}

func TestSupportedModelIDsIncludesOpenAIResponsesAccountModels(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.openai.com",
		APIKey:       "sk-test",
		Models:       []string{"gpt-4.1-direct"},
	})

	handler := &Handler{store: store}
	models := handler.supportedModelIDs(context.Background())
	for _, model := range models {
		if model == "gpt-4.1-direct" {
			return
		}
	}
	t.Fatalf("supported models missing direct OpenAI model: %v", models)
}

func TestClassify429UsageLimitExactResetUsesAccountCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	decision := classify429RateLimit(&auth.Account{PlanType: "team"}, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`), nil, now, "gpt-5.4")
	if decision.Scope != rateLimitScopeAccount || decision.Reason != "usage_limit" {
		t.Fatalf("decision = %#v, want account usage_limit", decision)
	}
	if decision.Cooldown != 120*time.Second {
		t.Fatalf("Cooldown = %v, want 120s", decision.Cooldown)
	}
}

func TestClassify429CapacityUsesModelCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`)
	decision := classify429RateLimit(&auth.Account{PlanType: "team"}, body, nil, now, "gpt-5.4")
	if decision.Scope != rateLimitScopeModel || decision.Reason != "model_capacity" {
		t.Fatalf("decision = %#v, want model capacity cooldown", decision)
	}
	if decision.Cooldown != 5*time.Minute {
		t.Fatalf("Cooldown = %v, want 5m", decision.Cooldown)
	}
}

func TestClassify429Header7dUsesAccountCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-secondary-used-percent", "100")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "3600")
	decision := classify429RateLimit(&auth.Account{PlanType: "team"}, nil, resp, now, "gpt-5.4")
	if decision.Scope != rateLimitScopeAccount || decision.Reason != "rate_limited_7d" {
		t.Fatalf("decision = %#v, want 7d account cooldown", decision)
	}
	if decision.Cooldown != time.Hour {
		t.Fatalf("Cooldown = %v, want 1h", decision.Cooldown)
	}
}

func TestShouldRetryHTTPStatusSplitsRateLimitBudget(t *testing.T) {
	generalRetries := 0
	rateLimitRetries := 0
	if !shouldRetryHTTPStatus(http.StatusTooManyRequests, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("first 429 should consume rate-limit retry budget")
	}
	if shouldRetryHTTPStatus(http.StatusTooManyRequests, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("second 429 should be blocked by rate-limit retry budget")
	}
	if !shouldRetryHTTPStatus(http.StatusServiceUnavailable, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("503 should still use general retry budget")
	}
	if generalRetries != 1 || rateLimitRetries != 1 {
		t.Fatalf("budgets = general %d rate %d, want 1/1", generalRetries, rateLimitRetries)
	}
}

func TestDeactivatedWorkspace402MarksAccountError(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 42, AccessToken: "at"}
	handler := &Handler{store: store}
	body := []byte(`{"detail":{"code":"deactivated_workspace"}}`)

	if !IsDeactivatedWorkspaceError(body) {
		t.Fatal("expected deactivated workspace body to be detected")
	}
	if got := upstreamErrorKind(http.StatusPaymentRequired, body, codex429Decision{}); got != "deactivated_workspace" {
		t.Fatalf("upstreamErrorKind = %q, want deactivated_workspace", got)
	}

	handler.applyCooldownForModel(account, http.StatusPaymentRequired, body, &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if got := account.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
	account.Mu().RLock()
	errorMsg := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMsg, "deactivated_workspace") {
		t.Fatalf("ErrorMsg = %q, want deactivated_workspace", errorMsg)
	}
}

func TestSendFinalUpstreamError_UsageLimitRewrites429(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_at":1775317531,"resets_in_seconds":602705}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusTooManyRequests, body)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Retry-After"); got != "602705" {
		t.Fatalf("Retry-After = %q, want %q", got, "602705")
	}

	var payload struct {
		Error struct {
			Message         string `json:"message"`
			Type            string `json:"type"`
			Code            string `json:"code"`
			PlanType        string `json:"plan_type"`
			ResetsAt        int64  `json:"resets_at"`
			ResetsInSeconds int64  `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Error.Type != "server_error" {
		t.Fatalf("type = %q, want %q", payload.Error.Type, "server_error")
	}
	if payload.Error.Code != "account_pool_usage_limit_reached" {
		t.Fatalf("code = %q, want %q", payload.Error.Code, "account_pool_usage_limit_reached")
	}
	if payload.Error.PlanType != "free" {
		t.Fatalf("plan_type = %q, want %q", payload.Error.PlanType, "free")
	}
	if payload.Error.ResetsAt != 1775317531 {
		t.Fatalf("resets_at = %d, want %d", payload.Error.ResetsAt, 1775317531)
	}
	if payload.Error.ResetsInSeconds != 602705 {
		t.Fatalf("resets_in_seconds = %d, want %d", payload.Error.ResetsInSeconds, 602705)
	}
	if payload.Error.Message == "" {
		t.Fatal("expected non-empty aggregated error message")
	}
}

func TestSendFinalUpstreamError_FallsBackForNonUsageLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"type":"rate_limit_error","message":"Too many requests"}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusTooManyRequests, body)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty", got)
	}
}

func TestSendFinalUpstreamError_UsageLimitMissingTimeFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	// usage_limit_reached 但不含 resets_at / resets_in_seconds
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"limit reached"}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusTooManyRequests, body)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	// 无 resets_in_seconds 时不应设置 Retry-After
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty (no resets_in_seconds)", got)
	}

	// 验证零值字段不出现在响应中
	var raw map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj := raw["error"]
	if _, exists := errObj["resets_at"]; exists {
		t.Fatal("resets_at should be omitted when 0")
	}
	if _, exists := errObj["resets_in_seconds"]; exists {
		t.Fatal("resets_in_seconds should be omitted when 0")
	}
	if _, exists := errObj["plan_type"]; exists {
		t.Fatal("plan_type should be omitted when empty")
	}
}

func TestSendFinalUpstreamError_Non429StatusPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"type":"server_error","message":"internal failure"}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusInternalServerError, body)

	// 非 429 直接透传原状态码
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

func TestCompute429CooldownPlusUsesWindowHeaders(t *testing.T) {
	handler := &Handler{}
	account := &auth.Account{PlanType: "plus"}
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-secondary-used-percent", "20")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")

	got := handler.compute429Cooldown(account, []byte(`{"error":{"type":"usage_limit_reached"}}`), resp)
	want := 5 * time.Hour
	if got != want {
		t.Fatalf("cooldown = %v, want %v", got, want)
	}
}

func TestCompute429CooldownPlusPrefersExactResetTime(t *testing.T) {
	handler := &Handler{}
	account := &auth.Account{PlanType: "plus"}
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")

	got := handler.compute429Cooldown(account, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":1800}}`), resp)
	want := 30 * time.Minute
	if got != want {
		t.Fatalf("cooldown = %v, want %v", got, want)
	}
}

func TestApply429CooldownPremiumMarks5hRateLimitFromWindow(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 101, PlanType: "plus"}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "900")

	decision := Apply429Cooldown(store, account, []byte(`{"error":{"type":"usage_limit_reached"}}`), resp, "gpt-5.4")

	if decision.Scope != rateLimitScopeAccount || decision.Reason != "rate_limited_5h" {
		t.Fatalf("decision = %#v, want premium 5h account decision", decision)
	}
	if !account.IsPremium5hRateLimited() {
		t.Fatal("expected account to enter premium 5h rate limited state")
	}
	pct5h, resetAt, ok := account.GetUsageSnapshot5h()
	if !ok {
		t.Fatal("expected 5h snapshot to be set")
	}
	if pct5h != 100 {
		t.Fatalf("usage_percent_5h = %v, want 100", pct5h)
	}
	if got := resetAt.Sub(time.Now()); got < 14*time.Minute || got > 16*time.Minute {
		t.Fatalf("resetAt delta = %v, want about 15m", got)
	}
}

func TestApply429CooldownUsageLimitUpdatesFreePlanMetadata(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "usage-limit-account", map[string]interface{}{
		"plan_type": "pro",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials 返回错误: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "pro"}
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_in_seconds":3600}}`)

	decision := Apply429Cooldown(store, account, body, &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if decision.Scope != rateLimitScopeAccount || decision.Reason != "usage_limit" {
		t.Fatalf("decision = %#v, want account usage_limit", decision)
	}
	if got := account.GetPlanType(); got != "free" {
		t.Fatalf("account plan_type = %q, want free", got)
	}
	pct, ok := account.GetUsagePercent7d()
	if !ok || pct != 100 {
		t.Fatalf("usage_percent_7d = %v ok=%v, want 100 true", pct, ok)
	}
	if got := account.RuntimeStatus(); got != "usage_exhausted" {
		t.Fatalf("RuntimeStatus() = %q, want usage_exhausted", got)
	}

	resetDelta := time.Until(account.GetReset7dAt())
	if resetDelta < 59*time.Minute || resetDelta > 61*time.Minute {
		t.Fatalf("reset_7d_at delta = %v, want about 1h", resetDelta)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID 返回错误: %v", err)
	}
	if got := row.GetCredential("plan_type"); got != "free" {
		t.Fatalf("persisted plan_type = %q, want free", got)
	}
	if got := row.GetCredential("codex_7d_used_percent"); got != "100" {
		t.Fatalf("persisted codex_7d_used_percent = %q, want 100", got)
	}
	if got := row.GetCredential("codex_7d_reset_at"); got == "" {
		t.Fatal("persisted codex_7d_reset_at is empty")
	}
}

func TestSyncCodexUsageStateUpdatesPlanTypeFromHeader(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New returned error: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "plan-header-account", map[string]interface{}{
		"plan_type": "free",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials returned error: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "free"}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-plan-type", "Enterprise")
	resp.Header.Set("x-codex-primary-used-percent", "12")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "1200")
	resp.Header.Set("x-codex-secondary-used-percent", "3")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "600000")

	result := SyncCodexUsageState(store, account, resp)

	if got := account.GetPlanType(); got != "enterprise" {
		t.Fatalf("account plan_type = %q, want enterprise", got)
	}
	if !result.Used5hHeaders || !result.HasUsage5h || !result.HasUsage7d {
		t.Fatalf("usage sync result = %#v, want 5h and 7d headers detected", result)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID returned error: %v", err)
	}
	if got := row.GetCredential("plan_type"); got != "enterprise" {
		t.Fatalf("persisted plan_type = %q, want enterprise", got)
	}
}

func TestApply429CooldownUnknown429UsesModelCooldown(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 102, PlanType: "pro"}

	decision := Apply429Cooldown(store, account, []byte(`{"error":{"type":"rate_limit_error","message":"Too many requests"}}`), &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if decision.Scope != rateLimitScopeModel {
		t.Fatalf("decision.Scope = %q, want model", decision.Scope)
	}
	if got := decision.ResetAt.Sub(time.Now()); got < 4*time.Minute || got > 6*time.Minute {
		t.Fatalf("resetAt delta = %v, want about 5m", got)
	}
	if !account.IsModelRateLimited("gpt-5.4") {
		t.Fatal("expected model cooldown")
	}
}

func TestSyncCodexUsageStateTriggersPremium5hLimitWith5hHeadersOnly(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 103, PlanType: "team"}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "600")

	result := SyncCodexUsageState(store, account, resp)

	if !result.Used5hHeaders {
		t.Fatal("expected 5h headers to be detected")
	}
	if result.HasUsage7d {
		t.Fatal("expected no 7d usage snapshot")
	}
	if !result.HasUsage5h {
		t.Fatal("expected 5h usage snapshot")
	}
	if !result.Persisted5hOnly {
		t.Fatal("expected 5h-only persistence path to be selected")
	}
	if !result.Premium5hRateLimited {
		t.Fatal("expected premium 5h rate limit to trigger")
	}
	if !account.IsPremium5hRateLimited() {
		t.Fatal("expected account to be premium 5h rate limited")
	}
}

func TestAuthMiddlewareSetsAPIKeyContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	key := "sk-test-auth-1234567890"
	id, err := db.InsertAPIKey(context.Background(), "Team A", key)
	if err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"id":     c.MustGet(contextAPIKeyID),
			"name":   c.MustGet(contextAPIKeyName),
			"masked": c.MustGet(contextAPIKeyMasked),
			"raw":    c.MustGet("apiKey"),
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Masked string `json:"masked"`
		Raw    string `json:"raw"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal 返回错误: %v", err)
	}

	if payload.ID != id {
		t.Fatalf("id = %d, want %d", payload.ID, id)
	}
	if payload.Name != "Team A" {
		t.Fatalf("name = %q, want %q", payload.Name, "Team A")
	}
	if payload.Masked == "" || payload.Masked == key {
		t.Fatalf("masked = %q, want masked value", payload.Masked)
	}
	if payload.Raw != key {
		t.Fatalf("raw = %q, want %q", payload.Raw, key)
	}
}

func TestAuthMiddlewareRejectsExpiredAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	key := "sk-test-expired-1234567890"
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name:      "Expired",
		Key:       key,
		ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("InsertAPIKeyWithOptions 返回错误: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != string(api.ErrCodeInvalidAuth) {
		t.Fatalf("error.code = %q, want %q", got, api.ErrCodeInvalidAuth)
	}
}

func TestAuthMiddlewareRejectsQuotaExhaustedAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	key := "sk-test-quota-1234567890"
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name:       "Quota",
		Key:        key,
		QuotaLimit: 0.01,
		QuotaUsed:  0.01,
	}); err != nil {
		t.Fatalf("InsertAPIKeyWithOptions 返回错误: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != string(api.ErrCodeRateLimitReached) {
		t.Fatalf("error.code = %q, want %q", got, api.ErrCodeRateLimitReached)
	}
}

func TestAuthMiddlewareUsesRuntimeAPIKeyCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	key := "sk-test-runtime-cache-1234567890"
	tc := cache.NewMemory(1)
	ctx := context.Background()
	keyPayload, _ := json.Marshal(apiKeyRuntimeRecord{
		ID:        42,
		Name:      "Cached Team",
		CreatedAt: time.Now(),
	})
	if err := tc.SetRuntime(ctx, apiKeyCacheNamespace, key, keyPayload, time.Minute); err != nil {
		t.Fatalf("SetRuntime api key: %v", err)
	}
	countPayload, _ := json.Marshal(apiKeyCountRuntimeRecord{Count: 1})
	if err := tc.SetRuntime(ctx, apiKeyCountCacheNamespace, "all", countPayload, time.Minute); err != nil {
		t.Fatalf("SetRuntime api key count: %v", err)
	}

	handler := NewHandler(nil, nil, nil, nil)
	handler.SetRuntimeCache(tc)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"id":   c.MustGet(contextAPIKeyID),
			"name": c.MustGet(contextAPIKeyName),
			"raw":  c.MustGet("apiKey"),
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Raw  string `json:"raw"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal 返回错误: %v", err)
	}
	if payload.ID != 42 || payload.Name != "Cached Team" || payload.Raw != key {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestSessionAffinityKeySeparatesDifferentAPIKeys(t *testing.T) {
	key1 := sessionAffinityKey("session-1", 1)
	key2 := sessionAffinityKey("session-1", 2)

	if key1 == key2 {
		t.Fatalf("sessionAffinityKey should differ for different apiKeyID: %q", key1)
	}
	if got := sessionAffinityKey("session-1", 0); got != "session-1" {
		t.Fatalf("sessionAffinityKey() with apiKeyID=0 = %q, want session-1", got)
	}
}
