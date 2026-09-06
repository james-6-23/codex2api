package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestAntigravitySettingsRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredAntigravitySettings()
	t.Cleanup(func() { auth.SetConfiguredAntigravitySettings(previous) })
	auth.SetConfiguredAntigravitySettings(auth.AntigravitySettings{})
	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/api/admin/settings/antigravity", handler.GetAntigravitySettings)
	router.PUT("/api/admin/settings/antigravity", handler.UpdateAntigravitySettings)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/settings/antigravity", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"gemini-3.8-flash-high"`) {
		t.Fatalf("GET status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/antigravity", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	if rec := put(`{"model_redirects":{"Gemini-3.8-Flash":" gemini-3.8-flash-high ","gemini-3.6-flash":""}}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := auth.AntigravityModelRedirect("gemini-3.8-flash"); got != "gemini-3.8-flash-high" {
		t.Fatalf("runtime redirect = %q", got)
	}
	if got := auth.AntigravityModelRedirect("gemini-3.6-flash"); got != "" {
		t.Fatalf("empty target must clear the redirect, got %q", got)
	}
	raw, err := db.LoadAntigravityConfig(context.Background())
	if err != nil || !strings.Contains(raw, `"gemini-3.8-flash":"gemini-3.8-flash-high"`) {
		t.Fatalf("persisted=%q err=%v", raw, err)
	}

	// 只改覆盖开关不得清掉已有重定向。
	if rec := put(`{"redirect_overrides_effort":true}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT override status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload antigravitySettingsResponse
	if err := json.Unmarshal(put(`{"redirect_overrides_effort":true}`).Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.RedirectOverridesEffort || payload.ModelRedirects["gemini-3.8-flash"] != "gemini-3.8-flash-high" || len(payload.Choices) == 0 {
		t.Fatalf("payload = %+v", payload)
	}

	for _, bad := range []string{
		`{"model_redirects":{"gemini-3.8-flash":"gemini-3.6-flash-high"}}`,
		`{"model_redirects":{"gemini-3.8-flash-high":"gemini-3.8-flash-low"}}`,
		`{"model_redirects":{"gemini-3.8-flash":"gemini-3.8-flash-tiered"}}`,
		`{}`,
	} {
		if rec := put(bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s status=%d, want 400 (body=%s)", bad, rec.Code, rec.Body.String())
		}
	}
	if got := auth.AntigravityModelRedirect("gemini-3.8-flash"); got != "gemini-3.8-flash-high" {
		t.Fatalf("rejected update must not change runtime config, got %q", got)
	}
}
