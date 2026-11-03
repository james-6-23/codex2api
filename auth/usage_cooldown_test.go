package auth

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func TestUsageCooldownNeverShortensActiveCooldown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage-cooldown.db")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	// Count actual cooldown writes, independently of snapshot writes.
	for _, query := range []string{
		`CREATE TABLE cooldown_writes (account_id INTEGER)`,
		`CREATE TRIGGER count_cooldown_writes AFTER UPDATE OF cooldown_until ON accounts BEGIN INSERT INTO cooldown_writes VALUES (NEW.id); END`,
	} {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	tokenCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = tokenCache.Close() })
	store := NewStore(db, tokenCache, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)

	for _, source := range []string{"5h", "7d", "responses"} {
		t.Run(source, func(t *testing.T) {
			defaultReason := premium5hCooldownReason
			if source == "7d" {
				defaultReason = "rate_limited"
			} else if source == "responses" {
				defaultReason = ResponsesRateLimitedCooldownReason
			}
			for _, tc := range []struct {
				name       string
				prior      time.Duration
				reset      time.Duration
				reason     string
				wantChange bool
			}{
				{"shorter authoritative", 6 * time.Hour, time.Hour, ResponsesRateLimitedCooldownReason, false},
				{"equal authoritative", time.Hour, time.Hour, ResponsesRateLimitedCooldownReason, false},
				{"extend authoritative", time.Hour, 2 * time.Hour, ResponsesRateLimitedCooldownReason, true},
				{"extend other reason", time.Hour, 2 * time.Hour, "upstream_error", true},
				{"shorter unauthorized", 6 * time.Hour, time.Hour, "unauthorized", false},
				{"extend unauthorized", time.Hour, 2 * time.Hour, "unauthorized", true},
				{"shorter usage", 6 * time.Hour, time.Hour, premium5hCooldownReason, false},
				{"equal usage", time.Hour, time.Hour, premium5hCooldownReason, false},
				{"shorter 7d usage", 6 * time.Hour, time.Hour, "rate_limited_7d", false},
				{"equal 7d usage", time.Hour, time.Hour, "rate_limited_7d", false},
				{"shorter legacy rate_limited", 6 * time.Hour, time.Hour, "rate_limited", false},
				{"shorter legacy usage_limited", 6 * time.Hour, time.Hour, "usage_limited", false},
				{"equal legacy usage_limit", time.Hour, time.Hour, "usage_limit", false},
				{"extend usage", time.Hour, 2 * time.Hour, premium5hCooldownReason, true},
				{"new cooldown", 0, 2 * time.Hour, "", true},
				{"expired cooldown", -time.Hour, 2 * time.Hour, ResponsesRateLimitedCooldownReason, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					legacy7d := tc.reason == "rate_limited" || tc.reason == "rate_limited_7d" || tc.reason == "usage_limited" || tc.reason == "usage_limit"
					now := time.Now().UTC().Truncate(time.Second)
					resetAt := now.Add(tc.reset)
					id, err := db.InsertAccountWithCredentials(ctx, "usage-cooldown", map[string]interface{}{"access_token": "test-token", "plan_type": "plus"}, "")
					if err != nil {
						t.Fatal(err)
					}
					acc := newPremium5hTestAccount("plus", resetAt)
					acc.DBID = id
					acc.FailureStreak = 3
					acc.UsagePercent5h = 40
					acc.UsagePercent7d, acc.UsagePercent7dValid, acc.Reset7dAt = 100, true, resetAt
					wantUntil, wantReason := resetAt, defaultReason
					wantWrites := 1
					if tc.prior != 0 {
						acc.Status, acc.CooldownUtil, acc.CooldownReason = StatusCooldown, now.Add(tc.prior), tc.reason
						if source == "responses" && legacy7d {
							acc.Reset7dAt = acc.CooldownUtil
						}
						if err := db.SetCooldown(ctx, id, tc.reason, acc.CooldownUtil); err != nil {
							t.Fatal(err)
						}
						wantWrites++
						if tc.prior > 0 {
							store.setCachedAccountCooldown(id, tc.reason, acc.CooldownUtil)
							if !tc.wantChange {
								wantUntil, wantReason = acc.CooldownUtil, tc.reason
								if source == "responses" && (tc.reason == premium5hCooldownReason || legacy7d) {
									wantReason = ResponsesRateLimitedCooldownReason
								} else {
									wantWrites--
								}
							} else if source != "responses" || tc.reason == "unauthorized" {
								wantReason = tc.reason
							}
						}
					}
					switch source {
					case "5h":
						store.MarkPremium5hRateLimited(acc, resetAt)
					case "7d":
						if !store.MarkUsage7dRateLimited(acc) {
							t.Fatal("exhausted 7d window was not recognized")
						}
					case "responses":
						store.MarkResponsesPremium5hRateLimited(acc, resetAt)
					}
					if acc.Status != StatusCooldown || !acc.CooldownUtil.Equal(wantUntil) || acc.CooldownReason != wantReason {
						t.Errorf("runtime cooldown = (%v, %v, %q), want cooldown/%v/%q", acc.Status, acc.CooldownUtil, acc.CooldownReason, wantUntil, wantReason)
					}
					wantStreak := 3
					if source == "7d" && tc.prior <= 0 {
						wantStreak++ // Preserve the existing new-cooldown behavior.
					}
					if acc.FailureStreak != wantStreak {
						t.Errorf("FailureStreak = %d, want %d", acc.FailureStreak, wantStreak)
					}
					row, err := db.GetAccountByID(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if !row.CooldownUntil.Valid || !row.CooldownUntil.Time.Equal(wantUntil) || row.CooldownReason != wantReason {
						t.Errorf("persisted cooldown = (%v, %q), want %v/%q", row.CooldownUntil, row.CooldownReason, wantUntil, wantReason)
					}
					record, ok := store.getCachedAccountCooldown(id)
					if !ok || !record.ResetAt.Equal(wantUntil) || record.Reason != wantReason {
						t.Errorf("cached cooldown = %+v, present=%v, want %v/%q", record, ok, wantUntil, wantReason)
					}
					var writes int
					if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM cooldown_writes WHERE account_id = ?`, id).Scan(&writes); err != nil {
						t.Fatal(err)
					}
					if writes != wantWrites {
						t.Errorf("cooldown writes = %d, want %d", writes, wantWrites)
					}
					if source != "7d" {
						if pct, reset, ok := acc.GetUsageSnapshot5h(); !ok || pct != 100 || !reset.Equal(resetAt) {
							t.Errorf("5h snapshot = (%v, %v, %v), want 100/%v/true", pct, reset, ok, resetAt)
						}
						if row.GetCredential("codex_5h_used_percent") != "100" {
							t.Error("5h snapshot was not persisted independently of the cooldown")
						}
					}
					// The same observation must not keep writing the cooldown.
					switch source {
					case "5h":
						store.MarkPremium5hRateLimited(acc, resetAt)
					case "7d":
						store.MarkUsage7dRateLimited(acc)
					case "responses":
						store.MarkResponsesPremium5hRateLimited(acc, resetAt)
					}
					if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM cooldown_writes WHERE account_id = ?`, id).Scan(&writes); err != nil {
						t.Fatal(err)
					}
					if writes != wantWrites || acc.FailureStreak != wantStreak {
						t.Errorf("repeated observation: writes=%d streak=%d, want %d/%d", writes, acc.FailureStreak, wantWrites, wantStreak)
					}
				})
			}
		})
	}
}

func TestUsageCooldownRetriesFailedPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cooldown-retry.db")
	db, err := database.New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	store := NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 1})
	t.Cleanup(store.Stop)
	for _, source := range []string{"5h", "7d", "responses"} {
		t.Run(source, func(t *testing.T) {
			id, err := db.InsertAccountWithCredentials(ctx, "cooldown-retry", map[string]interface{}{"plan_type": "plus"}, "")
			if err != nil {
				t.Fatal(err)
			}
			reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			acc := newPremium5hTestAccount("plus", reset)
			acc.DBID = id
			acc.UsagePercent7d, acc.UsagePercent7dValid, acc.Reset7dAt = 100, true, reset
			mark := func() {
				switch source {
				case "5h":
					store.MarkPremium5hRateLimited(acc, reset)
				case "7d":
					store.MarkUsage7dRateLimited(acc)
				case "responses":
					store.MarkResponsesPremium5hRateLimited(acc, reset)
				}
			}
			if _, err := conn.Exec(`CREATE TRIGGER fail_cooldown BEFORE UPDATE OF cooldown_until ON accounts BEGIN SELECT RAISE(FAIL, 'synthetic transient failure'); END`); err != nil {
				t.Fatal(err)
			}
			mark()
			streak := acc.FailureStreak
			row, err := db.GetAccountByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if row.CooldownUntil.Valid {
				t.Fatal("fault injection did not reject the cooldown write")
			}
			if _, err := conn.Exec(`DROP TRIGGER fail_cooldown`); err != nil {
				t.Fatal(err)
			}
			mark()
			row, err = db.GetAccountByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !row.CooldownUntil.Valid || !row.CooldownUntil.Time.Equal(reset) || row.CooldownReason != acc.GetCooldownReason() {
				t.Fatalf("cooldown was not retried after recovery: %+v", row.CooldownUntil)
			}
			if acc.FailureStreak != streak {
				t.Fatal("persistence retry counted another failure")
			}
		})
	}
}

func TestUsageCooldownDelayedPublisherRespectsCurrentState(t *testing.T) {
	for _, action := range []string{"unauthorized", "clear", "error"} {
		t.Run(action, func(t *testing.T) {
			store, db, acc := newUsageFramePersistenceFixture(t)
			reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
			acc.SetUsageSnapshot5h(100, reset)
			store.MarkUsageFrameCooldowns(acc, time.Now())
			// Pause the frame publisher before it can read/publish its state.
			acc.usageCooldownPersistMu.Lock()
			var release sync.Once
			unlock := func() { release.Do(acc.usageCooldownPersistMu.Unlock) }
			t.Cleanup(unlock)
			oldDone := make(chan struct{})
			go func() { store.persistUsageCooldown(acc); close(oldDone) }()
			newDone := make(chan struct{})
			go func() {
				defer close(newDone)
				switch action {
				case "unauthorized":
					store.MarkCooldownWithErrorExactDuration(acc, 6*time.Hour, "unauthorized", "synthetic auth error")
				case "clear":
					store.ClearCooldown(acc)
				case "error":
					store.MarkError(acc, "synthetic permanent error")
				}
			}()
			deadline := time.Now().Add(3 * time.Second)
			changed := false
			for time.Now().Before(deadline) {
				acc.mu.RLock()
				changed = acc.CooldownReason != premium5hCooldownReason
				acc.mu.RUnlock()
				if changed {
					break
				}
				time.Sleep(time.Millisecond)
			}
			unlock()
			for _, done := range []chan struct{}{oldDone, newDone} {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("cooldown publication deadlocked")
				}
			}
			if !changed {
				t.Fatal("new runtime state did not precede delayed publication")
			}
			row, err := db.GetAccountByID(context.Background(), acc.DBID)
			if err != nil {
				t.Fatal(err)
			}
			record, cached := store.getCachedAccountCooldown(acc.DBID)
			switch action {
			case "unauthorized":
				if row.CooldownReason != "unauthorized" || !row.CooldownUntil.Valid || !row.CooldownUntil.Time.Equal(acc.CooldownUtil) || !cached || record.Reason != "unauthorized" || !record.ResetAt.Equal(row.CooldownUntil.Time) {
					t.Fatalf("delayed usage publisher overwrote newer authentication cooldown: row=%v/%v runtime=%v cache=%+v present=%v", row.CooldownReason, row.CooldownUntil, acc.CooldownUtil, record, cached)
				}
			case "clear", "error":
				if row.CooldownUntil.Valid || row.CooldownReason != "" || cached {
					t.Fatal("delayed usage publisher resurrected cleared cooldown")
				}
				if action == "error" && row.Status != "error" {
					t.Fatal("delayed usage publisher cleared newer error")
				}
			}
		})
	}
}

func TestUsageCooldownRepeatedResponses429FencesInflightSuccess(t *testing.T) {
	store := NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	reset := time.Now().Add(time.Hour)
	acc := newPremium5hTestAccount("plus", reset)
	store.SetIgnoreUsageLimitStatus(true)
	store.AddAccount(acc)
	store.MarkResponsesPremium5hRateLimited(acc, reset)
	started := acc.LastRateLimitedAt.Add(time.Second)
	store.markPremium5hRateLimited(acc, reset, started.Add(time.Second), ResponsesRateLimitedCooldownReason, true)
	if store.ConfirmResponsesAvailableSince(acc, started) {
		t.Fatal("an in-flight success cleared newer repeated Responses 429 evidence")
	}
}
