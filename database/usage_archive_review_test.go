package database

import (
	"context"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

func TestUsageStatsSummaryCombinesArchivedFirstTokenSamples(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	start := time.Now().UTC().Truncate(time.Hour)
	for _, firstToken := range []int{100, 100, 0} {
		if _, err := db.conn.ExecContext(t.Context(), `INSERT INTO usage_logs (created_at, model, status_code, first_token_ms, channel)
			VALUES ($1, 'test-model', 200, $2, 'codex')`, start.Add(time.Minute), firstToken); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.rebuildUsageStatsRollup(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, archived := range []bool{false, true} {
		if archived {
			if _, err := db.conn.ExecContext(t.Context(), `INSERT INTO usage_account_hourly_rollups
				(bucket_start, channel, model, status_code, requests, first_token_ms_sum, first_token_samples)
				VALUES ($1, 'codex', 'test-model', 200, 1, 900, 1)`, start.Unix()); err != nil {
				t.Fatal(err)
			}
		}
		for _, driver := range []string{"sqlite", "postgres"} {
			queryDB := &DB{conn: db.conn, driver: driver}
			for _, explicitRange := range []bool{false, true} {
				rangeStart, rangeEnd := time.Time{}, time.Time{}
				if explicitRange {
					rangeStart, rangeEnd = start, start.Add(time.Hour)
				}
				stats, err := queryDB.GetUsageStatsSummary(t.Context(), rangeStart, rangeEnd, "codex")
				if err != nil {
					t.Fatalf("driver=%s archived=%t explicit=%t: %v", driver, archived, explicitRange, err)
				}
				want := 100.0
				if archived && explicitRange {
					want = 1100.0 / 3
				}
				if math.Abs(stats.AvgFirstTokenMs-want) > 0.001 {
					t.Fatalf("driver=%s archived=%t explicit=%t: average=%f want=%f", driver, archived, explicitRange, stats.AvgFirstTokenMs, want)
				}
			}
		}
	}
}

func TestPostgresBillingWindowMigrationUsesTransactionLock(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	config, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	adminConnection := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = adminConnection.Close() })
	schema := fmt.Sprintf("pr572_migration_%d", time.Now().UnixNano())
	if _, err := adminConnection.ExecContext(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := adminConnection.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Errorf("clean up test schema: %v", err)
		}
	})
	config.RuntimeParams["search_path"] = schema
	connection := stdlib.OpenDB(*config)
	t.Cleanup(func() { _ = connection.Close() })
	db := &DB{conn: connection, driver: "postgres"}
	if _, err := connection.ExecContext(t.Context(), `CREATE TABLE usage_account_billing_window_rollups (
		account_id BIGINT, window_start BIGINT, account_billed DOUBLE PRECISION, PRIMARY KEY (account_id, window_start))`); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Hour)
	if _, err := connection.ExecContext(t.Context(), `INSERT INTO usage_account_billing_window_rollups VALUES (1, $1, 12), (1, $2, 3)`, start.UnixNano(), start.Add(time.Second).UnixNano()); err != nil {
		t.Fatal(err)
	}
	blocker, err := connection.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback() })
	if _, err := blocker.ExecContext(t.Context(), `SELECT pg_advisory_xact_lock($1)`, billingWindowMigrationLockID); err != nil {
		t.Fatal(err)
	}
	waitContext, cancelWait := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancelWait()
	if err := db.migrateUsageAccountBillingWindowRollupKeys(waitContext); err == nil || waitContext.Err() != context.DeadlineExceeded {
		t.Fatalf("migration did not wait for transaction lock: %v", err)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	results := make(chan error, 2)
	for range 2 {
		go func() { results <- db.migrateUsageAccountBillingWindowRollupKeys(ctx) }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var key, count int64
	var billed float64
	if err := connection.QueryRowContext(ctx, `SELECT MIN(window_start), COUNT(*), SUM(account_billed) FROM usage_account_billing_window_rollups`).Scan(&key, &count, &billed); err != nil {
		t.Fatal(err)
	}
	if key != start.Unix() || count != 1 || billed != 15 {
		t.Fatalf("migration result key=%d rows=%d billed=%f", key, count, billed)
	}
}
