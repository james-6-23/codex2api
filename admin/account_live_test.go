package admin

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestGetAccountLiveStateReturnsVisibleInflightCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 42, AccessToken: "token"}
	atomic.StoreInt64(&account.ActiveRequests, 3)
	atomic.StoreInt64(&account.OccupiedRequests, 5)
	store.AddAccount(account)
	store.SetSessionSlotBufferEnabled(true)
	handler := &Handler{store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/live?ids=42,99", nil)
	handler.GetAccountLiveState(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Accounts                 map[string]accountLiveItem `json:"accounts"`
		SessionSlotBufferEnabled bool                       `json:"session_slot_buffer_enabled"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Accounts["42"].ActiveRequests; got != 3 {
		t.Fatalf("active_requests = %d, want 3", got)
	}
	if got := response.Accounts["42"].OccupiedRequests; got != 5 {
		t.Fatalf("occupied_requests = %d, want 5", got)
	}
	if !response.SessionSlotBufferEnabled {
		t.Fatal("session slot buffer enabled state was not returned")
	}
	if _, exists := response.Accounts["99"]; exists {
		t.Fatal("missing account unexpectedly returned")
	}
}

func TestAccountSessionCardsIncludeUserAverageWithoutRiskProfile(test *testing.T) {
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "session-card-stats.db"))
	if err != nil {
		test.Fatal(err)
	}
	closeDB := sync.OnceFunc(func() {
		if err := db.Close(); err != nil {
			test.Error(err)
		}
	})
	test.Cleanup(closeDB)
	firstID, err := db.InsertOpenAIResponsesAccount(test.Context(), "first", map[string]interface{}{"base_url": "https://example.test", "api_key": "test"}, "")
	if err != nil {
		test.Fatal(err)
	}
	secondID, err := db.InsertOpenAIResponsesAccount(test.Context(), "second", map[string]interface{}{"base_url": "https://example.test", "api_key": "test"}, "")
	if err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, entry := range []*database.UsageLogInput{
		{AccountID: firstID, SessionHash: "root-a", NewAPIPlatform: "a", NewAPIUserID: "7", SessionUsagePeriodID: "period-a", SessionUsageStartedAt: now.Add(-time.Minute), ObservedAt: now},
		{AccountID: secondID, SessionHash: "root-b", NewAPIPlatform: "a", NewAPIUserID: "7", SessionUsagePeriodID: "period-b", SessionUsageStartedAt: now.Add(-3 * time.Minute), ObservedAt: now},
		{AccountID: firstID, SessionHash: "root-c", NewAPIPlatform: "b", NewAPIUserID: "7", SessionUsagePeriodID: "period-c", SessionUsageStartedAt: now.Add(-10 * time.Minute), ObservedAt: now},
	} {
		if err := db.InsertUsageLog(test.Context(), entry); err != nil {
			test.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	store := auth.NewStore(nil, nil, nil)
	test.Cleanup(store.Stop)
	account := &auth.Account{DBID: firstID, AccessToken: "test", SessionCapacityEnabled: true, SessionCapacityMax: 10, SessionCapacityIdleTTLSeconds: 3600}
	store.AddAccount(account)
	owners := map[string]auth.AccountSessionOwner{
		"a-one":     {Platform: "a", UserID: "7", UserName: "bmyc"},
		"a-two":     {Platform: "a", UserID: "7", UserName: "bmyc"},
		"b-one":     {Platform: "b", UserID: "7", UserName: "other"},
		"missing":   {Platform: "a", UserID: "missing"},
		"anonymous": {APIKeyID: 9, APIKeyName: "standalone"},
	}
	for root, owner := range owners {
		if !store.AdmitAccountSession(account, root, now) {
			test.Fatal("failed to admit fixture window")
		}
		store.SetAccountSessionOwner(firstID, root, owner)
	}
	handler := &Handler{store: store, db: db}
	load := func() []accountSessionResponse {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/1/sessions", nil)
		ctx.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(firstID, 10)}}
		handler.GetAccountSessions(ctx)
		var response struct {
			Sessions []accountSessionResponse `json:"sessions"`
		}
		if recorder.Code != http.StatusOK || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
			test.Fatalf("response=%d %s", recorder.Code, recorder.Body.String())
		}
		if len(response.Sessions) != len(owners) {
			test.Fatal("window list changed")
		}
		return response.Sessions
	}
	for _, session := range load() {
		stats := session.UserSessionUsage
		switch session.SessionID {
		case "a-one", "a-two":
			if stats == nil || stats.WindowCount != 2 || stats.AverageDurationSeconds == nil || math.Abs(*stats.AverageDurationSeconds-120) > .001 {
				test.Fatalf("user average=%+v", stats)
			}
		case "b-one":
			if stats == nil || stats.AverageDurationSeconds == nil || math.Abs(*stats.AverageDurationSeconds-600) > .001 {
				test.Fatalf("platform average=%+v", stats)
			}
		case "missing":
			if stats == nil || stats.WindowCount != 0 || stats.AverageDurationSeconds != nil {
				test.Fatalf("missing average=%+v", stats)
			}
		case "anonymous":
			if stats != nil {
				test.Fatal("API key treated as a verified user")
			}
		}
	}
	closeDB()
	for _, session := range load() {
		if session.UserSessionUsage != nil {
			test.Fatal("database failure returned invented statistics")
		}
	}
}

func TestDeleteAccountSessionsUnbindsOnlyReleasedAccount(t *testing.T) {
	for _, query := range []string{"", "?session_id=released-root"} {
		t.Run("query="+query, func(t *testing.T) {
			runtimeCache := cache.NewMemory(1)
			t.Cleanup(func() { _ = runtimeCache.Close() })
			store := auth.NewStore(nil, runtimeCache, nil)
			t.Cleanup(store.Stop)
			account := &auth.Account{DBID: 42, AccessToken: "token", SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityIdleTTLSeconds: 3600}
			other := &auth.Account{DBID: 43, AccessToken: "other", SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityIdleTTLSeconds: 3600}
			store.AddAccount(account)
			store.AddAccount(other)
			store.BindSessionAffinity("released-root", account, "")
			store.BindSessionAffinity("rebound-root", account, "")
			store.BindSessionAffinity("rebound-root", other, "")
			if owner, found := store.SessionAffinityAccountID("released-root"); !found || owner != account.DBID {
				t.Fatal("initial binding is missing")
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Params = gin.Params{{Key: "id", Value: "42"}}
			ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/admin/accounts/42/sessions"+query, nil)
			(&Handler{store: store}).DeleteAccountSessions(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			if _, found := store.SessionAffinityAccountID("released-root"); found {
				t.Fatal("released root still has a local affinity")
			}
			if _, found, err := runtimeCache.GetSessionAffinity(t.Context(), "released-root"); err != nil || found {
				t.Fatalf("released cached affinity found=%t err=%v", found, err)
			}
			if owner, found := store.SessionAffinityAccountID("rebound-root"); !found || owner != other.DBID {
				t.Fatal("another account's affinity was removed")
			}
			if binding, found, err := runtimeCache.GetSessionAffinity(t.Context(), "rebound-root"); err != nil || !found || binding.AccountID != other.DBID {
				t.Fatalf("rebound cached affinity = %#v found=%t err=%v", binding, found, err)
			}
			if query == "" && store.AccountSessionCount(account.DBID, time.Now()) != 0 {
				t.Fatal("bulk release left occupied slots")
			}
		})
	}
}
