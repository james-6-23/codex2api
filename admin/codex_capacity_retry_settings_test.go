package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestCodexCapacityRetrySettingsRoundTrip(test *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	test.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(test)
	memoryCache := cache.NewMemory(4)
	test.Cleanup(func() { _ = memoryCache.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		test.Fatal(err)
	}
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	store := auth.NewStore(db, memoryCache, settings)
	test.Cleanup(store.Stop)
	handler := NewHandler(store, db, memoryCache, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, step := range []struct {
		patch string
		want  bool
	}{
		{"", false},
		{`{"codex_capacity_retry_enabled":true}`, true},
		{`{"site_name":"Codex2API capacity test"}`, true},
		{`{"codex_capacity_retry_enabled":false}`, false},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		if step.patch == "" {
			ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
			handler.GetSettings(ctx)
		} else {
			ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(step.patch))
			ctx.Request.Header.Set("Content-Type", "application/json")
			handler.UpdateSettings(ctx)
		}
		if recorder.Code != 200 {
			test.Fatalf("settings status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Enabled *bool `json:"codex_capacity_retry_enabled"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			test.Fatal(err)
		}
		if response.Enabled == nil || *response.Enabled != step.want {
			test.Fatalf("response toggle=%v, want %t: %s", response.Enabled, step.want, recorder.Body.String())
		}
		persisted, err := db.GetSystemSettings(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if persisted.CodexCapacityRetryEnabled != step.want || persisted.CodexOverloadPauseEnabled != settings.CodexOverloadPauseEnabled {
			test.Fatal("toggle did not persist independently of overload pause")
		}
		proxy.ApplyRuntimeSettingsFromSystem(persisted)
		if proxy.CurrentRuntimeSettings().CodexCapacityRetryEnabled != step.want {
			test.Fatal("runtime reload lost the capacity retry toggle")
		}
	}
}
