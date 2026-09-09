package auth

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestReservedSessionCapacityIsolationAndLifecycle(test *testing.T) {
	store, account := newSessionCapacityTestStore(3)
	account.SessionCapacityReserved = 2
	now := time.Now()
	expanded := &SelectionTrace{}
	expanded.SetExpandedWindow(true)
	if !(store.AdmitAccountSession(account, "ordinary", now)) {
		test.Fatal("failed: True store.AdmitAccountSession(account, \"ordinary\", now)")
	}
	if store.AdmitAccountSession(account, "overflow", now) {
		test.Fatal("failed: False store.AdmitAccountSession(account, \"overflow\", now)")
	}
	if !(store.AdmitAccountSession(account, "paid", now, expanded)) {
		test.Fatal("failed: True store.AdmitAccountSession(account, \"paid\", now, expanded)")
	}
	if !(store.AdmitAccountSession(account, "paid2", now, expanded)) {
		test.Fatal("failed: True store.AdmitAccountSession(account, \"paid2\", now, expanded)")
	}
	if store.AdmitAccountSession(account, "paid3", now, expanded) {
		test.Fatal("failed: False store.AdmitAccountSession(account, \"paid3\", now, expanded)")
	}
	total, reserved := store.AccountSessionSlotCounts(account.ID(), now)
	if 3 != total {
		test.Fatal("failed: EqualValues 3 / total")
	}
	if 2 != reserved {
		test.Fatal("failed: EqualValues 2 / reserved")
	}
	if !(store.AdmitAccountSession(account, "paid", now, expanded)) {
		test.Fatal("failed: True store.AdmitAccountSession(account, \"paid\", now, expanded)")
	}
	if !(store.AdmitAccountSession(account, RelatedSessionAffinityKey("paid"), now, expanded)) {
		test.Fatal("failed: True store.AdmitAccountSession(account, RelatedSessionAffinityKey(\"paid\"), now, expanded)")
	}
	if store.AdmitAccountSession(account, "paid", now, &SelectionTrace{}) {
		test.Fatal("failed: False store.AdmitAccountSession(account, \"paid\", now, &SelectionTrace{})")
	}
	store.RemoveAccountSession(account.ID(), "ordinary")
	if !(store.AdmitAccountSession(account, "paid", now, &SelectionTrace{})) {
		test.Fatal("failed: True store.AdmitAccountSession(account, \"paid\", now, &SelectionTrace{})")
	}
	total, reserved = store.AccountSessionSlotCounts(account.ID(), now)
	if 2 != total {
		test.Fatal("failed: EqualValues 2 / total")
	}
	if 1 != reserved {
		test.Fatal("failed: EqualValues 1 / reserved")
	}
	total, reserved = store.AccountSessionSlotCounts(account.ID(), now.Add(61*time.Second))
	if total != 0 {
		test.Fatal("failed: Zero total")
	}
	if reserved != 0 {
		test.Fatal("failed: Zero reserved")
	}
}

func TestReservedSessionCapacityConcurrentOrdinaryRequestsCannotBorrow(test *testing.T) {
	store, account := newSessionCapacityTestStore(4)
	account.SessionCapacityReserved = 2
	var accepted atomic.Int64
	var workers sync.WaitGroup
	now := time.Now()
	for index := 0; index < 8; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if store.AdmitAccountSession(account, fmt.Sprintf("root-%d", index), now) {
				accepted.Add(1)
			}
		}()
	}
	workers.Wait()
	if 2 != accepted.Load() {
		test.Fatal("failed: EqualValues 2 / accepted.Load()")
	}
}
