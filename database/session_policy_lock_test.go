package database

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionPolicyLockLineagePersistenceIsolationAndConflict(test *testing.T) {
	path := filepath.Join(test.TempDir(), "lineage.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	parent, child, grandchild := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	cyberKey, windowKey := strings.Repeat("d", 64), strings.Repeat("e", 24)
	require.NoError(test, db.RecordSessionPolicyLockIdentity(test.Context(), parent, "cyber", cyberKey))
	require.NoError(test, db.RecordSessionPolicyLockIdentity(test.Context(), parent, "window", windowKey))
	keys, err := db.SessionPolicyLockKeys(test.Context(), child, parent, "cyber")
	require.NoError(test, err)
	require.Equal(test, []string{cyberKey}, keys)
	keys, err = db.SessionPolicyLockKeys(test.Context(), grandchild, child, "window")
	require.NoError(test, err)
	require.Equal(test, []string{windowKey}, keys)
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	keys, err = db.SessionPolicyLockKeys(test.Context(), grandchild, "", "cyber")
	require.NoError(test, err)
	require.Equal(test, []string{cyberKey}, keys)
	keys, err = db.SessionPolicyLockKeys(test.Context(), strings.Repeat("f", 64), "", "cyber")
	require.NoError(test, err)
	require.Empty(test, keys)
	_, err = db.SessionPolicyLockKeys(test.Context(), child, grandchild, "cyber")
	require.ErrorIs(test, err, ErrSessionLineageConflict)
	_, err = db.SessionPolicyLockKeys(test.Context(), parent, grandchild, "cyber")
	require.ErrorIs(test, err, ErrSessionLineageConflict)
	require.Error(test, db.RecordSessionPolicyLockIdentity(test.Context(), parent, "window", cyberKey))
}
