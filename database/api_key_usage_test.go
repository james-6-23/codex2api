package database

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUsageStatsGroupRenamedAPIKeysByID(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	testUsageStatsGroupRenamedAPIKeysByID(t, db)
}

func testUsageStatsGroupRenamedAPIKeysByID(t *testing.T, db *DB) {
	t.Helper()
	ctx := t.Context()
	keyID, err := db.InsertAPIKey(ctx, "First name", "sk-test-rename-first")
	if err != nil {
		t.Fatal(err)
	}
	otherID, err := db.InsertAPIKey(ctx, "Current name", "sk-test-rename-other")
	if err != nil {
		t.Fatal(err)
	}

	const accountID = 7
	start := time.Now().Truncate(time.Hour).Add(-3 * time.Hour)
	end := start.Add(2*time.Hour + time.Minute)
	logs := []UsageLogInput{
		{APIKeyID: keyID, APIKeyName: "First name", TotalTokens: 100, InputTokens: 70, OutputTokens: 30, CachedTokens: 10, StatusCode: 200, CreatedAt: start.Add(time.Minute)},
		{APIKeyID: keyID, APIKeyName: "Second name", TotalTokens: 200, InputTokens: 150, OutputTokens: 50, CachedTokens: 20, StatusCode: 500, CreatedAt: start.Add(2*time.Hour + 30*time.Second)},
		{APIKeyID: otherID, APIKeyName: "Current name", TotalTokens: 50, StatusCode: 200, CreatedAt: start.Add(time.Minute)},
		{APIKeyMasked: "sk-env-a", TotalTokens: 25, StatusCode: 200, CreatedAt: start.Add(time.Minute)},
		{APIKeyMasked: "sk-env-b", TotalTokens: 20, StatusCode: 200, CreatedAt: start.Add(time.Minute)},
		{APIKeyID: keyID, APIKeyName: "Cancelled", TotalTokens: 1000, StatusCode: 499, CreatedAt: start.Add(2*time.Hour + 40*time.Second)},
	}
	for _, entry := range logs {
		entry.AccountID = accountID
		entry.Model = "gpt-5.5"
		if err := db.InsertUsageLog(ctx, &entry); err != nil {
			t.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	if err := db.UpdateAPIKeyName(ctx, keyID, "Current name"); err != nil {
		t.Fatal(err)
	}

	assertStats := func(name string) {
		t.Helper()
		t.Run(name+"/token totals", func(t *testing.T) {
			items, err := db.ListAPIKeyTokenStats(ctx, start, end)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 4 || items[1].APIKeyID != otherID {
				t.Fatalf("token stats should contain both distinct IDs and both untracked keys: %+v", items)
			}
			if got := items[0]; got.APIKeyID != keyID || got.APIKeyName != name || got.Label != name ||
				got.Requests != 2 || got.TotalTokens != 300 || got.InputTokens != 220 || got.OutputTokens != 80 ||
				got.CachedTokens != 30 || got.ErrorCount != 1 {
				t.Fatalf("token stats for renamed key: %+v", got)
			}
		})
		t.Run(name+"/account breakdown", func(t *testing.T) {
			detail, err := db.GetAccountUsageStats(ctx, accountID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(detail.ByAPIKey) != 4 {
				t.Fatalf("account usage should contain four keys: %+v", detail.ByAPIKey)
			}
			if got := detail.ByAPIKey[0]; got.APIKeyID != keyID || got.APIKeyName != name || got.Requests != 2 || got.Tokens != 300 {
				t.Fatalf("account usage for renamed key: %+v", got)
			}
		})
		t.Run(name+"/usage overview", func(t *testing.T) {
			stats, err := db.GetUsageStats(ctx, start, end, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(stats.APIKeyStats) != 4 {
				t.Fatalf("usage overview should contain four keys: %+v", stats.APIKeyStats)
			}
			if got := stats.APIKeyStats[0]; got.APIKeyID != keyID || got.Label != name || got.Requests != 2 || got.Tokens != 300 || got.ErrorCount != 1 {
				t.Fatalf("usage overview for renamed key: %+v", got)
			}
		})
	}

	assertStats("Current name")
	if err := db.DeleteAPIKey(ctx, keyID); err != nil {
		t.Fatal(err)
	}
	assertStats("Second name")
}
