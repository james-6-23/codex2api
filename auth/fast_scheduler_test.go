package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func newFastSchedulerTestAccount(id int64, tier AccountHealthTier, score float64, limit int64) *Account {
	return &Account{
		DBID:                     id,
		AccessToken:              "token",
		Status:                   StatusReady,
		HealthTier:               tier,
		SchedulerScore:           score,
		DispatchScore:            score,
		BaseConcurrencyEffective: limit,
		DynamicConcurrencyLimit:  limit,
	}
}

func TestFastSchedulerAcquirePrefersHealthyTier(t *testing.T) {
	warm := newFastSchedulerTestAccount(1, HealthTierWarm, 90, 2)
	healthy := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 2)

	scheduler := NewFastScheduler(2)
	scheduler.Rebuild([]*Account{warm, healthy})

	got := scheduler.Acquire()
	if got == nil {
		t.Fatal("Acquire() returned nil")
	}
	defer scheduler.Release(got)

	if got.DBID != healthy.DBID {
		t.Fatalf("Acquire() picked dbID=%d, want %d", got.DBID, healthy.DBID)
	}
}

func TestFastSchedulerSkipsDispatchPausedAccount(t *testing.T) {
	paused := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 2)
	atomic.StoreInt32(&paused.DispatchPaused, 1)
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 2)

	scheduler := NewFastScheduler(2)
	scheduler.Rebuild([]*Account{paused, fallback})

	got := scheduler.Acquire()
	if got == nil {
		t.Fatal("Acquire() returned nil")
	}
	defer scheduler.Release(got)

	if got.DBID != fallback.DBID {
		t.Fatalf("Acquire() picked dbID=%d, want %d", got.DBID, fallback.DBID)
	}
}

func TestFastSchedulerRespectsConcurrencyLimit(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 1)

	scheduler := NewFastScheduler(1)
	scheduler.Rebuild([]*Account{acc})

	first := scheduler.Acquire()
	if first == nil {
		t.Fatal("first Acquire() returned nil")
	}

	second := scheduler.Acquire()
	if second != nil {
		t.Fatal("second Acquire() should be nil when concurrency limit is reached")
	}

	scheduler.Release(first)
	third := scheduler.Acquire()
	if third == nil {
		t.Fatal("third Acquire() returned nil after Release()")
	}
	scheduler.Release(third)
}

func TestFastSchedulerRoundRobinWithinTier(t *testing.T) {
	a1 := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
	a2 := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 4)
	a3 := newFastSchedulerTestAccount(3, HealthTierHealthy, 100, 4)

	scheduler := NewFastScheduler(4)
	scheduler.Rebuild([]*Account{a1, a2, a3})

	var got []int64
	for i := 0; i < 3; i++ {
		acc := scheduler.Acquire()
		if acc == nil {
			t.Fatalf("Acquire() returned nil at iteration %d", i)
		}
		got = append(got, acc.DBID)
		scheduler.Release(acc)
	}

	want := []int64{1, 2, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("round robin mismatch: got=%v want=%v", got, want)
		}
	}
}

func TestStoreNextExcludingRespectsAPIKeyWhitelist(t *testing.T) {
	restricted := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	restricted.SetAllowedAPIKeyIDs([]int64{2})
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)

	store := &Store{
		accounts:       []*Account{restricted, fallback},
		maxConcurrency: 1,
	}

	got := store.NextExcluding(1, nil)
	if got == nil {
		t.Fatal("NextExcluding() returned nil")
	}
	defer store.Release(got)

	if got.DBID != 2 {
		t.Fatalf("NextExcluding() picked dbID=%d, want 2", got.DBID)
	}
}

func TestStoreNextSkipsDispatchPausedAccount(t *testing.T) {
	paused := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	atomic.StoreInt32(&paused.DispatchPaused, 1)
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)

	store := &Store{
		accounts:       []*Account{paused, fallback},
		maxConcurrency: 1,
	}

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil")
	}
	defer store.Release(got)

	if got.DBID != fallback.DBID {
		t.Fatalf("Next() picked dbID=%d, want %d", got.DBID, fallback.DBID)
	}
}

func TestDispatchPausedDoesNotBlockUsageProbe(t *testing.T) {
	paused := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	atomic.StoreInt32(&paused.DispatchPaused, 1)

	store := &Store{
		accounts: []*Account{paused},
	}
	var probed int32
	store.SetUsageProbeFunc(func(_ context.Context, account *Account) error {
		if account.DBID != paused.DBID {
			t.Fatalf("usage probe account dbID=%d, want %d", account.DBID, paused.DBID)
		}
		atomic.AddInt32(&probed, 1)
		return nil
	})

	store.parallelProbeUsage(context.Background())

	if got := atomic.LoadInt32(&probed); got != 1 {
		t.Fatalf("usage probe calls = %d, want 1", got)
	}
}

func TestDispatchPausedDoesNotBlockRecoveryProbe(t *testing.T) {
	paused := newFastSchedulerTestAccount(1, HealthTierBanned, 120, 1)
	paused.RefreshToken = "rt"
	paused.ExpiresAt = time.Now().Add(time.Hour)
	atomic.StoreInt32(&paused.DispatchPaused, 1)

	store := &Store{
		accounts: []*Account{paused},
	}
	var probed int32
	store.SetUsageProbeFunc(func(_ context.Context, account *Account) error {
		if account.DBID != paused.DBID {
			t.Fatalf("recovery probe account dbID=%d, want %d", account.DBID, paused.DBID)
		}
		atomic.AddInt32(&probed, 1)
		return nil
	})

	store.parallelRecoveryProbe(context.Background())

	if got := atomic.LoadInt32(&probed); got != 1 {
		t.Fatalf("recovery probe calls = %d, want 1", got)
	}
	if atomic.LoadInt32(&paused.DispatchPaused) != 1 {
		t.Fatal("recovery probe cleared DispatchPaused; enable/disable must remain independent")
	}
}

func TestDispatchPausedDoesNotBlockAutoClean(t *testing.T) {
	paused := newFastSchedulerTestAccount(1, HealthTierBanned, 120, 1)
	atomic.StoreInt32(&paused.DispatchPaused, 1)

	store := &Store{
		accounts: []*Account{paused},
	}

	if cleaned := store.CleanByRuntimeStatus(context.Background(), "unauthorized"); cleaned != 1 {
		t.Fatalf("CleanByRuntimeStatus cleaned %d accounts, want 1", cleaned)
	}
	if got := store.AccountCount(); got != 0 {
		t.Fatalf("AccountCount() = %d, want 0", got)
	}
}

func TestStoreNextExcludingWithFilterRespectsPlanFilter(t *testing.T) {
	plus := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	plus.PlanType = "plus"
	pro := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	pro.PlanType = "pro"

	store := &Store{
		accounts:       []*Account{plus, pro},
		maxConcurrency: 1,
	}

	got := store.NextExcludingWithFilter(0, nil, func(acc *Account) bool {
		return acc.GetPlanType() == "pro"
	})
	if got == nil {
		t.Fatal("NextExcludingWithFilter() returned nil")
	}
	defer store.Release(got)

	if got.DBID != 2 {
		t.Fatalf("NextExcludingWithFilter() picked dbID=%d, want 2", got.DBID)
	}
}

func TestFastSchedulerAcquireExcludingRespectsAPIKeyWhitelist(t *testing.T) {
	restricted := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	restricted.SetAllowedAPIKeyIDs([]int64{2})
	fallback := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)

	scheduler := NewFastScheduler(1)
	scheduler.Rebuild([]*Account{restricted, fallback})

	got := scheduler.AcquireExcluding(1, nil)
	if got == nil {
		t.Fatal("AcquireExcluding() returned nil")
	}
	defer scheduler.Release(got)

	if got.DBID != 2 {
		t.Fatalf("AcquireExcluding() picked dbID=%d, want 2", got.DBID)
	}
}

func TestFastSchedulerAcquireExcludingWithFilterRespectsPlanFilter(t *testing.T) {
	plus := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	plus.PlanType = "plus"
	pro := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	pro.PlanType = "pro"

	scheduler := NewFastScheduler(1)
	scheduler.Rebuild([]*Account{plus, pro})

	got := scheduler.AcquireExcludingWithFilter(0, nil, func(acc *Account) bool {
		return acc.GetPlanType() == "pro"
	})
	if got == nil {
		t.Fatal("AcquireExcludingWithFilter() returned nil")
	}
	defer scheduler.Release(got)

	if got.DBID != 2 {
		t.Fatalf("AcquireExcludingWithFilter() picked dbID=%d, want 2", got.DBID)
	}
}

func TestFastSchedulerUpdateMovesAccountBetweenBuckets(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 2)
	scheduler := NewFastScheduler(2)
	scheduler.Rebuild([]*Account{acc})

	sizes := scheduler.BucketSizes()
	if sizes[HealthTierHealthy] != 1 {
		t.Fatalf("healthy bucket size = %d, want 1", sizes[HealthTierHealthy])
	}

	acc.SetCooldownUntil(time.Now().Add(10*time.Minute), "rate_limited")
	scheduler.Update(acc)

	sizes = scheduler.BucketSizes()
	if sizes[HealthTierHealthy] != 0 || sizes[HealthTierWarm] != 0 || sizes[HealthTierRisky] != 0 {
		t.Fatalf("expected cooldown account to be removed from all buckets, got %#v", sizes)
	}

	acc.mu.Lock()
	acc.Status = StatusReady
	acc.CooldownUtil = time.Time{}
	acc.CooldownReason = ""
	acc.HealthTier = HealthTierWarm
	acc.DynamicConcurrencyLimit = 1
	acc.mu.Unlock()
	scheduler.Update(acc)

	sizes = scheduler.BucketSizes()
	if sizes[HealthTierWarm] != 1 {
		t.Fatalf("warm bucket size = %d, want 1", sizes[HealthTierWarm])
	}
}

func TestFastSchedulerSkipsStaleBucketEntryWithoutUpdate(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 1)
	scheduler := NewFastScheduler(1)
	scheduler.Rebuild([]*Account{acc})

	acc.SetCooldownUntil(time.Now().Add(5*time.Minute), "rate_limited")

	got := scheduler.Acquire()
	if got != nil {
		t.Fatalf("Acquire() = %+v, want nil for stale cooldown account", got)
	}
}

func TestBuildFastSchedulerFromStore(t *testing.T) {
	store := &Store{
		accounts: []*Account{
			newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4),
			newFastSchedulerTestAccount(2, HealthTierWarm, 80, 2),
		},
		maxConcurrency: 4,
	}

	scheduler := store.BuildFastScheduler()
	sizes := scheduler.BucketSizes()
	if sizes[HealthTierHealthy] != 1 || sizes[HealthTierWarm] != 1 {
		t.Fatalf("unexpected bucket sizes: %#v", sizes)
	}
}

func TestFastSchedulerProvenPhaseUsesTotalRequestsOnly(t *testing.T) {
	premium := newFastSchedulerTestAccount(1, HealthTierHealthy, 150, 1)
	atomic.StoreInt64(&premium.TotalRequests, 0)

	proven := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 1)
	atomic.StoreInt64(&proven.TotalRequests, 11)

	scheduler := NewFastScheduler(1)
	scheduler.Rebuild([]*Account{premium, proven})

	got := scheduler.Acquire()
	if got == nil {
		t.Fatal("Acquire() returned nil")
	}
	defer scheduler.Release(got)

	if got.DBID != proven.DBID {
		t.Fatalf("Acquire() picked dbID=%d, want proven account %d", got.DBID, proven.DBID)
	}
}

func TestStoreFastSchedulerToggle(t *testing.T) {
	cooling := newFastSchedulerTestAccount(1, HealthTierWarm, 80, 1)
	cooling.Status = StatusCooldown
	cooling.CooldownUtil = time.Now().Add(5 * time.Minute)
	cooling.CooldownReason = "rate_limited"

	store := &Store{
		accounts: []*Account{
			cooling,
			newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 1),
		},
		maxConcurrency: 2,
	}

	if store.FastSchedulerEnabled() {
		t.Fatal("FastSchedulerEnabled() should be false by default")
	}

	store.SetFastSchedulerEnabled(true)
	if !store.FastSchedulerEnabled() {
		t.Fatal("FastSchedulerEnabled() should be true after SetFastSchedulerEnabled(true)")
	}
	if store.fastScheduler.Load() == nil {
		t.Fatal("expected fast scheduler instance to be created")
	}

	acc := store.Next()
	if acc == nil {
		t.Fatal("Next() returned nil with fast scheduler enabled")
	}
	if acc.DBID != 2 {
		t.Fatalf("Next() picked dbID=%d, want 2", acc.DBID)
	}
	store.Release(acc)

	store.SetFastSchedulerEnabled(false)
	if store.FastSchedulerEnabled() {
		t.Fatal("FastSchedulerEnabled() should be false after disabling")
	}
	if store.fastScheduler.Load() != nil {
		t.Fatal("expected fast scheduler instance to be cleared after disabling")
	}
}

func TestStoreFastSchedulerTracksCooldownTransition(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 2)
	store := &Store{
		accounts:       []*Account{acc},
		maxConcurrency: 2,
	}
	store.SetFastSchedulerEnabled(true)

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil before cooldown")
	}
	store.Release(got)

	store.MarkCooldown(acc, 5*time.Minute, "rate_limited")
	if got = store.Next(); got != nil {
		t.Fatalf("Next() = %+v, want nil after cooldown", got)
	}

	store.ClearCooldown(acc)
	if got = store.Next(); got == nil {
		t.Fatal("Next() returned nil after ClearCooldown()")
	}
	store.Release(got)
}

func TestFastSchedulerPremium5hRateLimitUsesSingleConcurrencyAndRecoversAfterReset(t *testing.T) {
	acc := &Account{
		DBID:                1,
		AccessToken:         "token",
		PlanType:            "plus",
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(30 * time.Minute),
	}

	scheduler := NewFastScheduler(4)
	scheduler.Rebuild([]*Account{acc})

	sizes := scheduler.BucketSizes()
	if sizes[HealthTierRisky] != 1 {
		t.Fatalf("risky bucket size = %d, want 1", sizes[HealthTierRisky])
	}

	first := scheduler.Acquire()
	if first == nil {
		t.Fatal("first Acquire() returned nil")
	}

	second := scheduler.Acquire()
	if second != nil {
		t.Fatal("second Acquire() should be nil while premium 5h rate limit is active")
	}

	acc.mu.Lock()
	acc.Reset5hAt = time.Now().Add(-time.Minute)
	acc.mu.Unlock()
	scheduler.Release(first)

	third := scheduler.Acquire()
	if third == nil {
		t.Fatal("third Acquire() returned nil after premium 5h reset expired")
	}
	fourth := scheduler.Acquire()
	if fourth == nil {
		t.Fatal("fourth Acquire() returned nil, want recovered concurrency after reset")
	}

	sizes = scheduler.BucketSizes()
	if sizes[HealthTierHealthy] != 1 || sizes[HealthTierRisky] != 0 {
		t.Fatalf("unexpected bucket sizes after reset recovery: %#v", sizes)
	}

	scheduler.Release(third)
	scheduler.Release(fourth)
}

func TestFastSchedulerEnabledFromEnv(t *testing.T) {
	t.Setenv("FAST_SCHEDULER_ENABLED", "")
	t.Setenv("CODEX_FAST_SCHEDULER", "")
	if fastSchedulerEnabledFromEnv() {
		t.Fatal("fastSchedulerEnabledFromEnv() should be false when env is empty")
	}

	t.Setenv("FAST_SCHEDULER_ENABLED", "true")
	if !fastSchedulerEnabledFromEnv() {
		t.Fatal("fastSchedulerEnabledFromEnv() should be true for FAST_SCHEDULER_ENABLED=true")
	}

	t.Setenv("FAST_SCHEDULER_ENABLED", "")
	t.Setenv("CODEX_FAST_SCHEDULER", "1")
	if !fastSchedulerEnabledFromEnv() {
		t.Fatal("fastSchedulerEnabledFromEnv() should be true for CODEX_FAST_SCHEDULER=1")
	}
}

func BenchmarkStoreNext1000(b *testing.B) {
	benchmarkStoreNext(b, 1000)
}

func BenchmarkStoreNext2813(b *testing.B) {
	benchmarkStoreNext(b, 2813)
}

func BenchmarkFastSchedulerAcquire1000(b *testing.B) {
	benchmarkFastSchedulerAcquire(b, 1000)
}

func BenchmarkFastSchedulerAcquire2813(b *testing.B) {
	benchmarkFastSchedulerAcquire(b, 2813)
}

func BenchmarkStoreNextParallel1000(b *testing.B) {
	benchmarkStoreNextParallel(b, 1000)
}

func BenchmarkFastSchedulerAcquireParallel1000(b *testing.B) {
	benchmarkFastSchedulerAcquireParallel(b, 1000)
}

func benchmarkStoreNext(b *testing.B, total int) {
	store := newBenchmarkStore(total, 64)
	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		acc := store.Next()
		if acc == nil {
			b.Fatal("Next() returned nil")
		}
		store.Release(acc)
	}
}

func benchmarkFastSchedulerAcquire(b *testing.B, total int) {
	store := newBenchmarkStore(total, 64)
	scheduler := store.BuildFastScheduler()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		acc := scheduler.Acquire()
		if acc == nil {
			b.Fatal("Acquire() returned nil")
		}
		scheduler.Release(acc)
	}
}

func benchmarkStoreNextParallel(b *testing.B, total int) {
	store := newBenchmarkStore(total, 64)
	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			acc := store.Next()
			if acc == nil {
				b.Fatal("Next() returned nil")
			}
			store.Release(acc)
		}
	})
}

func benchmarkFastSchedulerAcquireParallel(b *testing.B, total int) {
	store := newBenchmarkStore(total, 64)
	scheduler := store.BuildFastScheduler()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			acc := scheduler.Acquire()
			if acc == nil {
				b.Fatal("Acquire() returned nil")
			}
			scheduler.Release(acc)
		}
	})
}

func newBenchmarkStore(total int, maxConcurrency int64) *Store {
	accounts := make([]*Account, 0, total)
	for i := 0; i < total; i++ {
		tier := HealthTierHealthy
		score := 100.0 - float64(i%11)
		limit := maxConcurrency

		switch {
		case i%17 == 0:
			tier = HealthTierWarm
			score = 84
			limit = maxConcurrency / 2
			if limit < 1 {
				limit = 1
			}
		case i%29 == 0:
			tier = HealthTierRisky
			score = 58
			limit = 1
		}

		accounts = append(accounts, &Account{
			DBID:                    int64(i + 1),
			AccessToken:             "token",
			Status:                  StatusReady,
			HealthTier:              tier,
			SchedulerScore:          score,
			DynamicConcurrencyLimit: limit,
		})
	}

	return &Store{
		accounts:       accounts,
		maxConcurrency: maxConcurrency,
	}
}

func TestFastSchedulerRelease(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 2)
	atomic.StoreInt64(&acc.ActiveRequests, 1)

	scheduler := NewFastScheduler(2)
	scheduler.Release(acc)

	if got := atomic.LoadInt64(&acc.ActiveRequests); got != 0 {
		t.Fatalf("ActiveRequests after Release() = %d, want 0", got)
	}
}
