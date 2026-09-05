package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func newGrokSchedulingTestAccount(dbID int64) *Account {
	return &Account{
		DBID:                     dbID,
		UpstreamType:             UpstreamGrok,
		APIKey:                   "grok-key",
		Status:                   StatusReady,
		HealthTier:               HealthTierHealthy,
		BaseConcurrencyEffective: 1,
		DynamicConcurrencyLimit:  1,
	}
}

func TestUsagePercentForSchedulingGrokUsesQuotaObservations(t *testing.T) {
	now := time.Now()
	acc := newGrokSchedulingTestAccount(1)

	if got := acc.usagePercentForScheduling(); got != 0 {
		t.Fatalf("no observations: got %v, want 0", got)
	}

	// 逐请求余量头：剩余 25% → 已用 75%。
	acc.setGrokRateLimitSnapshot(GrokRateLimitSnapshot{LimitTokens: 1000, RemainingTokens: 250, UpdatedAt: now}, false)
	if got := acc.usagePercentForScheduling(); got != 75 {
		t.Fatalf("rate-limit headers: got %v, want 75", got)
	}

	// 余量头过期后视为未知。
	acc.mu.Lock()
	acc.grokRateLimit.UpdatedAt = now.Add(-grokSchedulingRateLimitTTL - time.Minute)
	acc.mu.Unlock()
	if got := acc.usagePercentForScheduling(); got != 0 {
		t.Fatalf("stale rate-limit headers: got %v, want 0", got)
	}

	// 免费额度耗尽快照是权威用量，优先于余量头。
	acc.setGrokRateLimitSnapshot(GrokRateLimitSnapshot{LimitTokens: 1000, RemainingTokens: 900, UpdatedAt: now.Add(time.Second)}, false)
	acc.SetGrokFreeQuotaSnapshot(GrokFreeQuotaSnapshot{UsedTokens: 1200, LimitTokens: 1000, ExhaustedAt: now})
	if got := acc.usagePercentForScheduling(); got != 100 {
		t.Fatalf("free quota exhausted: got %v, want 100 (clamped)", got)
	}

	// 免费额度快照超过 24h 后回退到余量头。
	acc.mu.Lock()
	acc.grokFreeQuota.ExhaustedAt = now.Add(-grokSchedulingFreeQuotaTTL - time.Minute)
	acc.mu.Unlock()
	if got := acc.usagePercentForScheduling(); got != 10 {
		t.Fatalf("expired free quota falls back to headers: got %v, want 10", got)
	}
}

func TestUsagePercentForSchedulingCodexPathUnchanged(t *testing.T) {
	acc := &Account{DBID: 1, AccessToken: "token", UsagePercent7d: 42, UsagePercent7dValid: true}
	if got := acc.usagePercentForScheduling(); got != 42 {
		t.Fatalf("codex 7d usage: got %v, want 42", got)
	}
	acc.UsagePercent7dValid = false
	if got := acc.usagePercentForScheduling(); got != 0 {
		t.Fatalf("codex without valid 7d usage: got %v, want 0", got)
	}
}

func TestAntigravitySchedulingUsedPercent(t *testing.T) {
	// 有 quota_groups 时取最紧的桶。
	grouped := AntigravityQuotaSnapshot{
		Models: []AntigravityModelQuota{{ModelID: "a", RemainingFraction: 1}},
		Groups: []AntigravityQuotaGroup{{Buckets: []AntigravityQuotaBucket{{RemainingFraction: 0.9}, {RemainingFraction: 0.3}}}},
	}
	if pct, ok := antigravitySchedulingUsedPercent(grouped); !ok || pct != 70 {
		t.Fatalf("grouped: got (%v, %v), want (70, true)", pct, ok)
	}
	// 无分组时取模型平均剩余。
	modelsOnly := AntigravityQuotaSnapshot{Models: []AntigravityModelQuota{{RemainingFraction: 0.5}, {RemainingFraction: 1}}}
	if pct, ok := antigravitySchedulingUsedPercent(modelsOnly); !ok || pct != 25 {
		t.Fatalf("models only: got (%v, %v), want (25, true)", pct, ok)
	}
	if _, ok := antigravitySchedulingUsedPercent(AntigravityQuotaSnapshot{Forbidden: true, Models: []AntigravityModelQuota{{RemainingFraction: 0}}}); ok {
		t.Fatal("forbidden snapshot must not produce a sort key")
	}
	if _, ok := antigravitySchedulingUsedPercent(AntigravityQuotaSnapshot{}); ok {
		t.Fatal("empty snapshot must not produce a sort key")
	}
}

func TestAntigravityQuotaSchedulingProjectionAndTTL(t *testing.T) {
	now := time.Now()
	acc := &Account{DBID: 7, UpstreamType: UpstreamAntigravity, AccessToken: "at"}
	raw, _ := json.Marshal(AntigravityQuotaSnapshot{
		Models:    []AntigravityModelQuota{{RemainingFraction: 0.2}},
		UpdatedAt: now,
	})
	acc.applyAntigravityQuotaSchedulingLocked(string(raw))
	if got := acc.usagePercentForScheduling(); got != 80 {
		t.Fatalf("fresh antigravity quota: got %v, want 80", got)
	}

	stale, _ := json.Marshal(AntigravityQuotaSnapshot{
		Models:    []AntigravityModelQuota{{RemainingFraction: 0.2}},
		UpdatedAt: now.Add(-antigravitySchedulingQuotaTTL - time.Hour),
	})
	acc.applyAntigravityQuotaSchedulingLocked(string(stale))
	if got := acc.usagePercentForScheduling(); got != 0 {
		t.Fatalf("stale antigravity quota: got %v, want 0", got)
	}

	acc.applyAntigravityQuotaSchedulingLocked("not json")
	if acc.antigravityQuotaValid {
		t.Fatal("malformed snapshot must clear the sort key")
	}
	acc.applyAntigravityQuotaSchedulingLocked("")
	if acc.antigravityQuotaValid {
		t.Fatal("empty credential must clear the sort key")
	}
}

func TestFastSchedulerRemainingQuotaOrdersGrokByRateLimitHeaders(t *testing.T) {
	now := time.Now()
	mostUsed := newGrokSchedulingTestAccount(1)
	mostUsed.setGrokRateLimitSnapshot(GrokRateLimitSnapshot{LimitTokens: 1000, RemainingTokens: 100, UpdatedAt: now}, false)
	leastUsed := newGrokSchedulingTestAccount(2)
	leastUsed.setGrokRateLimitSnapshot(GrokRateLimitSnapshot{LimitTokens: 1000, RemainingTokens: 900, UpdatedAt: now}, false)
	unknown := newGrokSchedulingTestAccount(3)

	scheduler := NewFastScheduler(1, "remaining_quota")
	scheduler.Rebuild([]*Account{mostUsed, leastUsed, unknown})

	var got []int64
	for i := 0; i < 3; i++ {
		acc := scheduler.Acquire()
		if acc == nil {
			t.Fatalf("Acquire() returned nil at iteration %d", i)
		}
		got = append(got, acc.DBID)
	}
	// 未知用量按 0 处理排最前；随后剩余最多的账号；已用最多的最后。
	want := []int64{3, 2, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("remaining_quota grok order mismatch: got=%v want=%v", got, want)
		}
	}
}

func TestFastSchedulerFillFirstOrdersGrokByUsage(t *testing.T) {
	now := time.Now()
	mostUsed := newGrokSchedulingTestAccount(1)
	mostUsed.setGrokRateLimitSnapshot(GrokRateLimitSnapshot{LimitTokens: 1000, RemainingTokens: 100, UpdatedAt: now}, false)
	leastUsed := newGrokSchedulingTestAccount(2)
	leastUsed.setGrokRateLimitSnapshot(GrokRateLimitSnapshot{LimitTokens: 1000, RemainingTokens: 900, UpdatedAt: now}, false)

	scheduler := NewFastScheduler(1, "fill_first")
	scheduler.Rebuild([]*Account{leastUsed, mostUsed})
	if acc := scheduler.Acquire(); acc == nil || acc.DBID != mostUsed.DBID {
		t.Fatalf("fill_first should concentrate on the most-used grok account, got %+v", acc)
	}
}

func TestGrokRateLimitSnapshotRefreshesSchedulerEntry(t *testing.T) {
	now := time.Now()
	a := newGrokSchedulingTestAccount(1)
	b := newGrokSchedulingTestAccount(2)
	scheduler := NewFastScheduler(4, "remaining_quota")
	scheduler.Rebuild([]*Account{a, b})

	refresher := &recordingSchedulerRefresher{scheduler: scheduler}
	a.grokRuntimeSink = refresher
	b.grokRuntimeSink = refresher

	// a 用掉 90%，b 用掉 10%：b 应排到 a 前面，且排序变化是由快照写入触发的。
	a.SetGrokRateLimitSnapshot(GrokRateLimitSnapshot{LimitTokens: 1000, RemainingTokens: 100, UpdatedAt: now})
	b.SetGrokRateLimitSnapshot(GrokRateLimitSnapshot{LimitTokens: 1000, RemainingTokens: 900, UpdatedAt: now})
	if refresher.calls != 2 {
		t.Fatalf("expected 2 scheduler refresh calls, got %d", refresher.calls)
	}
	first := scheduler.Acquire()
	if first == nil || first.DBID != b.DBID {
		t.Fatalf("remaining_quota should pick the least-used grok account after header refresh, got %+v", first)
	}
}

type recordingSchedulerRefresher struct {
	scheduler *FastScheduler
	calls     int
}

func (r *recordingSchedulerRefresher) refreshSchedulerUsage(acc *Account) {
	r.calls++
	r.scheduler.Update(acc)
}

func (r *recordingSchedulerRefresher) persistGrokRuntimeFact(_ context.Context, _ *Account, _ GrokRuntimeFactObservation) error {
	return nil
}
