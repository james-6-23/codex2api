package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestManualWindowAPIRejectsStaleCardsAndAllowsUnlockAfterExpiry(test *testing.T) {
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "window-admin.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	memory := cache.NewMemory(1)
	test.Cleanup(func() { _ = memory.Close() })
	if err := db.UpsertPromptRiskIdentities(test.Context(), []database.PromptRiskIdentityInput{{Platform: "newapi", ExternalUserID: "42"}}); err != nil {
		test.Fatal(err)
	}
	profiles, _, err := db.ListPromptRiskProfiles(test.Context(), database.PromptRiskProfileQuery{SubjectType: database.PromptRiskSubjectNewAPIUser})
	if err != nil || len(profiles) != 1 {
		test.Fatalf("profiles=%+v err=%v", profiles, err)
	}
	root := strings.Repeat("a", 24)
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	state := cache.PromptSessionLimitState{Version: 2, Sessions: map[string]time.Time{root: expires}, Details: map[string]cache.PromptSessionWindowDetail{root: {CreatedAt: now, AccountID: 9}}}
	setState := func() {
		test.Helper()
		payload, err := json.Marshal(state)
		if err != nil {
			test.Fatal(err)
		}
		if err := memory.SetRuntime(test.Context(), cache.PromptSessionLimitRuntimeNamespace, cache.PromptSessionLimitSubject("newapi", "42"), payload, time.Hour); err != nil {
			test.Fatal(err)
		}
	}
	setState()
	handler := &Handler{db: db, cache: memory}
	router := gin.New()
	router.POST("/profiles/:subject_type/:subject_key/session-windows/:session_hash/lock", handler.LockPromptUserWindow)
	router.POST("/profiles/:subject_type/:subject_key/session-windows/:session_hash/unlock", handler.UnlockPromptUserWindow)
	router.GET("/profiles/:subject_type/:subject_key", handler.GetPromptRiskProfile)
	path := "/profiles/newapi_user/" + profiles[0].SubjectKey
	request := func(method, suffix, body string, status int) *httptest.ResponseRecorder {
		test.Helper()
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(method, path+suffix, strings.NewReader(body)))
		if recorder.Code != status {
			test.Fatalf("%s %s: %d %s", method, suffix, recorder.Code, recorder.Body.String())
		}
		return recorder
	}
	lockPath := "/session-windows/" + root + "/lock"
	request(http.MethodPost, lockPath, `{}`, 400)
	request(http.MethodPost, lockPath, `{"window_expires_at":"`+expires.Add(time.Second).Format(time.RFC3339Nano)+`"}`, 409)
	request(http.MethodPost, "/session-windows/"+strings.Repeat("b", 24)+"/lock", `{"window_expires_at":"`+expires.Format(time.RFC3339Nano)+`"}`, 409)
	request(http.MethodPost, lockPath, `{"window_expires_at":"`+expires.Format(time.RFC3339Nano)+`"}`, 200)
	state.Sessions = map[string]time.Time{}
	setState()
	detailResponse := request(http.MethodGet, "", "", 200)
	var detail promptRiskProfileDetailResponse
	if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil || len(detail.SessionWindows) != 0 || len(detail.ManualWindowLocks) != 1 {
		test.Fatalf("detail=%s err=%v", detailResponse.Body.String(), err)
	}
	request(http.MethodPost, "/session-windows/"+root+"/unlock", "", 200)
	locks, err := db.ListPromptUserWindowLocks(test.Context(), "newapi", "42", time.Now())
	if err != nil || len(locks) != 0 {
		test.Fatalf("locks=%+v err=%v", locks, err)
	}
}

func TestWindow500BadgeMatchesAccountAndWindowGeneration(test *testing.T) {
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "badge.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	root := strings.Repeat("a", 24)
	expires := now.Add(time.Hour)
	if err := db.InsertUsageLog(test.Context(), &database.UsageLogInput{AccountID: 9, StatusCode: 500, SessionHash: root, NewAPIPlatform: "newapi", NewAPIUserID: "42", ObservedAt: now, SessionWindowExpiresAt: expires}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	handler := &Handler{db: db}
	profile := &database.PromptRiskProfile{Platform: "newapi", NewAPIUserID: "42"}
	windows := []promptRiskSessionWindowResponse{
		{SessionHash: root, AccountID: 9, ExpiresAt: expires},
		{SessionHash: root, AccountID: 10, ExpiresAt: expires},
		{SessionHash: root, AccountID: 9, ExpiresAt: expires.Add(time.Hour)},
		{SessionHash: strings.Repeat("b", 24), AccountID: 9, ExpiresAt: expires},
	}
	if err := handler.attachPromptWindowAccountErrors(test.Context(), profile, windows); err != nil {
		test.Fatal(err)
	}
	for index, window := range windows {
		if (window.Last500At != nil) != (index == 0) {
			test.Fatalf("window %d marker leaked/missing: %+v", index, window)
		}
	}
	for _, scope := range [][2]string{{"newapi", "43"}, {"other", "42"}} {
		profile.Platform, profile.NewAPIUserID = scope[0], scope[1]
		other := []promptRiskSessionWindowResponse{{SessionHash: root, AccountID: 9, ExpiresAt: expires}}
		if err := handler.attachPromptWindowAccountErrors(test.Context(), profile, other); err != nil || other[0].Last500At != nil {
			test.Fatalf("scope=%v windows=%+v err=%v", scope, other, err)
		}
	}
}
