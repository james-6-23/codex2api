package auth

import (
	"testing"
	"time"
)

func TestFastSchedulerReset5hPriorityAndLoad(t *testing.T) {
	for _, equal := range []bool{false, true} {
		name := "earlier deadline beats lower load"
		if equal {
			name = "equal deadlines share concurrent load"
		}
		t.Run(name, func(t *testing.T) {
			first := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 4)
			second := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 4)
			first.Reset5hAt = time.Now().Add(time.Hour)
			second.Reset5hAt = first.Reset5hAt.Add(time.Hour)
			if equal {
				second.Reset5hAt = first.Reset5hAt
			}
			for _, acc := range []*Account{first, second} {
				acc.PlanType = "plus"
				acc.UsagePercent5h, acc.UsagePercent5hValid = 20, true
			}
			scheduler := NewFastScheduler(4, SchedulerModeReset5hFirst)
			scheduler.Rebuild([]*Account{second, first})
			a := scheduler.Acquire()
			if a == nil || a.DBID != 1 {
				t.Fatalf("first selection = %v, want account 1", a)
			}
			defer scheduler.Release(a)
			b := scheduler.Acquire()
			want := int64(1)
			if equal {
				want = 2
			}
			if b == nil || b.DBID != want {
				t.Fatalf("second selection = %v, want account %d", b, want)
			}
			defer scheduler.Release(b)
		})
	}
}

func TestFastSchedulerReset5hFiltersAndFallback(t *testing.T) {
	accounts := make([]*Account, 10)
	for i := range accounts {
		acc := newFastSchedulerTestAccount(int64(i+1), HealthTierHealthy, 100, 1)
		acc.PlanType = "plus"
		acc.UsagePercent5h, acc.UsagePercent5hValid = 20, true
		acc.Reset5hAt = time.Now().Add(time.Hour)
		accounts[i] = acc
	}
	// Unknown windows remain a fallback after all known windows are rejected.
	accounts[9].UsagePercent5hValid = false
	for _, want := range []int64{9, 10} {
		scheduler := NewFastScheduler(1, SchedulerModeReset5hFirst)
		scheduler.Rebuild(accounts)
		got := scheduler.AcquireExcludingWithFilter(1, nil, func(acc *Account) bool { return acc.ID() == want })
		if got == nil || got.ID() != want {
			t.Fatalf("filtered selection = %v, want %d", got, want)
		}
		scheduler.Release(got)
	}
}
