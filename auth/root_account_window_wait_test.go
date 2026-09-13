package auth

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRootAccountWindowWaitDoesNotTreatAffinityAsActiveWindow(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		store, account := newSessionCapacityTestStore(2)
		const rootKey = "main-root::api-key:101"
		store.BindSessionAffinity(rootKey, account, "")
		require.True(test, store.RemoveAccountSession(account.ID(), rootKey))
		ctx, cancel := context.WithTimeout(test.Context(), 30*time.Second)
		defer cancel()
		owner, err := store.WaitForRootAccount(ctx, rootKey)
		require.NoError(test, err)
		require.Equal(test, account.ID(), owner)
		results := make(chan error, 8)
		for range cap(results) {
			go func() { results <- store.WaitForRootAccountWindow(ctx, rootKey, owner) }()
		}
		synctest.Wait()
		require.Empty(test, results)
		require.Empty(test, store.accountSessions[account.ID()])
		require.True(test, store.AdmitAccountSession(account, "other-root::api-key:101", time.Now()))
		synctest.Wait()
		require.Empty(test, results)
		time.Sleep(1250 * time.Millisecond)
		admittedAt := time.Now()
		require.True(test, store.AdmitAccountSession(account, rootKey, admittedAt))
		for range cap(results) {
			require.NoError(test, <-results)
		}
		require.Equal(test, admittedAt, time.Now())
		require.Len(test, store.accountSessions[account.ID()], 2)
		require.Empty(test, store.rootAccountWaiters)
	})
}

func TestRootAccountWindowWaitTimeoutCancellationAndOwnerChange(test *testing.T) {
	for _, scenario := range []string{"timeout", "canceled", "owner_changed", "capacity_disabled"} {
		test.Run(scenario, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				store, account := newSessionCapacityTestStore(2)
				other := &Account{DBID: 2, AccessToken: "other", SessionCapacityEnabled: true, SessionCapacityMax: 2}
				store.accounts = append(store.accounts, other)
				const rootKey = "main-root::api-key:101"
				store.BindSessionAffinity(rootKey, account, "")
				require.True(test, store.RemoveAccountSession(account.ID(), rootKey))
				if scenario == "capacity_disabled" {
					account.SessionCapacityEnabled = false
				}
				ctx, cancel := context.WithTimeout(test.Context(), 7*time.Second)
				defer cancel()
				result := make(chan error, 1)
				started := time.Now()
				go func() { result <- store.WaitForRootAccountWindow(ctx, rootKey, account.ID()) }()
				synctest.Wait()
				switch scenario {
				case "canceled":
					cancel()
					require.ErrorIs(test, <-result, context.Canceled)
				case "owner_changed":
					store.BindSessionAffinity(rootKey, other, "")
					require.ErrorIs(test, <-result, ErrRootAccountOwnerChanged)
				case "timeout":
					require.ErrorIs(test, <-result, context.DeadlineExceeded)
					require.Equal(test, 7*time.Second, time.Since(started))
				case "capacity_disabled":
					require.NoError(test, <-result)
					require.Zero(test, time.Since(started))
				}
				require.Empty(test, store.rootAccountWaiters)
				require.Empty(test, store.accountSessions[account.ID()])
			})
		})
	}
}
