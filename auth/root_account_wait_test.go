package auth

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/codex2api/cache"
)

func TestRootAccountWaitWakesOnAdmissionWithoutTakingLease(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		store, account := newSessionCapacityTestStore(5)
		requestContext, cancel := context.WithTimeout(test.Context(), 30*time.Second)
		defer cancel()
		results := make(chan int64, 2)
		for range 2 {
			go func() {
				accountID, err := store.WaitForRootAccount(requestContext, "root::api-key:101")
				if err != nil {
					results <- 0
					return
				}
				results <- accountID
			}()
		}
		synctest.Wait()
		if !store.AdmitAccountSession(account, "root::api-key:102", time.Now()) {
			test.Fatal("could not admit the other API key")
		}
		synctest.Wait()
		select {
		case <-results:
			test.Fatal("another API key woke the root waiter")
		default:
		}
		time.Sleep(29*time.Second + 500*time.Millisecond)
		admittedAt := time.Now()
		if !store.AdmitAccountSession(account, "root::api-key:101", admittedAt) {
			test.Fatal("waiting background requests consumed account capacity")
		}
		synctest.Wait()
		for range 2 {
			if accountID := <-results; accountID != account.ID() {
				test.Fatalf("wrong root account: %d", accountID)
			}
		}
		if !time.Now().Equal(admittedAt) {
			test.Fatal("admission notification did not release waiters immediately")
		}
		if len(store.rootAccountWaiters) != 0 || len(store.accountSessions[account.ID()]) != 2 {
			test.Fatal("waiters leaked or created extra windows")
		}
	})
}

func TestRootAccountWaitTimeoutAndCancel(test *testing.T) {
	for _, canceled := range []bool{false, true} {
		synctest.Test(test, func(test *testing.T) {
			store, _ := newSessionCapacityTestStore(5)
			requestContext, cancel := context.WithTimeout(test.Context(), 30*time.Second)
			defer cancel()
			wantError, duration := context.DeadlineExceeded, 30*time.Second
			if canceled {
				cancel()
				wantError, duration = context.Canceled, 0
			}
			started := time.Now()
			accountID, err := store.WaitForRootAccount(requestContext, "missing")
			if accountID != 0 || !errors.Is(err, wantError) || time.Since(started) != duration {
				test.Fatalf("account=%d error=%v elapsed=%v", accountID, err, time.Since(started))
			}
			if len(store.rootAccountWaiters) != 0 {
				test.Fatal("waiter was not removed")
			}
		})
	}
}

type rootWaitTestCache struct {
	cache.TokenCache
	ready atomic.Bool
	reads atomic.Int32
}

func (store *rootWaitTestCache) GetRuntime(ctx context.Context, namespace, key string) (json.RawMessage, bool, error) {
	store.reads.Add(1)
	if store.ready.Load() && namespace == accountSessionOwnerRuntimeNamespace && key == "remote-root" {
		return json.RawMessage(`{"account_id":1}`), true, nil
	}
	return nil, false, ctx.Err()
}

func (store *rootWaitTestCache) GetSessionAffinity(ctx context.Context, key string) (cache.SessionAffinityBinding, bool, error) {
	return cache.SessionAffinityBinding{}, false, ctx.Err()
}

func TestRootAccountWaitPeriodicallyChecksSharedBinding(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		store, account := newSessionCapacityTestStore(5)
		sharedCache := &rootWaitTestCache{}
		store.tokenCache = sharedCache
		requestContext, cancel := context.WithTimeout(test.Context(), 30*time.Second)
		defer cancel()
		result := make(chan int64, 1)
		go func() {
			accountID, _ := store.WaitForRootAccount(requestContext, "remote-root")
			result <- accountID
		}()
		synctest.Wait()
		time.Sleep(2500 * time.Millisecond)
		sharedCache.ready.Store(true)
		if accountID := <-result; accountID != account.ID() {
			test.Fatalf("shared root account=%d", accountID)
		}
		if reads := sharedCache.reads.Load(); reads != 4 {
			test.Fatalf("expected one bounded shared lookup per second, got %d", reads)
		}
	})
}

func TestRootAccountWaitCoalescesSharedReads(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		store, account := newSessionCapacityTestStore(5)
		sharedCache := &rootWaitTestCache{}
		store.tokenCache = sharedCache
		requestContext, cancel := context.WithTimeout(test.Context(), 30*time.Second)
		defer cancel()
		results := make(chan int64, 8)
		for range 8 {
			go func() {
				accountID, _ := store.WaitForRootAccount(requestContext, "remote-root")
				results <- accountID
			}()
		}
		synctest.Wait()
		time.Sleep(2500 * time.Millisecond)
		sharedCache.ready.Store(true)
		for range 8 {
			if accountID := <-results; accountID != account.ID() {
				test.Fatalf("wrong account: %d", accountID)
			}
		}
		if reads := sharedCache.reads.Load(); reads != 4 {
			test.Fatalf("same-root waits amplified shared reads: %d", reads)
		}
		if len(store.rootAccountWaiters) != 0 {
			test.Fatal("shared waiter leaked")
		}
	})
}
