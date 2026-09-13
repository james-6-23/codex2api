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
	"github.com/stretchr/testify/require"
)

func TestCodexWSCompressionOptionsAdminRoundTrip(t *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(t)
	memory := cache.NewMemory(4)
	t.Cleanup(func() { _ = memory.Close() })
	settings := defaultBootstrapSettings()
	require.NoError(t, db.UpdateSystemSettings(context.Background(), settings))
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	store := auth.NewStore(db, memory, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, memory, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, step := range []struct {
		patch             string
		level             int
		disabled, enabled bool
		status            int
	}{
		{"", 1, false, false, 200},
		{`{"codex_ws_context_takeover":true,"codex_ws_compression_level":6,"codex_ws_disable_fragmentation":true}`, 6, true, true, 200},
		{`{"site_name":"unrelated"}`, 6, true, true, 200},
		{`{"codex_ws_compression_level":null,"codex_ws_disable_fragmentation":null}`, 6, true, true, 200},
		{`{"codex_ws_compression_level":0,"codex_ws_context_takeover":false}`, 6, true, true, 400},
		{`{"codex_ws_compression_level":10}`, 6, true, true, 400},
		{`{"codex_ws_compression_level":1.5}`, 6, true, true, 400},
		{`{"codex_ws_disable_fragmentation":"false"}`, 6, true, true, 400},
		{`{"codex_ws_context_takeover":false}`, 6, true, false, 200},
		{`{"codex_ws_context_takeover":true}`, 6, true, true, 200},
		{`{"codex_ws_compression_level":9,"codex_ws_disable_fragmentation":false}`, 9, false, true, 200},
		{"", 9, false, true, 200},
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
		require.Equal(t, step.status, recorder.Code, recorder.Body.String())
		if step.status == 200 {
			var got struct {
				Level    int  `json:"codex_ws_compression_level"`
				Disabled bool `json:"codex_ws_disable_fragmentation"`
			}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &got))
			require.Equal(t, step.level, got.Level)
			require.Equal(t, step.disabled, got.Disabled)
		}
		runtime := proxy.CurrentRuntimeSettings()
		require.Equal(t, step.level, runtime.CodexWSCompressionLevel)
		require.Equal(t, step.disabled, runtime.CodexWSDisableFragmentation)
		require.Equal(t, step.enabled, runtime.CodexWSContextTakeover)
		saved, err := db.GetSystemSettings(context.Background())
		require.NoError(t, err)
		require.Equal(t, step.level, saved.CodexWSCompressionLevel)
		require.Equal(t, step.disabled, saved.CodexWSDisableFragmentation)
		reloaded := proxy.ApplyRuntimeSettingsFromSystem(saved)
		require.Equal(t, step.level, reloaded.CodexWSCompressionLevel)
		require.Equal(t, step.disabled, reloaded.CodexWSDisableFragmentation)
	}
}
