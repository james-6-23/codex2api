package database

import (
	"context"
	"testing"
	"time"
)

func TestAccountDailyUsageUpsertOverwritesSameDay(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	day := time.Now().UTC().Format("2006-01-02")

	// 第一轮:当天尚未结算,上游不返回 token 明细。
	if err := db.UpsertAccountDailyUsage(ctx, AccountDailyUsageInput{
		AccountID: 7, Day: day, Credits: 100, Turns: 5, Settled: false,
		ClientsJSON: `[{"client_id":"CODEX_CLI"}]`,
	}); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	// 第二轮:同一天结算完成,必须整行覆盖而不是累加,否则 credits 会翻倍。
	if err := db.UpsertAccountDailyUsage(ctx, AccountDailyUsageInput{
		AccountID: 7, Day: day, Credits: 250, Turns: 12, TotalTokens: 99, Settled: true,
		ClientsJSON: `[{"client_id":"CODEX_CLI"},{"client_id":"CODEX_UNKNOWN_DEFAULT"}]`,
	}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	rows, err := db.ListAccountDailyUsage(ctx, 7, 7)
	if err != nil {
		t.Fatalf("ListAccountDailyUsage: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected a single row after re-upsert, got %d", len(rows))
	}
	got := rows[0]
	if got.Credits != 250 || got.Turns != 12 || got.TotalTokens != 99 || !got.Settled {
		t.Fatalf("row was not overwritten: %#v", got)
	}
}

func TestAccountDailyUsageSumAndPrune(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	ancient := now.AddDate(0, 0, -400).Format("2006-01-02")

	for _, item := range []AccountDailyUsageInput{
		{AccountID: 1, Day: today, Credits: 25, TotalTokens: 10, Turns: 2, Settled: true},
		{AccountID: 1, Day: yesterday, Credits: 50, TotalTokens: 20, Turns: 3, Settled: true},
		{AccountID: 2, Day: today, Credits: 75, TotalTokens: 30, Turns: 4, Settled: true},
		{AccountID: 1, Day: ancient, Credits: 999, TotalTokens: 999, Turns: 9, Settled: true},
	} {
		if err := db.UpsertAccountDailyUsage(ctx, item); err != nil {
			t.Fatalf("upsert %s: %v", item.Day, err)
		}
	}

	// 汇总窗口是 7 天,400 天前那条不能被算进来。
	totals, err := db.SumAccountDailyUsage(ctx, []int64{1, 2}, 7)
	if err != nil {
		t.Fatalf("SumAccountDailyUsage: %v", err)
	}
	if got := totals[1]; got.Credits != 75 || got.TotalTokens != 30 || got.Turns != 5 {
		t.Fatalf("account 1 totals = %#v", got)
	}
	older := now.AddDate(0, 0, -20).Format("2006-01-02")
	if err := db.UpsertAccountDailyUsage(ctx, AccountDailyUsageInput{
		AccountID: 1, Day: older, Credits: 10, TotalTokens: 4, Turns: 1, Settled: true,
	}); err != nil {
		t.Fatalf("upsert older snapshot: %v", err)
	}
	all, err := db.SumAccountDailyUsage(ctx, []int64{1}, 365)
	if err != nil {
		t.Fatalf("SumAccountDailyUsage 365: %v", err)
	}
	if got := all[1]; got.Credits != 85 || got.TotalTokens != 34 || got.Turns != 6 {
		t.Fatalf("account 1 365d totals = %#v, want credits=85", got)
	}
	if got := totals[2]; got.Credits != 75 || got.TotalTokens != 30 || got.Turns != 4 {
		t.Fatalf("account 2 totals = %#v", got)
	}

	// 未传入的账号不能出现在结果里,避免调用方误用。
	if _, ok := totals[3]; ok {
		t.Fatal("unrequested account leaked into totals")
	}

	if err := db.PruneAccountDailyUsage(ctx, 365); err != nil {
		t.Fatalf("PruneAccountDailyUsage: %v", err)
	}
	rows, err := db.ListAccountDailyUsage(ctx, 1, 500)
	if err != nil {
		t.Fatalf("ListAccountDailyUsage after prune: %v", err)
	}
	for _, row := range rows {
		if row.Day == ancient {
			t.Fatalf("row older than retention survived prune: %s", row.Day)
		}
	}
}
