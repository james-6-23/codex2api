package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestReconcileDispatchStateLoadsAccountAddedAfterStartup(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "dispatch-reconcile.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:       1,
		FastSchedulerEnabled: true,
	})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	if got := store.Next(); got != nil {
		store.Release(got)
		t.Fatalf("Next() before insert = account %d, want nil", got.ID())
	}

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "new-endpoint", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://healthy.example",
		"api_key":       "sk-new",
		"models":        []string{"gpt-5.6"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}

	_, err = store.ReconcileDispatchState(ctx)
	if err != nil {
		t.Fatalf("ReconcileDispatchState: %v", err)
	}
	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil after dispatch reconciliation")
	}
	defer store.Release(got)
	if got.ID() != accountID {
		t.Fatalf("Next() = account %d, want newly added account %d", got.ID(), accountID)
	}
}

func TestTriggerDispatchStateReconcileAsyncLoadsAccount(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "dispatch-reconcile-async.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:       1,
		FastSchedulerEnabled: true,
	})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}

	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "async-endpoint", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://healthy.example",
		"api_key":       "sk-async",
		"models":        []string{"gpt-5.6"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}

	done := store.TriggerDispatchStateReconcileAsync()
	if done == nil {
		t.Fatal("TriggerDispatchStateReconcileAsync() = nil")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("asynchronous dispatch reconciliation did not finish")
	}
	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil after asynchronous dispatch reconciliation")
	}
	defer store.Release(got)
	if got.ID() != accountID {
		t.Fatalf("Next() = account %d, want newly added account %d", got.ID(), accountID)
	}
}

func TestReconcileDispatchStateDoesNotQueueBehindAnotherRun(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "dispatch-reconcile-singleflight.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	done, owner := store.beginDispatchStateReconcile()
	if !owner {
		t.Fatal("beginDispatchStateReconcile() owner = false, want true")
	}
	defer store.finishDispatchStateReconcile(done)

	started := time.Now()
	changed, err := store.ReconcileDispatchState(ctx)
	if err != nil {
		t.Fatalf("ReconcileDispatchState: %v", err)
	}
	if changed {
		t.Fatal("ReconcileDispatchState reported a change while another run owned the lock")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("ReconcileDispatchState queued behind another run for %s", elapsed)
	}
}

func TestAsyncReconcileCoalescesOntoActiveRunCompletion(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "dispatch-reconcile-interleaving.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:       1,
		FastSchedulerEnabled: true,
	})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}
	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "interleaved-endpoint", map[string]interface{}{
		"upstream_type": UpstreamOpenAIResponses,
		"base_url":      "https://healthy.example",
		"api_key":       "sk-interleaved",
		"models":        []string{"gpt-5.6"},
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}

	activeDone, owner := store.beginDispatchStateReconcile()
	if !owner {
		t.Fatal("beginDispatchStateReconcile() owner = false, want true")
	}
	asyncDone := store.TriggerDispatchStateReconcileAsync()
	if asyncDone != activeDone {
		t.Fatal("async trigger did not coalesce onto the active reconciliation")
	}
	select {
	case <-asyncDone:
		t.Fatal("active reconciliation completion closed before the active run finished")
	default:
	}

	_, err = store.reconcileDispatchState(ctx)
	if err != nil {
		t.Fatalf("reconcileDispatchState: %v", err)
	}
	store.finishDispatchStateReconcile(activeDone)
	select {
	case <-asyncDone:
	case <-time.After(time.Second):
		t.Fatal("coalesced completion did not close after the active run finished")
	}

	got := store.Next()
	if got == nil {
		t.Fatal("Next() returned nil after the active reconciliation completed")
	}
	defer store.Release(got)
	if got.ID() != accountID {
		t.Fatalf("Next() = account %d, want newly added account %d", got.ID(), accountID)
	}
}

func TestTriggerDispatchStateReconcileAsyncThrottledReturnsNil(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "dispatch-reconcile-throttle.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}

	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(func() {
		store.Stop()
		_ = db.Close()
	})
	if err := store.Init(ctx); err != nil {
		t.Fatalf("Store.Init: %v", err)
	}

	if _, err := store.ReconcileDispatchState(ctx); err != nil {
		t.Fatalf("ReconcileDispatchState: %v", err)
	}

	// Inside the throttle window a new run would be a guaranteed no-op, so the
	// trigger must return nil instead of an instantly-closed channel — callers
	// use nil to skip their grace wait entirely.
	if done := store.TriggerDispatchStateReconcileAsync(); done != nil {
		t.Fatal("TriggerDispatchStateReconcileAsync() inside throttle window != nil")
	}

	// The throttled trigger must release ownership so a later run can start.
	done, owner := store.beginDispatchStateReconcile()
	if !owner {
		t.Fatal("beginDispatchStateReconcile() after throttled trigger: ownership not released")
	}
	store.finishDispatchStateReconcile(done)
}
