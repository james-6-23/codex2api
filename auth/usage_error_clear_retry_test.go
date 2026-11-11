package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func TestLostErrorClearIsRetriedByUsageFrame(t *testing.T) {
	for _, newerStatus := range []AccountStatus{StatusReady, StatusError, StatusCooldown} {
		t.Run(map[AccountStatus]string{StatusReady: "ready", StatusError: "new_error", StatusCooldown: "new_cooldown"}[newerStatus], func(t *testing.T) {
			testLostErrorClearRetry(t, newerStatus)
		})
	}
}

func testLostErrorClearRetry(t *testing.T, newerStatus AccountStatus) {
	t.Helper()
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "error-clear.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tokenCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = tokenCache.Close() })
	store := NewStore(db, tokenCache, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	id, err := db.InsertAccountWithCredentials(ctx, "error-clear", map[string]interface{}{"access_token": "test-token", "plan_type": "plus"}, "")
	if err != nil {
		t.Fatal(err)
	}
	acc := &Account{DBID: id, AccessToken: "test-token", PlanType: "plus"}
	acc.recomputeEffectiveIgnoreUsageLimitStatus(false)
	store.AddAccounts([]*Account{acc})
	store.MarkError(acc, "old authentication error")

	broken, err := database.New("sqlite", filepath.Join(t.TempDir(), "broken.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}
	store.db = broken
	store.ClearCooldown(acc)
	store.db = db
	acc.mu.RLock()
	pending := acc.usageCooldownPersistPending
	acc.mu.RUnlock()
	if !pending {
		t.Fatal("failed error clear must remain pending")
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.ErrorMessage != "old authentication error" {
		t.Fatalf("precondition: persisted error was not retained: %q", row.ErrorMessage)
	}

	// Model a newer runtime transition before its publisher acquires the
	// persistence lock. Retrying the old clear must publish this current state.
	until := time.Now().Add(time.Hour).Round(time.Second)
	acc.mu.Lock()
	acc.Status = newerStatus
	if newerStatus != StatusReady {
		acc.ErrorMsg = "new failure"
	}
	if newerStatus == StatusCooldown {
		acc.CooldownReason = "rate_limited"
		acc.CooldownUtil = until
	}
	acc.mu.Unlock()
	store.usageFramePersist.persist(store, usageFramePersistJob{
		account: acc, dbID: id, observedAt: time.Now(),
	})
	row, err = db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	wantError := ""
	if newerStatus != StatusReady {
		wantError = "new failure"
	}
	if row.ErrorMessage != wantError {
		t.Fatalf("retry persisted error %q, want %q", row.ErrorMessage, wantError)
	}
	if newerStatus == StatusCooldown && (!row.CooldownUntil.Valid || !row.CooldownUntil.Time.Equal(until) || row.CooldownReason != "rate_limited") {
		t.Fatalf("retry lost newer cooldown: %+v", row)
	}
	acc.mu.RLock()
	pending = acc.usageCooldownPersistPending
	acc.mu.RUnlock()
	if pending {
		t.Fatal("successful retry must clear pending")
	}
	reloaded := store.buildAccountFromRow(ctx, row, nil)
	if reloaded == nil || reloaded.Status != newerStatus {
		t.Fatalf("account must reload with status %v, got %+v", newerStatus, reloaded)
	}
}
