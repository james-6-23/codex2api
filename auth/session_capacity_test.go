package auth

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

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

func TestRelatedRequestBorrowsRootAccountWithoutRefreshingOrCreatingWindow(t *testing.T) {
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
		t.Fatalf("related request refreshed root last_seen: got %s want %s", snapshots[0].LastSeen, originalLastSeen)
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
	store.RecordRelatedAccountSession(account.DBID, relatedKey, source, "logical-request-1")

	snapshots := store.AccountSessionSnapshots(account.DBID, time.Now())
	if len(snapshots) != 1 {
		t.Fatalf("snapshots = %#v", snapshots)
	}
	if snapshots[0].RelatedRequestCount != 1 || len(snapshots[0].RelatedSources) != 1 {
		t.Fatalf("related stats = %#v", snapshots[0])
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
	if store.HasSessionCapacityExhaustionWithDispatch(0, nil, nil, DispatchPolicyStandard, "fresh", time.Now()) {
		t.Fatal("an eligible account with capacity disabled should keep the pool available")
	}
}
