package database

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionOutboundWindowNumbersRollbackRestartAndGenerations(test *testing.T) {
	path := filepath.Join(test.TempDir(), "numbers.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx := context.Background()
	_, err = db.CommitSessionContinuity(ctx, "root", SessionContinuityRecord{AccountID: 1, ThreadID: "main", NumberKnown: true, Number: 16})
	require.NoError(test, err)
	_, _, err = db.SwitchSessionContinuityAccount(ctx, SessionAccountFailover{RootKey: "root", ExpectedAccountID: 1, AccountID: 2, At: time.Now(), ResetOutboundWindow: true, WindowThreadID: "main", WindowNumber: 16, WindowContextID: "first", LossyContextRestart: true})
	require.NoError(test, err)
	resolve := func(thread, identity string, original, expected uint64) {
		test.Helper()
		numbers, failure := db.ResolveSessionOutboundWindowNumbers(ctx, "root", 2, 1, map[string]SessionOutboundWindowInput{thread: {Number: original, ContextID: identity}})
		require.NoError(test, failure)
		require.Equal(test, expected, numbers[thread])
	}
	resolve("main", "first", 16, 0)
	resolve("main", "rollback", 15, 1)
	resolve("main", "rollback", 15, 1)
	resolve("main", "advanced-again", 16, 2)
	resolve("main", "first", 16, 0)
	resolve("child", "child-context", 16, 0)
	resolve("child", "child-rollback", 15, 1)
	_, err = db.ResolveSessionOutboundWindowNumbers(ctx, "root", 2, 1, map[string]SessionOutboundWindowInput{"main": {Number: 17, ContextID: "first"}})
	require.Error(test, err)
	_, err = db.ResolveSessionOutboundWindowNumbers(ctx, "root", 2, 1, map[string]SessionOutboundWindowInput{"main": {Number: 16}})
	require.Error(test, err)
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	resolve("main", "rollback", 15, 1)
	record, found, err := db.ReadSessionContinuity(ctx, "root")
	require.NoError(test, err)
	require.True(test, found)
	require.True(test, record.LossyContextRestart)
	_, err = db.CommitSessionContinuity(ctx, "root", SessionContinuityRecord{AccountID: 2, ThreadID: "main", NumberKnown: true, Number: 17, FailoverCount: 1})
	require.NoError(test, err)
	resolve("main", "advanced-again", 16, 2)
	_, _, err = db.SwitchSessionContinuityAccount(ctx, SessionAccountFailover{RootKey: "root", ExpectedAccountID: 2, ExpectedGeneration: 1, AccountID: 1, At: time.Now(), ResetOutboundWindow: true, WindowThreadID: "main", WindowNumber: 15, WindowContextID: "rollback", LossyContextRestart: true})
	require.NoError(test, err)
	numbers, err := db.ResolveSessionOutboundWindowNumbers(ctx, "root", 1, 2, map[string]SessionOutboundWindowInput{"main": {Number: 15, ContextID: "rollback"}})
	require.NoError(test, err)
	require.EqualValues(test, 0, numbers["main"])
	_, err = db.ResolveSessionOutboundWindowNumbers(ctx, "root", 2, 1, map[string]SessionOutboundWindowInput{"main": {Number: 15, ContextID: "rollback"}})
	require.ErrorIs(test, err, ErrSessionOwnerConflict)
}

func TestSessionOutboundWindowNumbersConcurrentAndLegacy(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "legacy.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx := context.Background()
	_, err = db.CommitSessionContinuity(ctx, "root", SessionContinuityRecord{AccountID: 2, FailoverCount: 1, ThreadID: "main", NumberKnown: true, Number: 18, OutboundWindowReset: true, OutboundWindowBases: map[string]uint64{"main": 16}})
	require.NoError(test, err)
	for _, sample := range []struct {
		original, expected uint64
		identity           string
	}{{18, 2, "latest"}, {15, 3, "rollback"}, {16, 0, "first"}, {16, 4, "other"}} {
		numbers, failure := db.ResolveSessionOutboundWindowNumbers(ctx, "root", 2, 1, map[string]SessionOutboundWindowInput{"main": {Number: sample.original, ContextID: sample.identity}})
		require.NoError(test, failure)
		require.Equal(test, sample.expected, numbers["main"])
	}
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			numbers, failure := db.ResolveSessionOutboundWindowNumbers(ctx, "root", 2, 1, map[string]SessionOutboundWindowInput{"main": {Number: 17, ContextID: "concurrent"}})
			if failure != nil || numbers["main"] != 1 {
				test.Errorf("concurrent mapping = %v, %v", numbers, failure)
			}
		}()
	}
	group.Wait()
}
