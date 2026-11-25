package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAPIKeyWeeklyResetSQLite(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "weekly.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runWeeklyResetSuite(t, db)
}

func TestAPIKeyWeeklyQuotaResetPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated CODEX2API_TEST_POSTGRES_DSN")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.ClearUsageLogs(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer db.ClearUsageLogs(context.Background())
	runWeeklyResetSuite(t, db)
}

func runWeeklyResetSuite(t *testing.T, db *DB) {
	t.Helper()
	for _, test := range []struct {
		name string
		run  func(*testing.T, *DB)
	}{
		{"preserves_bills_and_other_limits", testWeeklyResetPreservesBills},
		{"partial_usage_and_cap", testWeeklyResetBoundedCost},
		{"credit_ages_with_original_bill", testWeeklyResetRollingExpiry},
		{"idempotent_and_late_logs", testWeeklyResetIdempotency},
		{"ordinary_reset_compatibility", testWeeklyResetOrdinaryReset},
		{"log_clear_and_key_deletion", testWeeklyResetCleanup},
		{"concurrent_operations", testWeeklyResetConcurrent},
		{"validation_and_corrected_bills", testWeeklyResetValidation},
		{"committed_subsecond_usage", testWeeklyResetSubsecond},
	} {
		t.Run(test.name, func(t *testing.T) { test.run(t, db) })
	}
}

func newWeeklyResetKey(t *testing.T, db *DB) int64 {
	t.Helper()
	id, err := db.InsertAPIKeyWithOptions(context.Background(), APIKeyInput{
		Name: "weekly reset test", Key: fmt.Sprintf("sk-weekly-reset-%d", time.Now().UnixNano()),
		QuotaLimit: 1000, QuotaUsed: 400,
		Limits: APIKeyLimits{CostLimit5h: 150, CostLimit7d: 120, CostLimit30d: 480},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.DeleteAPIKey(context.Background(), id) })
	return id
}

func insertWeeklyResetLog(t *testing.T, db *DB, keyID int64, at time.Time, cost float64, tokens int64, status int) int64 {
	t.Helper()
	var id int64
	err := db.conn.QueryRow(`INSERT INTO usage_logs
		(api_key_id, account_id, status_code, total_tokens, user_billed, created_at)
		VALUES ($1, 77, $2, $3, $4, $5) RETURNING id`, keyID, status, tokens, cost, db.timeArg(at)).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func weeklyReset(t *testing.T, db *DB, keyID int64, requestID string) *APIKeyWeeklyQuotaReset {
	t.Helper()
	result, err := db.ResetAPIKeyWeeklyQuota(context.Background(), keyID, requestID)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func weeklyWindow(t *testing.T, db *DB, keyID int64, window time.Duration) *APIKeyWindowUsage {
	t.Helper()
	usage, err := db.GetAPIKeyWindowUsage(context.Background(), keyID, window)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func assertWeeklyCost(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("%s = %.12f, want %.12f", name, got, want)
	}
}

func testWeeklyResetPreservesBills(t *testing.T, db *DB) {
	ctx := context.Background()
	id := newWeeklyResetKey(t, db)
	now := time.Now().UTC().Truncate(time.Second)
	insertWeeklyResetLog(t, db, id, now.Add(-14*24*time.Hour), 360, 3600, 200)
	insertWeeklyResetLog(t, db, id, now.Add(-2*time.Hour), 80, 800, 200)
	insertWeeklyResetLog(t, db, id, now.Add(-time.Hour), 40, 400, 200)
	insertWeeklyResetLog(t, db, id, now.Add(-30*time.Minute), 0, 50, 200)
	insertWeeklyResetLog(t, db, id, now.Add(-15*time.Minute), 9, 999, 499)
	if _, err := db.conn.Exec(`UPDATE api_keys SET total_used = 900, reset_count = 2 WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	result := weeklyReset(t, db, id, "initial-week")
	assertWeeklyCost(t, "granted credit", result.ExemptedCost, 120)
	if result.Replayed || result.ID != id || result.ResetAt.IsZero() {
		t.Fatalf("unexpected reset result: %+v", result)
	}
	week := weeklyWindow(t, db, id, 7*24*time.Hour)
	if week.UserBilled != 0 || week.Requests != 0 || week.Tokens != 0 || week.OldestAt != nil {
		t.Fatalf("weekly usage was not fully reset: %+v", week)
	}
	month := weeklyWindow(t, db, id, 30*24*time.Hour)
	assertWeeklyCost(t, "raw month", month.UserBilled, 480)
	assertWeeklyCost(t, "counted month", month.QuotaCost(), 360)
	if month.Tokens != 4850 || month.Requests != 4 {
		t.Fatalf("monthly tokens/requests changed: %+v", month)
	}
	fiveHours := weeklyWindow(t, db, id, 5*time.Hour)
	if fiveHours.UserBilled != 120 || fiveHours.Tokens != 1250 || fiveHours.Requests != 3 {
		t.Fatalf("5h usage changed: %+v", fiveHours)
	}
	daily, err := db.GetAPIKeyUsageSince(ctx, id, now.Add(-24*time.Hour))
	if err != nil || daily.UserBilled != 120 || daily.Tokens != 1250 {
		t.Fatalf("daily usage changed: %+v, %v", daily, err)
	}
	scope, err := db.GetAPIKeyAccountWindowUsage(ctx, id, 7*24*time.Hour)
	if err != nil || scope[77].UserBilled != 120 || scope[77].Tokens != 1250 {
		t.Fatalf("scope usage changed: %+v, %v", scope, err)
	}
	key, err := db.GetAPIKeyByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if key.QuotaUsed != 400 || key.TotalUsed != 900 || key.ResetCount != 2 || key.LastResetAt.Valid ||
		key.Limits.CostLimit7d != 120 || key.Limits.CostLimit30d != 480 {
		t.Fatalf("ordinary quotas or limits changed: %+v", key)
	}
	for _, test := range []struct {
		window time.Duration
		want   float64
	}{{5 * time.Hour, 120}, {7 * 24 * time.Hour, 0}, {30 * 24 * time.Hour, 360}} {
		costs, err := db.GetAllAPIKeysWindowCost(ctx, test.window)
		if err != nil {
			t.Fatal(err)
		}
		assertWeeklyCost(t, "batch window cost", costs[id], test.want)
	}
	// A second week's spend reaches the original monthly cap without changing it.
	insertWeeklyResetLog(t, db, id, result.ResetAt, 120, 1200, 200)
	month = weeklyWindow(t, db, id, 30*24*time.Hour)
	assertWeeklyCost(t, "raw month after spending", month.UserBilled, 600)
	assertWeeklyCost(t, "counted month after spending", month.QuotaCost(), 480)
	assertWeeklyCost(t, "new weekly usage", weeklyWindow(t, db, id, 7*24*time.Hour).QuotaCost(), 120)
	if err := db.RebuildUsageHourlyRollup(ctx); err != nil {
		t.Fatal(err)
	}
	report, err := db.GetAPIKeySelfUsageReport(ctx, id, time.Time{}, time.Time{}, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	assertWeeklyCost(t, "historical report", report.Summary.UserBilled, 600)
	assertWeeklyCost(t, "report weekly cost", report.Windows.Last7d.QuotaCost(), 120)
	assertWeeklyCost(t, "report monthly cost", report.Windows.Last30d.QuotaCost(), 480)
	if report.Windows.Last30d.CostExempted != 120 || report.Windows.Last30d.DecayAt != nil ||
		report.Windows.Last30d.Tokens != 6050 || report.Summary.Requests != 5 {
		t.Fatalf("report lost original history or displayed a misleading decay: %+v", report)
	}
}

func testWeeklyResetBoundedCost(t *testing.T, db *DB) {
	for _, test := range []struct {
		name   string
		costs  []float64
		credit float64
	}{{"zero_cost_tokens", []float64{0}, 0}, {"partly_used", []float64{80}, 80}, {"over_weekly_cap", []float64{80, 70}, 120}} {
		t.Run(test.name, func(t *testing.T) {
			id := newWeeklyResetKey(t, db)
			at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
			var total float64
			var logIDs []int64
			for i, cost := range test.costs {
				logIDs = append(logIDs, insertWeeklyResetLog(t, db, id, at.Add(time.Duration(i)*time.Second), cost, 100, 200))
				total += cost
			}
			result := weeklyReset(t, db, id, "bounded-credit")
			assertWeeklyCost(t, "bounded credit", result.ExemptedCost, test.credit)
			week := weeklyWindow(t, db, id, 7*24*time.Hour)
			if week.Tokens != 0 || week.Requests != 0 || week.UserBilled != 0 {
				t.Fatalf("all snapshot usage must reset, even beyond the credit cap: %+v", week)
			}
			month := weeklyWindow(t, db, id, 30*24*time.Hour)
			assertWeeklyCost(t, "original bills", month.UserBilled, total)
			assertWeeklyCost(t, "bounded net cost", month.QuotaCost(), total-test.credit)
			if len(logIDs) == 2 {
				var first, second float64
				if err := db.conn.QueryRow(`SELECT exempted_cost FROM api_key_weekly_reset_usage WHERE api_key_id=$1 AND usage_log_id=$2`, id, logIDs[0]).Scan(&first); err != nil {
					t.Fatal(err)
				}
				if err := db.conn.QueryRow(`SELECT exempted_cost FROM api_key_weekly_reset_usage WHERE api_key_id=$1 AND usage_log_id=$2`, id, logIDs[1]).Scan(&second); err != nil {
					t.Fatal(err)
				}
				assertWeeklyCost(t, "oldest-first credit", first, 80)
				assertWeeklyCost(t, "partial final credit", second, 40)
			}
		})
	}
}

func testWeeklyResetRollingExpiry(t *testing.T, db *DB) {
	id := newWeeklyResetKey(t, db)
	now := time.Now().UTC().Truncate(time.Second)
	firstAt, secondAt := now.Add(-6*24*time.Hour), now.Add(-24*time.Hour)
	insertWeeklyResetLog(t, db, id, firstAt, 70, 700, 200)
	insertWeeklyResetLog(t, db, id, secondAt, 50, 500, 200)
	weeklyReset(t, db, id, "aging")
	insertWeeklyResetLog(t, db, id, now, 30, 300, 200)
	for _, test := range []struct {
		since       time.Time
		raw, credit float64
	}{{firstAt, 150, 120}, {firstAt.Add(time.Second), 80, 50}, {secondAt.Add(time.Second), 30, 0}} {
		usage, err := db.getAPIKeyUsageSince(context.Background(), id, test.since, 30*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		assertWeeklyCost(t, "remaining raw spend", usage.UserBilled, test.raw)
		assertWeeklyCost(t, "credit tied to original timestamp", usage.CostExempted, test.credit)
		assertWeeklyCost(t, "net cost at rolling boundary", usage.QuotaCost(), 30)
	}
}

func testWeeklyResetIdempotency(t *testing.T, db *DB) {
	id := newWeeklyResetKey(t, db)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	insertWeeklyResetLog(t, db, id, at, 80, 800, 200)
	first := weeklyReset(t, db, id, "retryable-operation")
	// A late log can share the exact timestamp of a snapshot log, or predate
	// the reset. Its identity still places it in the new weekly period.
	insertWeeklyResetLog(t, db, id, at, 25, 250, 200)
	replayed := weeklyReset(t, db, id, "retryable-operation")
	if !replayed.Replayed || !replayed.ResetAt.Equal(first.ResetAt) || replayed.ExemptedCost != 80 {
		t.Fatalf("idempotent result changed: first=%+v replay=%+v", first, replayed)
	}
	week := weeklyWindow(t, db, id, 7*24*time.Hour)
	if week.UserBilled != 25 || week.Tokens != 250 || week.Requests != 1 {
		t.Fatalf("retry reset late-arriving usage: %+v", week)
	}
	assertWeeklyCost(t, "second operation covers only new logs", weeklyReset(t, db, id, "next-operation").ExemptedCost, 25)
	assertWeeklyCost(t, "no repeated credit", weeklyReset(t, db, id, "empty-operation").ExemptedCost, 0)
	assertWeeklyCost(t, "total unique credit", weeklyWindow(t, db, id, 30*24*time.Hour).CostExempted, 105)
	// Request IDs are scoped to a key, and successful retries remain valid
	// even if the administrator subsequently disables the cost limits.
	otherID := newWeeklyResetKey(t, db)
	insertWeeklyResetLog(t, db, otherID, at, 12, 120, 200)
	assertWeeklyCost(t, "key-scoped request ID", weeklyReset(t, db, otherID, "retryable-operation").ExemptedCost, 12)
	if _, err := db.conn.Exec(`UPDATE api_keys SET limits = '{}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if retry := weeklyReset(t, db, id, "retryable-operation"); !retry.Replayed || retry.ExemptedCost != 80 {
		t.Fatalf("configuration change broke retry: %+v", retry)
	}
	if _, err := db.ResetAPIKeyWeeklyQuota(context.Background(), id, "requires-current-limits"); !errors.Is(err, ErrAPIKeyWeeklyQuotaResetRequiresLimits) {
		t.Fatalf("new operation without limits: %v", err)
	}
}

func testWeeklyResetOrdinaryReset(t *testing.T, db *DB) {
	ctx := context.Background()
	id := newWeeklyResetKey(t, db)
	insertWeeklyResetLog(t, db, id, time.Now().Add(-time.Hour), 80, 800, 200)
	if _, err := db.ResetAPIKeyQuota(ctx, id); err != nil {
		t.Fatal(err)
	}
	insertWeeklyResetLog(t, db, id, time.Now().UTC(), 20, 200, 200)
	assertWeeklyCost(t, "only current ordinary reset period", weeklyReset(t, db, id, "after-ordinary").ExemptedCost, 20)
	month := weeklyWindow(t, db, id, 30*24*time.Hour)
	assertWeeklyCost(t, "older bills remain counted", month.QuotaCost(), 80)
	assertWeeklyCost(t, "original history", month.UserBilled, 100)
	assertWeeklyCost(t, "5h after weekly reset", weeklyWindow(t, db, id, 5*time.Hour).UserBilled, 20)
	if _, err := db.ResetAPIKeyQuota(ctx, id); err != nil {
		t.Fatal(err)
	}
	assertWeeklyCost(t, "ordinary reset does not change granted credit", weeklyWindow(t, db, id, 30*24*time.Hour).CostExempted, 20)
}

func testWeeklyResetCleanup(t *testing.T, db *DB) {
	ctx := context.Background()
	if err := db.ClearUsageLogs(ctx); err != nil {
		t.Fatal(err)
	}
	id := newWeeklyResetKey(t, db)
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	originalLogID := insertWeeklyResetLog(t, db, id, at, 80, 800, 200)
	weeklyReset(t, db, id, "before-clear")
	if err := db.ClearUsageLogs(ctx); err != nil {
		t.Fatal(err)
	}
	newLogID := insertWeeklyResetLog(t, db, id, at, 70, 700, 200)
	if newLogID != originalLogID {
		t.Fatalf("fixture did not exercise reused log ID: %d -> %d", originalLogID, newLogID)
	}
	month := weeklyWindow(t, db, id, 30*24*time.Hour)
	if month.UserBilled != 70 || month.CostExempted != 0 || weeklyWindow(t, db, id, 7*24*time.Hour).UserBilled != 70 {
		t.Fatalf("reused log inherited historical credit: %+v", month)
	}
	if retry := weeklyReset(t, db, id, "before-clear"); !retry.Replayed || retry.ExemptedCost != 80 {
		t.Fatalf("clear lost the durable operation result: %+v", retry)
	}
	assertWeeklyCost(t, "retry does not reset reused log", weeklyWindow(t, db, id, 7*24*time.Hour).UserBilled, 70)
	weeklyReset(t, db, id, "after-clear")
	if err := db.DeleteAPIKey(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"api_key_weekly_resets", "api_key_weekly_reset_usage"} {
		var count int
		if err := db.conn.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE api_key_id = $1", id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("orphan reset records in %s: %d, %v", table, count, err)
		}
	}
}

func testWeeklyResetConcurrent(t *testing.T, db *DB) {
	for _, sameRequest := range []bool{false, true} {
		t.Run(fmt.Sprintf("same_request_%v", sameRequest), func(t *testing.T) {
			id := newWeeklyResetKey(t, db)
			insertWeeklyResetLog(t, db, id, time.Now().Add(-time.Hour), 80, 800, 200)
			const callers = 6
			results := make(chan *APIKeyWeeklyQuotaReset, callers)
			errorsCh := make(chan error, callers)
			start := make(chan struct{})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var wg sync.WaitGroup
			for i := range callers {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					requestID := fmt.Sprintf("concurrent-%d", i)
					if sameRequest {
						requestID = "shared-request"
					}
					<-start
					result, err := db.ResetAPIKeyWeeklyQuota(ctx, id, requestID)
					if err != nil {
						errorsCh <- err
						return
					}
					results <- result
				}(i)
			}
			close(start)
			wg.Wait()
			close(results)
			close(errorsCh)
			for err := range errorsCh {
				t.Error(err)
			}
			var granted float64
			var replayed int
			for result := range results {
				if result.Replayed {
					replayed++
				} else {
					granted += result.ExemptedCost
				}
			}
			assertWeeklyCost(t, "concurrent unique grant", granted, 80)
			if sameRequest && replayed != callers-1 {
				t.Fatalf("replay count = %d, want %d", replayed, callers-1)
			}
			assertWeeklyCost(t, "concurrent stored credit", weeklyWindow(t, db, id, 30*24*time.Hour).CostExempted, 80)
		})
	}
}

func testWeeklyResetValidation(t *testing.T, db *DB) {
	ctx := context.Background()
	id := newWeeklyResetKey(t, db)
	for _, requestID := range []string{"", " \t", strings.Repeat("x", 129)} {
		if _, err := db.ResetAPIKeyWeeklyQuota(ctx, id, requestID); err == nil {
			t.Fatal("invalid request ID was accepted")
		}
	}
	if _, err := db.ResetAPIKeyWeeklyQuota(ctx, -1, "missing-key"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing key error = %v", err)
	}
	for _, limits := range []string{`{"cost_limit_7d":120}`, `{"cost_limit_30d":480}`, `{"cost_limit_7d":-1,"cost_limit_30d":480}`} {
		if _, err := db.conn.Exec(`UPDATE api_keys SET limits = $2 WHERE id = $1`, id, limits); err != nil {
			t.Fatal(err)
		}
		if _, err := db.ResetAPIKeyWeeklyQuota(ctx, id, "invalid-limits"); !errors.Is(err, ErrAPIKeyWeeklyQuotaResetRequiresLimits) {
			t.Fatalf("invalid limits %s: %v", limits, err)
		}
	}
	if _, err := db.conn.Exec(`UPDATE api_keys SET limits = '{"cost_limit_7d":120,"cost_limit_30d":480}' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	logID := insertWeeklyResetLog(t, db, id, time.Now().Add(-time.Hour), 80, 800, 200)
	weeklyReset(t, db, id, "correctable-bill")
	if _, err := db.conn.Exec(`UPDATE usage_logs SET user_billed = 10 WHERE id = $1`, logID); err != nil {
		t.Fatal(err)
	}
	month := weeklyWindow(t, db, id, 30*24*time.Hour)
	if month.UserBilled != 10 || month.CostExempted != 10 || month.QuotaCost() != 0 {
		t.Fatalf("credit exceeded corrected bill: %+v", month)
	}
	if _, err := db.conn.Exec(`DELETE FROM usage_logs WHERE id = $1`, logID); err != nil {
		t.Fatal(err)
	}
	assertWeeklyCost(t, "deleted bill cannot leave credit", weeklyWindow(t, db, id, 30*24*time.Hour).CostExempted, 0)
}

func testWeeklyResetSubsecond(t *testing.T, db *DB) {
	if db.isSQLite() {
		t.Skip("SQLite stores usage timestamps with second precision")
	}
	id := newWeeklyResetKey(t, db)
	// PostgreSQL must include already committed logs from the current second;
	// rounding the reset boundary down to a whole second would omit them.
	at := time.Now().UTC().Truncate(time.Second).Add(time.Microsecond)
	insertWeeklyResetLog(t, db, id, at, 8, 80, 200)
	assertWeeklyCost(t, "committed fractional-second bill", weeklyReset(t, db, id, "subsecond").ExemptedCost, 8)
	assertWeeklyCost(t, "subsecond weekly remainder", weeklyWindow(t, db, id, 7*24*time.Hour).UserBilled, 0)
}

func TestAPIKeyWeeklyResetSchemaUpgradeAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "upgrade.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{
		Name: "upgrade", Key: "sk-weekly-upgrade-1234567890",
		Limits: APIKeyLimits{CostLimit7d: 120, CostLimit30d: 480},
	})
	if err != nil {
		t.Fatal(err)
	}
	insertWeeklyResetLog(t, db, id, time.Now().Add(-time.Hour), 80, 800, 200)
	// Model an existing database from before the feature was installed.
	for _, table := range []string{"api_key_weekly_reset_usage", "api_key_weekly_resets"} {
		if _, err := db.conn.Exec("DROP TABLE " + table); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	assertWeeklyCost(t, "upgrade preserves existing weekly usage", weeklyWindow(t, db, id, 7*24*time.Hour).UserBilled, 80)
	weeklyReset(t, db, id, "persistent-operation")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if result := weeklyReset(t, db, id, "persistent-operation"); !result.Replayed || result.ExemptedCost != 80 {
		t.Fatalf("restart lost durable reset: %+v", result)
	}
	assertWeeklyCost(t, "restart preserves reset window", weeklyWindow(t, db, id, 7*24*time.Hour).UserBilled, 0)
	assertWeeklyCost(t, "restart preserves raw history", weeklyWindow(t, db, id, 30*24*time.Hour).UserBilled, 80)
}
