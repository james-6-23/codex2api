package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestBackgroundQuietHoursSettingsAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, tc, proxy.NewRateLimiter(0), "test-admin")
	update := func(body string, want int) {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdateSettings(c)
		if rec.Code != want {
			t.Fatalf("status %d body=%s", rec.Code, rec.Body.String())
		}
	}
	update(`{"background_quiet_hours":[{"start":"00:00","end":"24:00"},{"start":"12:00","end":"13:00"}]}`, 200)
	if !store.BackgroundInspectionPaused(time.Now()) {
		t.Fatal("runtime not applied")
	}
	raw := store.GetBackgroundQuietHours()
	update(`{"test_content":"hello"}`, 200)
	got, err := db.GetSystemSettings(context.Background())
	if err != nil || got.BackgroundQuietHours != raw {
		t.Fatal("partial save lost schedule")
	}
	update(`{"background_quiet_hours":[{"start":"25:00","end":"07:00"}],"max_concurrency":17}`, 400)
	if store.GetBackgroundQuietHours() != raw || store.GetMaxConcurrency() == 17 {
		t.Fatal("invalid request mutated runtime")
	}
	h.runWhamDailyUsageProbe(context.Background(), map[int64]time.Time{})
	update(`{"background_quiet_hours":[]}`, 200)
	if store.BackgroundInspectionPaused(time.Now()) {
		t.Fatal("empty array did not clear schedule")
	}
}
