package auth

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newFastSchedulerTestAccount(id int64, tier AccountHealthTier, score float64, limit int64) *Account {
	return &Account{
		DBID:                    id,
		AccessToken:             "token",
		Status:                  StatusReady,
		HealthTier:              tier,
		SchedulerScore:          score,
		DynamicConcurrencyLimit: limit,
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

// ==================== Model-Aware Tests (Phase 1) ====================

// newTestAccountWithModelState 创建带有ModelState的测试账号
func newTestAccountWithModelState(id int64, tier AccountHealthTier, score float64, limit int64, modelStates map[string]*ModelState) *Account {
	acc := newFastSchedulerTestAccount(id, tier, score, limit)
	acc.ModelStates = modelStates
	return acc
}

// TestFastScheduler_AcquireForModel_基本 测试基本model-aware调度
func TestFastScheduler_AcquireForModel_基本(t *testing.T) {
	model := "gpt-4"
	acc := newTestAccountWithModelState(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(), // ready状态
	})

	scheduler := NewFastScheduler(2)
	scheduler.Rebuild([]*Account{acc})

	got := scheduler.AcquireForModel(model, nil)
	if got == nil {
		t.Fatal("AcquireForModel() returned nil")
	}
	defer scheduler.Release(got)

	if got.DBID != acc.DBID {
		t.Fatalf("AcquireForModel() picked dbID=%d, want %d", got.DBID, acc.DBID)
	}
}

// TestFastScheduler_AcquireForModel_模型冷却排除 测试模型冷却时排除账号
func TestFastScheduler_AcquireForModel_模型冷却排除(t *testing.T) {
	model := "gpt-4"

	// 账号1: gpt-4模型冷却中
	coolingAcc := newTestAccountWithModelState(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": func() *ModelState {
			ms := NewModelState()
			ms.ApplyCooldown("rate_limited")
			return ms
		}(),
	})

	// 账号2: gpt-4模型可用
	availableAcc := newTestAccountWithModelState(2, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})

	scheduler := NewFastScheduler(2)
	scheduler.Rebuild([]*Account{coolingAcc, availableAcc})

	// 应该选到账号2（账号1的gpt-4在冷却中）
	got := scheduler.AcquireForModel(model, nil)
	if got == nil {
		t.Fatal("AcquireForModel() returned nil")
	}
	defer scheduler.Release(got)

	if got.DBID != availableAcc.DBID {
		t.Fatalf("AcquireForModel() picked dbID=%d, want %d (cooling account should be skipped)", got.DBID, availableAcc.DBID)
	}
}

// TestFastScheduler_AcquireForModel_无模型参数回退 测试无模型参数时回退到旧逻辑
func TestFastScheduler_AcquireForModel_无模型参数回退(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 2)

	scheduler := NewFastScheduler(2)
	scheduler.Rebuild([]*Account{acc})

	// 空模型名称应该回退到AcquireExcluding
	got := scheduler.AcquireForModel("", nil)
	if got == nil {
		t.Fatal("AcquireForModel(\"\") returned nil, should fallback to AcquireExcluding")
	}
	defer scheduler.Release(got)

	if got.DBID != acc.DBID {
		t.Fatalf("AcquireForModel() picked dbID=%d, want %d", got.DBID, acc.DBID)
	}
}

// TestFastScheduler_SnapshotForModel_单次锁 测试单次Account.mu.RLock完成检查
func TestFastScheduler_SnapshotForModel_单次锁(t *testing.T) {
	model := "gpt-4"
	acc := newTestAccountWithModelState(1, HealthTierHealthy, 100, 2, map[string]*ModelState{
		"gpt-4": NewModelState(),
	})

	now := time.Now()

	// 测试fastSchedulerSnapshotForModel在单次锁内完成
	tier, score, limit, available := acc.fastSchedulerSnapshotForModel(2, model, now)

	if !available {
		t.Fatal("fastSchedulerSnapshotForModel() returned available=false for ready model")
	}
	if tier != HealthTierHealthy {
		t.Fatalf("fastSchedulerSnapshotForModel() tier=%s, want %s", tier, HealthTierHealthy)
	}
	if score != 100 {
		t.Fatalf("fastSchedulerSnapshotForModel() score=%f, want 100", score)
	}
	if limit != 2 {
		t.Fatalf("fastSchedulerSnapshotForModel() limit=%d, want 2", limit)
	}
}

// TestFastScheduler_ConcurrentAcquireForModel 测试并发调度安全
func TestFastScheduler_ConcurrentAcquireForModel(t *testing.T) {
	model := "gpt-4"

	// 创建多个账号，每个都有gpt-4模型状态
	accounts := make([]*Account, 10)
	for i := 0; i < 10; i++ {
		accounts[i] = newTestAccountWithModelState(int64(i+1), HealthTierHealthy, 100, 2, map[string]*ModelState{
			"gpt-4": NewModelState(),
		})
	}

	scheduler := NewFastScheduler(2)
	scheduler.Rebuild(accounts)

	const concurrency = 20
	const iterations = 50

	var wg sync.WaitGroup
	acquiredCounts := make(map[int64]int64)
	var mu sync.Mutex

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				acc := scheduler.AcquireForModel(model, nil)
				if acc != nil {
					mu.Lock()
					acquiredCounts[acc.DBID]++
					mu.Unlock()

					// 模拟工作
					time.Sleep(time.Millisecond)
					scheduler.Release(acc)
				}
			}
		}()
	}

	wg.Wait()

	// 验证所有账号都被使用到了
	if len(acquiredCounts) == 0 {
		t.Fatal("No accounts were acquired")
	}

	// 验证没有超过并发限制
	for dbID, count := range acquiredCounts {
		if count <= 0 {
			t.Fatalf("Account %d was never acquired", dbID)
		}
	}

	t.Logf("Concurrent acquire completed: %d accounts used, total acquires=%d",
		len(acquiredCounts), func() int64 {
			var sum int64
			for _, c := range acquiredCounts {
				sum += c
			}
			return sum
		}())
}
