package database

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func runOnAllOutputGuardDBDrivers(t *testing.T, testFn func(t *testing.T, db *DB)) {
	t.Helper()
	t.Run("sqlite", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "output-guard-test.db")
		db, err := New("sqlite", dbPath)
		if err != nil {
			t.Fatalf("New(sqlite) error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		testFn(t, db)
	})

	if dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN"); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			db, err := New("postgres", dsn)
			if err != nil {
				t.Fatalf("New(postgres) error = %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			testFn(t, db)
		})
	}
}

func TestOutputGuardSettingsFreshDefaults(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		got, err := db.GetOutputGuardSettings(context.Background())
		if err != nil {
			t.Fatalf("GetOutputGuardSettings error = %v", err)
		}

		if got.Enabled == nil || *got.Enabled != false {
			t.Fatalf("default Enabled want false, got %v", got.Enabled)
		}
		if got.MaxToolArgumentBytes != 262144 {
			t.Fatalf("default MaxToolArgumentBytes want 262144, got %d", got.MaxToolArgumentBytes)
		}
		if got.MaxToolArgumentDeltas != 4096 {
			t.Fatalf("default MaxToolArgumentDeltas want 4096, got %d", got.MaxToolArgumentDeltas)
		}
		if got.MaxChannelBytes != 2097152 {
			t.Fatalf("default MaxChannelBytes want 2097152, got %d", got.MaxChannelBytes)
		}
		if got.MaxRequestBytes != 4194304 {
			t.Fatalf("default MaxRequestBytes want 4194304, got %d", got.MaxRequestBytes)
		}
		if got.ConfigGeneration < 1 {
			t.Fatalf("default ConfigGeneration want >= 1, got %d", got.ConfigGeneration)
		}
	})
}

func TestOutputGuardSettingsExplicitFalseTriStateAndRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "output-guard-tristate.db")
	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) error = %v", err)
	}

	ctx := context.Background()
	disabled := false
	updated, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
		Enabled: &disabled,
	})
	if err != nil {
		t.Fatalf("UpdateOutputGuardSettings error = %v", err)
	}
	if updated.Enabled == nil || *updated.Enabled != false {
		t.Fatalf("expected updated Enabled=false, got %v", updated.Enabled)
	}
	_ = db.Close()

	// 重新打开数据库，断言显式 false 得到正确持久化恢复
	restartedDB, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	t.Cleanup(func() { _ = restartedDB.Close() })

	readBack, err := restartedDB.GetOutputGuardSettings(ctx)
	if err != nil {
		t.Fatalf("readBack error = %v", err)
	}
	if readBack.Enabled == nil || *readBack.Enabled != false {
		t.Fatalf("expected readBack Enabled=false, got %v", readBack.Enabled)
	}
}

func TestOutputGuardSettingsNullMissingFallbackToFalse(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, output_guard_enabled)
			VALUES (1, NULL)
			ON CONFLICT (id) DO UPDATE SET output_guard_enabled = NULL
		`); err != nil {
			t.Fatalf("seed NULL enabled: %v", err)
		}

		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatalf("GetOutputGuardSettings error = %v", err)
		}
		if got.Enabled == nil || *got.Enabled != false {
			t.Fatalf("NULL enabled want fallback false, got %v", got.Enabled)
		}
	})
}

func TestOutputGuardSettingsValidationBoundariesAndRollback(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		before, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}

		invalidCases := []struct {
			name   string
			update OutputGuardSettingsUpdate
		}{
			{"tool < 16KiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(16383))}},
			{"tool > 4MiB", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(4194305))}},
			{"deltas < 256", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(255)}},
			{"deltas > 65536", OutputGuardSettingsUpdate{MaxToolArgumentDeltas: ptr(65537)}},
			{"channel < 128KiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(131071))}},
			{"channel > 16MiB", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(16777217))}},
			{"request < 256KiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(262143))}},
			{"request > 64MiB", OutputGuardSettingsUpdate{MaxRequestBytes: ptr(int64(67108865))}},
			{"tool > channel", OutputGuardSettingsUpdate{MaxToolArgumentBytes: ptr(int64(3000000)), MaxChannelBytes: ptr(int64(2097152))}},
			{"channel > request", OutputGuardSettingsUpdate{MaxChannelBytes: ptr(int64(5000000)), MaxRequestBytes: ptr(int64(4194304))}},
		}

		for _, tc := range invalidCases {
			_, err := db.UpdateOutputGuardSettings(ctx, tc.update)
			if !errors.Is(err, ErrInvalidOutputGuardSettings) {
				t.Fatalf("[%s] want ErrInvalidOutputGuardSettings, got %v", tc.name, err)
			}
		}

		after, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if after.ConfigGeneration != before.ConfigGeneration || after.MaxToolArgumentBytes != before.MaxToolArgumentBytes {
			t.Fatalf("invalid update didn't rollback: before=%+v, after=%+v", before, after)
		}
	})
}

func TestOutputGuardSettingsNarrowUpdateNotOverwrittenBySystemSettings(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		toolBytes, channelBytes, requestBytes := int64(262144), int64(2097152), int64(4194304)
		disabled := false
		committed, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			Enabled:              &disabled,
			MaxToolArgumentBytes: &toolBytes,
			MaxChannelBytes:      &channelBytes,
			MaxRequestBytes:      &requestBytes,
		})
		if err != nil {
			t.Fatalf("UpdateOutputGuardSettings error = %v", err)
		}

		if err := db.UpdateSystemSettings(ctx, &SystemSettings{SiteName: "stale full update"}); err != nil {
			t.Fatalf("UpdateSystemSettings error = %v", err)
		}

		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatalf("GetOutputGuardSettings error = %v", err)
		}
		if got.Enabled == nil || *got.Enabled != false || got.MaxToolArgumentBytes != committed.MaxToolArgumentBytes {
			t.Fatalf("OutputGuardSettings overwritten by UpdateSystemSettings: got %+v, want %+v", got, committed)
		}
	})
}

func TestOutputGuardSettingsGenerationOverflowRollback(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, output_guard_config_generation)
			VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET output_guard_config_generation = $1
		`, int64(math.MaxInt64)); err != nil {
			t.Fatalf("seed max generation: %v", err)
		}

		toolBytes := int64(65536)
		_, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			MaxToolArgumentBytes: &toolBytes,
		})
		if !errors.Is(err, ErrOutputGuardGenerationOverflow) {
			t.Fatalf("expected ErrOutputGuardGenerationOverflow, got %v", err)
		}

		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ConfigGeneration != math.MaxInt64 {
			t.Fatalf("generation after overflow = %d, want math.MaxInt64", got.ConfigGeneration)
		}
	})
}

func TestOutputGuardSettingsConcurrentDisjointUpdatesDoNotLoseChanges(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		var wg sync.WaitGroup
		errCh := make(chan error, 10)

		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				tb, cb, rb := int64(131072+(idx+1)*1024), int64(2097152+(idx+1)*1024), int64(4194304+(idx+1)*1024)
				_, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
					MaxToolArgumentBytes: &tb,
					MaxChannelBytes:      &cb,
					MaxRequestBytes:      &rb,
				})
				if err != nil {
					errCh <- err
				}
			}(i)
		}

		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("concurrent update failed: %v", err)
		}

		got, err := db.GetOutputGuardSettings(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if got.ConfigGeneration < 11 {
			t.Fatalf("expected generation >= 11 after 10 concurrent updates, got %d", got.ConfigGeneration)
		}
	})
}

func TestOutputGuardSettingsRestoreAllDefaults(t *testing.T) {
	runOnAllOutputGuardDBDrivers(t, func(t *testing.T, db *DB) {
		ctx := context.Background()
		disabled := false
		customTool, customDeltas, customChannel, customRequest := int64(524288), 8192, int64(4194304), int64(8388608)
		_, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			Enabled:               &disabled,
			MaxToolArgumentBytes:  &customTool,
			MaxToolArgumentDeltas: &customDeltas,
			MaxChannelBytes:       &customChannel,
			MaxRequestBytes:       &customRequest,
		})
		if err != nil {
			t.Fatalf("setup custom settings error: %v", err)
		}

		def := DefaultOutputGuardSettings()
		restored, err := db.UpdateOutputGuardSettings(ctx, OutputGuardSettingsUpdate{
			Enabled:               def.Enabled,
			MaxToolArgumentBytes:  &def.MaxToolArgumentBytes,
			MaxToolArgumentDeltas: &def.MaxToolArgumentDeltas,
			MaxChannelBytes:       &def.MaxChannelBytes,
			MaxRequestBytes:       &def.MaxRequestBytes,
		})
		if err != nil {
			t.Fatalf("restore error: %v", err)
		}

		if restored.Enabled == nil || *restored.Enabled != false ||
			restored.MaxToolArgumentBytes != 262144 || restored.MaxToolArgumentDeltas != 4096 ||
			restored.MaxChannelBytes != 2097152 || restored.MaxRequestBytes != 4194304 {
			t.Fatalf("restored mismatch: %+v", restored)
		}
	})
}

func ptr[T any](v T) *T { return &v }
