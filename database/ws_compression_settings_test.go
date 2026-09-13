package database

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexWSCompressionOptionsMigrationAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "compression-options.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	settings := &SystemSettings{SiteName: "preserved", CodexWSContextTakeover: true}
	require.NoError(t, db.UpdateSystemSettings(ctx, settings))
	saved, err := db.GetSystemSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, saved.CodexWSCompressionLevel)
	require.False(t, saved.CodexWSDisableFragmentation)
	for _, level := range []int{1, 6, 9} {
		for _, disabled := range []bool{true, false} {
			saved.CodexWSCompressionLevel = level
			saved.CodexWSDisableFragmentation = disabled
			require.NoError(t, db.UpdateSystemSettings(ctx, saved))
			require.NoError(t, db.Close())
			db, err = New("sqlite", path)
			require.NoError(t, err)
			saved, err = db.GetSystemSettings(ctx)
			require.NoError(t, err)
			require.Equal(t, level, saved.CodexWSCompressionLevel)
			require.Equal(t, disabled, saved.CodexWSDisableFragmentation)
			require.True(t, saved.CodexWSContextTakeover)
		}
	}
	_, err = db.conn.ExecContext(ctx, "UPDATE system_settings SET codex_ws_compression_level=NULL, codex_ws_disable_fragmentation=NULL WHERE id=1")
	require.NoError(t, err)
	saved, err = db.GetSystemSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, saved.CodexWSCompressionLevel)
	require.False(t, saved.CodexWSDisableFragmentation)
	for _, column := range []string{"codex_ws_compression_level", "codex_ws_disable_fragmentation"} {
		_, err = db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN "+column)
		require.NoError(t, err)
	}
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	saved, err = db.GetSystemSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, saved.CodexWSCompressionLevel)
	require.False(t, saved.CodexWSDisableFragmentation)
	require.Equal(t, "preserved", saved.SiteName)
	require.True(t, saved.CodexWSContextTakeover)
}
