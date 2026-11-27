package database

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCodexRefreshTransactions(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "refresh.db")
			if driver == "postgres" {
				dsn = os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
				}
			}
			db, err := New(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			rt := "old-" + uuid.NewString()
			var ids []int64
			for range 3 {
				id, err := db.InsertAccountWithCredentials(ctx, "refresh-transaction", map[string]any{"refresh_token": rt, "access_token": "old-at", "custom_headers": map[string]string{"Chatgpt-Account-Id": "keep-route"}}, "")
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, id)
			}
			first, err := db.GetAccountByID(ctx, ids[0])
			if err != nil {
				t.Fatal(err)
			}
			attempt, err := db.BeginCodexRefresh(ctx, ids[0], first.CredentialGeneration, rt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.BeginCodexRefresh(ctx, ids[1], first.CredentialGeneration, rt); !errors.Is(err, ErrCodexRefreshUncertain) {
				t.Fatalf("duplicate consumer admitted: %v", err)
			}
			// A replacement using the same RT but a different AT still advances
			// identity generation and must not be overwritten by an older result.
			if err := db.UpdateCredentials(ctx, ids[2], map[string]any{"access_token": "admin-at"}); err != nil {
				t.Fatal(err)
			}
			// A new 401 on a sibling route during the exchange remains durable.
			until := time.Now().UTC().Add(-time.Hour)
			if err := db.SetCooldownWithError(ctx, ids[1], "unauthorized", until, "sibling rejected"); err != nil {
				t.Fatal(err)
			}
			published, err := db.FinishCodexRefresh(ctx, attempt, map[string]any{"access_token": "new-at", "refresh_token": "new-" + rt})
			if err != nil || len(published) != 2 {
				t.Fatalf("publish count %d: %v", len(published), err)
			}
			for _, id := range ids[:2] {
				row, err := db.GetAccountByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if row.GetCredential("refresh_token") != "new-"+rt || row.GetCredentialStringMap("custom_headers")["Chatgpt-Account-Id"] != "keep-route" {
					t.Fatal("atomic shared route publication failed")
				}
			}
			quarantined, err := db.GetAccountByID(ctx, ids[1])
			if err != nil || quarantined.CooldownReason != "unauthorized" || !quarantined.CooldownUntil.Valid || quarantined.ErrorMessage != "sibling rejected" {
				t.Fatal("rotation changed the sibling's 401", err)
			}
			for _, token := range []string{"old-at", "new-at"} {
				cleared, err := db.ClearCodexAuthorizationCooldown(ctx, ids[1], quarantined.CredentialGeneration, token, until, "sibling rejected")
				if err != nil || cleared != (token == "new-at") {
					t.Fatalf("authorization cooldown comparison: changed=%t current_bearer=%t err=%v", cleared, token == "new-at", err)
				}
			}
			replaced, err := db.GetAccountByID(ctx, ids[2])
			if err != nil {
				t.Fatal(err)
			}
			if replaced.GetCredential("access_token") != "admin-at" {
				t.Fatal("administrative replacement overwritten")
			}
			if _, err := db.BeginCodexRefresh(ctx, ids[2], replaced.CredentialGeneration, rt); !errors.Is(err, ErrCodexRefreshUncertain) {
				t.Fatalf("old RT reuse after conflicting replacement: %v", err)
			}
			// A complete transaction can be retried after an unknown commit result.
			newAttempt, err := db.BeginCodexRefresh(ctx, ids[0], first.CredentialGeneration+1, "new-"+rt)
			if err != nil {
				t.Fatal(err)
			}
			updates := map[string]any{"access_token": "next-at", "refresh_token": "next-" + rt}
			for range 2 {
				published, err = db.FinishCodexRefresh(ctx, newAttempt, updates)
				if err != nil || len(published) != 2 {
					t.Fatalf("idempotent finish: count=%d err=%v", len(published), err)
				}
			}
			testCodexKeepaliveSettings(t, db)
		})
	}
}

func testCodexKeepaliveSettings(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO system_settings(id) VALUES (1) ON CONFLICT(id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings.CodexOAuthKeepaliveEnabled || settings.ClaudeOAuthKeepaliveEnabled {
		t.Fatal("oauth keepalive settings must default off")
	}
	for _, enabled := range []bool{true, false} {
		settings.CodexOAuthKeepaliveEnabled = enabled
		settings.ClaudeOAuthKeepaliveEnabled = enabled
		settings.PreservePromptFilterCustomPatterns = true
		settings.PreservePromptFilterReviewAPIKey = true
		if err := db.UpdateSystemSettings(ctx, settings); err != nil {
			t.Fatal(err)
		}
		stored, err := db.GetSystemSettings(ctx)
		if err != nil || stored.CodexOAuthKeepaliveEnabled != enabled || stored.ClaudeOAuthKeepaliveEnabled != enabled {
			t.Fatalf("keepalive setting lost: codex=%t claude=%t err=%v", stored.CodexOAuthKeepaliveEnabled, stored.ClaudeOAuthKeepaliveEnabled, err)
		}
	}
}

func TestCodexRefreshEncryptedAndPlaintextPeers(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "encrypted-refresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	setCredEncryptionKeyForTest("")
	t.Cleanup(func() { setCredEncryptionKeyForTest("") })
	plain, err := db.InsertAccountWithCredentials(ctx, "plaintext", map[string]any{"refresh_token": "shared-rt", "access_token": "old"}, "")
	if err != nil {
		t.Fatal(err)
	}
	setCredEncryptionKeyForTest("codex-refresh-test")
	encrypted, err := db.InsertAccountWithCredentials(ctx, "encrypted", map[string]any{"refresh_token": "shared-rt", "access_token": "old"}, "")
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := db.BeginCodexRefresh(ctx, plain, 1, "shared-rt")
	if err != nil {
		t.Fatal(err)
	}
	ids, err := db.FinishCodexRefresh(ctx, attempt, map[string]any{"refresh_token": "rotated-rt", "access_token": "new", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
	if err != nil || len(ids) != 2 {
		t.Fatalf("mixed credential rotation: %v, count %d", err, len(ids))
	}
	for _, id := range []int64{plain, encrypted} {
		row, err := db.GetAccountByID(ctx, id)
		if err != nil || row.GetCredential("refresh_token") != "rotated-rt" {
			t.Fatal("rotated encrypted RT not readable")
		}
		var raw string
		if err := db.conn.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id=$1`, id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(raw, "rotated-rt") {
			t.Fatal("rotated RT stored in plaintext")
		}
	}
}
