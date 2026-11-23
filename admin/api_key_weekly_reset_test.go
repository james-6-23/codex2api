package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestResetAPIKeyWeeklyQuotaHandlerPreservesOtherQuotasAndReplays(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	const key = "sk-weekly-reset-private-1234567890"
	id, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{
		Name: "weekly", Key: key, QuotaLimit: 1000, QuotaUsed: 7.5,
		Limits: database.APIKeyLimits{CostLimit5h: 30, CostLimit7d: 120, CostLimit30d: 480},
	})
	if err != nil {
		t.Fatal(err)
	}
	insertUsage := func() {
		t.Helper()
		if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
			APIKeyID: id, Model: "gpt-5.4", StatusCode: 200,
			InputTokens: 10000, TotalTokens: 10000, CreatedAt: time.Now().Add(-time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
		db.FlushUsageLogs()
	}
	insertUsage()
	before, err := db.GetAPIKeyByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	usageBefore, err := db.GetAPIKeyWindowUsage(ctx, id, 7*24*time.Hour)
	if err != nil || usageBefore.UserBilled <= 0 {
		t.Fatalf("seed weekly usage = %#v, err=%v", usageBefore, err)
	}
	tc := cache.NewMemory(1)
	t.Cleanup(func() { _ = tc.Close() })
	h := &Handler{db: db, cache: tc}
	router := gin.New()
	router.POST("/keys/:id/reset-weekly-quota", h.ResetAPIKeyWeeklyQuota)
	for _, label := range []string{"5h", "7d", "30d"} {
		if err := tc.SetRuntime(ctx, adminAPIKeyLimitsCacheNamespace,
			fmt.Sprintf("%d:usage:%s", id, label), json.RawMessage(`{"user_billed":480}`), time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if err := tc.SetRuntime(ctx, adminAPIKeyCacheNamespace, key, json.RawMessage(`{"id":1}`), time.Minute); err != nil {
		t.Fatal(err)
	}
	const requestID = "dac5c546-21d6-44de-8fba-08a582305a32"
	type resetResponse struct {
		RequestID    string    `json:"request_id"`
		ResetAt      time.Time `json:"reset_at"`
		ExemptedCost float64   `json:"exempted_cost"`
		Replayed     bool      `json:"replayed"`
	}
	invoke := func(requestID string) resetResponse {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost,
			fmt.Sprintf("/keys/%d/reset-weekly-quota", id),
			strings.NewReader(fmt.Sprintf(`{"request_id":%q}`, requestID)))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("reset response = %d %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), key) {
			t.Fatal("reset response leaked the API key")
		}
		var result resetResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := invoke(requestID)
	if first.Replayed || first.RequestID != requestID || first.ResetAt.IsZero() || first.ExemptedCost != usageBefore.UserBilled {
		t.Fatalf("reset result = %#v, want exempted_cost %v", first, usageBefore.UserBilled)
	}
	for _, label := range []string{"7d", "30d"} {
		if _, ok, err := tc.GetRuntime(ctx, adminAPIKeyLimitsCacheNamespace, fmt.Sprintf("%d:usage:%s", id, label)); err != nil || ok {
			t.Fatalf("%s limit cache survived reset: ok=%v err=%v", label, ok, err)
		}
	}
	if _, ok, err := tc.GetRuntime(ctx, adminAPIKeyLimitsCacheNamespace, fmt.Sprintf("%d:usage:5h", id)); err != nil || !ok {
		t.Fatalf("5h cache was changed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := tc.GetRuntime(ctx, adminAPIKeyCacheNamespace, key); err != nil || ok {
		t.Fatalf("auth cache survived reset: ok=%v err=%v", ok, err)
	}
	after, err := db.GetAPIKeyByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if after.QuotaUsed != before.QuotaUsed || after.TotalUsed != before.TotalUsed || after.ResetCount != before.ResetCount || after.LastResetAt != before.LastResetAt {
		t.Fatalf("cumulative reset state changed: before=%#v after=%#v", before, after)
	}
	insertUsage()
	// A retried request cannot reset consumption recorded after the original
	// reset, including when the UUID is spelled with uppercase hex digits.
	replay := invoke(strings.ToUpper(requestID))
	if !replay.Replayed || replay.ExemptedCost != first.ExemptedCost || !replay.ResetAt.Equal(first.ResetAt) {
		t.Fatalf("replayed result = %#v, first=%#v", replay, first)
	}
	usageAfter, err := db.GetAPIKeyWindowUsage(ctx, id, 7*24*time.Hour)
	if err != nil || usageAfter.Requests != 1 || usageAfter.UserBilled != usageBefore.UserBilled {
		t.Fatalf("new weekly usage was reset by replay: %#v err=%v", usageAfter, err)
	}
}

func TestResetAPIKeyWeeklyQuotaHandlerRejectsInvalidRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	id, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: "sk-weekly-no-limits-1234567890", Limits: database.APIKeyLimits{CostLimit30d: 480},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/keys/:id/reset-weekly-quota", (&Handler{db: db}).ResetAPIKeyWeeklyQuota)
	const validBody = `{"request_id":"25cf67a6-03ba-4fd9-921c-866cc525e6f9"}`
	for _, tt := range []struct {
		name, id, body string
		status         int
	}{
		{"invalid key id", "abc", validBody, http.StatusBadRequest},
		{"zero key id", "0", validBody, http.StatusBadRequest},
		{"malformed JSON", fmt.Sprint(id), "{", http.StatusBadRequest},
		{"missing request id", fmt.Sprint(id), `{}`, http.StatusBadRequest},
		{"invalid request id", fmt.Sprint(id), `{"request_id":"retry"}`, http.StatusBadRequest},
		{"nil UUID", fmt.Sprint(id), `{"request_id":"00000000-0000-0000-0000-000000000000"}`, http.StatusBadRequest},
		{"missing limits", fmt.Sprint(id), validBody, http.StatusBadRequest},
		{"unknown key", "99999", validBody, http.StatusNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/keys/"+tt.id+"/reset-weekly-quota", strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("response = %d %s, want %d", rec.Code, rec.Body.String(), tt.status)
			}
		})
	}
}

func TestResetAPIKeyWeeklyQuotaRouteRequiresAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(1)
	t.Cleanup(func() { _ = tc.Close() })
	store := auth.NewStore(db, tc, nil)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, tc, nil, "weekly-admin-secret")
	const key = "sk-weekly-user-key-1234567890"
	id, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: key, Limits: database.APIKeyLimits{CostLimit7d: 120, CostLimit30d: 480},
	})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	h.RegisterRoutes(router)
	for _, tt := range []struct {
		name, path, header, value string
		status                    int
	}{
		{"anonymous", fmt.Sprintf("/api/admin/keys/%d/reset-weekly-quota", id), "", "", http.StatusUnauthorized},
		{"API key bearer", fmt.Sprintf("/api/admin/keys/%d/reset-weekly-quota", id), "Authorization", "Bearer " + key, http.StatusUnauthorized},
		{"API key admin header", fmt.Sprintf("/api/admin/keys/%d/reset-weekly-quota", id), "X-Admin-Key", key, http.StatusUnauthorized},
		{"no public reset route", "/api/key-usage/reset-weekly-quota", "Authorization", "Bearer " + key, http.StatusNotFound},
		{"administrator", fmt.Sprintf("/api/admin/keys/%d/reset-weekly-quota", id), "X-Admin-Key", "weekly-admin-secret", http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(`{"request_id":"37d31b0e-5b6f-4ea8-8b32-eac8d50a30c7"}`))
			req.Header.Set("Content-Type", "application/json")
			if tt.header != "" {
				req.Header.Set(tt.header, tt.value)
			}
			router.ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Fatalf("response = %d %s, want %d", rec.Code, rec.Body.String(), tt.status)
			}
		})
	}
}
