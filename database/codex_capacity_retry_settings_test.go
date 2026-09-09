package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteCapacityRetryMigrationDefaultsOff(test *testing.T) {
	databasePath := filepath.Join(test.TempDir(), "capacity.db")
	db, err := New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	if err := db.UpdateSystemSettings(context.Background(), &SystemSettings{SiteName: "existing installation", CodexImagesMainModel: "gpt-5.6-luna"}); err != nil {
		test.Fatal(err)
	}
	if _, err := db.conn.ExecContext(context.Background(), "ALTER TABLE system_settings DROP COLUMN codex_capacity_retry_enabled"); err != nil {
		test.Fatal(err)
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	if settings.CodexCapacityRetryEnabled || settings.SiteName != "existing installation" || settings.CodexImagesMainModel != "gpt-5.6-luna" {
		test.Fatal("migration changed existing settings or enabled retries")
	}
	for _, enabled := range []bool{true, false} {
		settings.CodexCapacityRetryEnabled = enabled
		if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
			test.Fatal(err)
		}
		settings, err = db.GetSystemSettings(context.Background())
		if err != nil || settings.CodexCapacityRetryEnabled != enabled || settings.CodexImagesMainModel != "gpt-5.6-luna" {
			test.Fatalf("persisted retry toggle != %t: %v", enabled, err)
		}
	}
}
