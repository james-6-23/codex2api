package auth

import (
	"sync"
	"testing"
	"time"
)

func TestAccountBillingWindowSnapshot(t *testing.T) {
	reset5h := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	reset7d := reset5h.Add(6 * 24 * time.Hour)
	account := &Account{
		Reset5hAt:       reset5h,
		Reset7dAt:       reset7d,
		Window7dSeconds: 604800,
	}

	got := account.BillingWindowSnapshot()
	if !got.Reset5hAt.Equal(reset5h) || !got.Reset7dAt.Equal(reset7d) || got.Window7dSeconds != 604800 {
		t.Fatalf("BillingWindowSnapshot() = %+v", got)
	}

	var nilAccount *Account
	if got := nilAccount.BillingWindowSnapshot(); !got.Reset5hAt.IsZero() || !got.Reset7dAt.IsZero() || got.Window7dSeconds != 0 {
		t.Fatalf("nil BillingWindowSnapshot() = %+v, want zero snapshot", got)
	}
}

func TestSetUsageSnapshot7dAtPreservesPartialResetMetadata(t *testing.T) {
	oldReset := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	updatedAt := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	account := &Account{Reset7dAt: oldReset, Window7dSeconds: 604800}

	account.SetUsageSnapshot7dAt(42, time.Time{}, 0, updatedAt)
	got := account.BillingWindowSnapshot()
	if !got.Reset7dAt.Equal(oldReset) || got.Window7dSeconds != 604800 {
		t.Fatalf("partial SetUsageSnapshot7dAt reset metadata = %+v, want preserved reset/duration", got)
	}
	if pct, ok := account.GetUsagePercent7d(); !ok || pct != 42 || !account.GetUsageUpdatedAt().Equal(updatedAt) {
		t.Fatalf("partial SetUsageSnapshot7dAt usage = (%v, %v, %v), want 42/true/%v", pct, ok, account.GetUsageUpdatedAt(), updatedAt)
	}

	account.SetUsageSnapshot7dAt(43, time.Time{}, 2_592_000, updatedAt.Add(30*time.Second))
	got = account.BillingWindowSnapshot()
	if !got.Reset7dAt.Equal(oldReset) || got.Window7dSeconds != 604800 {
		t.Fatalf("duration-only SetUsageSnapshot7dAt reset metadata = %+v, want preserved pair", got)
	}

	account.SetUsageSnapshot7dAt(44, oldReset.Add(24*time.Hour), 0, updatedAt.Add(45*time.Second))
	got = account.BillingWindowSnapshot()
	if !got.Reset7dAt.Equal(oldReset) || got.Window7dSeconds != 604800 {
		t.Fatalf("reset-only SetUsageSnapshot7dAt reset metadata = %+v, want preserved pair", got)
	}

	newReset := oldReset.Add(7 * 24 * time.Hour)
	account.SetUsageSnapshot7dAt(7, newReset, 2_592_000, updatedAt.Add(time.Minute))
	got = account.BillingWindowSnapshot()
	if !got.Reset7dAt.Equal(newReset) || got.Window7dSeconds != 2_592_000 {
		t.Fatalf("complete SetUsageSnapshot7dAt reset metadata = %+v", got)
	}
}

func TestBillingWindowSnapshotNeverObservesMixedLongWindowWrite(t *testing.T) {
	resetA := time.Date(2026, time.September, 7, 12, 0, 0, 0, time.UTC)
	resetB := time.Date(2026, time.October, 2, 12, 0, 0, 0, time.UTC)
	const durationA int64 = 604800
	const durationB int64 = 2_592_000
	account := &Account{}
	account.SetUsageSnapshot7dAt(1, resetA, durationA, time.Now())

	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 20_000; i++ {
			if i%2 == 0 {
				account.SetUsageSnapshot7dAt(1, resetB, durationB, time.Now())
			} else {
				account.SetUsageSnapshot7dAt(1, resetA, durationA, time.Now())
			}
		}
	}()

	close(start)
	for i := 0; i < 20_000; i++ {
		snapshot := account.BillingWindowSnapshot()
		coherentA := snapshot.Reset7dAt.Equal(resetA) && snapshot.Window7dSeconds == durationA
		coherentB := snapshot.Reset7dAt.Equal(resetB) && snapshot.Window7dSeconds == durationB
		if !coherentA && !coherentB {
			t.Fatalf("observed mixed long-window snapshot: %+v", snapshot)
		}
	}
	wg.Wait()
}
