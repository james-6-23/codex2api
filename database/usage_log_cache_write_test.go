package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageLogPersistsCacheWriteTokensAndBillsThem(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "usage-cache-write.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	input := &UsageLogInput{
		AccountID: 1, Endpoint: "/v1/messages", Model: "claude-opus-5", EffectiveModel: "claude-opus-5", StatusCode: 200,
		PromptTokens: 6000, InputTokens: 6000, OutputTokens: 100, CompletionTokens: 100, TotalTokens: 6100,
		CachedTokens: 4000, CacheWrite5mTokens: 1000, CacheWrite1hTokens: 500,
	}
	if err := db.InsertUsageLog(ctx, input); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()
	logs, err := db.ListUsageLogsByTimeRange(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 {
		t.Fatalf("logs = %d, want 1", len(logs))
	}
	got := logs[0]
	if got.CacheWrite5mTokens != 1000 || got.CacheWrite1hTokens != 500 || got.CachedTokens != 4000 {
		t.Fatalf("persisted cache tokens = read %d / 5m %d / 1h %d", got.CachedTokens, got.CacheWrite5mTokens, got.CacheWrite1hTokens)
	}
	want := CalculateCostBreakdownWithCacheWrites(6000, 100, 4000, 1000, 500, "claude-opus-5", "").TotalCost
	if !approxEqual(got.AccountBilled, want) {
		t.Fatalf("account_billed = %v, want %v", got.AccountBilled, want)
	}
	if !approxEqual(got.CacheWrite5mCost, 1000.0/1e6*6.25) || !approxEqual(got.CacheWrite1hCost, 500.0/1e6*10) {
		t.Fatalf("breakdown costs = %v / %v", got.CacheWrite5mCost, got.CacheWrite1hCost)
	}
}
