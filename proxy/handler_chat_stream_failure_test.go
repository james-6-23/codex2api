package proxy

import (
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

func newChatStreamTerminalTestHandler(t *testing.T, events []string) (*Handler, *atomic.Int32) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
	t.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 1})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil), &calls
}

func invokeChatCompletionsStream(t *testing.T, handler *Handler) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.4","stream":true,"messages":[{"role":"user","content":"hello"}]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ChatCompletions(ctx)
	return recorder
}

func TestChatCompletionsStreamResponseFailedAfterContentHasNoDoneSentinel(t *testing.T) {
	handler, calls := newChatStreamTerminalTestHandler(t, []string{
		`{"type":"response.created","response":{"id":"resp_failed"}}`,
		`{"type":"response.output_text.delta","delta":"partial"}`,
		`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"upstream boom"}}}`,
	})

	recorder := invokeChatCompletionsStream(t, handler)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after visible content; body=%q", recorder.Code, body)
	}
	if !strings.Contains(body, `"content":"partial"`) || !strings.Contains(body, `"error"`) || !strings.Contains(body, "upstream boom") {
		t.Fatalf("partial content or stream error missing: %q", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("failed stream must not append the success sentinel: %q", body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1 after visible output", got)
	}
}

func TestChatCompletionsSuccessfulStreamStillAppendsDoneSentinel(t *testing.T) {
	handler, _ := newChatStreamTerminalTestHandler(t, []string{
		`{"type":"response.output_text.delta","delta":"complete"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
	})

	recorder := invokeChatCompletionsStream(t, handler)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("successful terminal chunk missing: status=%d body=%q", recorder.Code, body)
	}
	if got := strings.Count(body, "data: [DONE]\n\n"); got != 1 {
		t.Fatalf("successful stream [DONE] count = %d, want 1; body=%q", got, body)
	}
}

func TestChatCompletionsCatchAllDiscardsOutputFromFailedAttempt(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)

	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	upstream, calls := newAttemptSequenceSSEServer(t, [][]string{
		{
			`{"type":"response.created","response":{"id":"resp_failed"}}`,
			`{"type":"response.output_text.delta","delta":"failed-chat-partial"}`,
			`{"type":"response.failed","response":{"status":"failed","status_code":503,"error":{"code":"server_error","message":"must stay upstream"}}}`,
		},
		{
			`{"type":"response.output_text.delta","delta":"successful-chat"}`,
			`{"type":"response.completed","response":{"id":"resp_success","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
		},
	})
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := invokeChatCompletionsStream(t, handler)
	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if !strings.Contains(body, "successful-chat") || strings.Count(body, "data: [DONE]\n\n") != 1 {
		t.Fatalf("successful chat replay missing: %q", body)
	}
	if strings.Contains(body, "failed-chat-partial") || strings.Contains(body, "must stay upstream") {
		t.Fatalf("failed chat attempt leaked downstream: %q", body)
	}
}

func TestChatCompletionsCatchAllDiscardsExplicitErrorEventAfterPartialOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)

	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	upstream, calls := newAttemptSequenceRawSSEServer(t, [][]string{
		{
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"failed-chat-error-partial\"}\n\n",
			"event: error\ndata: {\"error\":{\"code\":\"future_error\",\"message\":\"must stay upstream\"}}\n\n",
		},
		{
			"data: {\"type\":\"response.output_text.delta\",\"delta\":\"successful-chat-after-error\"}\n\n",
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_success\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n",
		},
	})
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4", MaxRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	recorder := invokeChatCompletionsStream(t, handler)
	body := recorder.Body.String()
	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2; body=%q", got, body)
	}
	if !strings.Contains(body, "successful-chat-after-error") || strings.Count(body, "data: [DONE]\n\n") != 1 {
		t.Fatalf("successful Chat replay missing: %q", body)
	}
	if strings.Contains(body, "failed-chat-error-partial") || strings.Contains(body, "must stay upstream") || strings.Contains(body, "future_error") {
		t.Fatalf("explicit Chat error event attempt leaked downstream: %q", body)
	}
}
