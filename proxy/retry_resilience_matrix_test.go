package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestShouldRetryHTTPStatusUnlimitedBudgets(t *testing.T) {
	t.Run("general transient statuses", func(t *testing.T) {
		for _, statusCode := range []int{
			http.StatusInternalServerError,
			http.StatusServiceUnavailable,
		} {
			t.Run(http.StatusText(statusCode), func(t *testing.T) {
				generalRetries := 0
				rateLimitRetries := 0
				for attempt := 0; attempt < 64; attempt++ {
					if !shouldRetryHTTPStatus(statusCode, nil, &generalRetries, &rateLimitRetries, -1, 0) {
						t.Fatalf("status %d stopped at attempt %d with an unlimited general budget", statusCode, attempt+1)
					}
				}
				if rateLimitRetries != 0 {
					t.Fatalf("status %d consumed rate-limit budget: %d", statusCode, rateLimitRetries)
				}
			})
		}
	})

	t.Run("502 and 504 stay outside the legacy set", func(t *testing.T) {
		for _, statusCode := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
			generalRetries := 0
			rateLimitRetries := 0
			if shouldRetryHTTPStatus(statusCode, nil, &generalRetries, &rateLimitRetries, -1, -1) {
				t.Fatalf("status %d used an unlimited legacy budget", statusCode)
			}
			if generalRetries != 0 || rateLimitRetries != 0 {
				t.Fatalf("status %d changed counters: general=%d rate_limit=%d", statusCode, generalRetries, rateLimitRetries)
			}
		}
	})

	t.Run("rate limit budget stays independent", func(t *testing.T) {
		generalRetries := 0
		rateLimitRetries := 0
		for attempt := 0; attempt < 64; attempt++ {
			if !shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 0, -1) {
				t.Fatalf("429 stopped at attempt %d with an unlimited rate-limit budget", attempt+1)
			}
		}
		if generalRetries != 0 {
			t.Fatalf("429 consumed general budget: %d", generalRetries)
		}
	})

	t.Run("404 remains non-retryable", func(t *testing.T) {
		generalRetries := 0
		rateLimitRetries := 0
		if shouldRetryHTTPStatus(http.StatusNotFound, []byte(`{"error":{"message":"not found"}}`), &generalRetries, &rateLimitRetries, -1, -1) {
			t.Fatal("404 must not become globally retryable in unlimited mode")
		}
		if generalRetries != 0 || rateLimitRetries != 0 {
			t.Fatalf("404 changed retry counters: general=%d rate_limit=%d", generalRetries, rateLimitRetries)
		}
	})
}

func TestShouldRetryHTTPStatusFiniteAndDisabledBudgets(t *testing.T) {
	t.Run("zero disables retries", func(t *testing.T) {
		generalRetries := 0
		rateLimitRetries := 0
		if shouldRetryHTTPStatus(http.StatusServiceUnavailable, nil, &generalRetries, &rateLimitRetries, 0, -1) {
			t.Fatal("general retry budget 0 must disable 503 retries")
		}
		if shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, -1, 0) {
			t.Fatal("rate-limit retry budget 0 must disable 429 retries")
		}
	})

	t.Run("positive limits keep exact existing semantics", func(t *testing.T) {
		generalRetries := 0
		rateLimitRetries := 0
		for attempt := 0; attempt < 2; attempt++ {
			if !shouldRetryHTTPStatus(http.StatusServiceUnavailable, nil, &generalRetries, &rateLimitRetries, 2, 1) {
				t.Fatalf("503 retry %d unexpectedly denied", attempt+1)
			}
		}
		if shouldRetryHTTPStatus(http.StatusServiceUnavailable, nil, &generalRetries, &rateLimitRetries, 2, 1) {
			t.Fatal("503 retry exceeded the finite general budget")
		}

		if !shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 2, 1) {
			t.Fatal("first 429 retry unexpectedly denied")
		}
		if shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 2, 1) {
			t.Fatal("429 retry exceeded the finite rate-limit budget")
		}
	})
}

func TestBadGatewayAndGatewayTimeoutRetryPolicyMatrix(t *testing.T) {
	tests := []struct {
		name   string
		limit  int
		policy func(int) database.ContinuousRetryPolicy
		want   bool
	}{
		{
			name:  "disabled policy",
			limit: -1,
			policy: func(int) database.ContinuousRetryPolicy {
				return database.ContinuousRetryPolicy{Enabled: false, Categories: []string{database.ContinuousRetryCategoryHTTP5xx}}
			},
		},
		{
			name:  "finite legacy budget",
			limit: 2,
			policy: func(int) database.ContinuousRetryPolicy {
				return database.ContinuousRetryPolicy{}
			},
		},
		{
			name:  "http 5xx category",
			limit: 0,
			policy: func(int) database.ContinuousRetryPolicy {
				return database.ContinuousRetryPolicy{Enabled: true, Categories: []string{database.ContinuousRetryCategoryHTTP5xx}}
			},
			want: true,
		},
		{
			name:  "exact status",
			limit: 0,
			policy: func(status int) database.ContinuousRetryPolicy {
				return database.ContinuousRetryPolicy{Enabled: true, StatusCodes: []int{status}}
			},
			want: true,
		},
		{
			name:  "catch all",
			limit: 0,
			policy: func(int) database.ContinuousRetryPolicy {
				return database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
			},
			want: true,
		},
	}

	for _, statusCode := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		for _, tt := range tests {
			t.Run(fmt.Sprintf("%d/%s", statusCode, tt.name), func(t *testing.T) {
				policy := tt.policy(statusCode)
				generalRetries, rateLimitRetries := 0, 0
				if got := shouldRetryHTTPStatus(statusCode, nil, &generalRetries, &rateLimitRetries, tt.limit, 0, policy); got != tt.want {
					t.Fatalf("HTTP response retry = %v, want %v", got, tt.want)
				}

				generalRetries = 0
				requestErr := ErrUpstream(statusCode, http.StatusText(statusCode), errors.New("upstream status failure"))
				if got := shouldRetryRequestError(requestErr, &generalRetries, tt.limit, policy); got != tt.want {
					t.Fatalf("structured request-error retry = %v, want %v", got, tt.want)
				}
				if !tt.want && (generalRetries != 0 || rateLimitRetries != 0) {
					t.Fatalf("unselected status changed counters: general=%d rate_limit=%d", generalRetries, rateLimitRetries)
				}
			})
		}
	}
}

func TestHTTPRetryBackoffStateUsesMatchingBudget(t *testing.T) {
	if ordinal, limit := retryStateForHTTPStatus(http.StatusTooManyRequests, 9, 2, -1, 3); ordinal != 2 || limit != 3 {
		t.Fatalf("429 retry state = (%d, %d), want (2, 3)", ordinal, limit)
	}
	if ordinal, limit := retryStateForHTTPStatus(http.StatusServiceUnavailable, 4, 11, -1, 1); ordinal != 4 || limit != -1 {
		t.Fatalf("503 retry state = (%d, %d), want (4, -1)", ordinal, limit)
	}
}

func TestShouldRetryRequestErrorBudgetModes(t *testing.T) {
	retryable := errors.New("read tcp: connection reset by peer")

	t.Run("unlimited", func(t *testing.T) {
		generalRetries := 0
		for attempt := 0; attempt < 64; attempt++ {
			if !shouldRetryRequestError(retryable, &generalRetries, -1) {
				t.Fatalf("transport retry stopped at attempt %d with an unlimited budget", attempt+1)
			}
		}
	})

	t.Run("zero", func(t *testing.T) {
		generalRetries := 0
		if shouldRetryRequestError(retryable, &generalRetries, 0) {
			t.Fatal("transport retry budget 0 must disable retries")
		}
	})

	t.Run("finite", func(t *testing.T) {
		generalRetries := 0
		if !shouldRetryRequestError(retryable, &generalRetries, 1) {
			t.Fatal("first transport retry unexpectedly denied")
		}
		if shouldRetryRequestError(retryable, &generalRetries, 1) {
			t.Fatal("transport retry exceeded the finite budget")
		}
	})
}

func TestRetryableRequestErrorStructuredPrecedence(t *testing.T) {
	networkCause := errors.New("connection reset by peer")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "bad request", err: ErrBadRequest("invalid payload")},
		{name: "wrapped bad request", err: fmt.Errorf("executor: %w", ErrBadRequest("invalid payload"))},
		{name: "internal error with network-looking cause", err: ErrInternalError("serialization failed", networkCause)},
		{name: "wrapped internal error", err: fmt.Errorf("executor: %w", ErrInternalError("serialization failed", networkCause))},
		{name: "statusful non-retryable upstream error", err: ErrUpstream(http.StatusNotFound, "missing endpoint", networkCause)},
		{name: "statusless upstream transport error", err: ErrUpstream(0, "request failed", networkCause), want: true},
		{name: "plain transport error", err: networkCause, want: true},
		{name: "canceled error", err: context.Canceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRetryableRequestError(tt.err); got != tt.want {
				t.Fatalf("isRetryableRequestError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if isRetryableRequestErrorForContext(ctx, networkCause) {
		t.Fatal("a downstream-canceled request must not classify a transport error as retryable")
	}
}

func TestUnlimitedTransparentStreamRetryBoundaries(t *testing.T) {
	retryable := streamOutcome{
		logStatusCode:  logStatusUpstreamStreamBreak,
		failureKind:    "transport",
		failureMessage: "upstream failed before first downstream byte",
		penalize:       true,
	}

	if !shouldTransparentRetryStream(retryable, 100_000, -1, false, nil, nil) {
		t.Fatal("an early stream break should remain retryable with an unlimited budget")
	}
	if shouldTransparentRetryStream(retryable, 0, -1, true, nil, nil) {
		t.Fatal("a stream must never be replayed after downstream body bytes were written")
	}
	if shouldTransparentRetryStream(retryable, 0, -1, false, context.Canceled, nil) {
		t.Fatal("an unlimited retry loop must stop when the downstream context is canceled")
	}
	if shouldTransparentRetryStream(retryable, 0, -1, false, nil, errors.New("downstream write failed")) {
		t.Fatal("an unlimited retry loop must stop after a downstream write failure")
	}
	if shouldTransparentRetryStream(streamOutcome{penalize: false}, 0, -1, false, nil, nil) {
		t.Fatal("a non-retryable stream outcome must stay non-retryable in unlimited mode")
	}
	if shouldTransparentRetryStream(retryable, 0, 0, false, nil, nil) {
		t.Fatal("stream retry budget 0 must disable transparent retries")
	}
}

func TestTransparentStreamRetryBudgetsStayIndependent(t *testing.T) {
	rateLimited := streamOutcome{
		logStatusCode:  http.StatusTooManyRequests,
		failureKind:    "rate_limited",
		failureMessage: "temporarily rate limited",
		penalize:       true,
	}
	generalRetries := 0
	rateLimitRetries := 0
	for attempt := 0; attempt < 32; attempt++ {
		if !shouldTransparentRetryStreamWithBudgets(rateLimited, &generalRetries, &rateLimitRetries, 0, -1, false, nil, nil) {
			t.Fatalf("stream 429 stopped at attempt %d despite an unlimited rate-limit budget", attempt+1)
		}
	}
	if generalRetries != 0 || rateLimitRetries != 32 {
		t.Fatalf("stream 429 counters = general:%d rate_limit:%d, want 0/32", generalRetries, rateLimitRetries)
	}

	serverFailure := streamOutcome{
		logStatusCode:  http.StatusServiceUnavailable,
		failureKind:    "server",
		failureMessage: "temporarily unavailable",
		penalize:       true,
	}
	generalRetries = 0
	rateLimitRetries = 0
	if !shouldTransparentRetryStreamWithBudgets(serverFailure, &generalRetries, &rateLimitRetries, -1, 1, false, nil, nil) {
		t.Fatal("503 did not consume the unlimited general budget")
	}
	if !shouldTransparentRetryStreamWithBudgets(rateLimited, &generalRetries, &rateLimitRetries, -1, 1, false, nil, nil) {
		t.Fatal("first stream 429 retry was denied by the general counter")
	}
	if shouldTransparentRetryStreamWithBudgets(rateLimited, &generalRetries, &rateLimitRetries, -1, 1, false, nil, nil) {
		t.Fatal("stream 429 exceeded its finite independent rate-limit budget")
	}
}

func TestImageRetryCapOnlyYieldsToSelectedContinuousPolicy(t *testing.T) {
	generalRetries := 0
	streamErr := errors.New("upstream image stream disconnected")
	for attempt := 0; attempt < maxImageAttempts-1; attempt++ {
		if !shouldRetryImageStreamError(streamErr, &generalRetries, -1, attempt, maxImageAttempts) {
			t.Fatalf("image retry %d was denied before the total-attempt cap", attempt+1)
		}
	}
	if shouldRetryImageStreamError(streamErr, &generalRetries, -1, maxImageAttempts-1, maxImageAttempts) {
		t.Fatal("unselected legacy retry budget bypassed the image total-attempt cap")
	}

	policy := database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryTransport},
	}
	if !shouldRetryImageStreamError(streamErr, &generalRetries, 0, maxImageAttempts-1, maxImageAttempts, policy) {
		t.Fatal("selected continuous image failure did not bypass the ordinary attempt cap")
	}
	if !retryAllowedByEndpointCap(maxGrokMediaAttempts-1, maxGrokMediaAttempts, true) {
		t.Fatal("selected continuous Grok media failure did not bypass the ordinary attempt cap")
	}
	if retryAllowedByEndpointCap(maxGrokMediaAttempts-1, maxGrokMediaAttempts, false) {
		t.Fatal("ordinary Grok media retry bypassed the endpoint attempt cap")
	}
}

func TestUnlimitedResponseFailedSuppressionBoundaries(t *testing.T) {
	retryableFailed := []byte(`{"type":"response.failed","response":{"error":{"message":"upstream overloaded","status_code":503,"code":"server_error"}}}`)

	if !shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 100_000, -1, nil, nil) {
		t.Fatal("a retryable response.failed should stay hidden before the first token in unlimited mode")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, true, false, 0, -1, nil, nil) {
		t.Fatal("response.failed must not be hidden after first-token progress")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, true, 0, -1, nil, nil) {
		t.Fatal("response.failed must not be hidden after downstream body bytes were written")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 0, -1, context.Canceled, nil) {
		t.Fatal("response.failed must not be hidden after downstream cancellation")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 0, -1, nil, errors.New("downstream write failed")) {
		t.Fatal("response.failed must not be hidden after a downstream write failure")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 0, 0, nil, nil) {
		t.Fatal("response.failed must not be hidden when retries are disabled")
	}
}

func TestWaitBeforeRetryDeadlineCancelsLongInterval(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(30_000)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if h.waitBeforeRetry(ctx) {
		t.Fatal("waitBeforeRetry returned true after the downstream deadline")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("downstream deadline did not interrupt retry wait promptly: %v", elapsed)
	}
}

func TestUnlimitedRetryInvalidRetryAfterFallsBackToBackoff(t *testing.T) {
	h, store := newRetryTestHandler(t)
	store.SetRetryIntervalMS(0)
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("Retry-After", "not-a-valid-delay")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if h.waitBeforeRetryWithBudget(ctx, 1, -1, resp) {
		t.Fatal("invalid Retry-After bypassed unlimited retry backoff")
	}
}

func TestResponsesContinuousRetryCyclesSingleAccountAfter503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:    true,
			Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
		}
		return current
	})

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"temporarily unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_retried","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "test-relay-key",
		Models:       []string{"gpt-5.4"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello","stream":true}`)).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (503 then success on the same account)", got)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"response.completed"`) || strings.Contains(body, "temporarily unavailable") {
		t.Fatalf("retry was not transparent: %s", body)
	}
}

func TestResponsesContinuousRetryCatchAllRotatesAndRepeatsPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
		current.CodexForceWebsocket = false
		current.CodexWSSilentRetry = false
		current.CodexWSSilentRetries = 0
		current.CodexPreflightSSEPassthrough = true
		return current
	})

	var requestMu sync.Mutex
	var authorizationHeaders []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestMu.Lock()
		authorizationHeaders = append(authorizationHeaders, r.Header.Get("Authorization"))
		attempt := len(authorizationHeaders)
		requestMu.Unlock()

		if attempt == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTeapot)
			_, _ = io.WriteString(w, `{"error":{"code":"future_unknown_failure","message":"must stay upstream"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if attempt == 2 {
			_, _ = io.WriteString(w, `data: {"type":"codex.rate_limits","plan_type":"plus"}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"failed-attempt-partial"}`+"\n\n")
			_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"status":"failed","status_code":400,"error":{"code":"content_policy_violation","message":"must stay upstream"}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_catch_all_recovered","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	for _, account := range []struct {
		id  int64
		key string
	}{
		{id: 1, key: "catch-all-account-one"},
		{id: 2, key: "catch-all-account-two"},
	} {
		store.AddAccount(&auth.Account{
			DBID:         account.id,
			UpstreamType: auth.UpstreamOpenAIResponses,
			BaseURL:      upstream.URL,
			APIKey:       account.key,
			Models:       []string{"gpt-5.4"},
			PlanType:     "api",
		})
	}
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello","stream":true}`)).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.Responses(ctx)

	requestMu.Lock()
	gotHeaders := append([]string(nil), authorizationHeaders...)
	requestMu.Unlock()
	if len(gotHeaders) != 3 {
		t.Fatalf("upstream calls = %d, want 3 (two unknown failures then success)", len(gotHeaders))
	}
	if gotHeaders[0] == "" || gotHeaders[0] == gotHeaders[1] {
		t.Fatalf("catch-all did not rotate accounts before repeating the pool: first=%q second=%q", gotHeaders[0], gotHeaders[1])
	}
	if gotHeaders[2] != gotHeaders[0] && gotHeaders[2] != gotHeaders[1] {
		t.Fatalf("third attempt did not reuse the exhausted account pool: headers=%q", gotHeaders)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"id":"resp_catch_all_recovered"`) || strings.Contains(body, "failed-attempt-partial") || strings.Contains(body, "must stay upstream") || strings.Contains(body, "codex.rate_limits") {
		t.Fatalf("catch-all retry was not transparent: %s", body)
	}
}

func TestContinuousRetryCatchAllDoesNotReplaySuccessfulNonStreamingResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
		current.CodexForceWebsocket = false
		return current
	})

	tests := []struct {
		name   string
		path   string
		body   string
		invoke func(*Handler, *gin.Context)
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"gpt-4.1-direct","input":"hello","stream":false}`, invoke: (*Handler).Responses},
		{name: "chat completions", path: "/v1/chat/completions", body: `{"model":"gpt-4.1-direct","messages":[{"role":"user","content":"hello"}]}`, invoke: (*Handler).ChatCompletions},
		{name: "anthropic messages", path: "/v1/messages", body: `{"model":"gpt-4.1-direct","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`, invoke: (*Handler).Messages},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				requestBody, _ := io.ReadAll(r.Body)
				if !gjson.GetBytes(requestBody, "stream").Bool() {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp_success_once","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []string{
					`{"type":"response.created","response":{"id":"resp_success_once"}}`,
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`,
					`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"OK"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}`,
					`{"type":"response.completed","response":{"id":"resp_success_once","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				} {
					_, _ = io.WriteString(w, "data: "+event+"\n\n")
				}
			}))
			t.Cleanup(upstream.Close)

			store := newOpenAIResponsesRelayStore(upstream.URL)
			t.Cleanup(store.Stop)
			handler := NewHandler(store, nil, nil, nil)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(requestCtx)
			ctx.Request.Header.Set("Content-Type", "application/json")

			tc.invoke(handler, ctx)

			if got := calls.Load(); got != 1 {
				t.Fatalf("successful upstream calls = %d, want exactly 1; status=%d body=%s", got, recorder.Code, recorder.Body.String())
			}
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "OK") {
				t.Fatalf("successful response was not returned: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestContinuousRetryNonStreamingErrorEventCannotBeOverriddenByCompleted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCatchAllContinuousRetry(t)

	tests := []struct {
		name   string
		path   string
		body   string
		invoke func(*Handler, *gin.Context)
	}{
		{name: "responses", path: "/v1/responses", body: `{"model":"gpt-4.1-direct","input":"hello","stream":false}`, invoke: (*Handler).Responses},
		{name: "chat completions", path: "/v1/chat/completions", body: `{"model":"gpt-4.1-direct","messages":[{"role":"user","content":"hello"}]}`, invoke: (*Handler).ChatCompletions},
		{name: "anthropic messages", path: "/v1/messages", body: `{"model":"gpt-4.1-direct","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`, invoke: (*Handler).Messages},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempt := calls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if attempt == 1 {
					_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"code\":\"future_failure\",\"message\":\"must stay upstream\"}}\n\n")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_poisoned\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"poisoned-success\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
					return
				}
				if tc.name == "responses" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp_real","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"real-success"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
					return
				}
				for _, event := range []string{
					`{"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant","content":[]}}`,
					`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"real-success"}`,
					`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"real-success"}]}}`,
					`{"type":"response.completed","response":{"id":"resp_real","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"real-success"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				} {
					_, _ = io.WriteString(w, "data: "+event+"\n\n")
				}
			}))
			t.Cleanup(upstream.Close)

			store := newOpenAIResponsesRelayStore(upstream.URL)
			t.Cleanup(store.Stop)
			handler := NewHandler(store, nil, nil, nil)
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)).WithContext(requestCtx)
			ctx.Request.Header.Set("Content-Type", "application/json")

			tc.invoke(handler, ctx)

			body := recorder.Body.String()
			if got := calls.Load(); got != 2 {
				t.Fatalf("upstream calls = %d, want error attempt plus success; status=%d body=%s", got, recorder.Code, body)
			}
			if recorder.Code != http.StatusOK || !strings.Contains(body, "real-success") {
				t.Fatalf("successful retry missing: status=%d body=%s", recorder.Code, body)
			}
			if strings.Contains(body, "poisoned-success") || strings.Contains(body, "must stay upstream") || strings.Contains(body, "future_failure") {
				t.Fatalf("error attempt was accepted or leaked: %s", body)
			}
		})
	}
}

func TestContinuousRetryCatchAllStopsAtCYBAndRetainsPolicyDecision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	previousKeepaliveInterval := continuousRetryKeepaliveInterval
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
		continuousRetryKeepaliveInterval = previousKeepaliveInterval
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
		current.CodexForceWebsocket = false
		return current
	})
	continuousRetryKeepaliveInterval = time.Hour

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempt := calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if attempt == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"status":"failed","status_code":400,"error":{"code":"cyber_policy","message":"must stop this turn"}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"recovered-after-policy"}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_policy_recovered","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)

	handler, _ := newPromptConversationLockTestHandler(t)
	t.Cleanup(handler.store.Stop)
	for id := int64(1); id <= 2; id++ {
		handler.store.AddAccount(&auth.Account{
			DBID: id, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL,
			APIKey: fmt.Sprintf("policy-retry-%d", id), Models: []string{"gpt-4.1-direct"}, PlanType: "api",
		})
	}
	body := []byte(`{"model":"gpt-4.1-direct","input":"ordinary request","stream":true}`)
	c, recorder := signedBoundPromptConversationContextWithRecorder(t, "policy-retry-request", newAPIIdentity{
		UserID: "42", ClientIP: "203.0.113.8",
	}, body, "0123456789abcdef0123456789abcdef")

	handler.Responses(c)

	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want one explicit CYB attempt; body=%s", got, recorder.Body.String())
	}
	if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "recovered-after-policy") {
		t.Fatalf("explicit CYB did not terminate the request: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	metadata, ok := newAPIUpstreamCyberPolicyDecision(c)
	if !ok || !metadata.ConversationLocked {
		t.Fatalf("explicit CYB decision = %+v delegated=%t", metadata, ok)
	}
}

func TestResponsesContinuousRetrySelectedDeterministicStatuses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, statusCode := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusNotImplemented} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(statusCode)
					_, _ = io.WriteString(w, fmt.Sprintf(`{"error":{"code":"status_%d","message":"selected deterministic failure"}}`, statusCode))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_selected_status","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			}))
			t.Cleanup(upstream.Close)

			policy := database.ContinuousRetryPolicy{Enabled: true, StatusCodes: []int{statusCode}}
			previousRuntime := CurrentRuntimeSettings()
			t.Cleanup(func() {
				UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
			})
			UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
				current.ContinuousRetryPolicy = policy
				current.CodexForceWebsocket = false
				current.CodexWSSilentRetry = false
				current.CodexWSSilentRetries = 0
				return current
			})

			store := auth.NewStore(nil, nil, &database.SystemSettings{
				MaxConcurrency:      1,
				TestConcurrency:     1,
				TestModel:           "gpt-5.4",
				MaxRetries:          0,
				MaxRateLimitRetries: 0,
			})
			t.Cleanup(store.Stop)
			for _, id := range []int64{1, 2} {
				store.AddAccount(&auth.Account{
					DBID:         id,
					UpstreamType: auth.UpstreamOpenAIResponses,
					BaseURL:      upstream.URL,
					APIKey:       fmt.Sprintf("test-relay-key-%d", id),
					Models:       []string{"gpt-5.4"},
					PlanType:     "api",
				})
			}
			handler := NewHandler(store, nil, nil, nil)

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello","stream":true}`)).WithContext(requestCtx)
			ctx.Request.Header.Set("Content-Type", "application/json")
			handler.Responses(ctx)

			if got := calls.Load(); got != 2 {
				t.Fatalf("status %d upstream calls = %d, want 2", statusCode, got)
			}
			if recorder.Code != http.StatusOK {
				t.Fatalf("status %d downstream status = %d, want 200; body=%s", statusCode, recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `"type":"response.completed"`) || strings.Contains(body, "selected deterministic failure") {
				t.Fatalf("status %d retry was not transparent: %s", statusCode, body)
			}
		})
	}
}

func TestResponsesCompactContinuousRetryCyclesSingleAccountAfter503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:    true,
			Categories: []string{database.ContinuousRetryCategoryHTTP5xx},
		}
		return current
	})

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"temporarily unavailable"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp_compact_retried","object":"response.compaction","output":[]}`)
	}))
	t.Cleanup(upstream.Close)

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "test-relay-key",
		Models:       []string{"gpt-5.4"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello"}`)).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ResponsesCompact(ctx)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (503 then compact success on the same account)", got)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"id":"resp_compact_retried"`) || strings.Contains(body, "temporarily unavailable") {
		t.Fatalf("compact retry was not transparent: %s", body)
	}
}

func TestResponsesCompactContinuousRetrySelectsResponseFailedEvent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousRuntime := CurrentRuntimeSettings()
	previousResin := resinCfg.Load()
	t.Cleanup(func() {
		UpdateRuntimeSettings(func(RuntimeSettings) RuntimeSettings { return previousRuntime })
		resinCfg.Store(previousResin)
	})
	UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
		current.CompactViaResponses = true
		current.ContinuousRetryPolicy = database.ContinuousRetryPolicy{
			Enabled:    true,
			Categories: []string{database.ContinuousRetryCategoryResponseFailed},
		}
		return current
	})

	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses") {
			t.Fatalf("upstream path = %q, want Resin path ending /backend-api/codex/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `data: {"type":"response.failed","response":{"id":"resp_failed_once","status":"failed","status_code":503,"error":{"code":"server_error","message":"temporary compact failure"}}}`+"\n\n")
			return
		}
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_compact_recovered","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n")
	}))
	t.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		TestConcurrency:     1,
		TestModel:           "gpt-5.4",
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:        1,
		AccessToken: "test-token",
		AccountID:   "test-account",
		Models:      []string{"gpt-5.4"},
		PlanType:    "pro",
	})
	handler := NewHandler(store, nil, nil, nil)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewBufferString(`{"model":"gpt-5.4","input":"hello"}`)).WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.ResponsesCompact(ctx)

	if got := calls.Load(); got != 2 {
		t.Fatalf("upstream calls = %d, want 2 (response.failed then success on the same account)", got)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("downstream status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"id":"resp_compact_recovered"`) || strings.Contains(body, "temporary compact failure") {
		t.Fatalf("compact body-signal retry was not transparent: %s", body)
	}
}
