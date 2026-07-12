package auth

import "testing"

func TestNormalizeSchedulerPriority(t *testing.T) {
	tests := []struct {
		in   int64
		want int64
	}{
		{0, 0},
		{50, 50},
		{-50, -50},
		{200, 100},
		{-200, -100},
	}
	for _, tt := range tests {
		if got := normalizeSchedulerPriority(tt.in); got != tt.want {
			t.Errorf("normalizeSchedulerPriority(%d) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

// 高优先级账号严格先调度，哪怕低优先级账号的套餐分数偏置更高。
func TestNextExcludingPrefersHigherSchedulerPriority(t *testing.T) {
	// relay：plus 套餐默认 +50 分数偏置，若无优先级本应胜出
	relay := &Account{DBID: 1, AccessToken: "tok-relay", PlanType: "plus"}
	official := &Account{DBID: 2, AccessToken: "tok-official", PlanType: "free"}
	official.SetSchedulerPriority(10)

	store := &Store{
		accounts:       []*Account{relay, official},
		maxConcurrency: 4,
	}

	for i := 0; i < 5; i++ {
		acc := store.NextExcluding(0, nil)
		if acc == nil {
			t.Fatal("expected account")
		}
		if acc.DBID != 2 {
			t.Fatalf("attempt %d: picked DBID %d, want high-priority account 2", i, acc.DBID)
		}
		store.Release(acc)
	}
}

// 高优先级账号不可用（被排除）时回落到低优先级账号。
func TestNextExcludingFallsBackToLowerPriorityWhenExcluded(t *testing.T) {
	fallback := &Account{DBID: 1, AccessToken: "tok-fallback"}
	fallback.SetSchedulerPriority(-10)
	preferred := &Account{DBID: 2, AccessToken: "tok-preferred"}
	preferred.SetSchedulerPriority(10)

	store := &Store{
		accounts:       []*Account{fallback, preferred},
		maxConcurrency: 4,
	}

	acc := store.NextExcluding(0, map[int64]bool{2: true})
	if acc == nil {
		t.Fatal("expected fallback account")
	}
	if acc.DBID != 1 {
		t.Fatalf("picked DBID %d, want fallback account 1", acc.DBID)
	}
	store.Release(acc)
}

func TestApplyAccountSchedulerPriority(t *testing.T) {
	acc := &Account{DBID: 7, AccessToken: "tok"}
	store := &Store{accounts: []*Account{acc}, maxConcurrency: 2}

	value := int64(30)
	if !store.ApplyAccountSchedulerPriority(7, &value) {
		t.Fatal("expected apply to succeed")
	}
	if got := acc.GetSchedulerPriority(); got != 30 {
		t.Fatalf("priority = %d, want 30", got)
	}

	if !store.ApplyAccountSchedulerPriority(7, nil) {
		t.Fatal("expected reset to succeed")
	}
	if got := acc.GetSchedulerPriority(); got != 0 {
		t.Fatalf("priority after reset = %d, want 0", got)
	}

	if store.ApplyAccountSchedulerPriority(999, &value) {
		t.Fatal("unknown account should return false")
	}
}

// FastScheduler：同一健康桶内高优先级段先被轮询，段内耗尽才落到下一段。
func TestFastSchedulerPrefersHigherPrioritySegment(t *testing.T) {
	low := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
	high := newFastSchedulerTestAccount(2, HealthTierHealthy, 80, 1)
	high.SetSchedulerPriority(10)

	scheduler := NewFastScheduler(1, "round_robin")
	scheduler.Rebuild([]*Account{low, high})

	first := scheduler.Acquire()
	if first == nil {
		t.Fatal("first Acquire() returned nil")
	}
	if first.DBID != 2 {
		t.Fatalf("first Acquire() picked dbID=%d, want high-priority 2", first.DBID)
	}

	// 高优先级账号并发占满后，回落到低优先级账号
	second := scheduler.Acquire()
	if second == nil {
		t.Fatal("second Acquire() returned nil")
	}
	if second.DBID != 1 {
		t.Fatalf("second Acquire() picked dbID=%d, want fallback 1", second.DBID)
	}
	scheduler.Release(first)
	scheduler.Release(second)
}

// FastScheduler 的调度优先级必须全局先于健康桶：高优先级 warm
// 账号仍应先于低优先级 healthy 账号。
func TestFastSchedulerPriorityPrecedesHealthTier(t *testing.T) {
	for _, mode := range []string{"round_robin", "remaining_quota"} {
		t.Run(mode, func(t *testing.T) {
			lowHealthy := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
			highWarm := newFastSchedulerTestAccount(2, HealthTierWarm, 80, 1)
			highWarm.SetSchedulerPriority(10)

			scheduler := NewFastScheduler(1, mode)
			scheduler.Rebuild([]*Account{lowHealthy, highWarm})

			got := scheduler.Acquire()
			if got == nil {
				t.Fatal("Acquire() returned nil")
			}
			defer scheduler.Release(got)
			if got.DBID != highWarm.DBID {
				t.Fatalf("Acquire() picked dbID=%d, want high-priority warm account %d", got.DBID, highWarm.DBID)
			}
		})
	}
}

// 同一高优先级的所有健康桶都无可用容量后，才回落到低优先级账号。
func TestFastSchedulerFallsBackAfterHigherPriorityIsFullAcrossHealthTiers(t *testing.T) {
	for _, mode := range []string{"round_robin", "remaining_quota"} {
		t.Run(mode, func(t *testing.T) {
			lowHealthy := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 1)
			highHealthy := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 1)
			highWarm := newFastSchedulerTestAccount(3, HealthTierWarm, 80, 1)
			highHealthy.SetSchedulerPriority(10)
			highWarm.SetSchedulerPriority(10)

			scheduler := NewFastScheduler(1, mode)
			scheduler.Rebuild([]*Account{lowHealthy, highWarm, highHealthy})

			first := scheduler.Acquire()
			if first == nil || first.DBID != highHealthy.DBID {
				if first == nil {
					t.Fatal("first Acquire() returned nil")
				}
				t.Fatalf("first Acquire() picked dbID=%d, want high-priority healthy account %d", first.DBID, highHealthy.DBID)
			}
			second := scheduler.Acquire()
			if second == nil {
				scheduler.Release(first)
				t.Fatal("second Acquire() returned nil")
			}
			if second.DBID != highWarm.DBID {
				scheduler.Release(first)
				scheduler.Release(second)
				t.Fatalf("second Acquire() picked dbID=%d, want high-priority warm account %d", second.DBID, highWarm.DBID)
			}
			third := scheduler.Acquire()
			if third == nil {
				scheduler.Release(first)
				scheduler.Release(second)
				t.Fatal("third Acquire() returned nil")
			}
			if third.DBID != lowHealthy.DBID {
				scheduler.Release(first)
				scheduler.Release(second)
				scheduler.Release(third)
				t.Fatalf("third Acquire() picked dbID=%d, want lower-priority healthy fallback %d", third.DBID, lowHealthy.DBID)
			}

			scheduler.Release(first)
			scheduler.Release(second)
			scheduler.Release(third)
		})
	}
}
