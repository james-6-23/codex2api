package database

import (
	"fmt"
	"testing"
	"time"
)

func TestAccountBilledWindowLongEarlyResetRecalculates(test *testing.T) {
	for _, duration := range []time.Duration{7 * 24 * time.Hour, 30 * 24 * time.Hour} {
		for _, advance := range []time.Duration{5*time.Minute + time.Second, time.Hour, 23 * time.Hour} {
			for _, archive := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s_%s_archived_%t", duration, advance, archive), func(test *testing.T) {
					db := newPromptPolicySQLiteTestDB(test)
					start := time.Date(2026, time.September, 8, 1, 0, 0, 0, time.UTC)
					old := AccountBillingWindow{AccountID: 1, Kind: AccountBillingWindowLong, Start: start, Duration: duration}
					insert := func(at time.Time, cost float64) {
						test.Helper()
						if _, err := db.conn.ExecContext(test.Context(), `INSERT INTO usage_logs(account_id, status_code, account_billed, created_at) VALUES (1, 200, $1, $2)`, cost, sqliteTimeParam(at)); err != nil {
							test.Fatal(err)
						}
					}
					assertCost := func(window AccountBillingWindow, want float64) {
						test.Helper()
						got, err := db.GetAccountBilledWindow(test.Context(), window)
						if err != nil || got != want {
							test.Fatalf("cost = %v, want %v, err=%v", got, want, err)
						}
					}
					insert(start.Add(time.Minute), 12)
					assertCost(old, 12)
					if archive {
						if err := db.ClearUsageLogs(test.Context(), old); err != nil {
							test.Fatal(err)
						}
					}
					if _, err := db.conn.ExecContext(test.Context(), `CREATE TABLE billing_reset_writes (id INTEGER); CREATE TRIGGER billing_reset_write AFTER UPDATE ON usage_account_billing_window_states BEGIN INSERT INTO billing_reset_writes VALUES (1); END`); err != nil {
						test.Fatal(err)
					}
					current := old
					current.Start = start.Add(advance)
					assertCost(current, 0)
					insert(current.Start, 4)
					assertCost(current, 4)
					assertCost(old, 4)
					jitter := current
					jitter.Start = current.Start.Add(3 * time.Minute)
					for repeat := 0; repeat < 3; repeat++ {
						assertCost(jitter, 4)
					}
					var writes int
					if err := db.conn.QueryRowContext(test.Context(), `SELECT COUNT(*) FROM billing_reset_writes`).Scan(&writes); err != nil || writes != 1 {
						test.Fatalf("boundary changes wrote %d times, want 1, err=%v", writes, err)
					}
					if err := db.ClearUsageLogs(test.Context(), current); err != nil {
						test.Fatal(err)
					}
					assertCost(current, 4)
				})
			}
		}
	}
}

func TestAccountBilledWindowLongResetToleranceBoundary(test *testing.T) {
	db := newPromptPolicySQLiteTestDB(test)
	start := time.Date(2026, time.September, 8, 1, 0, 0, 0, time.UTC)
	window := AccountBillingWindow{AccountID: 1, Kind: AccountBillingWindowLong, Start: start, Duration: 7 * 24 * time.Hour}
	if _, err := db.conn.ExecContext(test.Context(), `INSERT INTO usage_logs(account_id, status_code, account_billed, created_at) VALUES (1, 200, 10, $1)`, sqliteTimeParam(start.Add(time.Minute))); err != nil {
		test.Fatal(err)
	}
	if err := db.ClearUsageLogs(test.Context(), window); err != nil {
		test.Fatal(err)
	}
	window.Start = start.Add(5 * time.Minute)
	if cost, err := db.GetAccountBilledWindow(test.Context(), window); err != nil || cost != 10 {
		test.Fatalf("at tolerance = %v, %v, want 10", cost, err)
	}
	window.Start = window.Start.Add(time.Second)
	if cost, err := db.GetAccountBilledWindow(test.Context(), window); err != nil || cost != 0 {
		test.Fatalf("beyond tolerance = %v, %v, want 0", cost, err)
	}
}
