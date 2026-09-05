package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestNormalizeAPIKeyModelRequestLimits(t *testing.T) {
	rules, err := NormalizeAPIKeyModelRequestLimits([]APIKeyModelRequestLimit{{Model: " gpt-6* ", MaxRequests: 50}})
	if err != nil {
		t.Fatal(err)
	}
	rule := rules[0]
	if rule.ID == "" || rule.Model != "gpt-6*" || rule.Window != "week" || rule.Timezone != "Asia/Shanghai" || rule.ResetWeekday != 1 || rule.ResetTime != "00:00" {
		t.Fatalf("unexpected defaults: %+v", rule)
	}
	again, err := NormalizeAPIKeyModelRequestLimits(rules)
	if err != nil || again[0] != rule {
		t.Fatalf("normalization must preserve identity: %+v / %v", again, err)
	}
	if (APIKeyLimits{ModelRequestLimits: rules}).IsZero() {
		t.Fatal("model request quota must make limits nonzero")
	}
	for _, tc := range []struct {
		name string
		edit func(*APIKeyModelRequestLimit)
	}{
		{"zero", func(r *APIKeyModelRequestLimit) { r.MaxRequests = 0 }},
		{"negative", func(r *APIKeyModelRequestLimit) { r.MaxRequests = -1 }},
		{"pattern", func(r *APIKeyModelRequestLimit) { r.Model = "gpt-?" }},
		{"empty model", func(r *APIKeyModelRequestLimit) { r.Model = "" }},
		{"window", func(r *APIKeyModelRequestLimit) { r.Window = "7d" }},
		{"timezone", func(r *APIKeyModelRequestLimit) { r.Timezone = "Not/AZone" }},
		{"local zone", func(r *APIKeyModelRequestLimit) { r.Timezone = "Local" }},
		{"weekday", func(r *APIKeyModelRequestLimit) { r.ResetWeekday = 8 }},
		{"time", func(r *APIKeyModelRequestLimit) { r.ResetTime = "24:00" }},
		{"short time", func(r *APIKeyModelRequestLimit) { r.ResetTime = "1:00" }},
		{"identity", func(r *APIKeyModelRequestLimit) { r.ID = "bad/id" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := rule
			tc.edit(&bad)
			if _, err := NormalizeAPIKeyModelRequestLimits([]APIKeyModelRequestLimit{bad}); err == nil {
				t.Fatal("invalid rule accepted")
			}
		})
	}
	if _, err := NormalizeAPIKeyModelRequestLimits([]APIKeyModelRequestLimit{rule, rule}); err == nil {
		t.Fatal("duplicate IDs accepted")
	}
	for _, tc := range []struct {
		pattern, model string
		want           bool
	}{
		{"gpt-6*", "gpt-6", true}, {"gpt-6*", "gpt-6-pro", true}, {"gpt-6*", "gpt-5", false},
		{"*6*pro", "gpt-6-pro", true}, {"gpt-6", "gpt-6-pro", false}, {"*", "claude/sonnet", true},
		{"gpt-6", "GPT-6", true}, {"gpt-6**", "gpt-6-mini", true},
	} {
		if got := MatchAPIKeyModelRequestLimit(tc.pattern, tc.model); got != tc.want {
			t.Errorf("match(%q,%q)=%v want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

func TestAPIKeyModelRequestWindowCalendarAndDST(t *testing.T) {
	for _, tc := range []struct {
		name, timezone, reset, now, start, end string
		weekday                                int
	}{
		{"before monday", "Asia/Shanghai", "00:00", "2026-09-06T15:59:59Z", "2026-08-30T16:00:00Z", "2026-09-06T16:00:00Z", 1},
		{"at monday", "Asia/Shanghai", "00:00", "2026-09-06T16:00:00Z", "2026-09-06T16:00:00Z", "2026-09-13T16:00:00Z", 1},
		{"custom reset", "Asia/Shanghai", "12:15", "2026-09-09T04:14:59Z", "2026-09-02T04:15:00Z", "2026-09-09T04:15:00Z", 3},
		{"spring short week", "America/New_York", "00:00", "2026-03-08T12:00:00Z", "2026-03-02T05:00:00Z", "2026-03-09T04:00:00Z", 1},
		{"fall long week", "America/New_York", "00:00", "2026-11-01T12:00:00Z", "2026-10-26T04:00:00Z", "2026-11-02T05:00:00Z", 1},
		{"nonexistent time forwards", "America/New_York", "02:30", "2026-03-08T08:00:00Z", "2026-03-08T07:30:00Z", "2026-03-15T06:30:00Z", 7},
		{"repeated time first", "America/New_York", "01:30", "2026-11-01T06:15:00Z", "2026-11-01T05:30:00Z", "2026-11-08T06:30:00Z", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now, _ := time.Parse(time.RFC3339, tc.now)
			rule := APIKeyModelRequestLimit{Model: "gpt-6*", MaxRequests: 50, Timezone: tc.timezone, ResetTime: tc.reset, ResetWeekday: tc.weekday}
			start, end, err := APIKeyModelRequestWindow(rule, now)
			if err != nil || start.Format(time.RFC3339) != tc.start || end.Format(time.RFC3339) != tc.end {
				t.Fatalf("window=(%v,%v,%v), want (%s,%s)", start, end, err, tc.start, tc.end)
			}
		})
	}
}

func TestAPIKeyModelRequestQuotasSQLite(t *testing.T) {
	runAPIKeyModelRequestQuotaSuite(t, "sqlite", filepath.Join(t.TempDir(), "quota.db"))
}

// Uses a writable test database, like the other PostgreSQL integration tests.
// Two independent DB handles verify serialization across process-local locks.
func TestAPIKeyModelRequestQuotasPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("CODEX2API_TEST_POSTGRES_DSN is not set")
	}
	runAPIKeyModelRequestQuotaSuite(t, "postgres", dsn)
}

func runAPIKeyModelRequestQuotaSuite(t *testing.T, driver, dsn string) {
	t.Helper()
	db, err := New(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db2, err := New(driver, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 6, 0, 0, 0, time.UTC)
	create := func(rules []APIKeyModelRequestLimit) (int64, []APIKeyModelRequestLimit) {
		t.Helper()
		key := "sk-model-quota-test-" + uuid.NewString()
		id, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{Name: "model quota test", Key: key, Limits: APIKeyLimits{ModelRequestLimits: rules}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Cleanup runs after function-scoped defers closed DB handles.
			conn, err := New(driver, dsn)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			if err := conn.DeleteAPIKey(ctx, id); err != nil {
				t.Error(err)
			}
		})
		row, err := db.GetAPIKeyByValue(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		return id, row.Limits.ModelRequestLimits
	}
	usage := func(id int64, rules []APIKeyModelRequestLimit, at time.Time) []APIKeyModelRequestUsage {
		t.Helper()
		got, err := db2.GetAPIKeyModelRequestUsage(ctx, id, rules, at)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	consume := func(id int64, request, model string, rules []APIKeyModelRequestLimit, at time.Time, wantExhausted bool) {
		t.Helper()
		exhausted, err := db.ConsumeAPIKeyModelRequest(ctx, id, request, model, rules, at)
		if err != nil || (exhausted != nil) != wantExhausted {
			t.Fatalf("consume %s: exhausted=%v err=%v", request, exhausted, err)
		}
	}

	t.Run("last slot across instances", func(t *testing.T) {
		id, rules := create([]APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: 1}})
		var wg sync.WaitGroup
		results := make(chan string, 32)
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				conn := db
				if i%2 == 1 {
					conn = db2
				}
				request := fmt.Sprintf("race-%d", i)
				ex, err := conn.ConsumeAPIKeyModelRequest(ctx, id, request, "gpt-6-pro", rules, now)
				if err != nil {
					results <- "error:" + err.Error()
				} else if ex == nil {
					results <- request
				}
			}(i)
		}
		wg.Wait()
		close(results)
		var winners []string
		for result := range results {
			if strings.HasPrefix(result, "error:") {
				t.Fatal(result)
			}
			winners = append(winners, result)
		}
		if len(winners) != 1 {
			t.Fatalf("%d winners, want 1: %v", len(winners), winners)
		}
		consume(id, winners[0], "gpt-6-pro", rules, now, false)
		consume(id, "unlimited-model", "gpt-5.4", rules, now, false)
		got := usage(id, rules, now)[0]
		if got.Used != 1 || got.Remaining != 0 || got.Limit != 1 {
			t.Fatalf("usage=%+v", got)
		}
	})

	t.Run("overlap atomic and effective model", func(t *testing.T) {
		id, rules := create([]APIKeyModelRequestLimit{
			{ID: "a-series", Model: "gpt-6*", MaxRequests: 3},
			{ID: "z-exact", Model: "gpt-6-pro", MaxRequests: 1},
		})
		consume(id, "r1", "gpt-6-mini", rules, now, false)
		consume(id, "r1", "gpt-6-pro", rules, now, false) // retry maps to another model: series dedupes, exact charges
		consume(id, "r2", "gpt-6-pro", rules, now, true)
		got := usage(id, rules, now)
		if got[0].Used != 1 || got[1].Used != 1 {
			t.Fatalf("partial charge: %+v", got)
		}
		consume(id, "r2", "gpt-6-mini", rules, now, false)
		got = usage(id, rules, now)
		if got[0].Used != 2 || got[1].Used != 1 {
			t.Fatalf("usage=%+v", got)
		}
	})

	t.Run("concurrent same request", func(t *testing.T) {
		id, rules := create([]APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: 1}})
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if ex, err := db.ConsumeAPIKeyModelRequest(ctx, id, "same-id", "gpt-6", rules, now); err != nil || ex != nil {
					t.Errorf("idempotent consume=%v,%v", ex, err)
				}
			}()
		}
		wg.Wait()
		if got := usage(id, rules, now)[0]; got.Used != 1 {
			t.Fatalf("usage=%+v", got)
		}
	})

	t.Run("week rollover and retry identity", func(t *testing.T) {
		id, rules := create([]APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: 1}})
		consume(id, "old-week", "gpt-6", rules, now, false)
		nextWeek := now.AddDate(0, 0, 7)
		consume(id, "old-week", "gpt-6", rules, nextWeek, false)
		if got := usage(id, rules, nextWeek)[0]; got.Used != 0 {
			t.Fatalf("retry charged new week: %+v", got)
		}
		consume(id, "new-week", "gpt-6", rules, nextWeek, false)
		if got := usage(id, rules, nextWeek)[0]; got.Used != 1 {
			t.Fatalf("new week usage=%+v", got)
		}
		if got := usage(id, rules, now)[0]; got.Used != 1 {
			t.Fatalf("old week lost count: %+v", got)
		}
	})

	t.Run("edits keep counts and use authoritative limits", func(t *testing.T) {
		id, rules := create([]APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: 50}})
		consume(id, "before-edit", "gpt-6", rules, now, false)
		changed := append([]APIKeyModelRequestLimit(nil), rules...)
		changed[0].MaxRequests = 1
		if err := db2.UpdateAPIKeyLimits(ctx, id, APIKeyLimits{ModelRequestLimits: changed}); err != nil {
			t.Fatal(err)
		}
		consume(id, "stale-limit", "gpt-6", rules, now, true)
		consume(id, "before-edit", "gpt-6", rules, now, false)
		if got := usage(id, changed, now)[0]; got.Used != 1 {
			t.Fatalf("limit edit reset usage: %+v", got)
		}
		changed[0].ResetTime = "12:00"
		if err := db.UpdateAPIKey(ctx, id, APIKeyUpdate{LimitsSet: true, Limits: APIKeyLimits{ModelRequestLimits: changed}}); err == nil {
			t.Fatal("immutable schedule change accepted")
		}
		changed[0] = rules[0]
		changed[0].Model = "gpt-*"
		if err := db.UpdateAPIKeyLimits(ctx, id, APIKeyLimits{ModelRequestLimits: changed}); err == nil {
			t.Fatal("immutable model change accepted")
		}
		if _, err := db.ResetAPIKeyQuota(ctx, id); err != nil {
			t.Fatal(err)
		}
		if got := usage(id, rules, now)[0]; got.Used != 1 {
			t.Fatalf("money quota reset changed request count: %+v", got)
		}
	})

	t.Run("key isolation", func(t *testing.T) {
		id, rules := create([]APIKeyModelRequestLimit{{ID: "shared-rule-name", Model: "gpt-6*", MaxRequests: 1}})
		other, otherRules := create(rules)
		consume(id, "same-request-id", "gpt-6", rules, now, false)
		if got := usage(other, otherRules, now)[0]; got.Used != 0 {
			t.Fatalf("cross-key leak: %+v", got)
		}
		consume(other, "same-request-id", "gpt-6", otherRules, now, false)
	})
}

func TestAPIKeyModelRequestQuotasSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "persistent.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now()
	key := "sk-model-quota-reopen"
	id, err := db.InsertAPIKeyWithOptions(ctx, APIKeyInput{Key: key, Limits: APIKeyLimits{ModelRequestLimits: []APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAPIKeyByValue(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	rules := row.Limits.ModelRequestLimits
	if ex, err := db.ConsumeAPIKeyModelRequest(ctx, id, "r1", "gpt-6", rules, now); err != nil || ex != nil {
		t.Fatalf("consume=%v,%v", ex, err)
	}
	db.Close()
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if ex, err := db.ConsumeAPIKeyModelRequest(ctx, id, "r2", "gpt-6", rules, now); err != nil || ex == nil {
		t.Fatalf("after reopen=%v,%v", ex, err)
	}
	if ex, err := db.ConsumeAPIKeyModelRequest(ctx, id, "r1", "gpt-6", rules, now); err != nil || ex != nil {
		t.Fatalf("reopened ledger=%v,%v", ex, err)
	}
	if err := db.DeleteAPIKey(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"api_key_model_request_counters", "api_key_model_request_ledger"} {
		var count int
		if err := db.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE api_key_id=$1", id).Scan(&count); err != nil || count != 0 {
			t.Fatalf("delete cleanup %s=%d,%v", table, count, err)
		}
	}
}
