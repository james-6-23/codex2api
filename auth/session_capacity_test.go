package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

type failOnceAccountSessionRuntimeCache struct {
	cache.TokenCache
	failed atomic.Bool
}

func (c *failOnceAccountSessionRuntimeCache) SetRuntime(ctx context.Context, namespace, key string, value json.RawMessage, ttl time.Duration) error {
	if namespace == accountSessionRuntimeNamespace && !c.failed.Swap(true) {
		return errors.New("forced account session cache failure")
	}
	return c.TokenCache.SetRuntime(ctx, namespace, key, value, ttl)
}

func newSessionCapacityTestStore(limit int64) (*Store, *Account) {
	account := &Account{
		DBID:                          1,
		AccessToken:                   "token",
		SessionCapacityEnabled:        true,
		SessionCapacityMax:            limit,
		SessionCapacityIdleTTLSeconds: 60,
	}
	return &Store{
		accounts:        []*Account{account},
		maxConcurrency:  256,
		accountSessions: make(map[int64]map[string]*accountSessionState),
		sessionBindings: make(map[string]sessionAffinity),
	}, account
}

func TestAccountSessionCapacityConcurrentAdmissionIsAtomic(t *testing.T) {
	store, account := newSessionCapacityTestStore(5)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			selected, _ := store.NextForSession(fmt.Sprintf("session-%d", i), 0, nil)
			if selected != nil {
				admitted.Add(1)
				store.Release(selected)
			}
		}(i)
	}
	wg.Wait()
	if got := admitted.Load(); got != 5 {
		t.Fatalf("admitted = %d, want 5", got)
	}
	if got := store.AccountSessionCount(account.DBID, time.Now()); got != 5 {
		t.Fatalf("active session count = %d, want 5", got)
	}
}

func TestAccountSessionCapacityAllowsExistingConversationWhenFull(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	first, _ := store.NextForSession("existing", 0, nil)
	if first != account {
		t.Fatal("first session was not admitted")
	}
	store.Release(first)
	if fresh, _ := store.NextForSession("fresh", 0, nil); fresh != nil {
		store.Release(fresh)
		t.Fatal("fresh session bypassed full capacity")
	}
	again, _ := store.NextForSession("existing", 0, nil)
	if again != account {
		t.Fatal("existing session could not continue at full capacity")
	}
	store.Release(again)
}

func TestAccountSessionCapacityReleasesIdleConversation(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	now := time.Now()
	if !store.AdmitAccountSession(account, "expired", now) {
		t.Fatal("initial admission failed")
	}
	store.accountSessionMu.Lock()
	store.accountSessions[account.DBID]["expired"].lastSeen = now.Add(-2 * time.Minute)
	store.accountSessionMu.Unlock()
	if !store.AdmitAccountSession(account, "replacement", now) {
		t.Fatal("expired slot was not released")
	}
}

func TestUnstableIdentityDoesNotConsumeAccountSessionCapacity(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	for i := 0; i < 10; i++ {
		if !store.AdmitAccountSession(account, UnstableSessionCapacityPrefix+fmt.Sprint(i), time.Now()) {
			t.Fatal("unstable identity should not be capacity limited")
		}
	}
	if got := store.AccountSessionCount(account.DBID, time.Now()); got != 0 {
		t.Fatalf("unstable identity consumed %d slots", got)
	}
}

func TestAuthenticatedSessionAccountingBypassKeepsAffinityWithoutConsumingWindow(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	if !store.AdmitAccountSession(account, "visible-window", time.Now()) {
		t.Fatal("failed to fill visible account window")
	}
	bypassKey := SessionAccountingBypassAffinityKey("ambient-session::api-key:7")
	selected, _ := store.NextForSession(bypassKey, 0, nil)
	if selected != account {
		t.Fatal("background request was blocked by visible session capacity")
	}
	store.BindSessionAffinity(bypassKey, selected, "")
	store.Release(selected)
	if got := store.AccountSessionCount(account.DBID, time.Now()); got != 1 {
		t.Fatalf("background request changed visible account windows to %d", got)
	}
	if _, found := store.SessionAffinityAccountID(bypassKey); !found {
		t.Fatal("background request lost normal affinity binding")
	}
	if store.HasSessionCapacityExhaustionWithDispatch(0, nil, nil, DispatchPolicyStandard, bypassKey, time.Now()) {
		t.Fatal("background request was reported as account session exhaustion")
	}
}

func TestAuthenticatedSessionAccountingBypassIsExcludedFromWindowBalance(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	account.SessionCapacityEnabled = false
	bypassKey := SessionAccountingBypassAffinityKey("ambient-session::api-key:7")
	store.BindSessionAffinity(bypassKey, account, "")
	if counts := store.accountWindowCountsForScheduling([]*Account{account}, time.Now()); counts[account.DBID] != 0 {
		t.Fatalf("background affinity entered window balancing: %#v", counts)
	}
}

func TestClientCannotForgeSessionAccountingBypassPrefix(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	if !store.AdmitAccountSession(account, "visible-window", time.Now()) {
		t.Fatal("failed to fill visible account window")
	}
	if store.AdmitAccountSession(account, "non-accounting-affinity:client-forged", time.Now()) {
		t.Fatal("client-controlled session id forged the private accounting bypass")
	}
}

func TestRelatedRequestRefreshesRootOnlyAfterDispatchWithoutCreatingWindow(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	now := time.Now()
	rootKey := "root-window::api-key:7"
	if !store.AdmitAccountSession(account, rootKey, now) {
		t.Fatal("root session admission failed")
	}
	store.BindSessionAffinity(rootKey, account, "")
	store.accountSessionMu.Lock()
	originalLastSeen := now.Add(-30 * time.Second)
	store.accountSessions[account.DBID][rootKey].lastSeen = originalLastSeen
	store.accountSessionMu.Unlock()

	relatedKey := RelatedSessionAffinityKey(rootKey)
	selected, _ := store.NextForSession(relatedKey, 0, nil)
	if selected != account {
		t.Fatal("related request did not borrow the root account")
	}
	store.BindSessionAffinity(relatedKey, selected, "")
	store.Release(selected)

	snapshots := store.AccountSessionSnapshots(account.DBID, now)
	if len(snapshots) != 1 || snapshots[0].SessionID != rootKey {
		t.Fatalf("related request changed window set: %#v", snapshots)
	}
	if !snapshots[0].LastSeen.Equal(originalLastSeen) {
		t.Fatalf("related selection refreshed root before dispatch: got %s want %s", snapshots[0].LastSeen, originalLastSeen)
	}

	store.RecordRelatedAccountSession(account.DBID, relatedKey, AccountSessionRelatedSource{
		ThreadSource: "subagent", RequestKind: "turn", SubagentKind: "thread_spawn",
	}, "related-request-1")
	refreshed := store.AccountSessionSnapshots(account.DBID, time.Now())
	if len(refreshed) != 1 || refreshed[0].SessionID != rootKey {
		t.Fatalf("related dispatch changed window set: %#v", refreshed)
	}
	if !refreshed[0].LastSeen.After(originalLastSeen) {
		t.Fatalf("related dispatch did not refresh root last_seen: got %s want after %s", refreshed[0].LastSeen, originalLastSeen)
	}
}

func TestAccountSessionStateRestoresFromRuntimeCacheAfterRestart(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	settings := &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1}
	firstStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(firstStore.Stop)
	firstAccount := &Account{
		DBID: 11, AccessToken: "first", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
	}
	firstStore.AddAccount(firstAccount)
	rootKey := "persisted-root::api-key:7"
	if !firstStore.AdmitAccountSession(firstAccount, rootKey, time.Now()) {
		t.Fatal("initial root admission failed")
	}
	firstStore.SetAccountSessionOwner(firstAccount.DBID, rootKey, AccountSessionOwner{
		Platform: "newapi", UserID: "42", UserName: "Arun", APIKeyID: 7, APIKeyName: "pro",
	})
	firstStore.RecordRelatedAccountSession(firstAccount.DBID, RelatedSessionAffinityKey(rootKey), AccountSessionRelatedSource{
		ThreadSource: "subagent", RequestKind: "turn", SubagentKind: "guardian",
	}, "guardian-request-1")
	before := firstStore.AccountSessionSnapshots(firstAccount.DBID, time.Now())
	if len(before) != 1 {
		t.Fatalf("initial snapshots = %#v", before)
	}

	secondStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(secondStore.Stop)
	secondAccount := &Account{
		DBID: 11, AccessToken: "second", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
	}
	secondStore.AddAccount(secondAccount)
	if secondStore.AdmitAccountSession(secondAccount, "new-root::api-key:7", time.Now()) {
		t.Fatal("restart bypassed the restored account-session capacity")
	}
	after := secondStore.AccountSessionSnapshots(secondAccount.DBID, time.Now())
	if len(after) != 1 || after[0].SessionID != rootKey || after[0].Owner.UserName != "Arun" ||
		after[0].RelatedRequestCount != 1 || len(after[0].RelatedSources) != 1 {
		t.Fatalf("restored snapshots = %#v", after)
	}
	if !after[0].LastSeen.Equal(before[0].LastSeen) {
		t.Fatalf("restored last_seen = %s, want %s", after[0].LastSeen, before[0].LastSeen)
	}
	firstPeriod := firstStore.AccountSessionUsagePeriod(firstAccount.DBID, rootKey, time.Now())
	secondPeriod := secondStore.AccountSessionUsagePeriod(secondAccount.DBID, rootKey, time.Now())
	if firstPeriod.ID == "" || firstPeriod.ID != secondPeriod.ID || !firstPeriod.StartedAt.Equal(secondPeriod.StartedAt) {
		t.Fatalf("usage period changed after restart: first=%+v second=%+v", firstPeriod, secondPeriod)
	}
}

func TestRelatedRequestRestoresAndRefreshesPersistedRootAfterRestart(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	settings := &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1}
	firstStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(firstStore.Stop)
	firstAccount := &Account{
		DBID: 12, AccessToken: "first", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 5, SessionCapacityIdleTTLSeconds: 3600,
	}
	firstStore.AddAccount(firstAccount)
	rootKey := "persisted-related-root::api-key:7"
	if !firstStore.AdmitAccountSession(firstAccount, rootKey, time.Now().Add(-time.Minute)) {
		t.Fatal("initial root admission failed")
	}
	firstStore.SetAccountSessionOwner(firstAccount.DBID, rootKey, AccountSessionOwner{})
	before := firstStore.AccountSessionSnapshots(firstAccount.DBID, time.Now())
	if len(before) != 1 {
		t.Fatalf("initial snapshots = %#v", before)
	}

	secondStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(secondStore.Stop)
	secondAccount := &Account{
		DBID: 12, AccessToken: "second", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 5, SessionCapacityIdleTTLSeconds: 3600,
	}
	secondStore.AddAccount(secondAccount)
	relatedKey := RelatedSessionAffinityKey(rootKey)
	selected, _ := secondStore.NextForSession(relatedKey, 0, nil)
	if selected != secondAccount {
		t.Fatalf("related request selected %#v, want restored account", selected)
	}
	secondStore.Release(selected)
	secondStore.RecordRelatedAccountSession(secondAccount.DBID, relatedKey, AccountSessionRelatedSource{
		ThreadSource: "subagent", RequestKind: "turn", SubagentKind: "thread_spawn",
	}, "restored-related-request-1")
	after := secondStore.AccountSessionSnapshots(secondAccount.DBID, time.Now())
	if len(after) != 1 || !after[0].LastSeen.After(before[0].LastSeen) {
		t.Fatalf("related request did not refresh restored root: before=%#v after=%#v", before, after)
	}
}

func TestRelatedRequestDoesNotReviveExpiredRootWindow(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	rootKey := "expired-root::api-key:7"
	if !store.AdmitAccountSession(account, rootKey, time.Now().Add(-2*time.Hour)) {
		t.Fatal("root session admission failed")
	}

	store.RecordRelatedAccountSession(account.DBID, RelatedSessionAffinityKey(rootKey), AccountSessionRelatedSource{
		ThreadSource: "subagent", RequestKind: "turn", SubagentKind: "thread_spawn",
	}, "late-related-request")
	if snapshots := store.AccountSessionSnapshots(account.DBID, time.Now()); len(snapshots) != 0 {
		t.Fatalf("expired root was revived by related request: %#v", snapshots)
	}
}

func TestRestoredNearExpiryRootPersistsItsFirstReuse(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	settings := &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1}
	firstStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(firstStore.Stop)
	firstAccount := &Account{
		DBID: 15, AccessToken: "first", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 60,
	}
	firstStore.AddAccount(firstAccount)
	const rootKey = "near-expiry-restored-root"
	oldLastSeen := time.Now().Add(-45 * time.Second)
	if !firstStore.AdmitAccountSession(firstAccount, rootKey, oldLastSeen) {
		t.Fatal("initial root admission failed")
	}
	firstStore.SetAccountSessionOwner(firstAccount.DBID, rootKey, AccountSessionOwner{UserName: "Arun"})

	secondStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(secondStore.Stop)
	secondAccount := &Account{
		DBID: 15, AccessToken: "second", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 60,
	}
	secondStore.AddAccount(secondAccount)
	reusedAt := time.Now()
	if !secondStore.AdmitAccountSession(secondAccount, rootKey, reusedAt) {
		t.Fatal("restored root reuse was rejected")
	}
	secondStore.SetAccountSessionOwner(secondAccount.DBID, rootKey, AccountSessionOwner{UserName: "Arun"})

	raw, found, err := runtimeCache.GetRuntime(context.Background(), accountSessionRuntimeNamespace, accountSessionRuntimeKey(secondAccount.DBID))
	if err != nil || !found {
		t.Fatalf("reused root cache missing: found=%v err=%v", found, err)
	}
	collection := persistedAccountSessionCollection{}
	if err := json.Unmarshal(raw, &collection); err != nil || len(collection.Sessions) != 1 {
		t.Fatalf("reused root cache invalid: sessions=%#v err=%v", collection.Sessions, err)
	}
	if collection.Sessions[0].LastSeen.Before(reusedAt) {
		t.Fatalf("restored first reuse was not persisted: got=%s want >=%s", collection.Sessions[0].LastSeen, reusedAt)
	}
}

func TestAccountSessionPersistenceRetriesImmediatelyAfterCacheFailure(t *testing.T) {
	baseCache := cache.NewMemory(1)
	defer baseCache.Close()
	runtimeCache := &failOnceAccountSessionRuntimeCache{TokenCache: baseCache}
	store := NewStore(nil, runtimeCache, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1})
	t.Cleanup(store.Stop)
	account := &Account{
		DBID: 16, AccessToken: "token", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
	}
	store.AddAccount(account)
	const rootKey = "retry-after-cache-failure-root"
	if !store.AdmitAccountSession(account, rootKey, time.Now()) {
		t.Fatal("initial root admission failed")
	}
	owner := AccountSessionOwner{UserName: "Arun"}
	store.SetAccountSessionOwner(account.DBID, rootKey, owner)
	store.SetAccountSessionOwner(account.DBID, rootKey, owner)

	if _, found, err := baseCache.GetRuntime(context.Background(), accountSessionRuntimeNamespace, accountSessionRuntimeKey(account.DBID)); err != nil || !found {
		t.Fatalf("failed cache write was not retried immediately: found=%v err=%v", found, err)
	}
}

func TestClearAccountSessionsRemovesPersistedRootAfterRestart(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	settings := &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1}
	firstStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(firstStore.Stop)
	firstAccount := &Account{
		DBID: 13, AccessToken: "first", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
	}
	firstStore.AddAccount(firstAccount)
	if !firstStore.AdmitAccountSession(firstAccount, "cleared-root", time.Now()) {
		t.Fatal("initial root admission failed")
	}
	firstStore.SetAccountSessionOwner(firstAccount.DBID, "cleared-root", AccountSessionOwner{UserName: "Arun"})
	firstStore.ClearAccountSessions(firstAccount.DBID)

	secondStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(secondStore.Stop)
	secondAccount := &Account{
		DBID: 13, AccessToken: "second", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
	}
	secondStore.AddAccount(secondAccount)
	if !secondStore.AdmitAccountSession(secondAccount, "replacement-root", time.Now()) {
		t.Fatal("cleared persisted root still consumed capacity after restart")
	}
	items := secondStore.AccountSessionSnapshots(secondAccount.DBID, time.Now())
	if len(items) != 1 || items[0].SessionID != "replacement-root" {
		t.Fatalf("snapshots after clear = %#v", items)
	}
}

func TestDisableAccountSessionCapacityAfterRestartClearsPersistedOwner(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	settings := &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1}
	firstStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(firstStore.Stop)
	firstAccount := &Account{
		DBID: 14, AccessToken: "first", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
	}
	firstStore.AddAccount(firstAccount)
	const rootKey = "disabled-after-restart-root"
	if !firstStore.AdmitAccountSession(firstAccount, rootKey, time.Now()) {
		t.Fatal("initial root admission failed")
	}
	firstStore.SetAccountSessionOwner(firstAccount.DBID, rootKey, AccountSessionOwner{UserName: "Arun"})

	secondStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(secondStore.Stop)
	secondAccount := &Account{
		DBID: 14, AccessToken: "second", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
	}
	secondStore.AddAccount(secondAccount)
	if !secondStore.ApplyAccountSessionCapacity(secondAccount.DBID, false, 1, 3600) {
		t.Fatal("failed to disable account session capacity")
	}

	thirdStore := NewStore(nil, runtimeCache, settings)
	t.Cleanup(thirdStore.Stop)
	thirdAccount := &Account{
		DBID: 14, AccessToken: "third", Status: StatusReady,
		SessionCapacityEnabled: true, SessionCapacityMax: 1, SessionCapacityIdleTTLSeconds: 3600,
	}
	thirdStore.AddAccount(thirdAccount)
	if _, found := thirdStore.AccountSessionAccountID(rootKey, time.Now()); found {
		t.Fatal("disabled account left a persisted reverse owner after restart")
	}
	if !thirdStore.AdmitAccountSession(thirdAccount, "replacement-after-disable", time.Now()) {
		t.Fatal("disabled account left persisted capacity occupied after restart")
	}
}

func TestRelatedRequestBorrowsOrdinaryRootAffinityWhenCapacityDisabled(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	account.SessionCapacityEnabled = false
	rootKey := "root-without-capacity::api-key:7"
	store.BindSessionAffinity(rootKey, account, "")

	relatedKey := RelatedSessionAffinityKey(rootKey)
	selected, _ := store.NextForSession(relatedKey, 0, nil)
	if selected != account {
		t.Fatal("related request did not borrow ordinary root affinity")
	}
	store.BindSessionAffinity(relatedKey, selected, "")
	store.Release(selected)
	if got := store.AccountSessionCount(account.DBID, time.Now()); got != 0 {
		t.Fatalf("related request created %d windows with capacity disabled", got)
	}
	if counts := store.accountWindowCountsForScheduling([]*Account{account}, time.Now()); counts[account.DBID] != 1 {
		t.Fatalf("window balancing counted related affinity: %#v", counts)
	}
}

func TestRelatedRequestNeverFallsBackFromKnownRootAccount(t *testing.T) {
	t.Run("ordinary affinity", func(t *testing.T) {
		store, rootAccount := newSessionCapacityTestStore(1)
		rootAccount.SessionCapacityEnabled = false
		fallbackAccount := &Account{DBID: 2, AccessToken: "fallback"}
		store.accounts = append(store.accounts, fallbackAccount)
		rootKey := "pinned-root::api-key:7"
		store.BindSessionAffinity(rootKey, rootAccount, "")

		relatedKey := RelatedSessionAffinityKey(rootKey)
		selected, _ := store.NextForSession(relatedKey, 0, map[int64]bool{rootAccount.DBID: true})
		if selected != nil {
			store.Release(selected)
			t.Fatalf("related retry escaped root account to %d", selected.DBID)
		}

		// A busy root is also a wait condition, not permission to use a different
		// account for the hidden turn.
		atomic.StoreInt64(&rootAccount.ActiveRequests, 1)
		store.maxConcurrency = 1
		selected, _ = store.NextForSession(relatedKey, 0, nil)
		if selected != nil {
			store.Release(selected)
			t.Fatalf("busy related request escaped root account to %d", selected.DBID)
		}
	})

	t.Run("hard window owner", func(t *testing.T) {
		store, rootAccount := newSessionCapacityTestStore(1)
		store.accounts = append(store.accounts, &Account{DBID: 2, AccessToken: "fallback"})
		rootKey := "capacity-root::api-key:7"
		if !store.AdmitAccountSession(rootAccount, rootKey, time.Now()) {
			t.Fatal("root session admission failed")
		}
		selected, _ := store.NextForSession(RelatedSessionAffinityKey(rootKey), 0, map[int64]bool{rootAccount.DBID: true})
		if selected != nil {
			store.Release(selected)
			t.Fatalf("related retry escaped hard-window owner to %d", selected.DBID)
		}
	})
}

func TestRelatedRequestStatsDeduplicateRetriesAndPreserveUnknownSources(t *testing.T) {
	store, account := newSessionCapacityTestStore(1)
	rootKey := "root-window::api-key:7"
	if !store.AdmitAccountSession(account, rootKey, time.Now()) {
		t.Fatal("root session admission failed")
	}
	relatedKey := RelatedSessionAffinityKey(rootKey)
	source := AccountSessionRelatedSource{ThreadSource: "future_new_source", RequestKind: "future_task", SubagentKind: "reviewer"}
	store.RecordRelatedAccountSession(account.DBID, relatedKey, source, "logical-request-1")
	store.accountSessionMu.Lock()
	beforeRetry := time.Now().Add(-30 * time.Second)
	store.accountSessions[account.DBID][rootKey].lastSeen = beforeRetry
	store.accountSessionMu.Unlock()
	store.RecordRelatedAccountSession(account.DBID, relatedKey, source, "logical-request-1")

	snapshots := store.AccountSessionSnapshots(account.DBID, time.Now())
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	if snapshots[0].RelatedRequestCount != 1 || len(snapshots[0].RelatedSources) != 1 {
		t.Fatalf("related stats = %#v", snapshots[0])
	}
	if !snapshots[0].LastSeen.After(beforeRetry) {
		t.Fatalf("deduplicated retry did not refresh root last_seen: got=%s want after %s", snapshots[0].LastSeen, beforeRetry)
	}
	got := snapshots[0].RelatedSources[0]
	if got.Count != 1 || got.ThreadSource != source.ThreadSource || got.RequestKind != source.RequestKind || got.SubagentKind != source.SubagentKind {
		t.Fatalf("source stats = %#v, want %#v", got, source)
	}
}

func TestSessionCapacityExhaustionIsScopedToEligibleAccounts(t *testing.T) {
	store, fullAccount := newSessionCapacityTestStore(1)
	if !store.AdmitAccountSession(fullAccount, "occupied", time.Now()) {
		t.Fatal("failed to fill account capacity")
	}
	if !store.HasSessionCapacityExhaustionWithDispatch(0, nil, func(account *Account) bool {
		return account.DBID == fullAccount.DBID
	}, DispatchPolicyStandard, "fresh", time.Now()) {
		t.Fatal("eligible full account was not reported as exhausted")
	}
	if store.HasSessionCapacityExhaustionWithDispatch(0, nil, func(account *Account) bool {
		return account.DBID == 999
	}, DispatchPolicyStandard, "fresh", time.Now()) {
		t.Fatal("unrelated full account caused a false capacity exhaustion")
	}

	unlimited := &Account{DBID: 2, AccessToken: "token-2"}
	store.accounts = append(store.accounts, unlimited)
	store.publishAccountSnapshot(store.accounts)
	if store.HasSessionCapacityExhaustionWithDispatch(0, nil, nil, DispatchPolicyStandard, "fresh", time.Now()) {
		t.Fatal("an eligible account with capacity disabled should keep the pool available")
	}
}
