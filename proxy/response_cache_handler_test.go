package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestResponsesContinuationUnavailableWaitsForRelayRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}],"stream":true}`)

	t.Run("Codex only returns immediate 409", func(t *testing.T) {
		resetResponseCacheStateForTest(testResponseCacheConfig())
		store := newContinuationCodexStore()
		handler := NewHandler(store, nil, nil, nil)
		recorder := invokeResponsesHandler(t, handler.Responses, raw)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
		}
		if code := gjson.Get(recorder.Body.String(), "error.code").String(); code != string(api.ErrCodeResponseContextUnavailable) {
			t.Fatalf("error code = %q, want response_context_unavailable; body=%s", code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "resp_missing") {
			t.Fatalf("error body echoed response ID: %s", recorder.Body.String())
		}
	})

	t.Run("relay only preserves previous response id", func(t *testing.T) {
		resetResponseCacheStateForTest(testResponseCacheConfig())
		var seenBody []byte
		upstream := newContinuationRelayUpstream(t, false, &seenBody)
		store := newContinuationRelayStore(upstream.URL)
		handler := NewHandler(store, nil, nil, nil)
		recorder := invokeResponsesHandler(t, handler.Responses, raw)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
		}
		if prev := gjson.GetBytes(seenBody, "previous_response_id").String(); prev != "resp_missing" {
			t.Fatalf("relay previous_response_id = %q, want preserved; body=%s", prev, seenBody)
		}
	})

	t.Run("mixed pool chooses relay", func(t *testing.T) {
		resetResponseCacheStateForTest(testResponseCacheConfig())
		var seenBody []byte
		upstream := newContinuationRelayUpstream(t, false, &seenBody)
		store := newContinuationRelayStore(upstream.URL)
		store.AddAccount(&auth.Account{DBID: 2, AccessToken: "codex-token", PlanType: "plus", AccountID: "codex"})
		handler := NewHandler(store, nil, nil, nil)
		recorder := invokeResponsesHandler(t, handler.Responses, raw)
		if recorder.Code != http.StatusOK || gjson.GetBytes(seenBody, "previous_response_id").String() != "resp_missing" {
			t.Fatalf("mixed result status=%d upstream=%s response=%s", recorder.Code, seenBody, recorder.Body.String())
		}
	})

	t.Run("compaction pin yields to relay fallback", func(t *testing.T) {
		resetResponseCacheStateForTest(testResponseCacheConfig())
		var seenBody []byte
		upstream := newContinuationRelayUpstream(t, false, &seenBody)
		store := newContinuationRelayStore(upstream.URL)
		store.AddAccount(&auth.Account{DBID: 2, AccessToken: "codex-token", PlanType: "plus", AccountID: "codex"})
		handler := NewHandler(store, nil, nil, nil)
		compactionRaw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"compaction_trigger"},{"type":"function_call_output","call_id":"call_1","output":"ok"}],"stream":true}`)
		recorder := invokeResponsesHandler(t, handler.Responses, compactionRaw)
		if recorder.Code != http.StatusOK || gjson.GetBytes(seenBody, "previous_response_id").String() != "resp_missing" {
			t.Fatalf("compaction fallback status=%d upstream=%s response=%s", recorder.Code, seenBody, recorder.Body.String())
		}
		input := gjson.GetBytes(seenBody, "input").Array()
		if len(input) == 0 || input[len(input)-1].Get("type").String() != "compaction_trigger" {
			t.Fatalf("compaction trigger must be the final upstream input item: %s", seenBody)
		}
	})
}

func TestResponsesContinuationBackendErrorReturns503AfterRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	backend.boundedErr = errSyntheticBackend
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})
	handler := NewHandler(newContinuationCodexStore(), nil, nil, nil)
	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}],"stream":true}`)

	recorder := invokeResponsesHandler(t, handler.Responses, raw)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestResponsesContinuationBackendErrorUsesRelayWhenAvailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheStateForTest(testResponseCacheConfig())
	backend := newRecordingResponseContextBackend(true)
	backend.boundedErr = errSyntheticBackend
	SetResponseContextCache(backend)
	t.Cleanup(func() {
		SetResponseContextCache(nil)
		_ = backend.TokenCache.Close()
	})
	var seenBody []byte
	upstream := newContinuationRelayUpstream(t, false, &seenBody)
	handler := NewHandler(newContinuationRelayStore(upstream.URL), nil, nil, nil)
	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}],"stream":true}`)

	recorder := invokeResponsesHandler(t, handler.Responses, raw)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want relay success; body=%s", recorder.Code, recorder.Body.String())
	}
	if prev := gjson.GetBytes(seenBody, "previous_response_id").String(); prev != "resp_missing" {
		t.Fatalf("relay previous_response_id = %q, want preserved; body=%s", prev, seenBody)
	}
}

func TestResponsesKnownLocalUnavailableReturns409(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tests := []struct {
		name  string
		setup func()
		id    string
	}{
		{
			name: "oversize",
			id:   "oversize",
			setup: func() {
				config := testResponseCacheConfig()
				config.maxEntryBytes = 1
				resetResponseCacheStateForTest(config)
				setResponseCache("anon", "oversize", []json.RawMessage{responseCacheTestItem(1, "oversize")})
			},
		},
		{
			name: "evicted",
			id:   "evicted",
			setup: func() {
				config := testResponseCacheConfig()
				config.maxEntries = 1
				resetResponseCacheStateForTest(config)
				setResponseCache("anon", "evicted", []json.RawMessage{responseCacheTestItem(1, "first")})
				setResponseCache("anon", "new", []json.RawMessage{responseCacheTestItem(2, "second")})
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.setup()
			handler := NewHandler(newContinuationCodexStore(), nil, nil, nil)
			raw := []byte(`{"model":"gpt-5.4","previous_response_id":"` + tt.id + `","input":[{"role":"user","content":"continue"}],"stream":true}`)
			recorder := invokeResponsesHandler(t, handler.Responses, raw)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestResponsesCompactContinuationUnavailableRelayParity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)

	t.Run("Codex only 409", func(t *testing.T) {
		resetResponseCacheStateForTest(testResponseCacheConfig())
		handler := NewHandler(newContinuationCodexStore(), nil, nil, nil)
		recorder := invokeResponsesHandler(t, handler.ResponsesCompact, raw)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("relay preserves previous response id", func(t *testing.T) {
		resetResponseCacheStateForTest(testResponseCacheConfig())
		var seenBody []byte
		upstream := newContinuationRelayUpstream(t, true, &seenBody)
		handler := NewHandler(newContinuationRelayStore(upstream.URL), nil, nil, nil)
		recorder := invokeResponsesHandler(t, handler.ResponsesCompact, raw)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
		}
		if prev := gjson.GetBytes(seenBody, "previous_response_id").String(); prev != "resp_missing" {
			t.Fatalf("compact relay previous_response_id = %q, want preserved; body=%s", prev, seenBody)
		}
	})
}

func TestResponsesDependentCorruptAndTooLargeReturn409(t *testing.T) {
	tests := []struct {
		name   string
		result cache.ResponseContextReadResult
	}{
		{name: "corrupt", result: cache.ResponseContextReadResult{Status: cache.ResponseContextReadCorrupt}},
		{name: "too large", result: cache.ResponseContextReadResult{Status: cache.ResponseContextReadTooLarge}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			backend := newRecordingResponseContextBackend(true)
			backend.bounded = tt.result
			SetResponseContextCache(backend)
			t.Cleanup(func() {
				SetResponseContextCache(nil)
				_ = backend.TokenCache.Close()
			})
			handler := NewHandler(newContinuationCodexStore(), nil, nil, nil)
			raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}],"stream":true}`)
			recorder := invokeResponsesHandler(t, handler.Responses, raw)
			if recorder.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestResponsesContinuationScopeBudget429PrecedesCacheUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	raw := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_missing","input":[{"type":"function_call_output","call_id":"call_1","output":"ok"}],"stream":true}`)
	tests := []struct {
		name    string
		handler func(*Handler, *gin.Context)
	}{
		{name: "responses", handler: func(h *Handler, c *gin.Context) { h.Responses(c) }},
		{name: "compact", handler: func(h *Handler, c *gin.Context) { h.ResponsesCompact(c) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			upstream := newContinuationRelayUpstream(t, tt.name == "compact", new([]byte))
			handler := NewHandler(newContinuationRelayStore(upstream.URL), nil, nil, nil)
			recorder := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
				gate := &scopeBudgetGate{
					blockedAccounts: map[int64]struct{}{1: {}},
					blockedGroups:   make(map[int64]struct{}),
					message:         "scope budget exhausted before continuation fallback",
				}
				c.Set(contextScopeBudgetGate, gate)
			}, func(c *gin.Context) {
				tt.handler(handler, c)
			}, raw)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want scope 429 before continuation error; body=%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "scope budget exhausted") {
				t.Fatalf("response missing scope exhaustion reason: %s", recorder.Body.String())
			}
		})
	}
}

func TestResponsesCompactNormalRequestWaitsForTemporarilyBusyAccountBeforeScope429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetResponseCacheStateForTest(testResponseCacheConfig())
	var seenBody []byte
	upstream := newContinuationRelayUpstream(t, true, &seenBody)
	store := newContinuationRelayStore(upstream.URL)
	store.SetMaxConcurrency(1)
	store.AddAccount(&auth.Account{
		DBID:         2,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "relay-token-2",
		Models:       []string{"gpt-5.4"},
		PlanType:     "api",
	})
	held := store.NextExcludingWithFilter(0, nil, func(account *auth.Account) bool {
		return account != nil && account.DBID == 2
	})
	if held == nil {
		t.Fatal("failed to occupy the temporarily busy relay account")
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(75 * time.Millisecond)
		store.Release(held)
		close(released)
	}()

	handler := NewHandler(store, nil, nil, nil)
	raw := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"compact this"}]}`)
	recorder := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
		c.Set(contextScopeBudgetGate, &scopeBudgetGate{
			blockedAccounts: map[int64]struct{}{1: {}},
			blockedGroups:   make(map[int64]struct{}),
			message:         "scope budget exhausted for one account",
		})
	}, handler.ResponsesCompact, raw)
	<-released

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want wait then 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if len(seenBody) == 0 {
		t.Fatal("temporarily busy relay account was not used after release")
	}
}

func invokeResponsesHandler(t *testing.T, handler func(*gin.Context), body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return invokeResponsesHandlerWithContext(t, nil, handler, body)
}

func invokeResponsesHandlerWithContext(t *testing.T, setup func(*gin.Context), handler func(*gin.Context), body []byte) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	if setup != nil {
		setup(ctx)
	}
	start := time.Now()
	handler(ctx)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("handler took %s, want immediate routing decision", elapsed)
	}
	return recorder
}

func newContinuationCodexStore() *auth.Store {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "codex-token", PlanType: "plus", AccountID: "codex"})
	return store
}

func newContinuationRelayStore(upstreamURL string) *auth.Store {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstreamURL,
		APIKey:       "relay-token",
		Models:       []string{"gpt-5.4"},
		PlanType:     "api",
	})
	return store
}

func newContinuationRelayUpstream(t *testing.T, compact bool, seenBody *[]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seenBody = readUpstreamRequestBody(r)
		if compact {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"resp_compact","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_relay","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}
