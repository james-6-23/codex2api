package database

import (
	"context"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func compactionBoolPtr(value bool) *bool {
	return &value
}

func usageLogModels(logs []*UsageLog) []string {
	models := make([]string, 0, len(logs))
	for _, logRow := range logs {
		models = append(models, logRow.Model)
	}
	sort.Strings(models)
	return models
}

func assertUsageLogModels(t *testing.T, logs []*UsageLog, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := usageLogModels(logs)
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}

func TestUsageLogCompactionStatesRoundTripAndFilter(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	apiKeyID, err := db.InsertAPIKey(ctx, "compaction states", "sk-compaction-states-1234567890")
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}

	inputs := []*UsageLogInput{
		{
			APIKeyID: apiKeyID, Endpoint: "/v1/responses", Model: "trigger-only",
			StatusCode: 200, Compact: true,
		},
		{
			APIKeyID: apiKeyID, Endpoint: "/v1/responses", Model: "history-only",
			StatusCode: 200, HasCompactionHistory: true,
		},
		{
			APIKeyID: apiKeyID, Endpoint: "/v1/responses", Model: "both",
			StatusCode: 200, Compact: true, HasCompactionHistory: true,
		},
		{
			APIKeyID: apiKeyID, Endpoint: "/v1/responses", Model: "neither",
			StatusCode: 200,
		},
	}
	for _, input := range inputs {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog(%s): %v", input.Model, err)
		}
	}
	db.FlushUsageLogs()

	recent, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs: %v", err)
	}
	if len(recent) != len(inputs) {
		t.Fatalf("len(recent) = %d, want %d", len(recent), len(inputs))
	}
	states := make(map[string][2]bool, len(recent))
	for _, logRow := range recent {
		states[logRow.Model] = [2]bool{logRow.Compact, logRow.HasCompactionHistory}
	}
	for model, want := range map[string][2]bool{
		"trigger-only": {true, false},
		"history-only": {false, true},
		"both":         {true, true},
		"neither":      {false, false},
	} {
		if got := states[model]; got != want {
			t.Fatalf("%s states = %v, want %v", model, got, want)
		}
	}

	now := time.Now()
	baseFilter := UsageLogFilter{
		Start: now.Add(-time.Hour), End: now.Add(time.Hour), Page: 1, PageSize: 10,
	}

	compactFilter := baseFilter
	compactFilter.CompactOnly = compactionBoolPtr(true)
	compactPage, err := db.ListUsageLogsByTimeRangePaged(ctx, compactFilter)
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged compact: %v", err)
	}
	assertUsageLogModels(t, compactPage.Logs, "both", "trigger-only")

	historyFilter := baseFilter
	historyFilter.CompactionHistoryOnly = compactionBoolPtr(true)
	historyPage, err := db.ListUsageLogsByTimeRangePaged(ctx, historyFilter)
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged history: %v", err)
	}
	assertUsageLogModels(t, historyPage.Logs, "both", "history-only")

	bothFilter := baseFilter
	bothFilter.CompactOnly = compactionBoolPtr(true)
	bothFilter.CompactionHistoryOnly = compactionBoolPtr(true)
	bothLogs, err := db.ListUsageLogsByFilter(ctx, bothFilter)
	if err != nil {
		t.Fatalf("ListUsageLogsByFilter both: %v", err)
	}
	assertUsageLogModels(t, bothLogs, "both")

	plainHistoryFilter := baseFilter
	plainHistoryFilter.CompactOnly = compactionBoolPtr(false)
	plainHistoryFilter.CompactionHistoryOnly = compactionBoolPtr(true)
	plainHistoryLogs, err := db.ListUsageLogsByFilter(ctx, plainHistoryFilter)
	if err != nil {
		t.Fatalf("ListUsageLogsByFilter history without trigger: %v", err)
	}
	assertUsageLogModels(t, plainHistoryLogs, "history-only")

	report, err := db.GetAPIKeySelfUsageReport(ctx, apiKeyID, now.Add(-time.Hour), now.Add(time.Hour), 1, 10)
	if err != nil {
		t.Fatalf("GetAPIKeySelfUsageReport: %v", err)
	}
	if len(report.RecentLogs) != len(inputs) {
		t.Fatalf("len(report.RecentLogs) = %d, want %d", len(report.RecentLogs), len(inputs))
	}
	selfStates := make(map[string][2]bool, len(report.RecentLogs))
	for _, logRow := range report.RecentLogs {
		selfStates[logRow.Model] = [2]bool{logRow.Compact, logRow.HasCompactionHistory}
	}
	for model, want := range states {
		if got := selfStates[model]; got != want {
			t.Fatalf("self usage %s states = %v, want %v", model, got, want)
		}
	}
}

func TestUsageLogInsertColumnCountIncludesCompactionHistory(t *testing.T) {
	// 50 legacy columns + 2 cache-write fields + 4 request/proxy trace fields.
	const want = 56
	if usageLogInsertColumnCount != want {
		t.Fatalf("usageLogInsertColumnCount = %d, want %d", usageLogInsertColumnCount, want)
	}
	if maxUsageLogInsertRowsPerSQL*usageLogInsertColumnCount > postgresMaxBindParams {
		t.Fatalf("single INSERT bind params = %d, exceed PostgreSQL limit %d",
			maxUsageLogInsertRowsPerSQL*usageLogInsertColumnCount, postgresMaxBindParams)
	}
}
