package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const antigravityQuotaExhaustedBody = `{"error":{"code":429,"message":"You have exhausted your capacity on this model.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","domain":"cloudcode-pa.googleapis.com","metadata":{"model":"gemini-3.7-flash-tiered"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"4m50s"}]}}`

const antigravityShortRateLimitBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3.7-flash-tiered"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"0.5s"}]}}`

const antigravityModelCapacityBody = `{"error":{"code":503,"message":"The model is overloaded.","status":"UNAVAILABLE","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"MODEL_CAPACITY_EXHAUSTED","metadata":{"model":"gemini-pro-agent"}}]}}`

// A bare RESOURCE_EXHAUSTED: no ErrorInfo, no RetryInfo. This is the shape
// Cloud Code returns when it sheds load, and it names neither a reason nor a
// retry delay, so it says nothing about the credential that received it.
const antigravityUnclassifiedRateLimitBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`

// A named RATE_LIMIT_EXCEEDED with no RetryInfo: Google attributes the limit
// but gives no timing hint.
const antigravityReasonOnlyRateLimitBody = `{"error":{"code":429,"message":"Rate limit exceeded.","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3.7-flash-tiered"}}]}}`

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

func TestApplyAntigravityCooldownBacksOffBrieflyWhenGoogleGivesNoHint(t *testing.T) {
	store := newProxyPremiumTestStore()

	quota, ok := parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody))
	if !ok || quota.Kind != antigravityQuotaKindRateLimit || quota.Reason != "" || quota.HasRetryDelay {
		t.Fatalf("bare RESOURCE_EXHAUSTED parse = %+v ok=%v, want a hint-less rate limit", quota, ok)
	}

	unclassified := &auth.Account{DBID: 7301, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	decision := applyAntigravityCooldown(store, unclassified, http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody), nil, "gemini-3.7-flash-high")
	if decision.Reason != "rate_limited_model" || decision.Model != "gemini-3.7-flash-high" {
		t.Fatalf("decision = %+v", decision)
	}
	if decision.Cooldown <= 0 || decision.Cooldown > antigravityUnclassifiedRateLimitCooldown {
		t.Fatalf("cooldown = %s, want a short back-off no longer than %s", decision.Cooldown, antigravityUnclassifiedRateLimitCooldown)
	}
	// The pair still steps out of rotation, just briefly: with no cooldown at
	// all the scheduler would hand this account the next request and collect
	// another 429.
	if !unclassified.IsModelRateLimited("gemini-3.7-flash-high") {
		t.Fatal("a bare 429 must still bench the (account, model) pair")
	}
	// Unlike a real QUOTA_EXHAUSTED, it must not poison the shared Gemini key.
	if antigravityAccountModelRateLimited(unclassified, "claude-sonnet-4-6", "claude-sonnet-4-6") {
		t.Fatal("a hint-less 429 must not cool down unrelated models")
	}

	// A named RATE_LIMIT_EXCEEDED with no delay keeps the conservative bench.
	named := &auth.Account{DBID: 7302, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	decision = applyAntigravityCooldown(store, named, http.StatusTooManyRequests, []byte(antigravityReasonOnlyRateLimitBody), nil, "gemini-3.7-flash-high")
	if decision.Cooldown < antigravityDefaultRateLimitCooldown-time.Second {
		t.Fatalf("cooldown = %s, want the conservative %s default for a named rate limit", decision.Cooldown, antigravityDefaultRateLimitCooldown)
	}
}

// TestApplyAntigravityCooldownPrefersRetryAfterHeaderOverTheShortBackOff 覆盖
// Retry-After 响应头优先：结构化 body 没有给出 RetryInfo 时，头部秒数必须取代裸
// RESOURCE_EXHAUSTED 的短回退时长。
func TestApplyAntigravityCooldownPrefersRetryAfterHeaderOverTheShortBackOff(t *testing.T) {
	store := newProxyPremiumTestStore()
	account := &auth.Account{DBID: 7303, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	resp := &http.Response{Header: http.Header{"Retry-After": []string{"12"}}}
	decision := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody), resp, "gemini-3.7-flash-high")
	if decision.Cooldown < 11*time.Second || decision.Cooldown > 12*time.Second {
		t.Fatalf("cooldown = %s, want the 12s Retry-After header instead of the short back-off", decision.Cooldown)
	}
}

// TestApplyAntigravityCooldownCoolsSharedWireModel 覆盖别名冷却联动：
// gemini-3.8-flash-low/medium/high 共用上游 backing gemini-3.8-flash-tiered，
// high 被限流后必须同时冷却 wire 模型，否则 low/medium 仍会选中同一账号并继续
// 收集 429。
func TestApplyAntigravityCooldownCoolsSharedWireModel(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	account := &auth.Account{DBID: 7401, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}

	decision := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityReasonOnlyRateLimitBody), nil, "gemini-3.8-flash-high")
	if decision.Model != "gemini-3.8-flash-high" || decision.Reason != "rate_limited_model" {
		t.Fatalf("decision = %+v", decision)
	}
	if !account.IsModelRateLimited("gemini-3.8-flash-high") {
		t.Fatal("the public tier was not cooled down")
	}
	if !account.IsModelRateLimited("gemini-3.8-flash-tiered") {
		t.Fatal("the shared wire model was not cooled down alongside the public tier")
	}
	if remaining, wireRemaining := account.ModelCooldownRemaining("gemini-3.8-flash-high"), account.ModelCooldownRemaining("gemini-3.8-flash-tiered"); remaining <= 0 || wireRemaining > remaining+time.Second {
		t.Fatalf("wire cooldown %s drifted from the public cooldown %s", wireRemaining, remaining)
	}
	// 同 backing 的兄弟档位被拦住，不同 backing 与 Claude 模型不受影响。
	if !antigravityAccountModelRateLimited(account, "gemini-3.8-flash-low", "gemini-3.8-flash-tiered") {
		t.Fatal("a sibling tier on the same backing must be blocked")
	}
	if antigravityAccountModelRateLimited(account, "gemini-3.6-flash-low", "gemini-3.6-flash-low") {
		t.Fatal("a different backing must stay schedulable")
	}
	if antigravityAccountModelRateLimited(account, "claude-sonnet-4-6", "claude-sonnet-4-6") {
		t.Fatal("Claude models must stay schedulable")
	}
}

// TestApplyAntigravityCooldownCoolsSharedWireModelOnUnstructured429 覆盖非结构化 429：
// 解析不出 google.rpc 结构的 429 走通用 429 策略，而通用策略只按 public id 落冷却，
// 同 wire 的兄弟档位仍会选中刚被限流的账号。这里必须补写与 public 同 deadline 的
// wire 冷却。
func TestApplyAntigravityCooldownCoolsSharedWireModelOnUnstructured429(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	// Antigravity 账号属于 relay 风格，通用 429 策略默认关闭其模型冷却，
	// 这里显式打开才能观察到 wire 补写。
	store.SetModelCooldownSettings(database.ModelCooldownSettings{
		RelayMode:    database.ModelCooldownModeFixed,
		RelaySeconds: 60,
		OAuthMode:    database.ModelCooldownModeFixed,
		OAuthSeconds: 60,
	})
	account := &auth.Account{DBID: 7451, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}

	if _, ok := parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`)); ok {
		t.Fatal("the body must stay unparseable so the generic 429 path is exercised")
	}
	decision := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`), &http.Response{Header: http.Header{}}, "gemini-3.8-flash-high")
	if decision.Scope != rateLimitScopeModel || decision.Model != "gemini-3.8-flash-high" || decision.ResetAt.IsZero() {
		t.Fatalf("decision = %+v, want a model-scoped cooldown", decision)
	}
	if !account.IsModelRateLimited("gemini-3.8-flash-high") {
		t.Fatal("the public tier was not cooled down")
	}
	if !account.IsModelRateLimited("gemini-3.8-flash-tiered") {
		t.Fatal("an unstructured 429 must cool the shared wire model alongside the public tier")
	}
	if remaining, wireRemaining := account.ModelCooldownRemaining("gemini-3.8-flash-high"), account.ModelCooldownRemaining("gemini-3.8-flash-tiered"); remaining <= 0 || wireRemaining > remaining+time.Second {
		t.Fatalf("wire cooldown %s drifted from the public cooldown %s", wireRemaining, remaining)
	}
	if !antigravityAccountModelRateLimited(account, "gemini-3.8-flash-low", "gemini-3.8-flash-tiered") {
		t.Fatal("a sibling tier on the same backing must be blocked")
	}
	if antigravityAccountModelRateLimited(account, "gemini-3.6-flash-low", "gemini-3.6-flash-low") {
		t.Fatal("a different backing must stay schedulable")
	}
	// 非结构化 429 依旧只记半个惩罚，且事件带上解析出的 wire 模型。
	if got := account.AntigravityPenalty(time.Now()); got < 0.49 || got > 0.51 {
		t.Fatalf("penalty = %v, want the unclassified 0.5", got)
	}
}

// TestApplyAntigravityCooldownPenaltyWeights 覆盖调度惩罚的事件权重：明确限流
// 记满，裸 RESOURCE_EXHAUSTED 与非结构化 429 记一半，共享容量与账号配额不记。
func TestApplyAntigravityCooldownPenaltyWeights(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	newAccount := func(id int64) *auth.Account {
		return &auth.Account{DBID: id, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	}

	cases := []struct {
		name       string
		statusCode int
		body       string
		want       float64
	}{
		{"explicit rate limit", http.StatusTooManyRequests, antigravityReasonOnlyRateLimitBody, 1.0},
		{"bare resource exhausted", http.StatusTooManyRequests, antigravityUnclassifiedRateLimitBody, 0.5},
		{"unstructured 429", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, 0.5},
		{"quota exhausted", http.StatusTooManyRequests, antigravityQuotaExhaustedBody, 0.0},
		{"shared model capacity", http.StatusServiceUnavailable, antigravityModelCapacityBody, 0.0},
	}
	for _, tc := range cases {
		account := newAccount(7410)
		applyAntigravityCooldown(store, account, tc.statusCode, []byte(tc.body), nil, "gemini-3.8-flash-high")
		if got := account.AntigravityPenalty(time.Now()); math.Abs(got-tc.want) > 0.01 {
			t.Fatalf("%s penalty = %v, want %v", tc.name, got, tc.want)
		}
	}

	// 同一账号连续被限流时惩罚累加，首绑避险据此把它压到队尾。
	account := newAccount(7411)
	applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityReasonOnlyRateLimitBody), nil, "gemini-3.8-flash-high")
	applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody), nil, "gemini-3.8-flash-high")
	if got := account.AntigravityPenalty(time.Now()); got < 1.49 || got > 1.51 {
		t.Fatalf("accumulated penalty = %v, want ~1.5", got)
	}
}

// TestApplyAntigravityCooldownAdminProbeSkipsPenalty 覆盖 admin 连通性测试隔离：
// 冷却照常写入（否则测试按钮探到的限流会被线上流量无视），但不进线上调度惩罚
// 累加器（否则一次手工测试就会把健康账号挤出首绑）。
func TestApplyAntigravityCooldownAdminProbeSkipsPenalty(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	probed := &auth.Account{DBID: 7420, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}

	if !ApplyAntigravityCooldown(store, probed, http.StatusTooManyRequests, []byte(antigravityReasonOnlyRateLimitBody), nil, "gemini-3.8-flash-high") {
		t.Fatal("the admin probe did not record a cooldown")
	}
	if !probed.IsModelRateLimited("gemini-3.8-flash-high") || !probed.IsModelRateLimited("gemini-3.8-flash-tiered") {
		t.Fatal("the admin probe must keep writing the public and wire model cooldowns")
	}
	if got := probed.AntigravityPenalty(time.Now()); got != 0 {
		t.Fatalf("admin probe penalty = %v, want 0", got)
	}

	live := &auth.Account{DBID: 7421, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
	applyAntigravityCooldown(store, live, http.StatusTooManyRequests, []byte(antigravityReasonOnlyRateLimitBody), nil, "gemini-3.8-flash-high")
	if got := live.AntigravityPenalty(time.Now()); got <= 0 {
		t.Fatalf("live dispatch penalty = %v, want a recorded event", got)
	}
}

// TestGeminiNativeCountTokensAppliesModelCooldown 覆盖 native countTokens 的
// 429 漏路径：该分支过去只计数与排除账号，从不写模型冷却。
func TestGeminiNativeCountTokensAppliesModelCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, antigravityReasonOnlyRateLimitBody)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.SetMaxRetries(0)
	store.SetMaxRateLimitRetries(0)
	account := &auth.Account{
		DBID: 7430, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project",
		Models: []string{"gemini-3.7-flash-tiered", "gemini-3.8-flash-tiered"},
	}
	store.AddAccount(account)

	handler := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelAntigravity}}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(contextAPIKeyRow, row)
		c.Next()
	})
	router.POST("/v1beta/models/*action", handler.GeminiModelsAction)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash-high:countTokens", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("upstream hits = %d, want one attempt", hits)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body=%s, want the upstream 429 surfaced", rec.Code, rec.Body.String())
	}
	if !account.IsModelRateLimited("gemini-3.8-flash-high") {
		t.Fatal("countTokens 429 did not cool down the public model")
	}
	if !account.IsModelRateLimited("gemini-3.8-flash-tiered") {
		t.Fatal("countTokens 429 did not cool down the shared wire model")
	}
	if got := account.AntigravityPenalty(time.Now()); got <= 0 {
		t.Fatalf("countTokens 429 penalty = %v, want a recorded event", got)
	}
}

// antigravityLongRateLimitBody 是 Google 明确告知 290s 恢复窗口的限流：低于
// antigravityQuotaExhaustedThreshold，因此仍按 rate_limit 分类。
const antigravityLongRateLimitBody = `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"RATE_LIMIT_EXCEEDED","metadata":{"model":"gemini-3.8-flash-tiered"}},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"290s"}]}}`

// newAntigravityCooldownTestAccount 构造带项目凭据的 Antigravity 账号，供冷却用例
// 复用同一组必需字段。
func newAntigravityCooldownTestAccount(id int64) *auth.Account {
	return &auth.Account{DBID: id, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
}

// TestApplyAntigravityCooldownSharedWireDeadlineNotShortenedByBrieferEvent 覆盖共享
// wire 冷却的 deadline 单调性：high 收到 290s 的 RATE_LIMIT_EXCEEDED 后，同账号 low
// 的裸 RESOURCE_EXHAUSTED（5s 回退）只能延长、不能把共享 wire 冷却改短，否则 5s 之后
// low/medium 又会选中刚被限流的凭据。public 冷却保持既有覆盖语义不变。
func TestApplyAntigravityCooldownSharedWireDeadlineNotShortenedByBrieferEvent(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	account := newAntigravityCooldownTestAccount(7461)

	long := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityLongRateLimitBody), nil, "gemini-3.8-flash-high")
	if long.Cooldown < 280*time.Second {
		t.Fatalf("long cooldown = %s, want the 290s retry hint", long.Cooldown)
	}
	if wireRemaining := account.ModelCooldownRemaining("gemini-3.8-flash-tiered"); wireRemaining < 280*time.Second {
		t.Fatalf("wire cooldown = %s after the long event, want ~290s", wireRemaining)
	}

	// 同账号兄弟档位的裸 RESOURCE_EXHAUSTED：只有 5s 的回退窗口。
	short := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody), nil, "gemini-3.8-flash-low")
	if short.Cooldown > 10*time.Second {
		t.Fatalf("short cooldown = %s, want the brief back-off", short.Cooldown)
	}
	if wireRemaining := account.ModelCooldownRemaining("gemini-3.8-flash-tiered"); wireRemaining < 60*time.Second {
		t.Fatalf("wire cooldown = %s after a briefer event, want the longer deadline kept", wireRemaining)
	}
	if !antigravityAccountModelRateLimited(account, "gemini-3.8-flash-medium", "gemini-3.8-flash-tiered") {
		t.Fatal("a sibling tier on the same backing must stay blocked")
	}
	// public 冷却保持既有语义：短事件照常覆盖同名 public 冷却，本次改动不触碰。
	if publicRemaining := account.ModelCooldownRemaining("gemini-3.8-flash-low"); publicRemaining <= 0 || publicRemaining > 10*time.Second {
		t.Fatalf("public cooldown = %s, want the brief back-off to win on the public key", publicRemaining)
	}
}

// TestApplyAntigravityCooldownSharedWireDeadlineExtendsWithLongerEvent 是反向用例：
// 先短后长必须延长共享 wire 冷却，AtLeast 变体不是「先到先得」。
func TestApplyAntigravityCooldownSharedWireDeadlineExtendsWithLongerEvent(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	account := newAntigravityCooldownTestAccount(7462)

	first := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody), nil, "gemini-3.8-flash-low")
	if first.Cooldown > 10*time.Second {
		t.Fatalf("first cooldown = %s, want the brief back-off", first.Cooldown)
	}
	if wireRemaining := account.ModelCooldownRemaining("gemini-3.8-flash-tiered"); wireRemaining > 10*time.Second {
		t.Fatalf("wire cooldown = %s after the brief event, want ~5s", wireRemaining)
	}

	applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityLongRateLimitBody), nil, "gemini-3.8-flash-medium")
	if wireRemaining := account.ModelCooldownRemaining("gemini-3.8-flash-tiered"); wireRemaining < 280*time.Second {
		t.Fatalf("wire cooldown = %s, want the longer deadline to extend the shared cooldown", wireRemaining)
	}
}

// TestApplyAntigravityCooldownGeminiFamilyDeadlineNotShortened 覆盖 Gemini 家族冷却
// 键：短窗口的 QUOTA_EXHAUSTED 不能把家族冷却改短，家族键仍拦住其它 Gemini 档位。
func TestApplyAntigravityCooldownGeminiFamilyDeadlineNotShortened(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	account := newAntigravityCooldownTestAccount(7463)

	longBody := strings.Replace(antigravityQuotaExhaustedBody, `"4m50s"`, `"10m"`, 1)
	applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(longBody), nil, "gemini-3.7-flash-high")
	if remaining := account.ModelCooldownRemaining(antigravityGeminiQuotaCooldownKey("gemini-3.8-flash-low", "")); remaining < 9*time.Minute {
		t.Fatalf("family cooldown = %s after the long event, want ~10m", remaining)
	}

	shortBody := strings.Replace(antigravityQuotaExhaustedBody, `"4m50s"`, `"60s"`, 1)
	applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(shortBody), nil, "gemini-3.6-flash-low")
	if remaining := account.ModelCooldownRemaining(antigravityGeminiQuotaCooldownKey("gemini-3.8-flash-low", "")); remaining < 9*time.Minute {
		t.Fatalf("family cooldown = %s after a shorter quota window, want the longer deadline kept", remaining)
	}
	if !antigravityAccountModelRateLimited(account, "gemini-3.5-pro-high", "gemini-3.5-pro-high") {
		t.Fatal("the Gemini family cooldown must keep blocking other Gemini tiers")
	}
}

// TestApplyAntigravityCooldownUnstructured429KeepsLongerWireDeadline 覆盖非结构化 429
// 补写 wire 冷却的同一约束：通用 429 策略给出的较短 deadline 不得覆盖同账号上更长的
// 共享 wire 冷却。
func TestApplyAntigravityCooldownUnstructured429KeepsLongerWireDeadline(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	store.SetModelCooldownSettings(database.ModelCooldownSettings{
		RelayMode:    database.ModelCooldownModeFixed,
		RelaySeconds: 60,
		OAuthMode:    database.ModelCooldownModeFixed,
		OAuthSeconds: 60,
	})
	account := newAntigravityCooldownTestAccount(7464)

	// 先由 high 的长限流把共享 wire 冷却写到 ~290s。
	if _, ok := parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(antigravityLongRateLimitBody)); !ok {
		t.Fatal("the long rate limit body must stay parseable")
	}
	applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityLongRateLimitBody), nil, "gemini-3.8-flash-high")

	if _, ok := parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`)); ok {
		t.Fatal("the body must stay unparseable so the generic 429 path is exercised")
	}
	decision := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`), &http.Response{Header: http.Header{}}, "gemini-3.8-flash-low")
	if decision.ResetAt.IsZero() {
		t.Fatalf("decision = %+v, want the generic 429 policy to record a cooldown", decision)
	}
	if wireRemaining := account.ModelCooldownRemaining("gemini-3.8-flash-tiered"); wireRemaining < 120*time.Second {
		t.Fatalf("wire cooldown = %s after an unstructured 429, want the longer deadline kept", wireRemaining)
	}
	if !antigravityAccountModelRateLimited(account, "gemini-3.8-flash-medium", "gemini-3.8-flash-tiered") {
		t.Fatal("a sibling tier on the same backing must stay blocked")
	}
}

// antigravityArgonAccount 返回一个只发现到 gemini-4-argon-high 这块上游 backing 的账号：
// gemini-4-argon-low/medium/high 三个公开档位都骑在它上面，其中 high 的公开名与 wire 名
// 完全相同，public 冷却键本身就是共享 wire 冷却键。
func antigravityArgonAccount(id int64) *auth.Account {
	return &auth.Account{
		DBID:                 id,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-token",
		AntigravityProjectID: "google-project",
		Models:               []string{"gemini-4-argon-high"},
	}
}

// TestAntigravitySharedWireCooldownKeyClassification 覆盖共享冷却键判定本身：公开名与
// 解析出的 wire 同名、或目录里另有档位将 model 当作 backing 时成立；公开档位解析到自己
// 独占且与公开名不同的 backing（或干脆解析不出 wire）时不成立——否则会把普通覆盖语义
// 无差别改成单调写，短事件再也无法缩短任何模型冷却。
func TestAntigravitySharedWireCooldownKeyClassification(t *testing.T) {
	cases := []struct {
		model     string
		wireModel string
		want      bool
	}{
		// Argon：公开名即共享 backing 名。
		{"gemini-4-argon-high", "gemini-4-argon-high", true},
		{"gemini-4-argon-high", "", true},
		// 兄弟档位的公开名不是任何档位的 backing。
		{"gemini-4-argon-low", "gemini-4-argon-high", false},
		// 原始 backing id 被三个公开档位共享。
		{"gemini-3.8-flash-tiered", "", true},
		{"gemini-3.7-flash-tiered", "", true},
		// 逻辑档位的 backing 同时是固定档位的公开名。
		{"gemini-3.6-flash-low", "", true},
		// 解析出的 wire 与公开名同名：按规则一律按共享键写入，保证单调性。
		{"claude-sonnet-4-6", "claude-sonnet-4-6", true},
		// 解析不到 wire、目录里也没有别的档位以它为 backing：保持覆盖语义。
		{"claude-sonnet-4-6", "", false},
		{"some-discovered-raw-model", "", false},
		// 公开档位解析到自己独占且与公开名不同的 backing：保持覆盖语义。
		{"gemini-3.8-flash-low", "gemini-3.8-flash-tiered", false},
		{"gemini-3.1-pro-high", "gemini-pro-agent", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := antigravitySharedWireCooldownKey(tc.model, tc.wireModel); got != tc.want {
			t.Fatalf("antigravitySharedWireCooldownKey(%q, %q) = %v, want %v", tc.model, tc.wireModel, got, tc.want)
		}
	}
}

// TestApplyAntigravityCooldownSharedNamePublicModelNotShortened 覆盖 P1-1：
// gemini-4-argon-high 的公开名与共享 wire 键同名，先由 low 的 290s 限流把共享冷却写到
// ~290s，随后 high 自己的 5s 裸 RESOURCE_EXHAUSTED 走的是无条件覆盖的 public 写入路径，
// 必须同样受 AtLeast 单调性保护，不能把兄弟档位的长冷却改短。
func TestApplyAntigravityCooldownSharedNamePublicModelNotShortened(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	account := antigravityArgonAccount(7471)

	if wire, ok := antigravityResolvePublicModelForAccount(account, "gemini-4-argon-high"); !ok || wire != "gemini-4-argon-high" {
		t.Fatalf("gemini-4-argon-high resolved to %q, ok=%v, want the shared backing itself", wire, ok)
	}

	first := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityLongRateLimitBody), nil, "gemini-4-argon-low")
	if first.Cooldown < 280*time.Second {
		t.Fatalf("sibling cooldown = %s, want the 290s retry hint", first.Cooldown)
	}
	if remaining := account.ModelCooldownRemaining("gemini-4-argon-high"); remaining < 280*time.Second {
		t.Fatalf("shared key cooldown = %s after the sibling event, want ~290s", remaining)
	}

	short := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody), nil, "gemini-4-argon-high")
	if remaining := account.ModelCooldownRemaining("gemini-4-argon-high"); remaining < 60*time.Second {
		t.Fatalf("shared key cooldown = %s after the same-name brief event, want the longer deadline kept", remaining)
	}
	// 决策本身也要报告生效的长 deadline，否则上层会按 5s 之后重新调度该账号。
	if short.ResetAt.IsZero() || time.Until(short.ResetAt) < 60*time.Second {
		t.Fatalf("decision = %+v, want the kept longer deadline", short)
	}
	if !antigravityAccountModelRateLimited(account, "gemini-4-argon-medium", "gemini-4-argon-high") {
		t.Fatal("a sibling tier on the same backing must stay blocked")
	}
}

// TestApplyAntigravityCooldownSharedNamePublicModelExtendsWithLongerEvent 是反向用例：
// high 先吃到 5s 短冷却，随后兄弟档位的 290s 限流必须能把同名共享键延长。
func TestApplyAntigravityCooldownSharedNamePublicModelExtendsWithLongerEvent(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	account := antigravityArgonAccount(7472)

	first := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody), nil, "gemini-4-argon-high")
	if first.Cooldown > 10*time.Second {
		t.Fatalf("first cooldown = %s, want the brief back-off", first.Cooldown)
	}
	if remaining := account.ModelCooldownRemaining("gemini-4-argon-high"); remaining > 10*time.Second {
		t.Fatalf("shared key cooldown = %s after the brief event, want ~5s", remaining)
	}

	extended := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityLongRateLimitBody), nil, "gemini-4-argon-medium")
	if extended.Cooldown < 280*time.Second {
		t.Fatalf("extended decision = %+v, want the 290s retry hint", extended)
	}
	if remaining := account.ModelCooldownRemaining("gemini-4-argon-high"); remaining < 280*time.Second {
		t.Fatalf("shared key cooldown = %s, want the longer deadline to extend it", remaining)
	}
}

// TestApplyAntigravityCooldownSharedNamePublicModelOnUnstructured429 覆盖非结构化 429
// 的同一约束：通用 429 策略按覆盖语义写 public 冷却，而 public 名就是共享 wire 键时这次
// 覆盖会把兄弟档位的长冷却改短，必须写回。
func TestApplyAntigravityCooldownSharedNamePublicModelOnUnstructured429(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	store.SetModelCooldownSettings(database.ModelCooldownSettings{
		RelayMode:    database.ModelCooldownModeFixed,
		RelaySeconds: 60,
		OAuthMode:    database.ModelCooldownModeFixed,
		OAuthSeconds: 60,
	})
	account := antigravityArgonAccount(7473)

	applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityLongRateLimitBody), nil, "gemini-4-argon-low")
	if remaining := account.ModelCooldownRemaining("gemini-4-argon-high"); remaining < 280*time.Second {
		t.Fatalf("shared key cooldown = %s after the sibling event, want ~290s", remaining)
	}

	if _, ok := parseAntigravityQuotaError(http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`)); ok {
		t.Fatal("the body must stay unparseable so the generic 429 path is exercised")
	}
	decision := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(`{"error":{"message":"slow down"}}`), &http.Response{Header: http.Header{}}, "gemini-4-argon-high")
	if decision.Scope != rateLimitScopeModel || decision.Model != "gemini-4-argon-high" {
		t.Fatalf("decision = %+v, want a model-scoped cooldown", decision)
	}
	if remaining := account.ModelCooldownRemaining("gemini-4-argon-high"); remaining < 120*time.Second {
		t.Fatalf("shared key cooldown = %s after an unstructured 429, want the longer deadline kept", remaining)
	}
	if !antigravityAccountModelRateLimited(account, "gemini-4-argon-medium", "gemini-4-argon-high") {
		t.Fatal("a sibling tier on the same backing must stay blocked")
	}
}

// TestApplyAntigravityCooldownUnsharedPublicModelKeepsOverwriteSemantics 是回归护栏：
// 公开档位解析到自己独占、且与公开名不同的 backing 时，短事件仍按既有语义覆盖同名
// public 冷却，单调性保护只作用于共享键（wire / family / 与 wire 同名的公开名）。
func TestApplyAntigravityCooldownUnsharedPublicModelKeepsOverwriteSemantics(t *testing.T) {
	store := newProxyPremiumTestStore()
	defer store.Stop()
	account := newAntigravityCooldownTestAccount(7474)

	if wire, ok := antigravityResolvePublicModelForAccount(account, "gemini-3.8-flash-low"); !ok || wire != "gemini-3.8-flash-tiered" {
		t.Fatalf("gemini-3.8-flash-low resolved to %q, ok=%v, want the shared backing", wire, ok)
	}

	applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityLongRateLimitBody), nil, "gemini-3.8-flash-low")
	if remaining := account.ModelCooldownRemaining("gemini-3.8-flash-low"); remaining < 280*time.Second {
		t.Fatalf("public cooldown = %s, want the 290s retry hint", remaining)
	}
	short := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(antigravityUnclassifiedRateLimitBody), nil, "gemini-3.8-flash-low")
	if short.Cooldown > 10*time.Second {
		t.Fatalf("decision = %+v, want the brief back-off", short)
	}
	if remaining := account.ModelCooldownRemaining("gemini-3.8-flash-low"); remaining <= 0 || remaining > 10*time.Second {
		t.Fatalf("public cooldown = %s, want the brief event to keep overwriting a non-shared public key", remaining)
	}
	// 同一次短事件仍不得改短共享 wire 键上的长冷却。
	if remaining := account.ModelCooldownRemaining("gemini-3.8-flash-tiered"); remaining < 280*time.Second {
		t.Fatalf("wire cooldown = %s, want the longer deadline kept on the shared key", remaining)
	}
}
