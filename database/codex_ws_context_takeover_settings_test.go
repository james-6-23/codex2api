package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexWSContextTakeoverSettingsSQLiteDefaultsAndMigration(test *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(test.TempDir(), "ws-context-takeover.db")
	db, err := New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	if _, err := db.conn.ExecContext(ctx, "INSERT INTO system_settings (id) VALUES (1)"); err != nil {
		test.Fatal(err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexWSContextTakeover {
		test.Fatalf("fresh setting must default off: settings=%+v err=%v", settings, err)
	}
	if _, err := db.conn.ExecContext(ctx, "UPDATE system_settings SET codex_ws_context_takeover = NULL WHERE id = 1"); err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexWSContextTakeover {
		test.Fatalf("null setting must default off: settings=%+v err=%v", settings, err)
	}
	settings.SiteName = "existing installation"
	settings.CodexImagesMainModel = "gpt-5.6-luna"
	settings.CodexTelemetryEnabled = true
	settings.CodexSessionFailoverEnabled = true
	settings.CodexRequestCompression = true
	settings.CodexWSContextTakeover = true
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		test.Fatal(err)
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || !settings.CodexWSContextTakeover {
		test.Fatalf("restart or repeated migration lost saved context takeover: settings=%+v err=%v", settings, err)
	}
	if _, err := db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN codex_ws_context_takeover"); err != nil {
		test.Fatal(err)
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", databasePath)
	if err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil {
		test.Fatalf("read migrated settings: %v", err)
	}
	if settings.CodexWSContextTakeover || settings.SiteName != "existing installation" || settings.CodexImagesMainModel != "gpt-5.6-luna" || !settings.CodexTelemetryEnabled || !settings.CodexSessionFailoverEnabled || !settings.CodexRequestCompression {
		test.Fatal("migration enabled context takeover or changed existing settings")
	}
	testCodexWSContextTakeoverSettingsRoundTrip(test, db)
}

func TestCodexWSContextTakeoverSettingsSQLiteInsert(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "ws-context-takeover-insert.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil || settings != nil {
		test.Fatalf("expected an empty settings table: settings=%+v err=%v", settings, err)
	}
	testCodexWSContextTakeoverSettingsRoundTrip(test, db)
}

func TestCodexWSContextTakeoverSettingsPostgres(test *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		test.Skip("requires an isolated CODEX2API_TEST_POSTGRES_DSN database")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	settings := &SystemSettings{SiteName: "existing installation", CodexWSContextTakeover: true, CodexSessionFailoverEnabled: true}
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		test.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, "ALTER TABLE system_settings DROP COLUMN codex_ws_context_takeover"); err != nil {
		test.Fatal(err)
	}
	if err := db.migrate(ctx); err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexWSContextTakeover || settings.SiteName != "existing installation" || !settings.CodexSessionFailoverEnabled {
		test.Fatalf("legacy PostgreSQL migration changed settings or enabled context takeover: settings=%+v err=%v", settings, err)
	}
	if _, err := db.conn.ExecContext(ctx, "UPDATE system_settings SET codex_ws_context_takeover = NULL WHERE id = 1"); err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || settings.CodexWSContextTakeover {
		test.Fatalf("PostgreSQL null must default off: settings=%+v err=%v", settings, err)
	}
	testCodexWSContextTakeoverSettingsRoundTrip(test, db)
	settings.CodexWSContextTakeover = true
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		test.Fatal(err)
	}
	if err := db.migrate(ctx); err != nil {
		test.Fatal(err)
	}
	settings, err = db.GetSystemSettings(ctx)
	if err != nil || settings == nil || !settings.CodexWSContextTakeover {
		test.Fatalf("repeated PostgreSQL migration lost context takeover: settings=%+v err=%v", settings, err)
	}
}

func testCodexWSContextTakeoverSettingsRoundTrip(test *testing.T, db *DB) {
	test.Helper()
	ctx := context.Background()
	for _, enabled := range []bool{true, false} {
		for _, preservePatterns := range []bool{true, false} {
			for _, preserveKey := range []bool{true, false} {
				test.Run(fmt.Sprintf("enabled=%t/patterns=%t/key=%t", enabled, preservePatterns, preserveKey), func(test *testing.T) {
					settings := &SystemSettings{
						CodexWSContextTakeover:      !enabled,
						CodexSessionFailoverEnabled: preservePatterns,
						CodexRequestCompression:     preserveKey,
						CodexImagesMainModel:        "gpt-5.6-luna",
						CodexTelemetryEnabled:       true,
						PromptFilterCustomPatterns:  "[]",
						PromptFilterReviewAPIKey:    "original-key",
					}
					if err := db.UpdateSystemSettings(ctx, settings); err != nil {
						test.Fatal(err)
					}
					inserted, err := db.GetSystemSettings(ctx)
					if err != nil || inserted == nil || inserted.CodexWSContextTakeover != !enabled || inserted.CodexSessionFailoverEnabled != preservePatterns || inserted.CodexRequestCompression != preserveKey {
						test.Fatalf("initial write did not preserve independent settings: settings=%+v err=%v", inserted, err)
					}
					settings.CodexWSContextTakeover = enabled
					settings.PromptFilterCustomPatterns = `[{"id":"new","pattern":"new"}]`
					settings.PromptFilterReviewAPIKey = "new-key"
					settings.PreservePromptFilterCustomPatterns = preservePatterns
					settings.PreservePromptFilterReviewAPIKey = preserveKey
					if err := db.UpdateSystemSettings(ctx, settings); err != nil {
						test.Fatal(err)
					}
					persisted, err := db.GetSystemSettings(ctx)
					if err != nil || persisted == nil {
						test.Fatalf("read updated settings: %v", err)
					}
					if persisted.CodexWSContextTakeover != enabled || persisted.CodexImagesMainModel != settings.CodexImagesMainModel || !persisted.CodexTelemetryEnabled || persisted.CodexSessionFailoverEnabled != preservePatterns || persisted.CodexRequestCompression != preserveKey {
						test.Fatal("context takeover did not round trip independently of adjacent SQL parameters")
					}
					wantPatterns, wantKey := settings.PromptFilterCustomPatterns, settings.PromptFilterReviewAPIKey
					if preservePatterns {
						wantPatterns = "[]"
					}
					if preserveKey {
						wantKey = "original-key"
					}
					if persisted.PromptFilterCustomPatterns != wantPatterns || persisted.PromptFilterReviewAPIKey != wantKey {
						test.Fatal("prompt filter preservation guards no longer match their SQL parameters")
					}
				})
			}
		}
	}
}
