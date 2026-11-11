package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

// Regression test for issue #813: a runtime usage-limit cooldown clear whose
// DB write fails must not leave the stale cooldown in the database. A later
// scheduler-outbox reload (e.g. PATCH /api/admin/accounts/:id/credit) builds a
// snapshot from that stale row and resurrects the old 7d cooldown, so a
// healthy account keeps returning 429 until a manual status reset.
func TestLostUsageCooldownClearIsRetried(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage-cooldown-clear.db")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tokenCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = tokenCache.Close() })
	store := NewStore(db, tokenCache, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)

	id, err := db.InsertAccountWithCredentials(ctx, "clear-retry", map[string]interface{}{"access_token": "test-token", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}

	acc := &Account{DBID: id, AccessToken: "test-token", PlanType: "plus"}
	acc.recomputeEffectiveIgnoreUsageLimitStatus(false)
	store.AddAccounts([]*Account{acc})

	until := time.Now().Add(7 * 24 * time.Hour).Round(time.Second)
	// Inject a lost clear: point the store at a broken database so the clear's
	// write fails, then perform the runtime-only clear the WHAM probe does.
	probeStartedAt := time.Now()
	acc.mu.Lock()
	acc.LastRateLimitedAt = time.Now().Add(-time.Minute)
	acc.setCooldownUntilLocked(until, "rate_limited_7d")
	acc.usageCooldownPersistPending = true
	acc.mu.Unlock()
	store.persistUsageCooldown(acc)

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !row.CooldownUntil.Valid || row.CooldownReason != "rate_limited_7d" {
		t.Fatalf("cooldown should be persisted before the incident: reason=%q until valid=%v", row.CooldownReason, row.CooldownUntil.Valid)
	}

	// Inject a lost clear: point the store at a broken database so the clear's
	// write fails, then perform the runtime-only clear the WHAM probe does.
	broken, err := database.New("sqlite", filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}
	realDB := store.db
	store.db = broken
	if !store.ClearUsageLimitCooldownSince(acc, probeStartedAt) {
		t.Fatal("runtime cooldown should have been cleared")
	}
	store.db = realDB

	// The stale cooldown must still sit in the database after the failed
	// persist; the fix marks the write pending.
	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !row.CooldownUntil.Valid {
		t.Fatal("precondition: the clear write was lost, DB must still hold the cooldown")
	}

	// The next usage-frame publication retries the lost clear.
	acc.mu.Lock()
	acc.usageCooldownPersistPending = true
	acc.mu.Unlock()
	store.persistUsageCooldown(acc)

	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CooldownUntil.Valid || row.CooldownReason != "" {
		t.Fatalf("stale cooldown must be cleared after retry: reason=%q until=%v", row.CooldownReason, row.CooldownUntil)
	}

	// The account must be dispatchable again after a snapshot reload.
	reloaded := store.buildAccountFromRow(ctx, row, nil)
	if reloaded == nil {
		t.Fatal("reload failed")
	}
	if reloaded.Status != StatusReady {
		t.Fatalf("reloaded account must be ready, got status=%v reason=%q", reloaded.Status, reloaded.CooldownReason)
	}
}
