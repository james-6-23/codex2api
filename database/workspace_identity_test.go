package database

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"testing"
)

func workspaceIdentityJWT(t *testing.T, email, workspaceID string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]interface{}{
		"email": email,
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": workspaceID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "header." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestSQLiteWorkspaceIdentityV3(t *testing.T) {
	ctx := context.Background()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	if _, err := db.conn.ExecContext(ctx, `DELETE FROM data_migrations WHERE version = $1`, dataMigrationWorkspaceIdentityV3); err != nil {
		t.Fatalf("clear migration marker: %v", err)
	}

	backfillID, err := db.InsertAccountWithCredentials(ctx, "backfill", map[string]interface{}{
		"refresh_token": "rt-backfill",
		"email":         "User@Example.com",
		"id_token":      workspaceIdentityJWT(t, "user@example.com", "workspace-1"),
	}, "")
	if err != nil {
		t.Fatalf("insert backfill account: %v", err)
	}
	duplicateID, err := db.InsertAccountWithCredentials(ctx, "duplicate", map[string]interface{}{
		"refresh_token":   "rt-duplicate",
		"email":           "user@example.com",
		"workspace_id":    "workspace-1",
		"allow_duplicate": "true",
	}, "")
	if err != nil {
		t.Fatalf("insert duplicate account: %v", err)
	}

	var unknownIDs []int64
	for _, token := range []string{"rt-unknown-1", "rt-unknown-2"} {
		id, err := db.InsertAccountWithCredentials(ctx, token, map[string]interface{}{
			"refresh_token": token,
			"email":         "unknown@example.com",
		}, "")
		if err != nil {
			t.Fatalf("insert unknown account: %v", err)
		}
		unknownIDs = append(unknownIDs, id)
	}

	mismatchID, err := db.InsertAccountWithCredentials(ctx, "mismatch", map[string]interface{}{
		"refresh_token": "rt-mismatch",
		"email":         "stored@example.com",
		"id_token":      workspaceIdentityJWT(t, "token@example.com", "workspace-mismatch"),
	}, "")
	if err != nil {
		t.Fatalf("insert mismatch account: %v", err)
	}
	deletedID, err := db.InsertAccountWithCredentials(ctx, "deleted", map[string]interface{}{
		"refresh_token": "rt-deleted",
		"email":         "deleted@example.com",
		"id_token":      workspaceIdentityJWT(t, "deleted@example.com", "workspace-deleted"),
	}, "")
	if err != nil {
		t.Fatalf("insert deleted account: %v", err)
	}
	if err := db.SoftDeleteAccount(ctx, deletedID); err != nil {
		t.Fatalf("soft delete account: %v", err)
	}
	agentID, err := db.InsertAccountWithCredentials(ctx, "agent", map[string]interface{}{
		"email":        "user@example.com",
		"workspace_id": "workspace-1",
		"auth_mode":    "agentIdentity",
	}, "")
	if err != nil {
		t.Fatalf("insert agent account: %v", err)
	}

	if err := db.runDataMigrationsWithTimeout(); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	var activeDuplicates int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts WHERE id IN ($1, $2) AND status <> 'deleted'`, backfillID, duplicateID).Scan(&activeDuplicates); err != nil {
		t.Fatalf("count workspace duplicates: %v", err)
	}
	if activeDuplicates != 1 {
		t.Fatalf("active workspace duplicates = %d, want 1", activeDuplicates)
	}
	backfilled, err := db.GetAccountByIDIncludingDeleted(ctx, backfillID)
	if err != nil {
		t.Fatalf("get backfilled account: %v", err)
	}
	if got := backfilled.GetCredential("workspace_id"); got != "workspace-1" {
		t.Fatalf("backfilled workspace_id = %q, want workspace-1", got)
	}

	for _, id := range append(unknownIDs, agentID) {
		if _, err := db.GetAccountByID(ctx, id); err != nil {
			t.Fatalf("account %d should remain active: %v", id, err)
		}
	}
	for _, id := range []int64{mismatchID, deletedID} {
		row, err := db.GetAccountByIDIncludingDeleted(ctx, id)
		if err != nil {
			t.Fatalf("get account %d: %v", id, err)
		}
		if got := row.GetCredential("workspace_id"); got != "" {
			t.Fatalf("account %d workspace_id = %q, want empty", id, got)
		}
	}
}
