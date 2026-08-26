package database

import "testing"

func TestNormalizeAPIKeyScopeLimitsDropsInvalidEntries(t *testing.T) {
	out := NormalizeAPIKeyScopeLimits([]APIKeyScopeLimit{
		{ScopeType: "GROUP", ScopeID: 3, Cost1d: 5},          // 大写类型应被归一
		{ScopeType: "team", ScopeID: 4, Cost1d: 5},           // 未知类型 → 丢弃
		{ScopeType: "group", ScopeID: 0, Cost1d: 5},          // 非法 ID → 丢弃
		{ScopeType: "group", ScopeID: 7},                     // 全 0 限额 → 丢弃
		{ScopeType: "account", ScopeID: 9, Cost5h: -3},       // 负值置 0 后无限额 → 丢弃
		{ScopeType: "account", ScopeID: 11, Requests1d: 100}, // 保留
	})
	if len(out) != 2 {
		t.Fatalf("normalized %d entries, want 2: %+v", len(out), out)
	}
	if out[0].ScopeType != APIKeyScopeTypeGroup || out[0].ScopeID != 3 {
		t.Fatalf("first entry = %+v, want group 3", out[0])
	}
	if out[0].OnExhausted != APIKeyScopeOnExhaustedSkip {
		t.Fatalf("default on_exhausted = %q, want skip", out[0].OnExhausted)
	}
	if out[1].ScopeType != APIKeyScopeTypeAccount || out[1].Requests1d != 100 {
		t.Fatalf("second entry = %+v, want account 11 with 100 requests", out[1])
	}
}

func TestNormalizeAPIKeyScopeLimitsDedupesSameScope(t *testing.T) {
	out := NormalizeAPIKeyScopeLimits([]APIKeyScopeLimit{
		{ScopeType: "group", ScopeID: 3, Cost1d: 5},
		{ScopeType: "group", ScopeID: 3, Cost1d: 9, OnExhausted: "reject"},
	})
	if len(out) != 1 {
		t.Fatalf("normalized %d entries, want 1", len(out))
	}
	if out[0].Cost1d != 9 || out[0].OnExhausted != APIKeyScopeOnExhaustedReject {
		t.Fatalf("deduped entry = %+v, want the later config to win", out[0])
	}
}

func TestAPIKeyScopeLimitCheckWindow(t *testing.T) {
	scope := APIKeyScopeLimit{ScopeType: "group", ScopeID: 1, Cost1d: 2, Token5h: 1000, Requests1d: 10}

	if _, exhausted := scope.CheckWindow("1d", APIKeyWindowUsage{UserBilled: 1.99, Requests: 9}); exhausted {
		t.Fatal("under limit should not be exhausted")
	}
	// 达到上限即视为用尽（>=），与 Key 级限额一致。
	hit, exhausted := scope.CheckWindow("1d", APIKeyWindowUsage{UserBilled: 2})
	if !exhausted || hit.Metric != "cost" {
		t.Fatalf("cost limit hit = %+v exhausted=%v, want cost", hit, exhausted)
	}
	hit, exhausted = scope.CheckWindow("1d", APIKeyWindowUsage{Requests: 10})
	if !exhausted || hit.Metric != "requests" {
		t.Fatalf("request limit hit = %+v exhausted=%v, want requests", hit, exhausted)
	}
	hit, exhausted = scope.CheckWindow("5h", APIKeyWindowUsage{Tokens: 1000})
	if !exhausted || hit.Metric != "tokens" {
		t.Fatalf("token limit hit = %+v exhausted=%v, want tokens", hit, exhausted)
	}
	// 未配限额的窗口永不触发。
	if _, exhausted := scope.CheckWindow("30d", APIKeyWindowUsage{UserBilled: 9999}); exhausted {
		t.Fatal("unconfigured window must never be exhausted")
	}
}

func TestAPIKeyScopeLimitNeedsWindow(t *testing.T) {
	scope := APIKeyScopeLimit{Cost7d: 10}
	if scope.NeedsWindow("5h") || scope.NeedsWindow("1d") || scope.NeedsWindow("30d") {
		t.Fatal("only the 7d window should be needed")
	}
	if !scope.NeedsWindow("7d") {
		t.Fatal("7d window should be needed")
	}
}

func TestPruneAPIKeyScopeLimitsForScope(t *testing.T) {
	in := []APIKeyScopeLimit{
		{ScopeType: "group", ScopeID: 3, Cost1d: 5},
		{ScopeType: "group", ScopeID: 4, Cost1d: 5},
		{ScopeType: "account", ScopeID: 3, Cost1d: 5},
	}
	out, changed := PruneAPIKeyScopeLimitsForScope(in, APIKeyScopeTypeGroup, 3)
	if !changed || len(out) != 2 {
		t.Fatalf("prune returned %+v changed=%v, want 2 entries", out, changed)
	}
	for _, item := range out {
		if item.ResolveScopeType() == APIKeyScopeTypeGroup && item.ScopeID == 3 {
			t.Fatal("deleted group scope survived pruning")
		}
	}
	if _, changed := PruneAPIKeyScopeLimitsForScope(out, APIKeyScopeTypeGroup, 99); changed {
		t.Fatal("pruning a scope that is not referenced must not report a change")
	}
	if out, changed := PruneAPIKeyScopeLimitsForScope([]APIKeyScopeLimit{{ScopeType: "group", ScopeID: 3, Cost1d: 5}}, APIKeyScopeTypeGroup, 3); !changed || out != nil {
		t.Fatalf("pruning the only entry = %+v changed=%v, want nil", out, changed)
	}
}

func TestAPIKeyLimitsIsZeroCoversScopeLimits(t *testing.T) {
	limits := APIKeyLimits{ScopeLimits: []APIKeyScopeLimit{{ScopeType: "group", ScopeID: 1, Cost1d: 5}}}
	if limits.IsZero() {
		t.Fatal("limits with scope entries must not be treated as empty, otherwise enforcement short-circuits")
	}
	if encoded := encodeAPIKeyLimits(limits); encoded == "{}" {
		t.Fatalf("encoded limits = %q, want scope limits persisted", encoded)
	}
}

func TestAPIKeyLimitsIsZeroCoversAllowLive(t *testing.T) {
	limits := APIKeyLimits{AllowLive: true}
	if limits.IsZero() {
		t.Fatal("allow_live must keep API key limits out of the empty-limits short circuit")
	}
	if encoded := encodeAPIKeyLimits(limits); encoded == "{}" {
		t.Fatalf("encoded limits = %q, want allow_live persisted", encoded)
	}
}

func TestAPIKeyLimitsIsZeroCoversNoAffinityGroups(t *testing.T) {
	limits := APIKeyLimits{NoAffinityGroupIDs: []int64{20}}
	if limits.IsZero() {
		t.Fatal("no-affinity routing must keep the full API key metadata out of the slim auth cache")
	}
	if encoded := encodeAPIKeyLimits(limits); encoded == "{}" {
		t.Fatalf("encoded limits = %q, want no-affinity groups persisted", encoded)
	}
}

func TestAPIKeyScopeExhaustionDescribeKeepsTinyBudgetsReadable(t *testing.T) {
	hit := APIKeyScopeExhaustion{Window: "1d", Metric: "cost", Used: 0.0003875, Limit: 0.00005}
	msg := hit.Describe(`group "111"`)
	if msg == `API key scope budget exhausted: group "111" used $0.00 / $0.00 in last 1d` {
		t.Fatalf("tiny budgets collapsed to $0.00: %s", msg)
	}
	if got := formatScopeCost(12.5); got != "12.50" {
		t.Fatalf("formatScopeCost(12.5) = %q, want 12.50", got)
	}
	if got := formatScopeCost(0); got != "0.00" {
		t.Fatalf("formatScopeCost(0) = %q, want 0.00", got)
	}
}
