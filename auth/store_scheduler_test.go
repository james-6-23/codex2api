package auth

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func int64Ptr(v int64) *int64 {
	return &v
}

func recomputeTestAccount(acc *Account, baseLimit int64) {
	acc.mu.Lock()
	acc.recomputeSchedulerLocked(baseLimit)
	acc.mu.Unlock()
}

func TestAccountPremiumPlanGetsDefaultScoreBias(t *testing.T) {
	acc := &Account{
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "plus",
	}

	recomputeTestAccount(acc, 6)

	if acc.SchedulerScore != 100 {
		t.Fatalf("SchedulerScore = %v, want 100", acc.SchedulerScore)
	}
	if acc.DispatchScore != 150 {
		t.Fatalf("DispatchScore = %v, want 150", acc.DispatchScore)
	}
	if acc.ScoreBiasEffective != 50 {
		t.Fatalf("ScoreBiasEffective = %d, want 50", acc.ScoreBiasEffective)
	}
	if acc.BaseConcurrencyEffective != 6 {
		t.Fatalf("BaseConcurrencyEffective = %d, want 6", acc.BaseConcurrencyEffective)
	}
}

func TestAccountScoreBiasOverrideReplacesPlanDefault(t *testing.T) {
	acc := &Account{
		AccessToken:       "token",
		Status:            StatusReady,
		PlanType:          "team",
		ScoreBiasOverride: int64Ptr(12),
	}

	recomputeTestAccount(acc, 6)

	if acc.DispatchScore != 112 {
		t.Fatalf("DispatchScore = %v, want 112", acc.DispatchScore)
	}
	if acc.ScoreBiasEffective != 12 {
		t.Fatalf("ScoreBiasEffective = %d, want 12", acc.ScoreBiasEffective)
	}
}

func TestAccountRiskyTierDoesNotApplyScoreBias(t *testing.T) {
	acc := &Account{
		AccessToken:        "token",
		Status:             StatusReady,
		PlanType:           "pro",
		LastUnauthorizedAt: time.Now(),
	}

	recomputeTestAccount(acc, 6)

	if acc.HealthTier != HealthTierRisky {
		t.Fatalf("HealthTier = %s, want %s", acc.HealthTier, HealthTierRisky)
	}
	if acc.SchedulerScore >= 60 {
		t.Fatalf("SchedulerScore = %v, want < 60", acc.SchedulerScore)
	}
	if acc.DispatchScore != acc.SchedulerScore {
		t.Fatalf("DispatchScore = %v, want raw score %v when risky", acc.DispatchScore, acc.SchedulerScore)
	}
	if acc.ScoreBiasEffective != 0 {
		t.Fatalf("ScoreBiasEffective = %d, want 0", acc.ScoreBiasEffective)
	}
}

func TestAccountBaseConcurrencyOverrideControlsDynamicLimit(t *testing.T) {
	acc := &Account{
		AccessToken:             "token",
		Status:                  StatusReady,
		PlanType:                "plus",
		BaseConcurrencyOverride: int64Ptr(4),
	}

	recomputeTestAccount(acc, 10)
	if acc.DynamicConcurrencyLimit != 4 {
		t.Fatalf("healthy DynamicConcurrencyLimit = %d, want 4", acc.DynamicConcurrencyLimit)
	}

	acc.mu.Lock()
	acc.LastFailureAt = time.Now()
	acc.mu.Unlock()
	recomputeTestAccount(acc, 10)
	if acc.HealthTier != HealthTierWarm {
		t.Fatalf("warm HealthTier = %s, want %s", acc.HealthTier, HealthTierWarm)
	}
	if acc.DynamicConcurrencyLimit != 2 {
		t.Fatalf("warm DynamicConcurrencyLimit = %d, want 2", acc.DynamicConcurrencyLimit)
	}

	acc.mu.Lock()
	acc.LastUnauthorizedAt = time.Now()
	acc.mu.Unlock()
	recomputeTestAccount(acc, 10)
	if acc.HealthTier != HealthTierRisky {
		t.Fatalf("risky HealthTier = %s, want %s", acc.HealthTier, HealthTierRisky)
	}
	if acc.DynamicConcurrencyLimit != 1 {
		t.Fatalf("risky DynamicConcurrencyLimit = %d, want 1", acc.DynamicConcurrencyLimit)
	}
}

func TestAccountSkipWarmTierPromotesWarmScoreToHealthy(t *testing.T) {
	acc := &Account{
		AccessToken:   "token",
		Status:        StatusReady,
		PlanType:      "pro",
		SkipWarmTier:  true,
		LastTimeoutAt: time.Now(),
	}

	recomputeTestAccount(acc, 6)

	if acc.SchedulerScore >= 85 || acc.SchedulerScore < 60 {
		t.Fatalf("SchedulerScore = %v, want warm score range", acc.SchedulerScore)
	}
	if acc.HealthTier != HealthTierHealthy {
		t.Fatalf("HealthTier = %s, want %s", acc.HealthTier, HealthTierHealthy)
	}
	if acc.DynamicConcurrencyLimit != 6 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want full healthy limit 6", acc.DynamicConcurrencyLimit)
	}
}

func TestAccountSkipWarmTierPromotesRecentFailureWarmToHealthy(t *testing.T) {
	acc := &Account{
		AccessToken:   "token",
		Status:        StatusReady,
		PlanType:      "pro",
		SkipWarmTier:  true,
		LastFailureAt: time.Now(),
	}

	recomputeTestAccount(acc, 4)

	if acc.HealthTier != HealthTierHealthy {
		t.Fatalf("HealthTier = %s, want %s", acc.HealthTier, HealthTierHealthy)
	}
	if acc.DynamicConcurrencyLimit != 4 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want full healthy limit 4", acc.DynamicConcurrencyLimit)
	}
}

func TestAccountSkipWarmTierDoesNotPromoteRiskyOrBanned(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		acc  *Account
		want AccountHealthTier
	}{
		{
			name: "low score remains risky",
			acc: &Account{
				AccessToken:        "token",
				Status:             StatusReady,
				PlanType:           "pro",
				SkipWarmTier:       true,
				LastUnauthorizedAt: now,
			},
			want: HealthTierRisky,
		},
		{
			name: "banned remains banned",
			acc: &Account{
				AccessToken:  "token",
				Status:       StatusReady,
				PlanType:     "pro",
				HealthTier:   HealthTierBanned,
				SkipWarmTier: true,
			},
			want: HealthTierBanned,
		},
		{
			name: "premium 5h limit remains risky",
			acc: &Account{
				AccessToken:         "token",
				Status:              StatusReady,
				PlanType:            "plus",
				SkipWarmTier:        true,
				UsagePercent5h:      100,
				UsagePercent5hValid: true,
				Reset5hAt:           now.Add(time.Hour),
			},
			want: HealthTierRisky,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recomputeTestAccount(tc.acc, 6)
			if tc.acc.HealthTier != tc.want {
				t.Fatalf("HealthTier = %s, want %s", tc.acc.HealthTier, tc.want)
			}
		})
	}
}

func TestNeedsUsageProbeRateLimitedAllowsResetCreditsRefresh(t *testing.T) {
	// 429 冷却 + 重置次数从未探测过（stale）：应允许探针（wham-only）刷新「主动重置次数」。
	acc := &Account{
		AccessToken:    "token",
		Status:         StatusCooldown,
		CooldownReason: "rate_limited",
	}
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return true when reset credits are stale, even during rate_limited cooldown")
	}

	// 该状态应被标记为 limited，以保证探针只走 wham、不回退 /responses。
	if !acc.InLimitedState() {
		t.Fatal("InLimitedState should be true for rate_limited cooldown")
	}

	// 重置次数刚探测过（fresh）：429 冷却期间不应再发探针（避免 /responses 加重限流）。
	acc.MarkResetCreditsProbed(time.Now())
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return false during rate_limited cooldown once reset credits are fresh")
	}
}

func TestNeedsUsageProbeSkipsUnauthorized(t *testing.T) {
	acc := &Account{
		AccessToken:    "token",
		Status:         StatusCooldown,
		CooldownReason: "unauthorized",
	}
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return false for unauthorized cooldown")
	}
}

func TestNeedsUsageProbeAllowsReadyAccount(t *testing.T) {
	acc := &Account{
		AccessToken: "token",
		Status:      StatusReady,
	}
	// UsagePercent7dValid = false，应该返回 true
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return true for ready account without valid usage data")
	}
}

func TestNeedsUsageProbeRefreshesStaleResetCreditsDespiteFreshUsage(t *testing.T) {
	now := time.Now()
	// 核心修复：账号用量快照很新鲜（活跃账号被业务流量持续刷新），
	// 但「主动重置次数」从未/很久没探测过，仍应触发 wham 探针刷新它。
	acc := &Account{
		AccessToken:         "token",
		Status:              StatusReady,
		UsagePercent7d:      30,
		UsagePercent7dValid: true,
		UsageUpdatedAt:      now, // 用量刚刷新
	}
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return true when reset credits are stale even if usage snapshot is fresh")
	}

	// 重置次数也刚探测过：用量与重置次数都新鲜，则无需探针。
	acc.MarkResetCreditsProbed(now)
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe should return false when both usage and reset credits are fresh")
	}
}

func TestNeedsUsageProbeRequires5hSnapshotWhen5hAutoPauseEnabled(t *testing.T) {
	acc := &Account{
		AccessToken:          "token",
		Status:               StatusReady,
		UsagePercent7d:       12,
		UsagePercent7dValid:  true,
		UsageUpdatedAt:       time.Now(),
		AutoPause5hThreshold: 0.95,
	}
	acc.recomputeEffectiveAutoPause(nil)
	acc.MarkResetCreditsProbed(time.Now()) // 隔离 reset-credits 过期影响，专测 5h 快照缺失路径

	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true when 5h auto-pause is enabled but 5h snapshot is missing")
	}
}

// TestNeedsUsageProbeRefreshesStale5hAfterWindowReset 验证 Bug B 修复：
// 5h 窗口的重置时间已过、但快照仍是重置前的高用量时，应触发一次 wham 刷新，
// 让用量进度条跟随官方窗口重置而恢复，而不是一直停在旧值（如 100%）。
func TestNeedsUsageProbeRefreshesStale5hAfterWindowReset(t *testing.T) {
	now := time.Now()
	acc := &Account{
		AccessToken:         "token",
		Status:              StatusReady,
		UsagePercent7d:      12,
		UsagePercent7dValid: true,
		UsageUpdatedAt:      now, // 7d 快照新鲜，隔离 7d 路径
		// 5h：窗口重置时间已过，但快照仍是重置前采集的 100%
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           now.Add(-1 * time.Minute),
		UsageUpdatedAt5h:    now.Add(-3 * time.Hour),
	}
	acc.MarkResetCreditsProbed(now) // 隔离 reset-credits 过期影响，专测窗口重置路径

	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true: 5h window reset passed but snapshot is stale")
	}

	// 刷新后（快照刚更新、重置时间在未来）：不应再反复触发探针。
	acc.SetUsageSnapshot5hAt(8, now.Add(5*time.Hour), now)
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = true, want false after 5h snapshot refreshed")
	}
}

func TestPersistUsageSnapshotKeeps5hProbeRequiredWhen5hSnapshotMissing(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	acc := &Account{
		DBID:                 1,
		AccessToken:          "token",
		Status:               StatusReady,
		AutoPause5hThreshold: 0.95,
		AutoPause7dThreshold: 0.95,
		UsagePercent5hValid:  false,
		UsagePercent7dValid:  false,
	}
	acc.recomputeEffectiveAutoPause(store)

	store.PersistUsageSnapshot(acc, 20)
	acc.MarkResetCreditsProbed(time.Now()) // 隔离 reset-credits 过期影响，专测 5h 快照缺失路径

	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true after 7d-only persistence when 5h snapshot is still missing")
	}
}

func TestPersistUsageSnapshot5hOnlyDoesNotRefreshStale7dSnapshot(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		UsagePercent7d:      40,
		UsagePercent7dValid: true,
		UsageUpdatedAt:      time.Now().Add(-20 * time.Minute),
		UsagePercent5h:      25,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
	}

	store.PersistUsageSnapshot5hOnly(acc)
	acc.MarkResetCreditsProbed(time.Now()) // 隔离 reset-credits 过期影响，专测 7d 新鲜度不被 5h-only 持久化刷新

	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true because 5h-only persistence must not refresh stale 7d freshness")
	}
}

func TestTriggerUsageProbeAsyncRunsInLazyMode(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.SetLazyMode(true)
	store.AddAccount(&Account{DBID: 1, AccessToken: "token", Status: StatusReady})

	called := make(chan struct{}, 1)
	store.SetUsageProbeFunc(func(ctx context.Context, acc *Account) error {
		called <- struct{}{}
		return nil
	})

	store.TriggerUsageProbeAsync()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("usage probe was not triggered in lazy mode")
	}
}

func TestTriggerUsageProbeForceAsyncRunsInLazyMode(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.SetLazyMode(true)
	store.AddAccount(&Account{DBID: 1, AccessToken: "token", Status: StatusReady})

	called := make(chan struct{}, 1)
	store.SetUsageProbeFunc(func(ctx context.Context, acc *Account) error {
		called <- struct{}{}
		return nil
	})

	store.TriggerUsageProbeForceAsync()

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("forced usage probe was not triggered in lazy mode")
	}
}

func TestRefreshSingleBypassesCachedAccessToken(t *testing.T) {
	ctx := context.Background()
	tokenCache := cache.NewMemory(1)
	if err := tokenCache.SetAccessToken(ctx, 7, "cached-token", time.Hour); err != nil {
		t.Fatalf("SetAccessToken 返回错误: %v", err)
	}

	store := NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&Account{
		DBID:        7,
		AccessToken: "old-token",
		ExpiresAt:   time.Now().Add(time.Hour),
		Status:      StatusReady,
	})

	err := store.RefreshSingle(ctx, 7)
	if err == nil {
		t.Fatal("RefreshSingle should force upstream refresh instead of using cached token")
	}
	if !strings.Contains(err.Error(), "refresh_token 为空") {
		t.Fatalf("RefreshSingle error = %v, want missing refresh_token", err)
	}
}

func TestApplyRefreshedPlanTypeKeepsFreeUsageLimitAuthoritative(t *testing.T) {
	now := time.Now()
	acc := &Account{
		PlanType:            "free",
		UsagePercent7d:      100,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(time.Hour),
	}

	acc.mu.Lock()
	plan, applied := acc.applyRefreshedPlanTypeLocked("pro", now)
	acc.mu.Unlock()

	if plan != "pro" {
		t.Fatalf("plan = %q, want pro", plan)
	}
	if applied {
		t.Fatal("refreshed pro plan should not override active free usage-limit metadata")
	}
	if got := acc.GetPlanType(); got != "free" {
		t.Fatalf("PlanType = %q, want free", got)
	}
}

func TestApplyRefreshedPlanTypeKeepsActiveFreeUsageWindowAuthoritative(t *testing.T) {
	now := time.Now()
	acc := &Account{
		PlanType:            "free",
		UsagePercent7d:      3,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(24 * time.Hour),
	}

	acc.mu.Lock()
	plan, applied := acc.applyRefreshedPlanTypeLocked("pro", now)
	acc.mu.Unlock()

	if plan != "pro" {
		t.Fatalf("plan = %q, want pro", plan)
	}
	if applied {
		t.Fatal("refreshed pro plan should not override an active free 7d usage window")
	}
	if got := acc.GetPlanType(); got != "free" {
		t.Fatalf("PlanType = %q, want free", got)
	}
}

func TestApplyRefreshedPlanTypeAllowsPlanUpgradeAfterUsageReset(t *testing.T) {
	now := time.Now()
	acc := &Account{
		PlanType:            "free",
		UsagePercent7d:      100,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(-time.Minute),
	}

	acc.mu.Lock()
	plan, applied := acc.applyRefreshedPlanTypeLocked("pro", now)
	acc.mu.Unlock()

	if plan != "pro" || !applied {
		t.Fatalf("plan=%q applied=%v, want pro true", plan, applied)
	}
	if got := acc.GetPlanType(); got != "pro" {
		t.Fatalf("PlanType = %q, want pro", got)
	}
}

func TestStoreNextPrefersHigherDispatchScoreWithinTier(t *testing.T) {
	premium := &Account{
		DBID:        1,
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "pro",
	}
	regular := &Account{
		DBID:        2,
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "free",
	}
	recomputeTestAccount(premium, 2)
	recomputeTestAccount(regular, 2)

	store := &Store{
		accounts: []*Account{regular, premium},
	}
	store.SetMaxConcurrency(2)

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != premium.DBID {
		t.Fatalf("Next() picked dbID=%d, want premium account %d", got.DBID, premium.DBID)
	}
}

func TestStoreNextConcurrentAcquireDoesNotExceedDynamicLimit(t *testing.T) {
	acc := &Account{
		DBID:        1,
		AccessToken: "token",
		Status:      StatusReady,
		PlanType:    "pro",
	}
	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 1,
	}

	const workers = 32
	var entered int64
	start := make(chan struct{})
	filterGate := make(chan struct{})
	results := make(chan *Account, workers)

	filter := func(candidate *Account) bool {
		if candidate != nil && candidate.DBID == acc.DBID {
			atomic.AddInt64(&entered, 1)
		}
		<-filterGate
		return true
	}

	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			<-start
			results <- store.NextExcludingWithFilter(0, nil, filter)
		}()
	}
	close(start)

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt64(&entered) < workers {
		select {
		case <-deadline:
			close(filterGate)
			t.Fatalf("only %d/%d workers reached the scheduler filter", atomic.LoadInt64(&entered), workers)
		default:
			time.Sleep(time.Millisecond)
		}
	}

	acc.mu.Lock()
	close(filterGate)
	time.Sleep(20 * time.Millisecond)
	acc.mu.Unlock()

	wg.Wait()
	close(results)

	acquired := 0
	for got := range results {
		if got != nil {
			acquired++
		}
	}
	if acquired != 1 {
		t.Fatalf("acquired accounts = %d, want 1", acquired)
	}
	if got := atomic.LoadInt64(&acc.ActiveRequests); got != 1 {
		t.Fatalf("ActiveRequests = %d, want 1", got)
	}
	store.Release(acc)
}

func TestAccountPremium5hUrgencyBonusOnlyAffectsDispatchScore(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      20,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(30 * time.Minute),
		UsagePercent7d:      45,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(4 * 24 * time.Hour),
	}

	snapshot := acc.GetSchedulerDebugSnapshot(4)

	if snapshot.SchedulerScore != 100 {
		t.Fatalf("SchedulerScore = %v, want 100", snapshot.SchedulerScore)
	}
	if snapshot.Breakdown.UsageUrgencyBonus5h <= 20 {
		t.Fatalf("UsageUrgencyBonus5h = %v, want > 20", snapshot.Breakdown.UsageUrgencyBonus5h)
	}
	if snapshot.DispatchScore <= 170 {
		t.Fatalf("DispatchScore = %v, want plan bias plus urgency bonus", snapshot.DispatchScore)
	}
	if snapshot.HealthTier != string(HealthTierHealthy) {
		t.Fatalf("HealthTier = %q, want %q", snapshot.HealthTier, HealthTierHealthy)
	}
}

func TestAccountPremium5hUrgencyBonusSkipsNearlyExhaustedWindow(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      96,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(30 * time.Minute),
	}

	snapshot := acc.GetSchedulerDebugSnapshot(4)

	if snapshot.Breakdown.UsageUrgencyBonus5h != 0 {
		t.Fatalf("UsageUrgencyBonus5h = %v, want 0", snapshot.Breakdown.UsageUrgencyBonus5h)
	}
	if snapshot.DispatchScore != 150 {
		t.Fatalf("DispatchScore = %v, want only plan bias", snapshot.DispatchScore)
	}
}

func TestAccountPremium7dUrgencyBonusOnlyAffectsDispatchScore(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      63,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(36 * time.Hour),
	}

	snapshot := acc.GetSchedulerDebugSnapshot(4)

	if snapshot.SchedulerScore != 100 {
		t.Fatalf("SchedulerScore = %v, want 100", snapshot.SchedulerScore)
	}
	if snapshot.Breakdown.UsageUrgencyBonus7d <= 20 {
		t.Fatalf("UsageUrgencyBonus7d = %v, want > 20", snapshot.Breakdown.UsageUrgencyBonus7d)
	}
	if snapshot.DispatchScore <= 170 {
		t.Fatalf("DispatchScore = %v, want plan bias plus 7d urgency bonus", snapshot.DispatchScore)
	}
	if snapshot.HealthTier != string(HealthTierHealthy) {
		t.Fatalf("HealthTier = %q, want %q", snapshot.HealthTier, HealthTierHealthy)
	}
}

func TestAccountPremium7dUrgencyBonusSkipsDistantReset(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      63,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(5 * 24 * time.Hour),
	}

	snapshot := acc.GetSchedulerDebugSnapshot(4)

	if snapshot.Breakdown.UsageUrgencyBonus7d != 0 {
		t.Fatalf("UsageUrgencyBonus7d = %v, want 0", snapshot.Breakdown.UsageUrgencyBonus7d)
	}
	if snapshot.DispatchScore != 150 {
		t.Fatalf("DispatchScore = %v, want only plan bias", snapshot.DispatchScore)
	}
}

func TestStoreNextPrefersPremium7dResetSoonOverProvenAccount(t *testing.T) {
	now := time.Now()
	soon := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      63,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(36 * time.Hour),
	}
	later := &Account{
		DBID:                2,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent7d:      68,
		UsagePercent7dValid: true,
		Reset7dAt:           now.Add(5 * 24 * time.Hour),
	}
	atomic.StoreInt64(&later.TotalRequests, 450)
	recomputeTestAccount(soon, 2)
	recomputeTestAccount(later, 2)

	store := &Store{
		accounts: []*Account{later, soon},
	}
	store.SetMaxConcurrency(2)

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != soon.DBID {
		t.Fatalf("Next() picked dbID=%d, want 7d reset-soon account %d", got.DBID, soon.DBID)
	}
}

func TestStoreNextPrefersPremium5hResetSoonWithinTier(t *testing.T) {
	now := time.Now()
	soon := &Account{
		DBID:                1,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      25,
		UsagePercent5hValid: true,
		Reset5hAt:           now.Add(30 * time.Minute),
	}
	later := &Account{
		DBID:                2,
		AccessToken:         "token",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      25,
		UsagePercent5hValid: true,
		Reset5hAt:           now.Add(5 * time.Hour),
	}
	recomputeTestAccount(soon, 2)
	recomputeTestAccount(later, 2)

	store := &Store{
		accounts: []*Account{later, soon},
	}
	store.SetMaxConcurrency(2)

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != soon.DBID {
		t.Fatalf("Next() picked dbID=%d, want reset-soon account %d", got.DBID, soon.DBID)
	}
}
