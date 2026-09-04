package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

// A threshold can be changed while the indexed scheduler is live. Accounts
// that were excluded at insertion time must be reinserted when the threshold
// is relaxed; otherwise a subsequent fresh request can incorrectly report an
// empty pool even though the account is healthy again.
func TestGlobalAutoPauseThresholdRefreshesIndexedScheduler(t *testing.T) {
	settings := &database.SystemSettings{
		MaxConcurrency:       1,
		FastSchedulerEnabled: true,
		SchedulerEngine:      "indexed",
	}
	store := NewStore(nil, nil, settings)
	owner := &Account{
		DBID:                1,
		AccessToken:         "owner-token",
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		UsagePercent5h:      99,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(time.Hour),
	}
	fallback := &Account{DBID: 2, AccessToken: "fallback-token", Status: StatusReady, HealthTier: HealthTierHealthy}
	store.AddAccounts([]*Account{owner, fallback})
	store.SetGlobalAutoPauseThresholds(0.95, 0)
	if owner.IsAvailable() {
		t.Fatal("owner should be fenced after enabling the auto-pause threshold")
	}

	if selected := store.NextExcluding(0, map[int64]bool{fallback.DBID: true}); selected != nil {
		store.Release(selected)
		t.Fatalf("auto-paused owner entered the indexed pool before threshold reset: account %d", selected.DBID)
	}

	store.SetGlobalAutoPauseThresholds(0, 0)
	selected := store.NextExcluding(0, map[int64]bool{fallback.DBID: true})
	if selected != owner {
		if selected != nil {
			store.Release(selected)
		}
		t.Fatalf("owner after threshold reset = %#v, want account %d", selected, owner.DBID)
	}
	store.Release(selected)
}
