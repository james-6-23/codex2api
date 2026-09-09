package database

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountStatusProfilesAggregateSessionObservationsWithoutRisk(t *testing.T) {
	ctx := context.Background()
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-session-observations.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	accountID, err := db.InsertOpenAIResponsesAccount(ctx, "observed-account", map[string]interface{}{
		"upstream_type": "openai_responses", "base_url": "https://relay.example", "api_key": "sk-test",
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	now := time.Now().UTC()
	inputs := []*UsageLogInput{
		{AccountID: accountID, StatusCode: 200, SessionHash: "session-old", RecordSessionObservation: true, ObservedAt: now.Add(-25 * time.Hour), NewAPIPlatform: "newapi", NewAPIUserID: "7", NewAPIUserName: "旧用户"},
		{AccountID: accountID, StatusCode: 200, SessionHash: "session-a", RecordSessionObservation: true, ObservedAt: now.Add(-time.Hour), NewAPIPlatform: "newapi", NewAPIUserID: "7", NewAPIUserName: "用户甲"},
		{AccountID: accountID, StatusCode: 200, SessionHash: "session-b", RecordSessionObservation: true, ObservedAt: now, NewAPIPlatform: "newapi", NewAPIUserID: "8", NewAPIUserName: "用户乙"},
	}
	for _, input := range inputs {
		if err := db.InsertUsageLog(ctx, input); err != nil {
			t.Fatalf("InsertUsageLog: %v", err)
		}
	}
	db.FlushUsageLogs()

	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{SubjectType: PromptRiskSubjectAccountStatus, Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListPromptRiskProfiles(account_status): %v", err)
	}
	if total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d len=%d, want 1", total, len(profiles))
	}
	profile := profiles[0]
	if profile.RiskScore != 0 || profile.SessionWindows24h != 2 || profile.SessionUniqueUsers != 2 || profile.SessionWindowsTotal != 3 {
		t.Fatalf("unexpected account status profile: %+v", profile)
	}

	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{Start: now.Add(-48 * time.Hour), End: now.Add(time.Hour), Page: 1, PageSize: 20, IncludeCanceled: true})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged: %v", err)
	}
	foundUserName := false
	for _, item := range page.Logs {
		if item.NewAPIUserName == "用户乙" {
			foundUserName = true
			break
		}
	}
	if !foundUserName {
		t.Fatalf("NewAPI user name was not persisted in usage logs: %+v", page.Logs)
	}
}
