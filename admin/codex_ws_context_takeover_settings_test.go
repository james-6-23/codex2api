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

func TestCodexWSContextTakeoverSettingsRoundTrip(test *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	test.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(test)
	memoryCache := cache.NewMemory(4)
	test.Cleanup(func() { _ = memoryCache.Close() })
	settings := defaultBootstrapSettings()
	if settings.CodexWSContextTakeover {
		test.Fatal("bootstrap must leave WS context takeover off")
	}
	settings.CodexRequestCompression = false
	settings.CodexSessionFailoverEnabled = true
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		test.Fatal(err)
	}
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	store := auth.NewStore(db, memoryCache, settings)
	test.Cleanup(store.Stop)
	handler := NewHandler(store, db, memoryCache, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, step := range []struct {
		name   string
		patch  string
		want   bool
		status int
		stale  bool
	}{
		{name: "default", status: http.StatusOK},
		{name: "null defaults off", patch: `{"codex_ws_context_takeover":null}`, status: http.StatusOK},
		{name: "enable", patch: `{"codex_ws_context_takeover":true}`, want: true, status: http.StatusOK},
		{name: "get enabled", want: true, status: http.StatusOK},
		{name: "omitted preserves persisted value", patch: `{"site_name":"WS context takeover test"}`, want: true, status: http.StatusOK, stale: true},
		{name: "null is omitted", patch: `{"codex_ws_context_takeover":null}`, want: true, status: http.StatusOK, stale: true},
		{name: "invalid boolean", patch: `{"codex_ws_context_takeover":"true"}`, want: true, status: http.StatusBadRequest},
		{name: "invalid number", patch: `{"codex_ws_context_takeover":1}`, want: true, status: http.StatusBadRequest},
		{name: "disable", patch: `{"codex_ws_context_takeover":false}`, status: http.StatusOK},
		{name: "get disabled", status: http.StatusOK},
		{name: "omitted preserves persisted false", patch: `{"site_name":"WS context takeover off"}`, status: http.StatusOK, stale: true},
		{name: "null preserves persisted false", patch: `{"codex_ws_context_takeover":null}`, status: http.StatusOK, stale: true},
	} {
		test.Run(step.name, func(test *testing.T) {
			if step.stale {
				proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
					current.CodexWSContextTakeover = !step.want
					return current
				})
			}
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
			if recorder.Code != step.status {
				test.Fatalf("settings status=%d want=%d body=%s", recorder.Code, step.status, recorder.Body.String())
			}
			if step.status == http.StatusOK {
				var response struct {
					Enabled *bool `json:"codex_ws_context_takeover"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					test.Fatal(err)
				}
				if response.Enabled == nil || *response.Enabled != step.want {
					test.Fatalf("response toggle=%v want=%t", response.Enabled, step.want)
				}
			}
			if proxy.CurrentRuntimeSettings().CodexWSContextTakeover != step.want {
				test.Fatal("runtime toggle does not match response")
			}
			persisted, err := db.GetSystemSettings(context.Background())
			if err != nil || persisted == nil {
				test.Fatalf("read persisted settings: %v", err)
			}
			if persisted.CodexWSContextTakeover != step.want || persisted.CodexRequestCompression != settings.CodexRequestCompression || persisted.CodexSessionFailoverEnabled != settings.CodexSessionFailoverEnabled {
				test.Fatal("WS context takeover did not persist independently of HTTP compression and session failover")
			}
			proxy.ApplyRuntimeSettingsFromSystem(persisted)
			if proxy.CurrentRuntimeSettings().CodexWSContextTakeover != step.want {
				test.Fatal("runtime reload lost the WS context takeover toggle")
			}
		})
	}
}
