package auth

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newSessionSlotBufferTestStore(limit int64, accounts ...*Account) *Store {
	store := &Store{
		accounts:                accounts,
		maxConcurrency:          limit,
		sessionBindings:         make(map[string]sessionAffinity),
		sessionSlotReservations: make(map[int64]map[string][]uint64),
	}
	store.SetSessionSlotBuffer(50 * time.Millisecond)
	store.SetSessionSlotBufferEnabled(true)
	return store
}

func TestSessionSlotBufferDisabledByDefault(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := &Store{
		accounts:                []*Account{account},
		maxConcurrency:          1,
		sessionBindings:         make(map[string]sessionAffinity),
		sessionSlotReservations: make(map[int64]map[string][]uint64),
	}
	store.SetSessionSlotBuffer(50 * time.Millisecond)

	acquired := store.Next()
	store.ReleaseForSession(acquired, "owner")
	if got := accountOccupiedRequests(account); got != 0 {
		t.Fatalf("occupied while disabled = %d, want 0", got)
	}
}

func TestSessionSlotBufferAffinityOffReleasesImmediately(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	store.SetAffinityMode(AffinityModeOff)

	acquired := store.Next()
	store.ReleaseForSession(acquired, "owner")
	if got := account.GetActiveRequests(); got != 0 {
		t.Fatalf("active with affinity off = %d, want 0", got)
	}
	if got := account.GetOccupiedRequests(); got != 0 {
		t.Fatalf("occupied with affinity off = %d, want 0", got)
	}
}

func TestDisablingSessionSlotBufferReleasesReservations(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	acquired := store.Next()
	store.ReleaseForSession(acquired, "owner")

	store.SetSessionSlotBufferEnabled(false)
	if got := accountOccupiedRequests(account); got != 0 {
		t.Fatalf("occupied after disabling = %d, want 0", got)
	}
	if next := store.Next(); next != account {
		t.Fatal("capacity was not immediately available after disabling")
	} else {
		store.Release(next)
	}
}

func TestSessionSlotBufferOwnerReclaimsBeforeFreshSession(t *testing.T) {
	primary := &Account{DBID: 1, AccessToken: "tok-1"}
	fallback := &Account{DBID: 2, AccessToken: "tok-2"}
	store := newSessionSlotBufferTestStore(1, primary, fallback)
	store.BindSessionAffinity("owner", primary, "")

	first, _ := store.NextForSession("owner", 0, nil)
	if first != primary {
		t.Fatalf("owner first account = %p, want primary %p", first, primary)
	}
	store.ReleaseForSession(first, "owner")
	if got := primary.GetActiveRequests(); got != 0 {
		t.Fatalf("active after buffering = %d, want 0", got)
	}
	if got := accountOccupiedRequests(primary); got != 1 {
		t.Fatalf("occupied after buffering = %d, want 1", got)
	}

	fresh, _ := store.NextForSession("fresh", 0, nil)
	if fresh != fallback {
		t.Fatalf("fresh session account = %p, want fallback %p", fresh, fallback)
	}
	store.Release(fresh)

	reclaimed, _ := store.NextForSession("owner", 0, nil)
	if reclaimed != primary {
		t.Fatalf("reclaimed account = %p, want primary %p", reclaimed, primary)
	}
	if got := primary.GetActiveRequests(); got != 1 {
		t.Fatalf("active after reclaim = %d, want 1", got)
	}
	if got := accountOccupiedRequests(primary); got != 1 {
		t.Fatalf("occupied after reclaim = %d, want 1", got)
	}
	store.Release(reclaimed)
}

func TestRelatedSessionUsesOneProtectedConcurrencyLeaseOnRootAccount(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	rootKey := "root-session::api-key:7"
	relatedKey := ProtectedRelatedSessionAffinityKey(rootKey)
	store.BindSessionAffinity(rootKey, account, "")

	main, _ := store.NextForSession(rootKey, 7, nil)
	if main != account {
		t.Fatalf("main account = %p, want %p", main, account)
	}
	guardian, _ := store.NextForSession(relatedKey, 7, nil)
	if guardian != account {
		store.Release(main)
		t.Fatalf("related account = %p, want root %p", guardian, account)
	}
	if got := account.GetActiveRequests(); got != 2 {
		t.Fatalf("active with main and related = %d, want 2", got)
	}
	if got := account.GetOccupiedRequests(); got != 2 {
		t.Fatalf("occupied with protected lease = %d, want 2", got)
	}
	if extra, _ := store.NextForSession(relatedKey, 7, nil); extra != nil {
		store.Release(extra)
		t.Fatal("a second related request exceeded the one-slot protected allowance")
	}

	store.ReleaseForSession(guardian, relatedKey)
	if got := account.GetActiveRequests(); got != 1 {
		t.Fatalf("active after related release = %d, want main only", got)
	}
	if got := account.GetOccupiedRequests(); got != 1 {
		t.Fatalf("related release stranded a buffered child slot: occupied=%d", got)
	}
	store.Release(main)
}

func TestProtectedRelatedSessionPreservesRootBuffer(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	rootKey := "root-session::api-key:7"
	relatedKey := ProtectedRelatedSessionAffinityKey(rootKey)
	store.BindSessionAffinity(rootKey, account, "")

	main, _ := store.NextForSession(rootKey, 7, nil)
	if main != account {
		t.Fatalf("main account = %p, want %p", main, account)
	}
	store.ReleaseForSession(main, rootKey)

	guardian, _ := store.NextForSession(relatedKey, 7, nil)
	if guardian != account {
		t.Fatalf("protected related account = %p, want root %p", guardian, account)
	}
	if got := account.GetOccupiedRequests(); got != 2 {
		t.Fatalf("occupied with root buffer and protected lease = %d, want 2", got)
	}
	store.ReleaseForSession(guardian, relatedKey)
	if got := account.GetOccupiedRequests(); got != 1 {
		t.Fatalf("protected release consumed root buffer: occupied=%d, want 1", got)
	}
	if fresh, _ := store.NextForSession("fresh", 7, nil); fresh != nil {
		store.Release(fresh)
		t.Fatal("fresh session entered before the root buffer was reclaimed")
	}

	continued, _ := store.NextForSession(rootKey, 7, nil)
	if continued != account {
		t.Fatalf("root could not reclaim preserved reservation: %p", continued)
	}
	store.Release(continued)
}

func TestRelatedSessionReturnsReclaimedBufferToRootKey(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	rootKey := "root-session::api-key:7"
	relatedKey := RelatedSessionAffinityKey(rootKey)
	store.BindSessionAffinity(rootKey, account, "")

	main, _ := store.NextForSession(rootKey, 7, nil)
	if main != account {
		t.Fatalf("main account = %p, want %p", main, account)
	}
	store.ReleaseForSession(main, rootKey)

	guardian, _ := store.NextForSession(relatedKey, 7, nil)
	if guardian != account {
		t.Fatalf("related request did not reclaim the root reservation: %p", guardian)
	}
	store.ReleaseForSession(guardian, relatedKey)

	continued, _ := store.NextForSession(rootKey, 7, nil)
	if continued != account {
		t.Fatalf("root could not reclaim its reservation after related release: %p", continued)
	}
	if got := account.GetOccupiedRequests(); got != 1 {
		t.Fatalf("root reclaim occupied = %d, want 1", got)
	}
	store.Release(continued)
}

func TestSessionSlotBufferExpiresAndReleasesCapacity(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	store.BindSessionAffinity("owner", account, "")

	acquired, _ := store.NextForSession("owner", 0, nil)
	store.ReleaseForSession(acquired, "owner")
	if fresh, _ := store.NextForSession("fresh", 0, nil); fresh != nil {
		store.Release(fresh)
		t.Fatal("fresh session acquired a slot before buffer expiry")
	}

	deadline := time.Now().Add(time.Second)
	for accountOccupiedRequests(account) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := accountOccupiedRequests(account); got != 0 {
		t.Fatalf("occupied after expiry = %d, want 0", got)
	}
	fresh, _ := store.NextForSession("fresh", 0, nil)
	if fresh != account {
		t.Fatalf("fresh session after expiry = %p, want account %p", fresh, account)
	}
	store.Release(fresh)
}

func TestImmediateReleaseDoesNotCreateSessionReservation(t *testing.T) {
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	store := newSessionSlotBufferTestStore(1, account)
	acquired := store.Next()
	if acquired != account {
		t.Fatal("failed to acquire test account")
	}
	store.Release(acquired)
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("active after immediate release = %d, want 0", got)
	}
	if got := accountOccupiedRequests(account); got != 0 {
		t.Fatalf("occupied after immediate release = %d, want 0", got)
	}
}

func TestConcurrentAccountSlotAcquireReleaseNeverLeaksOrExceedsLimit(t *testing.T) {
	const (
		limit      = int64(3)
		goroutines = 24
		iterations = 200
	)
	account := &Account{DBID: 1, AccessToken: "tok-1"}
	var maxOccupied int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				for !reserveOccupiedAccountSlot(account, limit) {
					runtime.Gosched()
				}
				occupied := atomic.LoadInt64(&account.OccupiedRequests)
				for previous := atomic.LoadInt64(&maxOccupied); occupied > previous; previous = atomic.LoadInt64(&maxOccupied) {
					if atomic.CompareAndSwapInt64(&maxOccupied, previous, occupied) {
						break
					}
				}
				if occupied > limit {
					t.Errorf("occupied = %d, exceeds limit %d", occupied, limit)
				}
				releaseOccupiedAccountSlot(account)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("final active = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&account.OccupiedRequests); got != 0 {
		t.Fatalf("final occupied = %d, want 0", got)
	}
	if maxOccupied > limit {
		t.Fatalf("max occupied = %d, exceeds limit %d", maxOccupied, limit)
	}

	store := newSessionSlotBufferTestStore(limit, account)
	acquired := make([]*Account, 0, limit)
	for i := int64(0); i < limit; i++ {
		got := store.Next()
		if got == nil {
			t.Fatalf("acquire %d returned nil", i)
		}
		acquired = append(acquired, got)
	}
	for i, got := range acquired {
		store.ReleaseForSession(got, string(rune('a'+i)))
	}
	if got := account.GetActiveRequests(); got != 0 {
		t.Fatalf("buffered active = %d, want 0", got)
	}
	if got := account.GetOccupiedRequests(); got != limit {
		t.Fatalf("buffered occupied = %d, want %d", got, limit)
	}
	store.SetSessionSlotBufferEnabled(false)
	if got := account.GetOccupiedRequests(); got != 0 {
		t.Fatalf("occupied after disabling = %d, want 0", got)
	}
}
