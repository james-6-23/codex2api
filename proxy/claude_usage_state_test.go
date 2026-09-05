package proxy

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// respWith 构造一个带指定状态码与限流头的假响应,用于离线验证 SyncClaudeUsageState 的归因。
func respWith(status int, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h}
}

func newSyncTestStore() *auth.Store {
	return auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
}

// 通用/边缘限流(rate_limit_error,无 unified 配额头)→ 只做短退避,绝不标 5h=100%。
func TestSyncClaudeUsageState_GenericRateLimit_ShortBackoff_No5h(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}

	// 真实的 Cloudflare/Anthropic 通用 rate_limit_error 429:带边缘头但无 unified 配额头。
	SyncClaudeUsageState(store, acc, respWith(http.StatusTooManyRequests, map[string]string{
		"content-type": "application/json",
		"cf-ray":       "a32a38ebdcecf343-BOS",
	}))

	if pct, ok := acc.GetUsagePercent5h(); ok && pct >= 100 {
		t.Fatalf("通用 429 不应把 5h 置 100,实际 pct=%v ok=%v", pct, ok)
	}
	if acc.Status != auth.StatusCooldown {
		t.Fatalf("通用 429 应进入短冷却,status=%v", acc.Status)
	}
	// 短退避:冷却应在 ~1 分钟量级,远小于 5h。
	if until := time.Until(acc.CooldownUtil); until <= 0 || until > 20*time.Minute {
		t.Fatalf("通用 429 冷却应为短退避(<=20m),实际 until=%v", until)
	}
	t.Logf("通用限流: status=cooldown, 冷却剩余=%v, 5h 未被误置", time.Until(acc.CooldownUtil).Round(time.Second))
}

func TestSyncClaudeUsageState_RateLimitWithoutResponseHeadersStillBacksOff(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}
	SyncClaudeUsageState(store, acc, &http.Response{StatusCode: http.StatusTooManyRequests})
	if acc.Status != auth.StatusCooldown {
		t.Fatalf("headerless Claude 429 status = %v, want cooldown", acc.Status)
	}
}

func TestSyncClaudeUsageState_HeaderlessSuccessUpdatesProbeFreshness(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	SyncClaudeUsageState(store, acc, respWith(http.StatusOK, nil))
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("a successful native Claude response without quota headers should count as a fresh observation")
	}
}

// 5h 窗口真实耗尽(utilization=100 + representative-claim=five_hour)→ 标 5h=100,冷却到 5h 重置。
func TestSyncClaudeUsageState_FiveHourExhausted_Marks5h(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}
	reset5h := time.Now().Add(3 * time.Hour).Unix()

	SyncClaudeUsageState(store, acc, respWith(http.StatusTooManyRequests, map[string]string{
		"anthropic-ratelimit-unified-status":               "rejected",
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
		"anthropic-ratelimit-unified-5h-utilization":       "1.0",
		"anthropic-ratelimit-unified-5h-reset":             itoa(reset5h),
	}))

	if pct, ok := acc.GetUsagePercent5h(); !ok || pct < 100 {
		t.Fatalf("5h 耗尽应标 5h=100,实际 pct=%v ok=%v", pct, ok)
	}
	if acc.Status != auth.StatusCooldown {
		t.Fatalf("5h 耗尽应进入冷却,status=%v", acc.Status)
	}
	t.Logf("5h 耗尽: 5h=100, 冷却剩余≈%v", time.Until(acc.CooldownUtil).Round(time.Minute))
}

// 周窗口真实耗尽(7d-utilization=100 + representative-claim=seven_day)→ 记 7d,不砸 5h。
func TestSyncClaudeUsageState_SevenDayExhausted_Marks7dNot5h(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}
	reset7d := time.Now().Add(3 * 24 * time.Hour).Unix()

	SyncClaudeUsageState(store, acc, respWith(http.StatusTooManyRequests, map[string]string{
		"anthropic-ratelimit-unified-status":               "rejected",
		"anthropic-ratelimit-unified-representative-claim": "seven_day",
		"anthropic-ratelimit-unified-7d-utilization":       "1.0",
		"anthropic-ratelimit-unified-7d-reset":             itoa(reset7d),
	}))

	if pct, ok := acc.GetUsagePercent5h(); ok && pct >= 100 {
		t.Fatalf("周窗口耗尽不应把 5h 置 100,实际 pct=%v ok=%v", pct, ok)
	}
	if pct, ok := acc.GetUsagePercent7d(); !ok || pct < 100 {
		t.Fatalf("周窗口耗尽应标 7d=100,实际 pct=%v ok=%v", pct, ok)
	}
	if acc.Status != auth.StatusCooldown {
		t.Fatalf("周窗口耗尽应进入冷却,status=%v", acc.Status)
	}
	t.Logf("周窗口耗尽: 7d=100, 5h 未被误置, 冷却剩余≈%v", time.Until(acc.CooldownUtil).Round(time.Hour))
}

// 200 正常响应携带利用率头 → 只更新快照,不进入任何冷却。
func TestSyncClaudeUsageState_OK200_UpdatesSnapshotNoCooldown(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}

	SyncClaudeUsageState(store, acc, respWith(http.StatusOK, map[string]string{
		"anthropic-ratelimit-unified-status":         "allowed",
		"anthropic-ratelimit-unified-5h-utilization": "0.01",
		"anthropic-ratelimit-unified-7d-utilization": "0.0",
	}))

	if pct, ok := acc.GetUsagePercent5h(); !ok || pct != 1 {
		t.Fatalf("200 响应应写入 5h=1(0.01→1%%),实际 pct=%v ok=%v", pct, ok)
	}
	if acc.Status == auth.StatusCooldown {
		t.Fatalf("200 响应不应进入冷却")
	}
}

func TestSyncClaudeUsageState_SevenDayOnlyClearsStaleFiveHourSnapshot(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}
	acc.SetUsageSnapshot5hAt(100, time.Now().Add(2*time.Hour), time.Now().Add(-time.Minute))

	SyncClaudeUsageState(store, acc, respWith(http.StatusOK, map[string]string{
		"anthropic-ratelimit-unified-status":         "allowed",
		"anthropic-ratelimit-unified-7d-utilization": "0.2",
	}))

	if _, ok := acc.GetUsagePercent5h(); ok {
		t.Fatal("authoritative 7d-only response must clear a stale 5h snapshot")
	}
	if pct, ok := acc.GetUsagePercent7d(); !ok || pct != 20 {
		t.Fatalf("7d snapshot = (%v, %v), want 20%% valid", pct, ok)
	}
}

func TestSyncClaudeUsageState_BothWindowsExhaustedPrefersSevenDayReset(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}
	reset5h := time.Now().Add(2 * time.Hour).Unix()
	reset7d := time.Now().Add(48 * time.Hour).Unix()

	SyncClaudeUsageState(store, acc, respWith(http.StatusTooManyRequests, map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "1.0",
		"anthropic-ratelimit-unified-5h-reset":       itoa(reset5h),
		"anthropic-ratelimit-unified-7d-utilization": "1.0",
		"anthropic-ratelimit-unified-7d-reset":       itoa(reset7d),
	}))

	if remaining := time.Until(acc.CooldownUtil); remaining < 47*time.Hour {
		t.Fatalf("both exhausted cooldown = %v, want seven-day reset", remaining)
	}
}

func TestClaudeRatelimitHeaderTimeAcceptsRFC3339(t *testing.T) {
	want := time.Date(2026, 8, 29, 12, 34, 56, 0, time.UTC)
	if got := claudeRatelimitHeaderTime(want.Format(time.RFC3339)); !got.Equal(want) {
		t.Fatalf("RFC3339 reset = %v, want %v", got, want)
	}
}

func TestClaudeRatelimitHeaderTimeNormalizesMillisecondsAndRejectsOutliers(t *testing.T) {
	want := time.Date(2026, 8, 29, 12, 34, 56, 0, time.UTC)
	if got := claudeRatelimitHeaderTime(strconv.FormatInt(want.Unix()*1000, 10)); !got.Equal(want) {
		t.Fatalf("epoch-millisecond reset = %v, want %v", got, want)
	}
	if got := claudeRatelimitHeaderTime("999999999999999999"); !got.IsZero() {
		t.Fatalf("outlier reset = %v, want zero", got)
	}
}

func TestClaudeRatelimitHeaderPctRejectsNonFiniteValues(t *testing.T) {
	for _, raw := range []string{"NaN", "+Inf", "-Inf"} {
		if value, ok := claudeRatelimitHeaderPct(raw); ok || value != 0 {
			t.Fatalf("utilization %q parsed as (%v, %v), want invalid", raw, value, ok)
		}
	}
}

func TestClaudeAccountSupportsOnlyNativeModelIDs(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, Models: []string{"gpt-5.4", "claude-sonnet-4-5"}}
	if claudeAccountSupportsModel(account, "gpt-5.4") {
		t.Fatal("Claude account must not claim an OpenAI model")
	}
	if !claudeAccountSupportsModel(account, "claude-sonnet-4-5") {
		t.Fatal("Claude account should support its native model")
	}
}

func TestClaudeNativeBodyOnlyAuthFailureCoolsAccount(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "token", Status: auth.StatusReady}
	h := &Handler{store: store}
	outcome := streamOutcome{
		logStatusCode:  http.StatusUnauthorized,
		failurePayload: []byte(`{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`),
	}
	_ = h.applyClaudeNativeFailureCooldown(account, outcome, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, "claude-sonnet-4-5")
	reason, _ := account.GetCooldownSnapshot()
	if reason != "unauthorized" {
		t.Fatalf("body-only Claude auth failure reason = %q, want unauthorized", reason)
	}
}

func TestClaudeNativeBodyOnlyRateLimitDoesNotOverwriteAuthoritativeWindowCooldown(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "token", Status: auth.StatusReady}
	reset := time.Now().Add(4 * time.Hour)
	SyncClaudeUsageState(store, account, respWith(http.StatusOK, map[string]string{
		"anthropic-ratelimit-unified-status":               "rejected",
		"anthropic-ratelimit-unified-representative-claim": "five_hour",
		"anthropic-ratelimit-unified-5h-utilization":       "1",
		"anthropic-ratelimit-unified-5h-reset":             strconv.FormatInt(reset.Unix(), 10),
	}))
	_, before := account.GetCooldownSnapshot()
	outcome := streamOutcome{logStatusCode: http.StatusTooManyRequests, failurePayload: []byte(`{"type":"error","error":{"type":"rate_limit_error"}}`)}
	_ = (&Handler{store: store}).applyClaudeNativeFailureCooldown(account, outcome, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, "claude-sonnet-4-5")
	reason, after := account.GetCooldownSnapshot()
	if reason != auth.ResponsesRateLimitedCooldownReason || after.Before(before.Add(-time.Second)) || after.After(before.Add(time.Second)) {
		t.Fatalf("body-only fallback overwrote authoritative cooldown: reason=%q before=%v after=%v", reason, before, after)
	}
}

func TestDefaultClaudeModelIDsFiltersInvalidAndDuplicateEntries(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, Models: []string{
		"gpt-5.4", "Claude-Sonnet-4-5", "claude-sonnet-4-5", "gemini-2.5-pro",
	}}
	got := DefaultClaudeModelIDsForAccount(account)
	if len(got) != 1 || got[0] != "Claude-Sonnet-4-5" {
		t.Fatalf("filtered Claude model catalog = %v, want one native deduplicated ID", got)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// credits_required(模型级计费门槛)→ 只冷却该模型,不冷却账号。
func TestHandleClaudeModelBillingRejection_CreditsRequired_ModelLevel(t *testing.T) {
	store := newSyncTestStore()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","model":"claude-fable-5","can_user_purchase_credits":true}}}`)

	handled := HandleClaudeModelBillingRejection(store, acc, "claude-fable-5", http.StatusTooManyRequests, body)
	if !handled {
		t.Fatal("credits_required 应被识别并处理为模型级冷却")
	}
	// 账号本身不应进入冷却(其它模型仍可用)。
	if acc.Status == auth.StatusCooldown {
		t.Fatalf("credits_required 不应冷却整个账号,status=%v", acc.Status)
	}
	if pct, ok := acc.GetUsagePercent5h(); ok && pct >= 100 {
		t.Fatalf("credits_required 不应把 5h 置 100,pct=%v", pct)
	}
	// 被拒模型应处于冷却。
	if !acc.IsModelRateLimited("claude-fable-5") {
		t.Fatal("claude-fable-5 应被标记为模型级冷却")
	}
	// 其它模型不受影响。
	if acc.IsModelRateLimited("claude-haiku-4-5-20251001") {
		t.Fatal("其它模型不应受 credits_required 影响")
	}
	t.Log("credits_required: 仅 fable-5 冷却, 账号与其它模型不受影响")
}

// 非 credits_required 的普通 429 → HandleClaudeModelBillingRejection 不处理(交给账号级同步)。
func TestHandleClaudeModelBillingRejection_OtherError_NotHandled(t *testing.T) {
	store := newSyncTestStore()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Rate limited. Please try again later."}}`)
	if HandleClaudeModelBillingRejection(store, acc, "claude-haiku-4-5-20251001", http.StatusTooManyRequests, body) {
		t.Fatal("普通限流不应被当作 credits_required 处理")
	}
}

func TestHandleClaudeModelBillingRejection_MessageOnly(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{UpstreamType: auth.UpstreamClaude}
	body := []byte(`{"error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","model":"claude-fable-5"}}`)
	if !HandleClaudeModelBillingRejection(store, acc, "claude-fable-5", http.StatusTooManyRequests, body) {
		t.Fatal("message-only usage credits response should be handled as model-level rejection")
	}
	if !acc.IsModelRateLimited("claude-fable-5") || acc.HasActiveCooldown() {
		t.Fatal("message-only usage credits response must only cool down the model")
	}
}

func TestClaudeNativeCreditsRequiredIsModelScoped(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{DBID: 91, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	outcome := streamOutcome{
		logStatusCode:  http.StatusTooManyRequests,
		failurePayload: []byte(`{"type":"response.failed","response":{"status_code":429,"error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","model":"claude-fable-5"}}}}`),
	}
	got := (&Handler{store: store}).applyClaudeNativeFailureCooldown(acc, outcome, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, "claude-fable-5")
	if acc.HasActiveCooldown() || acc.RuntimeStatus() == "rate_limited" {
		t.Fatalf("native credits_required must not cool down account: status=%q", acc.RuntimeStatus())
	}
	if !acc.IsModelRateLimited("claude-fable-5") {
		t.Fatal("native credits_required should cool down only Fable 5")
	}
	if got.failureKind != "rate_limited_model" {
		t.Fatalf("native credits_required failure kind = %q, want rate_limited_model", got.failureKind)
	}
}

func TestClaudeNativeCreditsRequiredWithoutStatusEvidenceIsModelScoped(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{DBID: 92, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	outcome := streamOutcome{
		logStatusCode:  http.StatusInternalServerError,
		failurePayload: []byte(`{"type":"response.failed","response":{"error":{"message":"Usage credits are required for this model.","model":"claude-fable-5"}}}`),
	}
	got := (&Handler{store: store}).applyClaudeNativeFailureCooldown(acc, outcome, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, "claude-fable-5")
	if acc.HasActiveCooldown() || !acc.IsModelRateLimited("claude-fable-5") {
		t.Fatalf("message-only native billing failure must be model scoped: status=%q", acc.RuntimeStatus())
	}
	if got.failureKind != "rate_limited_model" {
		t.Fatalf("message-only native billing failure kind = %q, want rate_limited_model", got.failureKind)
	}
}

func TestClaudeNativeClientCompatibilityDoesNotCoolAccount(t *testing.T) {
	store := newSyncTestStore()
	defer store.Stop()
	acc := &auth.Account{DBID: 93, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	outcome := streamOutcome{
		logStatusCode:  http.StatusBadRequest,
		failurePayload: []byte(`{"type":"response.failed","response":{"status_code":400,"error":{"type":"invalid_request_error","message":"Claude Code 2.1.205 does not support this model; version 2.1.251 or newer is required."}}}`),
	}
	got := (&Handler{store: store}).applyClaudeNativeFailureCooldown(acc, outcome, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header)}, "claude-fable-5-1")
	if acc.HasActiveCooldown() || got.failureKind != "client_compatibility" {
		t.Fatalf("compatibility failure changed account state: cooldown=%v kind=%q", acc.HasActiveCooldown(), got.failureKind)
	}
}

// credits_required 说明该账号套餐没有这个模型：除冷却外还要把模型从账号白名单里移除，
// 否则调度器会在冷却过期后继续把请求派给它。
func TestHandleClaudeModelBillingRejection_DropsModelFromWhitelist(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &auth.Account{DBID: 251, UpstreamType: auth.UpstreamClaude, Models: []string{"claude-fable-5-1", "claude-opus-5"}}
	store.SetAccountsForTest([]*auth.Account{acc})
	body := []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","model":"claude-fable-5-1","disabled_reason":"org_level_disabled"}}}`)
	if !HandleClaudeModelBillingRejection(store, acc, "claude-fable-5-1", http.StatusTooManyRequests, body) {
		t.Fatal("credits_required must be handled")
	}
	acc.Mu().RLock()
	models := append([]string(nil), acc.Models...)
	acc.Mu().RUnlock()
	if len(models) != 1 || models[0] != "claude-opus-5" {
		t.Fatalf("whitelist after rejection = %v, want only claude-opus-5", models)
	}
	if !claudeAccountSupportsModel(acc, "claude-opus-5") || claudeAccountSupportsModel(acc, "claude-fable-5-1") {
		t.Fatal("scheduler eligibility must reflect the pruned whitelist")
	}
}
