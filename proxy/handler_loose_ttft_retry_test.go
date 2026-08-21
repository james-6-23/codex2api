package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func newAttemptSequenceSSEServer(t *testing.T, attempts [][]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := int(calls.Add(1)) - 1
		if attempt >= len(attempts) {
			attempt = len(attempts) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range attempts[attempt] {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func newAttemptSequenceRawSSEServer(t *testing.T, attempts [][]string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := int(calls.Add(1)) - 1
		if attempt >= len(attempts) {
			attempt = len(attempts) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range attempts[attempt] {
			_, _ = io.WriteString(w, frame)
		}
	}))
	t.Cleanup(server.Close)
	return server, &calls
}

func enableLooseResponseFailedContinuousRetry(t *testing.T) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.FirstTokenMode = FirstTokenModeLoose
	next.CodexPreflightSSEPassthrough = false
	next.CodexWSSilentRetry = false
	next.CodexWSSilentRetries = 0
	next.CodexWSHideErrors = false
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryResponseFailed},
	}
	ApplyRuntimeSettings(next)
}

func enableCatchAllContinuousRetry(t *testing.T) {
	t.Helper()
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	next := previous
	next.CodexPreflightSSEPassthrough = true
	next.CodexWSSilentRetry = false
	next.CodexWSSilentRetries = 0
	next.CodexWSHideErrors = false
	next.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	ApplyRuntimeSettings(next)
}

func looseTTFTFailureThenSuccessEvents(successText string) [][]string {
	return [][]string{
		{
			`{"type":"response.created","response":{"id":"resp_retry_1"}}`,
			`{"type":"response.output_item.added","item":{"type":"reasoning"}}`,
			`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"server_error","message":"temporary upstream failure"}}}`,
		},
		{
			`{"type":"response.created","response":{"id":"resp_retry_2"}}`,
			`{"type":"response.output_text.delta","delta":"` + successText + `"}`,
			`{"type":"response.completed","response":{"id":"resp_retry_2","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		},
	}
}

func TestChatCompletionsLooseTTFTDoesNotBlockPreContentContinuousRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableLooseResponseFailedContinuousRetry(t)

	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	upstream, calls := newAttemptSequenceSSEServer(t, looseTTFTFailureThenSuccessEvents("recovered-chat"))
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := invokeChatCompletionsStream(t, handler)
	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after a pre-content response.failed retry; body=%q", got, body)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"content":"recovered-chat"`) {
		t.Fatalf("recovered chat response missing: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "temporary upstream failure") || strings.Count(body, "data: [DONE]\n\n") != 1 {
		t.Fatalf("failed attempt leaked or success terminal is invalid: %q", body)
	}
}

func TestRelayResponsesLooseTTFTDefersMetadataForContinuousRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableLooseResponseFailedContinuousRetry(t)

	attempts := looseTTFTFailureThenSuccessEvents("recovered-responses")
	attempts[0][1] = `{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":10}}}`
	upstream, calls := newAttemptSequenceSSEServer(t, attempts)
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after buffered preflight metadata; body=%q", got, body)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(body, "recovered-responses") {
		t.Fatalf("recovered Responses stream missing: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "temporary upstream failure") || strings.Contains(body, "codex.rate_limits") {
		t.Fatalf("buffered failed-attempt events leaked downstream: %q", body)
	}
}

func TestRelayResponsesCatchAllRetriesHTTP200ErrorJSONBeforeReply(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableLooseResponseFailedContinuousRetry(t)
	settings := CurrentRuntimeSettings()
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	ApplyRuntimeSettings(settings)

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"error":{"code":"future_relay_failure","message":"temporary pseudo-success"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_recovered","object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"recovered-json"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	store := newOpenAIResponsesRelayStore(upstream.URL)
	store.AddAccount(&auth.Account{
		DBID:         2,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-direct-2",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
	})
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-4.1-direct","input":"hello","stream":false}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 after HTTP 200 error JSON", got)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "recovered-json") {
		t.Fatalf("recovered non-stream response missing: status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "temporary pseudo-success") || strings.Contains(recorder.Body.String(), "future_relay_failure") {
		t.Fatalf("HTTP 200 error payload leaked downstream: %q", recorder.Body.String())
	}
}

func TestRelayResponsesCatchAllDiscardsOutputFromFailedAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)

	attempts := [][]string{
		{
			`{"type":"response.created","response":{"id":"resp_failed"}}`,
			`{"type":"response.output_text.delta","delta":"failed-attempt-partial"}`,
			`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"future_failure","message":"must stay upstream"}}}`,
		},
		{
			`{"type":"response.created","response":{"id":"resp_success"}}`,
			`{"type":"response.output_text.delta","delta":"only-success"}`,
			`{"type":"response.completed","response":{"id":"resp_success","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		},
	}
	upstream, calls := newAttemptSequenceSSEServer(t, attempts)
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if recorder.Code != http.StatusOK || !strings.Contains(body, "only-success") || !strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("successful replay missing: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "failed-attempt-partial") || strings.Contains(body, "must stay upstream") || strings.Contains(body, "future_failure") {
		t.Fatalf("failed attempt leaked downstream: %q", body)
	}
}

func TestRelayResponsesCatchAllDiscardsExplicitErrorEventAfterPartialOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)

	upstream, calls := newAttemptSequenceRawSSEServer(t, [][]string{
		{
			"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed\"}}\n\n",
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"failed-error-event-partial\"}\n\n",
			"event: error\ndata: {\"error\":{\"code\":\"future_error\",\"message\":\"must stay upstream\"}}\n\n",
		},
		{
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered-after-error-event\"}\n\n",
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_success\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
		},
	})
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if !strings.Contains(body, "recovered-after-error-event") || !strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("successful replay missing: status=%d body=%q", recorder.Code, body)
	}
	if strings.Contains(body, "failed-error-event-partial") || strings.Contains(body, "must stay upstream") || strings.Contains(body, "future_error") {
		t.Fatalf("explicit error event attempt leaked downstream: %q", body)
	}
}

func TestRelayResponsesCatchAllDiscardsPartialAttemptWithoutTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)

	upstream, calls := newAttemptSequenceSSEServer(t, [][]string{
		{
			`{"type":"response.created","response":{"id":"resp_eof"}}`,
			`{"type":"response.output_text.delta","delta":"failed-eof-partial"}`,
		},
		{
			`{"type":"response.output_text.delta","delta":"recovered-after-eof"}`,
			`{"type":"response.completed","response":{"id":"resp_success","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		},
	})
	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if !strings.Contains(body, "recovered-after-eof") || strings.Contains(body, "failed-eof-partial") || strings.Contains(body, ErrorCodeUpstreamStreamBreak) {
		t.Fatalf("unterminated attempt leaked or successful replay missing: %q", body)
	}
}

func TestResponsesWebSocketCatchAllStopsAtExplicitCyberPolicy(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableLooseResponseFailedContinuousRetry(t)
	nextSettings := CurrentRuntimeSettings()
	nextSettings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	nextSettings.CodexPreflightSSEPassthrough = true
	ApplyRuntimeSettings(nextSettings)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	attempts := [][]string{
		{
			`{"type":"response.created","response":{"id":"resp_retry_1"}}`,
			`{"type":"codex.rate_limits","plan_type":"plus"}`,
			`{"type":"response.failed","response":{"status":"failed","status_code":400,"error":{"code":"cyber_policy","message":"must stop this turn"}}}`,
		},
		{
			`{"type":"response.created","response":{"id":"resp_retry_2"}}`,
			`{"type":"response.output_text.delta","delta":"recovered-websocket"}`,
			`{"type":"response.completed","response":{"id":"resp_retry_2","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		},
	}
	var calls atomic.Int32
	attemptAccounts := make(chan int64, 2)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attemptAccounts <- account.ID()
		attempt := int(calls.Add(1)) - 1
		if attempt >= len(attempts) {
			attempt = len(attempts) - 1
		}
		var sse strings.Builder
		for _, event := range attempts[attempt] {
			sse.WriteString("data: ")
			sse.WriteString(event)
			sse.WriteString("\n\n")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse.String())),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Responses websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial Responses websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write Responses websocket request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var downstream strings.Builder
	terminalType := ""
	for i := 0; i < 8; i++ {
		_, event, readErr := conn.ReadMessage()
		if readErr != nil {
			t.Fatalf("read Responses websocket event: %v; downstream=%s", readErr, downstream.String())
		}
		downstream.Write(event)
		eventType := gjson.GetBytes(event, "type").String()
		if eventType == "response.completed" || eventType == "response.failed" || eventType == "error" {
			terminalType = eventType
			break
		}
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 for explicit CYB hard stop; downstream=%s", got, downstream.String())
	}
	<-attemptAccounts
	select {
	case accountID := <-attemptAccounts:
		t.Fatalf("catch-all rotated to unexpected second account %d", accountID)
	default:
	}
	if terminalType != "response.failed" {
		t.Fatalf("terminal event = %q, want response.failed; downstream=%s", terminalType, downstream.String())
	}
	if body := downstream.String(); !strings.Contains(body, "cyber_policy") || strings.Contains(body, "recovered-websocket") || strings.Contains(body, "response.completed") {
		t.Fatalf("Responses websocket did not return the final CYB failure: %s", body)
	}
}

func TestResponsesWebSocketCatchAllTreatsErrorEventAndEOFAsFailedAttempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	var calls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		var sse string
		switch calls.Add(1) {
		case 1:
			sse = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_error\"}}\n\n" +
				"event: error\ndata: {\"type\":\"provider_specific_failure\",\"error\":{\"code\":\"future_error\",\"message\":\"must retry\"}}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"contradictory_success\",\"status\":\"completed\"}}\n\n"
		case 2:
			sse = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_eof\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"truncated-at-eof\"}\n\n"
		default:
			sse = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_success\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"recovered-after-errors\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_success\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 0, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	for id := int64(1); id <= 3; id++ {
		store.AddAccount(&auth.Account{DBID: id, AccessToken: "at", PlanType: "pro", AccountID: fmt.Sprintf("acct-%d", id)})
	}
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Responses websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial Responses websocket: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var downstream strings.Builder
	for i := 0; i < 8; i++ {
		_, event, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read Responses websocket event: %v; downstream=%s", err, downstream.String())
		}
		downstream.Write(event)
		if gjson.GetBytes(event, "type").String() == "response.completed" {
			break
		}
	}

	if got := calls.Load(); got != 3 {
		t.Fatalf("upstream calls = %d, want 3; downstream=%s", got, downstream.String())
	}
	body := downstream.String()
	if !strings.Contains(body, "recovered-after-errors") || strings.Contains(body, "future_error") || strings.Contains(body, "contradictory_success") || strings.Contains(body, "truncated-at-eof") {
		t.Fatalf("failed error/EOF attempt leaked or stopped retry: %s", body)
	}
}

func TestResponsesWebSocketSelectiveRetriesSelectedFailureAfterPartialOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	settings.CodexWSSilentRetry = false
	settings.CodexWSSilentRetries = 0
	settings.CodexWSHideErrors = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled: true, Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
	}
	ApplyRuntimeSettings(settings)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	var calls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		var sse string
		if calls.Add(1) == 1 {
			sse = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"discard-selected-partial\"}\n\n" +
				"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"status_code\":503,\"error\":{\"code\":\"server_error\",\"message\":\"selected upstream failure\"}}}\n\n"
		} else {
			sse = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_success\"}}\n\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"selective-recovered\"}\n\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_success\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 0, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Responses websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var downstream strings.Builder
	for i := 0; i < 6; i++ {
		_, event, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read Responses websocket event: %v; downstream=%s", err, downstream.String())
		}
		downstream.Write(event)
		if gjson.GetBytes(event, "type").String() == "response.completed" {
			break
		}
	}

	body := downstream.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; downstream=%s", got, body)
	}
	if !strings.Contains(body, "selective-recovered") || !strings.Contains(body, `"type":"response.completed"`) {
		t.Fatalf("selected failure did not recover: %s", body)
	}
	if strings.Contains(body, "discard-selected-partial") || strings.Contains(body, "selected upstream failure") {
		t.Fatalf("selected failed attempt leaked downstream: %s", body)
	}
}

func TestResponsesWebSocketSelectiveReturnsOnlyUnselectedTerminalFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousSettings) })
	settings := previousSettings
	settings.CodexWSSilentRetry = false
	settings.CodexWSSilentRetries = 0
	settings.CodexWSHideErrors = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
		Enabled: true, Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
	}
	ApplyRuntimeSettings(settings)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	var calls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		calls.Add(1)
		sse := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_failed\"}}\n\n" +
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"discard-unselected-partial\"}\n\n" +
			"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"status_code\":400,\"error\":{\"code\":\"invalid_request\",\"message\":\"not selected\"}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "pro", AccountID: "acct"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Responses websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(event, "type").String(); got != "response.failed" {
		t.Fatalf("first downstream event = %q, want response.failed: %s", got, event)
	}
	if calls.Load() != 1 || strings.Contains(string(event), "discard-unselected-partial") {
		t.Fatalf("unselected failure retried or leaked partial output: calls=%d event=%s", calls.Load(), event)
	}
}

func TestResponsesWebSocketReplayCachesOnlyAfterSuccessfulFilteredWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		sse := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"blocked-success-output\"}\n\n" +
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_must_not_cache\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_blocked\",\"name\":\"blocked\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "pro", AccountID: "acct"})
	filterConfig := promptfilter.DefaultConfig()
	filterConfig.Enabled = true
	filterConfig.Mode = promptfilter.ModeBlock
	filterConfig.StrictTerminalEnabled = true
	filterConfig.CustomPatterns = []promptfilter.PatternConfig{{
		Name: "blocked_success_output", Pattern: `blocked-success-output`, Weight: 100, Strict: true,
	}}
	filterConfig.Advanced.Output = promptfilter.OutputConfig{Enabled: true, BufferBytes: 512, OverlapBytes: 64, StrictOnly: true}
	store.SetPromptFilterConfig(filterConfig)

	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial Responses websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	request := `{"type":"response.create","model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hello"}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(request)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(event, "error.code").String(); got != "response_policy_violation" {
		t.Fatalf("output filter event code = %q, want response_policy_violation: %s", got, event)
	}
	if cached := getResponseCache("anon", "resp_must_not_cache"); cached != nil {
		t.Fatalf("locally blocked replay populated continuation cache: %s", cached)
	}
}

func TestResponsesHTTPReplayCachesOnlyAfterSuccessfulFilteredWrite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)
	resetResponseCacheForTest()
	t.Cleanup(resetResponseCacheForTest)

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"blocked-http-success-output\"}\n\n")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http_must_not_cache\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call_blocked\",\"name\":\"blocked\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := newOpenAIResponsesRelayStore(upstream.URL)
	t.Cleanup(store.Stop)
	filterConfig := promptfilter.DefaultConfig()
	filterConfig.Enabled = true
	filterConfig.Mode = promptfilter.ModeBlock
	filterConfig.StrictTerminalEnabled = true
	filterConfig.CustomPatterns = []promptfilter.PatternConfig{{
		Name: "blocked_http_success_output", Pattern: `blocked-http-success-output`, Weight: 100, Strict: true,
	}}
	filterConfig.Advanced.Output = promptfilter.OutputConfig{Enabled: true, BufferBytes: 512, OverlapBytes: 64, StrictOnly: true}
	store.SetPromptFilterConfig(filterConfig)

	handler := NewHandler(store, nil, nil, nil)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(
		`{"model":"gpt-4.1-direct","input":"hello","stream":true}`,
	))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	if got := calls.Load(); got != 1 {
		t.Fatalf("local replay rejection made %d upstream calls, want 1", got)
	}
	if cached := getResponseCache("anon", "resp_http_must_not_cache"); cached != nil {
		t.Fatalf("locally blocked HTTP replay populated continuation cache: %s", cached)
	}
}
