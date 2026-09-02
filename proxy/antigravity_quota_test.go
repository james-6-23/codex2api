package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

const antigravityQuotaExhaustedBody = `{"error":{"code":429,"message":"You have exhausted your capacity on this model.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"model":"gemini-3.7-flash-tiered"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"4m50s"}]}}`

const antigravityShortRateLimitBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3.7-flash-tiered"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.5s"}]}}`

const antigravityModelCapacityBody = `{"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","metadata":{"model":"gemini-pro-agent"}}]}}`

func stubAntigravitySleep(t *testing.T) *[]time.Duration {
	t.Helper()
	previous := antigravitySleep
	waits := &[]time.Duration{}
	antigravitySleep = func(ctx context.Context, d time.Duration) error {
		*waits = append(*waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { antigravitySleep = previous })
	return waits
}

func TestParseAntigravityQuotaErrorClassifiesGoogleStatusDetails(t *testing.T) {
	quota, ok := parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(antigravityQuotaExhaustedBody))
	if !ok || quota.Kind != antigravityQuotaKindQuotaExhausted || quota.Model != "gemini-3.7-flash-tiered" || !quota.HasRetryDelay || quota.RetryDelay != 4*time.Minute+50*time.Second {
		t.Fatalf("quota exhausted parse = %+v ok=%v", quota, ok)
	}
	quota, ok = parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(antigravityShortRateLimitBody))
	if !ok || quota.Kind != antigravityQuotaKindRateLimit || quota.RetryDelay != 500*time.Millisecond {
		t.Fatalf("short rate limit parse = %+v ok=%v", quota, ok)
	}
	quota, ok = parseAntigravityQuotaError(http.StatusServiceUnavailable, []byte(antigravityModelCapacityBody))
	if !ok || quota.Kind != antigravityQuotaKindModelCapacity || quota.HasRetryDelay {
		t.Fatalf("model capacity parse = %+v ok=%v", quota, ok)
	}
	// A RATE_LIMIT_EXCEEDED whose delay is a whole quota window is a quota, not a burst.
	long := strings.Replace(antigravityShortRateLimitBody, `"0.5s"`, `"6m"`, 1)
	if quota, ok = parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(long)); !ok || quota.Kind != antigravityQuotaKindQuotaExhausted {
		t.Fatalf("long rate limit parse = %+v ok=%v", quota, ok)
	}
	// Proto object form of the duration.
	object := `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED"},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":{"seconds":2,"nanos":500000000}}]}}`
	if quota, ok = parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(object)); !ok || quota.RetryDelay != 2500*time.Millisecond {
		t.Fatalf("object retryDelay parse = %+v ok=%v", quota, ok)
	}
	// Unstructured bodies keep the generic handling.
	if _, ok = parseAntigravityQuotaError(http.StatusServiceUnavailable, []byte(`{"error":{"message":"Internal error encountered.","status":"INTERNAL"}}`)); ok {
		t.Fatal("unrelated 503 must not be classified as a quota signal")
	}
	if _, ok = parseAntigravityQuotaError(http.StatusBadRequest, []byte(antigravityQuotaExhaustedBody)); ok {
		t.Fatal("non-429/503 statuses must never be classified")
	}
}

func TestAntigravityExecutorRetriesSharedCapacityShortageInPlace(t *testing.T) {
	waits := stubAntigravitySleep(t)
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if n <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, antigravityModelCapacityBody)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	account := &auth.Account{DBID: 7101, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.1-pro-high", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || atomic.LoadInt32(&hits) != 3 {
		t.Fatalf("status=%d hits=%d, want the same endpoint retried twice then 200", resp.StatusCode, hits)
	}
	if len(*waits) != 2 || (*waits)[0] != antigravityModelCapacityRetryWait {
		t.Fatalf("waits = %v, want two one-second in-place waits", *waits)
	}
}

func TestAntigravityExecutorWaitsOutSubSecondRateLimitButNotQuota(t *testing.T) {
	waits := stubAntigravitySleep(t)
	var hits int32
	body := antigravityShortRateLimitBody
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, body)
			return
		}
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })
	account := &auth.Account{DBID: 7102, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}

	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.7-flash-high", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits != 2 || len(*waits) != 1 || (*waits)[0] != 500*time.Millisecond {
		t.Fatalf("status=%d hits=%d waits=%v, want one 500ms in-place wait then success", resp.StatusCode, hits, *waits)
	}

	// A real quota exhaustion is never waited out in place: with a single
	// endpoint the 429 goes straight back to the handler.
	atomic.StoreInt32(&hits, 0)
	*waits = nil
	body = antigravityQuotaExhaustedBody
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, body)
	}))
	defer server2.Close()
	antigravityOAuthEndpointBases = []string{server2.URL}
	resp, err = ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3.7-flash-high", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || hits != 1 || len(*waits) != 0 {
		t.Fatalf("status=%d hits=%d waits=%v, want the quota 429 returned without in-place retries", resp.StatusCode, hits, *waits)
	}
}

func TestApplyAntigravityCooldownUsesRetryDelayAndGeminiFamilyKey(t *testing.T) {
	store := newProxyPremiumTestStore()
	account := &auth.Account{DBID: 7201, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}

	decision := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityQuotaExhaustedBody), nil, "gemini-3.7-flash-high")
	if decision.Scope != rateLimitScopeModel || decision.Reason != "quota_exhausted" || decision.Model != "gemini-3.7-flash-high" {
		t.Fatalf("decision = %+v", decision)
	}
	if decision.Cooldown < 4*time.Minute+45*time.Second || decision.Cooldown > 4*time.Minute+50*time.Second {
		t.Fatalf("cooldown = %s, want the upstream retryDelay of 4m50s", decision.Cooldown)
	}
	if !account.IsModelRateLimited("gemini-3.7-flash-high") {
		t.Fatal("public model was not cooled down")
	}
	if !antigravityAccountModelRateLimited(account, "gemini-3.6-flash-low", "gemini-3.6-flash-low") {
		t.Fatal("sibling Gemini model should be blocked through the shared family key after QUOTA_EXHAUSTED")
	}
	if antigravityAccountModelRateLimited(account, "claude-sonnet-4-6", "claude-sonnet-4-6") {
		t.Fatal("Claude models must not be blocked by the Gemini family key")
	}

	capacity := &auth.Account{DBID: 7202, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	decision = applyAntigravityCooldown(store, capacity, http.StatusServiceUnavailable, []byte(antigravityModelCapacityBody), nil, "gemini-3.1-pro-high")
	if decision.Reason != "model_capacity" || decision.Cooldown > antigravityModelCapacityCooldown || decision.Cooldown <= 0 {
		t.Fatalf("capacity decision = %+v", decision)
	}
	if antigravityAccountModelRateLimited(capacity, "gemini-3.6-flash-low", "gemini-3.6-flash-low") {
		t.Fatal("a shared capacity shortage must not cool down the whole Gemini family")
	}
	if !antigravityNonPenalizingUpstreamFailure(capacity, http.StatusServiceUnavailable, []byte(antigravityModelCapacityBody)) {
		t.Fatal("MODEL_CAPACITY_EXHAUSTED must not count against the account")
	}
	if antigravityNonPenalizingUpstreamFailure(capacity, http.StatusTooManyRequests, []byte(antigravityQuotaExhaustedBody)) {
		t.Fatal("a real quota exhaustion is still an account-level signal")
	}

	generic := &auth.Account{DBID: 7203, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	decision = applyAntigravityCooldown(store, generic, http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`), &http.Response{Header: http.Header{}}, "gemini-3.7-flash-high")
	if decision.Reason != "rate_limited_model" {
		t.Fatalf("unstructured 429 should fall back to the relay policy, got %+v", decision)
	}
}
