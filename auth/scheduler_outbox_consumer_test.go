package auth

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func waitForSchedulerProjection(t *testing.T, predicate func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if predicate() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("scheduler projection did not converge before timeout")
}

func TestSchedulerOutboxConsumerLoadsUpdatesAndRemovesAccount(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduler-consumer.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	store := NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:  1,
		SchedulerEngine: "indexed",
	})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "outbox-account", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://outbox.example",
		"api_key":       "sk-outbox",
		"models":        []string{"gpt-5.6"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	waitForSchedulerProjection(t, func() bool { return store.FindByID(accountID) != nil })

	selected := store.Next()
	if selected == nil || selected.ID() != accountID {
		t.Fatalf("Next() = %v, want account %d", selected, accountID)
	}
	store.Release(selected)

	if err := db.SetAccountEnabled(ctx, accountID, false); err != nil {
		t.Fatalf("SetAccountEnabled(false): %v", err)
	}
	waitForSchedulerProjection(t, func() bool {
		acc := store.FindByID(accountID)
		return acc != nil && atomic.LoadInt32(&acc.DispatchPaused) != 0
	})
	if selected := store.Next(); selected != nil {
		store.Release(selected)
		t.Fatalf("Next() selected disabled account %d", selected.ID())
	}

	if err := db.SoftDeleteAccount(ctx, accountID); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}
	waitForSchedulerProjection(t, func() bool { return store.FindByID(accountID) == nil })

	metrics := store.GetSchedulerMetrics()
	if metrics.OutboxEvents < 3 || metrics.OutboxErrors != 0 {
		t.Fatalf("outbox metrics = %+v, want >=3 events and no errors", metrics)
	}
}

func TestIndexedAvailabilityWaitWakesOnOutboxAccountInsert(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduler-wait.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "indexed"})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}

	result := make(chan *Account, 1)
	go func() {
		acc, _ := store.WaitForSessionAvailableWithDispatch(ctx, "", 10*time.Second, 0, nil, nil, DispatchPolicyStandard)
		result <- acc
	}()
	waitForSchedulerProjection(t, func() bool { return store.GetSchedulerMetrics().Waiters == 1 })

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "wake-account", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://wake.example",
		"api_key":       "sk-wake",
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	select {
	case acc := <-result:
		if acc == nil || acc.ID() != accountID {
			t.Fatalf("wait result = %v, want account %d", acc, accountID)
		}
		store.Release(acc)
	case <-time.After(11 * time.Second):
		t.Fatal("availability wait was not woken by outbox account insert")
	}
}

func TestIsSchedulerOutboxTerminalError(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name     string
		ctx      context.Context
		err      error
		terminal bool
	}{
		{"nil error", context.Background(), nil, false},
		{"consumer ctx canceled", canceled, context.Canceled, true},
		// 子调用自带超时(如 ReloadProxyPool 5s)不能杀死消费者。
		{"sub-call deadline", context.Background(), context.DeadlineExceeded, false},
		{"conn done is transient", context.Background(), sql.ErrConnDone, false},
		{"database closed", context.Background(), errWrap("sql: database is closed"), true},
		{"plain network error", context.Background(), errWrap("read tcp: connection reset"), false},
	}
	for _, tc := range cases {
		if got := isSchedulerOutboxTerminalError(tc.ctx, tc.err); got != tc.terminal {
			t.Errorf("%s: terminal = %v, want %v", tc.name, got, tc.terminal)
		}
	}
}

func errWrap(message string) error {
	return fmt.Errorf("%s", message)
}

func TestSchedulerOutboxCursorHoleTracking(t *testing.T) {
	now := time.Now()
	cursor := &schedulerOutboxCursor{watermark: 10, holes: make(map[int64]time.Time)}
	events := []database.SchedulerOutboxEvent{{ID: 11}, {ID: 14}, {ID: 15}}
	cursor.noteHoles(events, now)
	if len(cursor.holes) != 2 {
		t.Fatalf("holes = %v, want ids 12 and 13", cursor.holes)
	}
	for _, id := range []int64{12, 13} {
		if _, ok := cursor.holes[id]; !ok {
			t.Fatalf("hole %d not tracked: %v", id, cursor.holes)
		}
	}

	due := cursor.dueHoleIDs(now)
	if len(due) != 2 {
		t.Fatalf("due holes = %v, want 2", due)
	}
	// 超过宽限期的空洞视为已回滚的事务,自动放弃。
	expired := cursor.dueHoleIDs(now.Add(schedulerOutboxHoleGrace + time.Minute))
	if len(expired) != 0 || len(cursor.holes) != 0 {
		t.Fatalf("expired holes should be dropped, got due=%v holes=%v", expired, cursor.holes)
	}
}

func TestApplyPersistentAccountSnapshotRoutingInvalidationGate(t *testing.T) {
	accounts := sparseRoutingAccounts(16, 9)
	store := newIndexedRoutingTestStore(accounts)
	store.SetAPIKeyAllowedGroups(21, []int64{9})
	if acc := store.NextExcluding(21, nil); acc != nil {
		store.Release(acc)
	}
	baseline := store.GetSchedulerMetrics().RoutingCacheInvalidations

	dst := accounts[len(accounts)-1]
	statusOnly := newFastSchedulerTestAccount(dst.DBID, HealthTierHealthy, 100, 1)
	statusOnly.GroupIDs = cloneInt64Slice(dst.GroupIDs)
	statusOnly.Status = StatusCooldown
	store.applyPersistentAccountSnapshot(dst, statusOnly, true)
	if got := store.GetSchedulerMetrics().RoutingCacheInvalidations; got != baseline {
		t.Fatalf("status-only snapshot invalidated routing cache: %d -> %d", baseline, got)
	}

	moved := newFastSchedulerTestAccount(dst.DBID, HealthTierHealthy, 100, 1)
	moved.GroupIDs = []int64{1}
	store.applyPersistentAccountSnapshot(dst, moved, true)
	if got := store.GetSchedulerMetrics().RoutingCacheInvalidations; got <= baseline {
		t.Fatalf("membership snapshot did not invalidate routing cache (still %d)", got)
	}
}

func TestApplyPersistentAccountSnapshotPreservesRuntimeState(t *testing.T) {
	store := newIndexedRoutingTestStore(nil)
	dst := newFastSchedulerTestAccount(1, HealthTierWarm, 100, 1)
	dst.usageObservedAt = time.Now()
	atomic.StoreInt64(&dst.ActiveRequests, 3)
	dst.SuccessStreak = 5
	src := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 1)
	src.CredentialGeneration = dst.CredentialGeneration

	store.applyPersistentAccountSnapshot(dst, src, true)
	if atomic.LoadInt64(&dst.ActiveRequests) != 3 || dst.SuccessStreak != 5 {
		t.Fatalf("runtime state clobbered: active=%d streak=%d", atomic.LoadInt64(&dst.ActiveRequests), dst.SuccessStreak)
	}
	if dst.usageObservedAt.IsZero() {
		t.Fatal("persistent snapshot should not erase a newer runtime observation timestamp")
	}

	rotated := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 1)
	rotated.CredentialGeneration = dst.CredentialGeneration + 1
	rotated.SuccessStreak = 9
	store.applyPersistentAccountSnapshot(dst, rotated, true)
	if dst.SuccessStreak != 0 {
		t.Fatalf("identity change should reset streaks, got %d", dst.SuccessStreak)
	}
}

func TestReloadDispatchAccountsByIDsAppliesBatchProjection(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "scheduler-batch-reload.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1, SchedulerEngine: "indexed"})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})

	groupID, err := db.CreateAccountGroup(ctx, "outbox-batch", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("CreateAccountGroup: %v", err)
	}
	firstID, err := db.InsertOpenAIResponsesAccount(ctx, "batch-first", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://batch-first.example",
		"api_key":       "sk-batch-first",
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount(first): %v", err)
	}
	secondID, err := db.InsertOpenAIResponsesAccount(ctx, "batch-second", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://batch-second.example",
		"api_key":       "sk-batch-second",
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount(second): %v", err)
	}
	if err := db.SetAccountGroups(ctx, firstID, []int64{groupID}); err != nil {
		t.Fatalf("SetAccountGroups: %v", err)
	}
	if err := db.SetModelCooldown(ctx, firstID, "gpt-5.6", "rate_limited", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SetModelCooldown: %v", err)
	}

	if err := store.reloadDispatchAccountsByIDs(ctx, []int64{secondID, firstID, firstID, 0}); err != nil {
		t.Fatalf("reloadDispatchAccountsByIDs(initial): %v", err)
	}
	first := store.FindByID(firstID)
	if first == nil || store.FindByID(secondID) == nil {
		t.Fatalf("batch reload did not add both accounts: first=%v second=%v", first, store.FindByID(secondID))
	}
	first.mu.Lock()
	groups := cloneInt64Slice(first.GroupIDs)
	_, hasCooldown := first.ModelCooldowns["gpt-5.6"]
	first.mu.Unlock()
	if len(groups) != 1 || groups[0] != groupID || !hasCooldown {
		t.Fatalf("first projection groups=%v cooldown=%v, want group %d and active cooldown", groups, hasCooldown, groupID)
	}

	if err := db.SetAccountEnabled(ctx, firstID, false); err != nil {
		t.Fatalf("SetAccountEnabled: %v", err)
	}
	if err := db.SoftDeleteAccount(ctx, secondID); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}
	if err := store.reloadDispatchAccountsByIDs(ctx, []int64{firstID, secondID}); err != nil {
		t.Fatalf("reloadDispatchAccountsByIDs(update/delete): %v", err)
	}
	if atomic.LoadInt32(&first.DispatchPaused) == 0 {
		t.Fatal("disabled account remained dispatchable after batch reload")
	}
	if store.FindByID(secondID) != nil {
		t.Fatal("deleted account remained in the runtime pool after batch reload")
	}
}
