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
