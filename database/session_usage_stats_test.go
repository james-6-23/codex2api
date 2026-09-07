package database

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionUsagePeriodsRemainSeparateAndSummaryIgnoresPagination(test *testing.T) {
	ctx := context.Background()
	path := filepath.Join(test.TempDir(), "session-usage.db")
	db, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	accounts := make([]int64, 0, 3)
	for _, name := range []string{"observed-a", "observed-b", "idle-account"} {
		accountID, insertErr := db.InsertOpenAIResponsesAccount(ctx, name, map[string]interface{}{"upstream_type": "openai_responses", "base_url": "https://example.test", "api_key": "test"}, "")
		if insertErr != nil {
			test.Fatal(insertErr)
		}
		accounts = append(accounts, accountID)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, input := range []*UsageLogInput{
		{AccountID: accounts[0], SessionHash: "root", RecordSessionObservation: true, ObservedAt: now.Add(-2*time.Hour + time.Minute), NewAPIPlatform: "gateway-a", NewAPIUserID: "7", SessionUsagePeriodID: "old-period", SessionUsageStartedAt: now.Add(-2 * time.Hour)},
		{AccountID: accounts[0], SessionHash: "root", ObservedAt: now.Add(-10*time.Minute + 3*time.Minute), NewAPIPlatform: "gateway-a", NewAPIUserID: "7", SessionUsagePeriodID: "new-period", SessionUsageStartedAt: now.Add(-10 * time.Minute)},
		{AccountID: accounts[0], SessionHash: "historical-root", RecordSessionObservation: true, ObservedAt: now, NewAPIPlatform: "gateway-a", NewAPIUserID: "7"},
		{AccountID: accounts[1], SessionHash: "root", RecordSessionObservation: true, ObservedAt: now, NewAPIPlatform: "gateway-b", NewAPIUserID: "7", SessionUsagePeriodID: "other-account-period", SessionUsageStartedAt: now.Add(-10 * time.Minute)},
		{AccountID: accounts[0], SessionHash: "root", ObservedAt: now.Add(-2*time.Hour + 30*time.Second), NewAPIPlatform: "gateway-a", NewAPIUserID: "7", SessionUsagePeriodID: "old-period", SessionUsageStartedAt: now.Add(-2 * time.Hour)},
	} {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			test.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	var periodCount int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM account_session_usage_periods`).Scan(&periodCount); err != nil || periodCount != 3 {
		test.Fatalf("period count=%d err=%v", periodCount, err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{SubjectType: PromptRiskSubjectAccountStatus, Query: "observed", PageSize: 1, Page: 1})
	if err != nil || total != 2 || len(profiles) != 1 {
		test.Fatalf("profiles=%+v total=%d err=%v", profiles, total, err)
	}
	if profiles[0].SessionAverageDurationSeconds == nil || math.Abs(*profiles[0].SessionAverageDurationSeconds-120) > .001 {
		test.Fatalf("account duration=%v", profiles[0].SessionAverageDurationSeconds)
	}
	summary, err := db.GetAccountSessionSummary(ctx, PromptRiskProfileQuery{Query: "observed", PageSize: 1, Page: 2})
	if err != nil {
		test.Fatal(err)
	}
	if summary.AccountCount != 2 || summary.AverageWindowsTotal != 1.5 || summary.AverageUniqueUsers != 1 || summary.AverageDurationSeconds == nil || math.Abs(*summary.AverageDurationSeconds-280) > .001 {
		test.Fatalf("summary=%+v", summary)
	}
	idle, err := db.GetAccountSessionSummary(ctx, PromptRiskProfileQuery{ActivityState: "identity_only"})
	if err != nil || idle.AccountCount != 1 || idle.AverageDurationSeconds != nil {
		test.Fatalf("idle=%+v err=%v", idle, err)
	}
	user, err := db.GetNewAPIUserSessionUsage(ctx, "gateway-a", "7")
	if err != nil || user.WindowCount != 2 || user.AverageDurationSeconds == nil || math.Abs(*user.AverageDurationSeconds-120) > .001 {
		test.Fatalf("user=%+v err=%v", user, err)
	}
	unknown, err := db.GetNewAPIUserSessionUsage(ctx, "gateway-a", "missing")
	if err != nil || unknown.WindowCount != 0 || unknown.AverageDurationSeconds != nil {
		test.Fatalf("unknown=%+v err=%v", unknown, err)
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	restored, err := db.GetNewAPIUserSessionUsage(ctx, "gateway-a", "7")
	if err != nil || restored.WindowCount != 2 || restored.AverageDurationSeconds == nil || math.Abs(*restored.AverageDurationSeconds-120) > .001 {
		test.Fatalf("restored=%+v err=%v", restored, err)
	}
}
