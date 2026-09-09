package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

type recordingRuntimeTTLCache struct {
	cache.TokenCache
	mu   sync.Mutex
	ttls map[string]time.Duration
}

func (c *recordingRuntimeTTLCache) SetRuntime(ctx context.Context, namespace, key string, value json.RawMessage, ttl time.Duration) error {
	c.mu.Lock()
	if c.ttls == nil {
		c.ttls = make(map[string]time.Duration)
	}
	c.ttls[namespace+"\x00"+key] = ttl
	c.mu.Unlock()
	return c.TokenCache.SetRuntime(ctx, namespace, key, value, ttl)
}

func (c *recordingRuntimeTTLCache) runtimeTTL(namespace, key string) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ttls[namespace+"\x00"+key]
}

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

func TestApplyPersistentAccountSnapshotCopiesProviderPersistentState(t *testing.T) {
	dst := newFastSchedulerTestAccount(1, HealthTierWarm, 100, 1)
	dst.CredentialGeneration = 7
	dst.AntigravityProjectID = "old-project"
	dst.PermanentRefreshFailures = 2
	dst.ClaudeClientPlatformOverride = string(ClaudeClientPlatformAny)
	dst.ClaudeVersionPolicyOverride = string(ClaudeVersionPolicyPassthrough)
	store := newIndexedRoutingTestStore([]*Account{dst})

	src := newFastSchedulerTestAccount(1, HealthTierRisky, 100, 1)
	src.CredentialGeneration = dst.CredentialGeneration
	src.AntigravityProjectID = "new-project"
	src.AntigravityHardBlocked = true
	src.AntigravityHardBlockReason = "permanent refresh failure"
	src.PermanentRefreshFailures = permanentRefreshFailureTerminalLimit
	src.ClaudeClientPlatformOverride = string(ClaudeClientPlatformCLIOnly)
	src.ClaudeVersionPolicyOverride = string(ClaudeVersionPolicyMinimum)
	src.ClaudeClientVersionOverride = "1.2.3"
	store.applyPersistentAccountSnapshot(dst, src, true)

	dst.mu.RLock()
	if dst.AntigravityProjectID != src.AntigravityProjectID ||
		dst.AntigravityHardBlocked != src.AntigravityHardBlocked ||
		dst.AntigravityHardBlockReason != src.AntigravityHardBlockReason {
		t.Fatalf("Antigravity state = project %q hard=%t reason=%q, want project %q hard=%t reason=%q",
			dst.AntigravityProjectID, dst.AntigravityHardBlocked, dst.AntigravityHardBlockReason,
			src.AntigravityProjectID, src.AntigravityHardBlocked, src.AntigravityHardBlockReason)
	}
	if dst.ClaudeClientPlatformOverride != src.ClaudeClientPlatformOverride ||
		dst.ClaudeVersionPolicyOverride != src.ClaudeVersionPolicyOverride ||
		dst.ClaudeClientVersionOverride != src.ClaudeClientVersionOverride {
		t.Fatalf("Claude overrides = %q/%q/%q, want %q/%q/%q",
			dst.ClaudeClientPlatformOverride, dst.ClaudeVersionPolicyOverride, dst.ClaudeClientVersionOverride,
			src.ClaudeClientPlatformOverride, src.ClaudeVersionPolicyOverride, src.ClaudeClientVersionOverride)
	}
	if dst.PermanentRefreshFailures != permanentRefreshFailureTerminalLimit {
		t.Fatalf("persistent permanent failures = %d, want %d", dst.PermanentRefreshFailures, permanentRefreshFailureTerminalLimit)
	}
	dst.mu.RUnlock()

	// A routine snapshot of the same hard-fence state must not erase newer
	// process-local refresh failure history.
	dst.mu.Lock()
	dst.PermanentRefreshFailures = 3
	dst.mu.Unlock()
	stable := newFastSchedulerTestAccount(1, HealthTierRisky, 100, 1)
	stable.CredentialGeneration = src.CredentialGeneration
	stable.AntigravityHardBlocked = src.AntigravityHardBlocked
	stable.AntigravityHardBlockReason = src.AntigravityHardBlockReason
	store.applyPersistentAccountSnapshot(dst, stable, true)
	dst.mu.RLock()
	if dst.PermanentRefreshFailures != 3 {
		t.Fatalf("routine snapshot clobbered runtime permanent failures: got %d want 3", dst.PermanentRefreshFailures)
	}
	dst.mu.RUnlock()
	durable := newFastSchedulerTestAccount(1, HealthTierRisky, 100, 1)
	durable.CredentialGeneration = stable.CredentialGeneration
	durable.AntigravityHardBlocked = stable.AntigravityHardBlocked
	durable.AntigravityHardBlockReason = stable.AntigravityHardBlockReason
	durable.PermanentRefreshFailures = permanentRefreshFailureTerminalLimit
	store.applyPersistentAccountSnapshot(dst, durable, true)
	dst.mu.RLock()
	if dst.PermanentRefreshFailures != permanentRefreshFailureTerminalLimit {
		t.Fatalf("durable hard fence did not repair runtime failures: got %d want %d", dst.PermanentRefreshFailures, permanentRefreshFailureTerminalLimit)
	}
	dst.mu.RUnlock()

	cleared := newFastSchedulerTestAccount(1, HealthTierHealthy, 100, 1)
	cleared.CredentialGeneration = durable.CredentialGeneration
	store.applyPersistentAccountSnapshot(dst, cleared, true)
	dst.mu.RLock()
	if dst.AntigravityHardBlocked || dst.AntigravityHardBlockReason != "" || dst.PermanentRefreshFailures != 0 {
		t.Fatalf("cleared hard fence = hard=%t reason=%q failures=%d",
			dst.AntigravityHardBlocked, dst.AntigravityHardBlockReason, dst.PermanentRefreshFailures)
	}
	dst.mu.RUnlock()

	rotated := newFastSchedulerTestAccount(1, HealthTierRisky, 100, 1)
	rotated.CredentialGeneration = cleared.CredentialGeneration + 1
	rotated.AntigravityHardBlocked = true
	rotated.AntigravityHardBlockReason = "rotated permanent refresh failure"
	rotated.PermanentRefreshFailures = permanentRefreshFailureTerminalLimit
	store.applyPersistentAccountSnapshot(dst, rotated, true)
	dst.mu.RLock()
	if dst.PermanentRefreshFailures != permanentRefreshFailureTerminalLimit {
		t.Fatalf("identity refresh discarded persistent permanent failures: got %d want %d", dst.PermanentRefreshFailures, permanentRefreshFailureTerminalLimit)
	}
	dst.mu.RUnlock()
}

func TestApplyPersistentAccountSnapshotReconcilesSessionCapacity(t *testing.T) {
	t.Run("unregistered account falls back to field copy", func(t *testing.T) {
		store := newIndexedRoutingTestStore(nil)
		dst := newFastSchedulerTestAccount(91, HealthTierHealthy, 100, 1)
		src := newFastSchedulerTestAccount(91, HealthTierHealthy, 100, 1)
		src.SessionCapacityEnabled = true
		src.SessionCapacityMax = 9
		src.SessionCapacityIdleTTLSeconds = 7200

		store.applyPersistentAccountSnapshot(dst, src, true)
		dst.mu.RLock()
		enabled := dst.SessionCapacityEnabled
		limit := dst.SessionCapacityMax
		idleTTLSeconds := dst.SessionCapacityIdleTTLSeconds
		dst.mu.RUnlock()
		if !enabled || limit != 9 || idleTTLSeconds != 7200 {
			t.Fatalf("fallback capacity = enabled=%t limit=%d ttl=%d", enabled, limit, idleTTLSeconds)
		}
	})

	t.Run("disable hydrates and clears persisted windows before provider change", func(t *testing.T) {
		runtimeCache := cache.NewMemory(1)
		t.Cleanup(func() { _ = runtimeCache.Close() })
		settings := &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1}
		firstStore := NewStore(nil, runtimeCache, settings)
		firstAccount := &Account{
			DBID: 92, AccessToken: "first", Status: StatusReady,
			SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
		}
		firstStore.AddAccount(firstAccount)
		const sessionID = "persisted-before-disable"
		if !firstStore.AdmitAccountSession(firstAccount, sessionID, time.Now()) {
			t.Fatal("failed to admit persisted session")
		}
		firstStore.SetAccountSessionOwner(firstAccount.DBID, sessionID, AccountSessionOwner{UserName: "owner"})

		secondStore := NewStore(nil, runtimeCache, settings)
		dst := &Account{
			DBID: 92, AccessToken: "second", Status: StatusReady,
			SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
		}
		secondStore.AddAccount(dst)
		src := &Account{
			DBID: 92, UpstreamType: UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk-relay",
			// A stale persisted true value must not retain windows after this
			// account changes to a relay provider, where capacity is inapplicable.
			SessionCapacityEnabled: true, SessionCapacityMax: 2, SessionCapacityIdleTTLSeconds: 600,
		}
		secondStore.applyPersistentAccountSnapshot(dst, src, true)

		ctx := context.Background()
		if _, found, err := runtimeCache.GetRuntime(ctx, accountSessionRuntimeNamespace, accountSessionRuntimeKey(dst.DBID)); err != nil || found {
			t.Fatalf("persisted account sessions after disable: found=%t err=%v", found, err)
		}
		if _, found, err := runtimeCache.GetRuntime(ctx, accountSessionOwnerRuntimeNamespace, sessionID); err != nil || found {
			t.Fatalf("persisted reverse owner after disable: found=%t err=%v", found, err)
		}
		secondStore.accountSessionMu.Lock()
		remaining := len(secondStore.accountSessions[dst.DBID])
		secondStore.accountSessionMu.Unlock()
		if remaining != 0 {
			t.Fatalf("in-memory sessions after disable = %d, want 0", remaining)
		}
	})

	t.Run("longer ttl refreshes persisted account and owner leases", func(t *testing.T) {
		runtimeCache := &recordingRuntimeTTLCache{TokenCache: cache.NewMemory(1)}
		t.Cleanup(func() { _ = runtimeCache.Close() })
		store := NewStore(nil, runtimeCache, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1})
		dst := &Account{
			DBID: 93, AccessToken: "token", Status: StatusReady,
			SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 60,
		}
		store.AddAccount(dst)
		const sessionID = "ttl-refresh-session"
		if !store.AdmitAccountSession(dst, sessionID, time.Now()) {
			t.Fatal("failed to admit ttl refresh session")
		}
		store.SetAccountSessionOwner(dst.DBID, sessionID, AccountSessionOwner{UserName: "owner"})
		initialTTL := runtimeCache.runtimeTTL(accountSessionRuntimeNamespace, accountSessionRuntimeKey(dst.DBID))

		src := &Account{
			DBID: 93, AccessToken: "token", Status: StatusReady,
			SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityIdleTTLSeconds: 3600,
		}
		store.applyPersistentAccountSnapshot(dst, src, true)

		accountTTL := runtimeCache.runtimeTTL(accountSessionRuntimeNamespace, accountSessionRuntimeKey(dst.DBID))
		ownerTTL := runtimeCache.runtimeTTL(accountSessionOwnerRuntimeNamespace, sessionID)
		if initialTTL <= 0 || accountTTL < 30*time.Minute || ownerTTL < 30*time.Minute {
			t.Fatalf("session TTLs = initial %s account %s owner %s, want refreshed leases", initialTTL, accountTTL, ownerTTL)
		}
		enabled, limit, idleTTL := dst.SessionCapacityConfig()
		if !enabled || limit != 3 || idleTTL != time.Hour {
			t.Fatalf("capacity after ttl refresh = enabled=%t limit=%d ttl=%s", enabled, limit, idleTTL)
		}
	})
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
