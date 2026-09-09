package admin

import (
	"encoding/json"
	"math"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestRiskProfileAPIsExposeUserDurationAndAllFilteredAccountAverages(test *testing.T) {
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "profile-stats.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	accountID, err := db.InsertOpenAIResponsesAccount(test.Context(), "active", map[string]interface{}{"base_url": "https://example.test", "api_key": "test"}, "")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := db.InsertOpenAIResponsesAccount(test.Context(), "idle", map[string]interface{}{"base_url": "https://example.test", "api_key": "test"}, ""); err != nil {
		test.Fatal(err)
	}
	if err := db.UpsertPromptRiskIdentities(test.Context(), []database.PromptRiskIdentityInput{{Platform: "gateway-a", ExternalUserID: "42"}}); err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.InsertUsageLog(test.Context(), &database.UsageLogInput{AccountID: accountID, SessionHash: "root", RecordSessionObservation: true, ObservedAt: now, SessionUsagePeriodID: "period", SessionUsageStartedAt: now.Add(-time.Minute), NewAPIPlatform: "gateway-a", NewAPIUserID: "42"}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/risk-profiles?subject_type=account_status&page=2&page_size=1", nil)
	handler.ListPromptRiskProfiles(context)
	var list promptRiskProfilesResponse
	if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &list) != nil {
		test.Fatalf("response=%d %s", recorder.Code, recorder.Body.String())
	}
	if len(list.Profiles) != 1 || list.AccountSummary == nil || list.AccountSummary.AccountCount != 2 || list.AccountSummary.AverageWindowsTotal != .5 {
		test.Fatalf("list=%+v", list)
	}
	profiles, _, err := db.ListPromptRiskProfiles(test.Context(), database.PromptRiskProfileQuery{SubjectType: database.PromptRiskSubjectNewAPIUser})
	if err != nil || len(profiles) != 1 {
		test.Fatalf("profiles=%+v err=%v", profiles, err)
	}
	recorder = httptest.NewRecorder()
	context, _ = gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/risk-profiles/user", nil)
	context.Params = gin.Params{{Key: "subject_type", Value: database.PromptRiskSubjectNewAPIUser}, {Key: "subject_key", Value: profiles[0].SubjectKey}}
	handler.GetPromptRiskProfile(context)
	var detail promptRiskProfileDetailResponse
	if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &detail) != nil {
		test.Fatalf("response=%d %s", recorder.Code, recorder.Body.String())
	}
	if detail.SessionUsage == nil || detail.SessionUsage.WindowCount != 1 || detail.SessionUsage.AverageDurationSeconds == nil || math.Abs(*detail.SessionUsage.AverageDurationSeconds-60) > .002 {
		test.Fatalf("usage=%+v", detail.SessionUsage)
	}
}
