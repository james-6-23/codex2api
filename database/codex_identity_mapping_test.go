package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexIdentityMappingPersistsPolicySecretAndOldSessions(test *testing.T) {
	path := filepath.Join(test.TempDir(), "mapping.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	ctx := context.Background()
	root, legacy := strings.Repeat("a", 64), strings.Repeat("b", 64)
	require.NoError(test, db.ClaimCodexIdentities(ctx, []string{legacy}, strings.Repeat("c", 64)))
	preserved, err := db.ResolveCodexIdentityMapping(ctx, root, []string{legacy}, true)
	require.NoError(test, err)
	require.Equal(test, "preserve", preserved.Mode)
	mappedRoot := strings.Repeat("d", 64)
	mapped, err := db.ResolveCodexIdentityMapping(ctx, mappedRoot, nil, true)
	require.NoError(test, err)
	require.Equal(test, CodexIdentityMappingUUIDv7, mapped.Mode)
	require.Len(test, mapped.Secret, 64)
	encoded, err := json.Marshal(mapped)
	require.NoError(test, err)
	require.NotContains(test, string(encoded), mapped.Secret)
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	resumed, err := db.ResolveCodexIdentityMapping(ctx, mappedRoot, []string{legacy}, false)
	require.NoError(test, err)
	require.Equal(test, mapped, resumed)
	other, err := db.ResolveCodexIdentityMapping(ctx, strings.Repeat("e", 64), nil, true)
	require.NoError(test, err)
	require.Equal(test, mapped.Secret, other.Secret)
	preserved, err = db.ResolveCodexIdentityMapping(ctx, root, nil, true)
	require.NoError(test, err)
	require.Equal(test, "preserve", preserved.Mode)
}

func TestCodexIdentityAliasesRejectCollisionsAndRollback(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "aliases.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx := context.Background()
	first := CodexIdentityAliasClaim{AliasKey: strings.Repeat("b", 64), SourceKey: strings.Repeat("1", 64)}
	fresh := CodexIdentityAliasClaim{AliasKey: strings.Repeat("a", 64), SourceKey: strings.Repeat("2", 64)}
	require.NoError(test, db.ClaimCodexIdentityAliases(ctx, []CodexIdentityAliasClaim{first, first}))
	collision := first
	collision.SourceKey = strings.Repeat("3", 64)
	require.ErrorIs(test, db.ClaimCodexIdentityAliases(ctx, []CodexIdentityAliasClaim{fresh, collision}), ErrCodexIdentityAliasCollision)
	changed := first
	changed.AliasKey = strings.Repeat("c", 64)
	require.ErrorIs(test, db.ClaimCodexIdentityAliases(ctx, []CodexIdentityAliasClaim{changed}), ErrCodexIdentityAliasCollision)
	var count int
	require.NoError(test, db.conn.QueryRow(`SELECT count(*) FROM codex_identity_alias_claims`).Scan(&count))
	require.Equal(test, 1, count)
	require.NoError(test, db.ClaimCodexIdentityAliases(ctx, []CodexIdentityAliasClaim{fresh}))
}

func TestCodexIdentityMappingConcurrentInstances(test *testing.T) {
	path := filepath.Join(test.TempDir(), "shared.db")
	first, err := New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, first.Close()) })
	second, err := New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, second.Close()) })
	type result struct {
		policy CodexIdentityMappingPolicy
		err    error
	}
	results := make(chan result, 2)
	for _, db := range []*DB{first, second} {
		go func() {
			policy, err := db.ResolveCodexIdentityMapping(context.Background(), strings.Repeat("f", 64), nil, true)
			results <- result{policy, err}
		}()
	}
	left, right := <-results, <-results
	require.NoError(test, left.err)
	require.NoError(test, right.err)
	require.Equal(test, left.policy, right.policy)
}

func TestCodexIdentityMappingMissingSecretDoesNotRegenerate(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "missing-secret.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	_, err = db.ResolveCodexIdentityMapping(context.Background(), strings.Repeat("a", 64), nil, true)
	require.NoError(test, err)
	_, err = db.conn.Exec(`DELETE FROM codex_identity_mapping_secret WHERE id=1`)
	require.NoError(test, err)
	_, err = db.ResolveCodexIdentityMapping(context.Background(), strings.Repeat("b", 64), nil, true)
	require.Error(test, err)
	var count int
	require.NoError(test, db.conn.QueryRow(`SELECT count(*) FROM codex_identity_mapping_secret`).Scan(&count))
	require.Zero(test, count)
}
