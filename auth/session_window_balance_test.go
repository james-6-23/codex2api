package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func newSessionWindowBalanceStore(t *testing.T, accounts ...*Account) *Store {
	t.Helper()
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 8, TestConcurrency: 1, TestModel: "gpt-5.4",
	})
	for _, account := range accounts {
		account.SessionCapacityEnabled = true
		account.SessionCapacityMax = 10
		account.SessionCapacityIdleTTLSeconds = 3600
		store.AddAccount(account)
	}
	store.SetSessionWindowBalanceEnabled(true)
	return store
}

func admitWindow(t *testing.T, store *Store, account *Account, key string) {
	t.Helper()
	if !store.AdmitAccountSession(account, key, time.Now()) {
		t.Fatalf("failed to admit %s to account %d", key, account.DBID)
	}
}

func TestSessionWindowBalanceDefaultsOff(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	if store.SessionWindowBalanceEnabled() {
		t.Fatal("session window balance should default to disabled")
	}
}

func TestFreshSessionPrefersLowerWindowCountBeforeScore(t *testing.T) {
	most := newFastSchedulerTestAccount(1, HealthTierHealthy, 120, 8)
	middle := newFastSchedulerTestAccount(2, HealthTierHealthy, 100, 8)
	least := newFastSchedulerTestAccount(3, HealthTierHealthy, 80, 8)
	// Keep it healthy but lower its recomputed dispatch score, proving that
	// window count is compared before score within the same health tier.
	least.LastServerErrorAt = time.Now()
	store := newSessionWindowBalanceStore(t, most, middle, least)
	admitWindow(t, store, most, "most-a")
	admitWindow(t, store, most, "most-b")
	admitWindow(t, store, middle, "middle-a")

	got := store.nextAccountForFreshAffinity("fresh-window", 0, nil, nil)
	if got == nil {
		t.Fatal("no account selected")
	}
	defer store.Release(got)
	if got.DBID != least.DBID {
		t.Fatalf("selected account %d, want least-window account %d", got.DBID, least.DBID)
	}
}

func TestFreshSessionBalanceKeepsHealthTierAheadOfWindowCount(t *testing.T) {
	healthy := newFastSchedulerTestAccount(1, HealthTierHealthy, 80, 8)
	warm := newFastSchedulerTestAccount(2, HealthTierWarm, 120, 8)
	warm.LastTimeoutAt = time.Now()
	store := newSessionWindowBalanceStore(t, healthy, warm)
	admitWindow(t, store, healthy, "healthy-a")
	admitWindow(t, store, healthy, "healthy-b")

	got := store.nextAccountForFreshAffinity("fresh-window", 0, nil, nil)
	if got == nil {
		t.Fatal("no account selected")
	}
	defer store.Release(got)
	if got.DBID != healthy.DBID {
		t.Fatalf("selected account %d, want healthy account %d", got.DBID, healthy.DBID)
	}
}

func TestFreshSessionBalanceKeepsSchedulerPriorityAheadOfWindowCount(t *testing.T) {
	highPriority := newFastSchedulerTestAccount(1, HealthTierHealthy, 80, 8)
	highPriority.SetSchedulerPriority(10)
	lowPriority := newFastSchedulerTestAccount(2, HealthTierHealthy, 120, 8)
	store := newSessionWindowBalanceStore(t, highPriority, lowPriority)
	admitWindow(t, store, highPriority, "priority-a")
	admitWindow(t, store, highPriority, "priority-b")

	got := store.nextAccountForFreshAffinity("fresh-window", 0, nil, nil)
	if got == nil {
		t.Fatal("no account selected")
	}
	defer store.Release(got)
	if got.DBID != highPriority.DBID {
		t.Fatalf("selected account %d, want high-priority account %d", got.DBID, highPriority.DBID)
	}
}

func TestSessionWindowBalanceDoesNotMoveExistingSession(t *testing.T) {
	first := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 8)
	second := newFastSchedulerTestAccount(2, HealthTierHealthy, 90, 8)
	store := newSessionWindowBalanceStore(t, first, second)

	selected, _ := store.NextForSession("existing-window", 0, nil)
	if selected == nil {
		t.Fatal("initial selection returned nil")
	}
	store.BindSessionAffinity("existing-window", selected, "")
	selectedID := selected.DBID
	store.Release(selected)

	reused, _ := store.NextForSession("existing-window", 0, nil)
	if reused == nil {
		t.Fatal("existing session returned nil")
	}
	defer store.Release(reused)
	if reused.DBID != selectedID {
		t.Fatalf("existing session moved from %d to %d", selectedID, reused.DBID)
	}
}
