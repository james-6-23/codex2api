package auth

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestIsSparkUsagePlanFoldsProlite(t *testing.T) {
	if !IsSparkUsagePlan("pro") || !IsSparkUsagePlan("prolite") || !IsSparkUsagePlan("ProLite") {
		t.Fatal("pro/prolite should show a spark usage bar")
	}
	if IsSparkUsagePlan("plus") || IsSparkUsagePlan("free") {
		t.Fatal("plus/free should not show a spark usage bar")
	}
}

func TestSparkDispatchIgnoresAccount5hLimit(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:       2,
		TestConcurrency:      1,
		TestModel:            "gpt-5.4",
		FastSchedulerEnabled: true,
	})
	acc := &Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "pro",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	store.AddAccount(acc)
	store.MarkPremium5hRateLimited(acc, time.Now().Add(3*time.Hour))
	acc.SetUsageSnapshot5h(100, time.Now().Add(3*time.Hour))

	if acc.IsAvailable() {
		t.Fatal("standard availability should still treat 5h=100% as exhausted")
	}
	if got := store.NextExcludingWithFilter(0, nil, nil); got != nil {
		store.Release(got)
		t.Fatal("gpt-5.5/standard dispatch should miss a 5h-exhausted account")
	}
	got := store.NextExcludingWithDispatch(0, nil, nil, DispatchPolicySpark)
	if got == nil {
		t.Fatal("gpt-5.3-codex-spark should still select a 5h-exhausted Pro account")
	}
	store.Release(got)
}

func TestSparkDispatchBlocksWhenSparkExhausted(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:       2,
		TestConcurrency:      1,
		TestModel:            "gpt-5.4",
		FastSchedulerEnabled: true,
	})
	acc := &Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "pro",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	store.AddAccount(acc)
	acc.SetUsageSnapshot5h(20, time.Now().Add(3*time.Hour))
	acc.SetUsageSnapshotSpark(100, time.Now().Add(3*time.Hour))

	if !acc.IsAvailable() {
		t.Fatal("standard models should still see a main 5h window that is not full")
	}
	if got := store.NextExcludingWithDispatch(0, nil, nil, DispatchPolicySpark); got != nil {
		store.Release(got)
		t.Fatal("spark dispatch should miss an account whose spark window is full")
	}
	got := store.NextExcludingWithFilter(0, nil, nil)
	if got == nil {
		t.Fatal("standard dispatch should still select an account with unused 5h quota")
	}
	store.Release(got)
}

func TestFastSchedulerKeeps5hExhaustedSparkAccount(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 2)
	acc.PlanType = "pro"
	acc.SetUsageSnapshot5h(100, time.Now().Add(3*time.Hour))

	scheduler := NewFastScheduler(2, "round_robin")
	scheduler.Rebuild([]*Account{acc})

	if got := scheduler.AcquireExcludingWithDispatch(0, nil, nil, DispatchPolicyStandard); got != nil {
		scheduler.Release(got)
		t.Fatal("standard acquire should skip a 5h-exhausted account")
	}
	got := scheduler.AcquireExcludingWithDispatch(0, nil, nil, DispatchPolicySpark)
	if got == nil {
		t.Fatal("spark acquire should keep a 5h-exhausted but spark-eligible account in the pool")
	}
	scheduler.Release(got)
}

func TestSparkSnapshotHydratesFromCredentials(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	resetAt := time.Now().Add(90 * time.Minute).UTC().Truncate(time.Second)
	id, err := db.InsertAccountWithCredentials(ctx, "spark-hydrate", map[string]interface{}{
		"access_token":                 "token",
		"plan_type":                    "pro",
		"codex_spark_used_percent":     66,
		"codex_spark_reset_at":         resetAt.Format(time.RFC3339),
		"codex_spark_usage_updated_at": time.Now().UTC().Truncate(time.Second).Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	acc := store.FindByID(id)
	if acc == nil {
		t.Fatal("hydrated account missing")
	}
	pct, gotReset, ok := acc.GetUsageSnapshotSpark()
	if !ok {
		t.Fatal("spark snapshot was not hydrated from credentials")
	}
	if pct != 66 {
		t.Fatalf("spark percent = %v, want 66", pct)
	}
	if !gotReset.Equal(resetAt) {
		t.Fatalf("spark reset = %s, want %s", gotReset.Format(time.RFC3339), resetAt.Format(time.RFC3339))
	}
}

// DispatchPaused 是锁外原子标志:标准路径在 fastSchedulerSnapshotWithUsageOverride
// 里显式拦截,spark 路径此前只走 sparkDispatchEligibleLocked(与 isAvailableLocked
// 一样不读原子标志),导致运维手动停调度或过载熔断置位的账号仍被 spark 选中。
func TestSparkDispatchRespectsDispatchPaused(t *testing.T) {
	acc := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 2)
	acc.PlanType = "pro"

	scheduler := NewFastScheduler(2, "round_robin")
	scheduler.Rebuild([]*Account{acc})

	got := scheduler.AcquireExcludingWithDispatch(0, nil, nil, DispatchPolicySpark)
	if got == nil {
		t.Fatal("spark acquire should succeed before the account is paused")
	}
	scheduler.Release(got)

	atomic.StoreInt32(&acc.DispatchPaused, 1)
	scheduler.Update(acc)

	if got := scheduler.AcquireExcludingWithDispatch(0, nil, nil, DispatchPolicySpark); got != nil {
		scheduler.Release(got)
		t.Fatal("spark acquire must skip a dispatch-paused account")
	}
	if got := scheduler.AcquireExcludingWithDispatch(0, nil, nil, DispatchPolicyStandard); got != nil {
		scheduler.Release(got)
		t.Fatal("standard acquire must skip a dispatch-paused account")
	}
}
