package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCodexIdentityUUIDv7PersistsAcrossInstancesAndRestart(test *testing.T) {
	path := filepath.Join(test.TempDir(), "uuid7.db")
	first, err := New("sqlite", path)
	require.NoError(test, err)
	second, err := New("sqlite", path)
	require.NoError(test, err)
	type result struct {
		value string
		err   error
	}
	results := make(chan result, 2)
	key, seed := strings.Repeat("a", 64), strings.Repeat("b", 64)
	started := time.Now().UTC().Truncate(time.Millisecond)
	for _, db := range []*DB{first, second} {
		go func() {
			value, err := db.ResolveCodexIdentityUUIDv7(context.Background(), key, seed)
			results <- result{value, err}
		}()
	}
	left, right := <-results, <-results
	require.NoError(test, left.err)
	require.NoError(test, right.err)
	require.Equal(test, left.value, right.value)
	parsed, err := uuid.Parse(left.value)
	require.NoError(test, err)
	require.EqualValues(test, 7, parsed.Version())
	require.Equal(test, uuid.RFC4122, parsed.Variant())
	seconds, nanoseconds := parsed.Time().UnixTime()
	require.False(test, time.Unix(seconds, nanoseconds).Before(started))
	require.False(test, time.Unix(seconds, nanoseconds).After(time.Now()))
	require.NoError(test, first.Close())
	require.NoError(test, second.Close())
	resumed, err := New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, resumed.Close()) })
	value, err := resumed.ResolveCodexIdentityUUIDv7(test.Context(), key, seed)
	require.NoError(test, err)
	require.Equal(test, left.value, value)
	_, err = resumed.ResolveCodexIdentityUUIDv7(test.Context(), key, strings.Repeat("c", 64))
	require.Error(test, err)
	_, err = resumed.conn.ExecContext(test.Context(), `UPDATE codex_identity_uuid7_values SET value='invalid' WHERE identity_key=$1`, key)
	require.NoError(test, err)
	_, err = resumed.ResolveCodexIdentityUUIDv7(test.Context(), key, seed)
	require.Error(test, err)
}

func TestCodexIdentityMappingRetainsExistingSuffixVersion(test *testing.T) {
	db := newGrokStateTestDB(test)
	key := strings.Repeat("d", 64)
	policy, err := db.ResolveCodexIdentityMapping(test.Context(), key, nil, true)
	require.NoError(test, err)
	_, err = db.conn.ExecContext(test.Context(), `UPDATE codex_identity_mapping_policies SET mode='account-suffix-v1' WHERE root_key=$1`, key)
	require.NoError(test, err)
	retained, err := db.ResolveCodexIdentityMapping(test.Context(), key, nil, true)
	require.NoError(test, err)
	require.Equal(test, "account-suffix-v1", retained.Mode)
	require.Equal(test, policy.Secret, retained.Secret)
}
