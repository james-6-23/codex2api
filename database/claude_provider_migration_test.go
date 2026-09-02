package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestBackfillClaudeProviderDataIsConservative(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "claude-provider-migration.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	claudeID, err := db.InsertAccountWithUpstream(ctx, "claude", "anthropic", "oauth", map[string]interface{}{
		"upstream_type": "claude",
		"access_token":  "claude-token",
		"refresh_token": "claude-refresh",
	}, "")
	if err != nil {
		t.Fatalf("insert Claude account: %v", err)
	}
	codexID, err := db.InsertAccountWithUpstream(ctx, "codex", "openai", "oauth", map[string]interface{}{
		"upstream_type": "codex",
		"access_token":  "codex-token",
	}, "")
	if err != nil {
		t.Fatalf("insert Codex account: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `
		INSERT INTO usage_logs (account_id, credential_generation, channel, endpoint, model, status_code)
		VALUES (?, ?, 'codex', '/v1/messages', 'claude-sonnet-4-5', 200),
		       (?, ?, 'codex', '/v1/responses', 'gpt-5.4', 200),
		       (?, ?, 'codex', '/v1/messages', 'claude-sonnet-4-5', 200)`,
		claudeID, 1, claudeID, 1, claudeID, 999); err != nil {
		t.Fatalf("insert usage fixtures: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `
		INSERT INTO usage_logs (account_id, credential_generation, channel, endpoint, model, status_code)
		VALUES (?, ?, 'codex', '/v1/messages', 'claude-sonnet-4-5', 200)`, codexID, 1); err != nil {
		t.Fatalf("insert Codex usage fixture: %v", err)
	}

	pureClaude, err := db.CreateAccountGroup(ctx, "pure-claude", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("create Claude group: %v", err)
	}
	mixed, err := db.CreateAccountGroup(ctx, "mixed", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("create mixed group: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_group_members (account_id, group_id) VALUES (?, ?), (?, ?)`, claudeID, pureClaude, claudeID, mixed); err != nil {
		t.Fatalf("insert Claude group memberships: %v", err)
	}
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_group_members (account_id, group_id) VALUES (?, ?)`, codexID, mixed); err != nil {
		t.Fatalf("insert mixed group membership: %v", err)
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin migration transaction: %v", err)
	}
	if err := db.backfillClaudeProviderData(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatalf("backfillClaudeProviderData: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit migration transaction: %v", err)
	}

	rows, err := db.conn.QueryContext(ctx, `SELECT account_id, credential_generation, channel FROM usage_logs ORDER BY id`)
	if err != nil {
		t.Fatalf("read usage fixtures: %v", err)
	}
	defer rows.Close()
	var channels []string
	for rows.Next() {
		var accountID, generation int64
		var channel string
		if err := rows.Scan(&accountID, &generation, &channel); err != nil {
			t.Fatal(err)
		}
		channels = append(channels, channel)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got, want := channels, []string{"claude", "codex", "codex", "codex"}; len(got) != len(want) {
		t.Fatalf("migrated channels = %v, want %v", got, want)
	} else {
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("migrated channels = %v, want %v", got, want)
			}
		}
	}

	groups, err := db.ListAccountGroups(ctx)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	for _, group := range groups {
		switch group.ID {
		case pureClaude:
			if group.Channel != AccountGroupChannelClaude {
				t.Fatalf("pure Claude group channel = %q, want claude", group.Channel)
			}
		case mixed:
			if group.Channel != AccountGroupChannelCodex {
				t.Fatalf("mixed group channel = %q, want codex", group.Channel)
			}
		}
	}
}
