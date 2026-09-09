package database

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootNamingClaimIsAtomicPersistentAndScoped(test *testing.T) {
	path := filepath.Join(test.TempDir(), "naming.db")
	first, err := New("sqlite", path)
	require.NoError(test, err)
	second, err := New("sqlite", path)
	require.NoError(test, err)
	var claimed atomic.Int32
	var workers sync.WaitGroup
	errors := make(chan error, 2)
	for _, db := range []*DB{first, second} {
		workers.Add(1)
		go func(db *DB) {
			defer workers.Done()
			won, err := db.ClaimRootNaming(context.Background(), "platform:user", "root")
			if won {
				claimed.Add(1)
			}
			errors <- err
		}(db)
	}
	workers.Wait()
	require.NoError(test, <-errors)
	require.NoError(test, <-errors)
	require.EqualValues(test, 1, claimed.Load())
	require.NoError(test, first.Close())
	require.NoError(test, second.Close())
	reopened, err := New("sqlite", path)
	require.NoError(test, err)
	defer reopened.Close()
	won, err := reopened.ClaimRootNaming(context.Background(), "platform:user", "root")
	require.NoError(test, err)
	require.False(test, won)
	for _, subjectRoot := range [][2]string{{"other:user", "root"}, {"platform:user", "another-root"}} {
		won, err = reopened.ClaimRootNaming(context.Background(), subjectRoot[0], subjectRoot[1])
		require.NoError(test, err)
		require.True(test, won)
	}
}
