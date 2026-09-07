package database

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestReviewCapabilitySnapshotsPurgedInSQLite(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		db := newPromptPolicySQLiteTestDB(t)
		ctx := context.Background()
		id, err := db.InsertAccount(ctx, "snapshot", "test-rt", "")
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.SaveModelCapabilities(ctx, ModelCapabilitySnapshot{AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: 1, Models: map[string]map[string]json.RawMessage{"model": {"context_window": json.RawMessage("1000")}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET status='deleted', deleted_at=CURRENT_TIMESTAMP WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		if bulk {
			_, err = db.PurgeDeletedAccounts(ctx)
		} else {
			err = db.PurgeAccount(ctx, id)
		}
		if err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.conn.QueryRowContext(ctx, `SELECT count(*) FROM model_capability_snapshots WHERE account_id=$1`, id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan snapshot remains: count=%d err=%v", count, err)
		}
	}
}

func TestReviewDailyUsageLegacyColumns(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	testReviewDailyUsageLegacyColumns(t, db)
}

// Only use with a disposable test database. Recreate the pre-breakdown table
// with an existing row so ALTER branches, defaults and preservation are tested.
func testReviewDailyUsageLegacyColumns(t *testing.T, db *DB) {
	t.Helper()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `DROP TABLE IF EXISTS account_daily_usage`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE account_daily_usage (
 account_id BIGINT NOT NULL, day VARCHAR(10) NOT NULL,
 credits DOUBLE PRECISION NOT NULL DEFAULT 0, users INT NOT NULL DEFAULT 0,
 threads INT NOT NULL DEFAULT 0, turns INT NOT NULL DEFAULT 0,
 uncached_input_tokens BIGINT NOT NULL DEFAULT 0, cached_input_tokens BIGINT NOT NULL DEFAULT 0,
 output_tokens BIGINT NOT NULL DEFAULT 0, total_tokens BIGINT NOT NULL DEFAULT 0,
 settled BOOLEAN NOT NULL DEFAULT FALSE, clients_json TEXT NOT NULL DEFAULT '[]',
 models_json TEXT NOT NULL DEFAULT '[]', synced_at TIMESTAMP NOT NULL,
 PRIMARY KEY (account_id, day))`); err != nil {
		t.Fatal(err)
	}
	day := time.Now().UTC().Format("2006-01-02")
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO account_daily_usage(account_id, day, credits, turns, settled, synced_at) VALUES(1,$1,25,2,TRUE,CURRENT_TIMESTAMP)`, day); err != nil {
		t.Fatal(err)
	}
	accountDailyUsageSchemaMu.Lock()
	delete(accountDailyUsageSchemaReady, db)
	accountDailyUsageSchemaMu.Unlock()
	for i := 0; i < 2; i++ {
		if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.ListAccountDailyUsage(ctx, 1, 7)
	if err != nil || len(rows) != 1 {
		t.Fatalf("legacy row lost: %+v %v", rows, err)
	}
	if rows[0].Credits != 25 || rows[0].BreakdownPercent != 0 || rows[0].BreakdownJSON != "[]" || rows[0].SurfacesJSON != "{}" {
		t.Fatalf("incorrect legacy defaults: %+v", rows[0])
	}
	if err := db.UpsertAccountDailyBreakdown(ctx, AccountDailyBreakdownInput{AccountID: 1, Day: day, Percent: 50, Surfaces: map[string]float64{"cli": 50}}); err != nil {
		t.Fatal(err)
	}
	rows, err = db.ListAccountDailyUsage(ctx, 1, 7)
	if err != nil || rows[0].Credits != 25 || rows[0].BreakdownPercent != 50 {
		t.Fatalf("breakdown overwrote counts: %+v %v", rows, err)
	}
}
