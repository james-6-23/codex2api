package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestParseGrokFreeQuotaUsage(t *testing.T) {
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1003617/1000000. Upgrade to a Grok subscription for higher limits: https://grok.com/supergrok"}`)

	if !IsGrokFreeQuotaExhaustedError(body) {
		t.Fatal("expected free quota exhausted error to be detected")
	}
	used, limit, ok := parseGrokFreeQuotaUsage(body)
	if !ok {
		t.Fatal("expected usage numbers to parse")
	}
	if used != 1003617 || limit != 1000000 {
		t.Fatalf("unexpected usage: used=%d limit=%d", used, limit)
	}
	if model := parseGrokFreeQuotaModel(body); model != "grok-4.5-build-free" {
		t.Fatalf("unexpected model: %q", model)
	}
}

func TestParseGrokFreeQuotaUsageVariants(t *testing.T) {
	// 带空格的变体与缺失数字的降级
	spaced := []byte(`tokens (actual/limit) : 42 / 100`)
	used, limit, ok := parseGrokFreeQuotaUsage(spaced)
	if !ok || used != 42 || limit != 100 {
		t.Fatalf("spaced variant: used=%d limit=%d ok=%v", used, limit, ok)
	}

	noNumbers := []byte(`{"code":"subscription:free-usage-exhausted","error":"no numbers here"}`)
	if !IsGrokFreeQuotaExhaustedError(noNumbers) {
		t.Fatal("code-only body should still be detected as exhausted")
	}
	if _, _, ok := parseGrokFreeQuotaUsage(noNumbers); ok {
		t.Fatal("expected parse failure without numbers")
	}

	if _, _, ok := parseGrokFreeQuotaUsage([]byte(`tokens (actual/limit): 5/0`)); ok {
		t.Fatal("zero limit must not parse")
	}

	// model 字段优先于错误文案
	withField := []byte(`{"model":"grok-4-fast","error":"used all the included free usage for model grok-4.5-build-free"}`)
	if model := parseGrokFreeQuotaModel(withField); model != "grok-4-fast" {
		t.Fatalf("model field should win, got %q", model)
	}
}

func newGrokTestAccount(plan string) (*auth.Store, *auth.Account) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	acc := &auth.Account{DBID: 1, APIKey: "xai-test", UpstreamType: auth.UpstreamGrok, PlanType: plan}
	store.AddAccount(acc)
	return store, acc
}

// free 账号免费额度耗尽 → 整号冷却 24h(usage_limited),权威用量快照落运行时。
func TestApplyGrokCooldownFreeQuotaExhausted_FreePlan(t *testing.T) {
	store, acc := newGrokTestAccount("free")
	h := NewHandler(store, nil, nil, nil)
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1003617/1000000."}`)

	decision := h.applyGrokCooldownForModel(acc, 429, body, nil, "grok-4.5-build-free")
	if decision.Reason != "usage_limited" {
		t.Fatalf("reason = %q, want usage_limited", decision.Reason)
	}
	if decision.Scope == rateLimitScopeModel {
		t.Fatal("free plan should get account-level cooldown, not model scope")
	}
	if got := acc.RuntimeStatus(); got != "usage_limited" {
		t.Fatalf("runtime status = %q, want usage_limited", got)
	}
	snap, ok := acc.GetGrokFreeQuotaSnapshot()
	if !ok {
		t.Fatal("expected free quota snapshot recorded")
	}
	if snap.UsedTokens != 1003617 || snap.LimitTokens != 1000000 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Model != "grok-4.5-build-free" {
		t.Fatalf("snapshot model = %q", snap.Model)
	}
}

// 批量测试/连通性测试经 Apply429Cooldown 也必须走 Grok 专用映射:免费额度耗尽须落
// 权威用量快照并标 usage_limited,而非误标 rate_limited、丢失快照(条会退化成 "usage —")。
func TestApply429CooldownRoutesGrokFreeQuota(t *testing.T) {
	store, acc := newGrokTestAccount("free")
	body := []byte(`{"code":"subscription:free-usage-exhausted","error":"You've used all the included free usage for model grok-4.5-build-free for now. Usage resets over a rolling 24-hour window — tokens (actual/limit): 1003617/1000000."}`)

	decision := Apply429Cooldown(store, acc, body, nil, "grok-4.5-build-free")
	if decision.Reason != "usage_limited" {
		t.Fatalf("reason = %q, want usage_limited", decision.Reason)
	}
	if got := acc.RuntimeStatus(); got != "usage_limited" {
		t.Fatalf("runtime status = %q, want usage_limited", got)
	}
	snap, ok := acc.GetGrokFreeQuotaSnapshot()
	if !ok {
		t.Fatal("expected free quota snapshot recorded via batch-test path")
	}
	if snap.UsedTokens != 1003617 || snap.LimitTokens != 1000000 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// 付费账号免费模型耗尽 → 模型级冷却接近 24h(不被 30min 钳制),账号整体仍可用。
func TestApplyGrokCooldownFreeQuotaExhausted_PaidPlan(t *testing.T) {
	store, acc := newGrokTestAccount("SuperGrok")
	h := NewHandler(store, nil, nil, nil)
	body := []byte(`{"error":"You've used all the included free usage for model grok-4.5-build-free for now. tokens (actual/limit): 55/50."}`)

	decision := h.applyGrokCooldownForModel(acc, 429, body, nil, "grok-4.5-build-free")
	if decision.Scope != rateLimitScopeModel {
		t.Fatalf("scope = %v, want model scope", decision.Scope)
	}
	remaining := acc.ModelCooldownRemaining("grok-4.5-build-free")
	if remaining < 23*time.Hour {
		t.Fatalf("model cooldown remaining = %s, want ~24h (30min clamp bug)", remaining)
	}
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("runtime status = %q, want active (only the model is cooled)", got)
	}
}
