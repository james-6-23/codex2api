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

	// 只改思考内容开关同样保留重定向与覆盖开关。
	payload = antigravitySettingsResponse{}
	if err := json.Unmarshal(put(`{"expose_thoughts":true}`).Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.ExposeThoughts || !payload.RedirectOverridesEffort || payload.ModelRedirects["gemini-3.8-flash"] != "gemini-3.8-flash-high" {
		t.Fatalf("expose_thoughts payload = %+v", payload)
	}
	if !auth.AntigravityExposeThoughts() {
		t.Fatal("runtime expose_thoughts not applied")
	}
	if raw, err := db.LoadAntigravityConfig(context.Background()); err != nil || !strings.Contains(raw, `"expose_thoughts":true`) {
		t.Fatalf("persisted=%q err=%v", raw, err)
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

// TestAntigravitySettingsPUTQuotaFieldsWithRevision 断言 PUT 支持新配额字段并严格按 revision 单调递增更新。
func TestAntigravitySettingsPUTQuotaFieldsWithRevision(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredAntigravitySettings()
	startRevision := auth.AppliedAntigravityConfigRevision()
	t.Cleanup(func() { auth.SetConfiguredAntigravitySettings(previous) })
	auth.SetConfiguredAntigravitySettings(auth.AntigravitySettings{})
	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	router := gin.New()
	router.PUT("/api/admin/settings/antigravity", handler.UpdateAntigravitySettings)

	put := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/antigravity", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		return rec
	}
	saved := func(t *testing.T) string {
		t.Helper()
		raw, err := db.GetAntigravityConfigRaw(context.Background())
		if err != nil {
			t.Fatalf("read raw failed: %v", err)
		}
		return raw
	}
	decode := func(rec *httptest.ResponseRecorder) antigravitySettingsResponse {
		t.Helper()
		var payload antigravitySettingsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode %s: %v", rec.Body.String(), err)
		}
		return payload
	}

	// 仅含新 quota 字段的 PUT 合法，且生成单调 revision。
	rec := put(`{"quota_gate_enabled":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT gate status=%d body=%s", rec.Code, rec.Body.String())
	}
	first := decode(rec)
	if first.QuotaGateEnabled || first.ConfigRevision <= startRevision {
		t.Fatalf("PUT gate payload = %+v (start revision %d)", first, startRevision)
	}
	if !first.QuotaRefreshEnabled || first.QuotaRefreshIntervalMinutes != 60 {
		t.Fatalf("untouched quota fields must fall back to defaults, payload = %+v", first)
	}
	if raw := saved(t); !strings.Contains(raw, `"quota_gate_enabled":false`) {
		t.Fatalf("persisted gate off missing: %s", raw)
	}

	// 只改间隔必须保留已生效的 quota_gate_enabled=false，并继续递增 revision。
	rec = put(`{"quota_refresh_interval_minutes":5}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT interval status=%d body=%s", rec.Code, rec.Body.String())
	}
	second := decode(rec)
	if second.QuotaGateEnabled || second.QuotaRefreshIntervalMinutes != 5 || second.ConfigRevision != first.ConfigRevision+1 {
		t.Fatalf("PUT interval payload = %+v", second)
	}
	if runtime := auth.ConfiguredAntigravitySettings(); runtime.GetQuotaGateEnabled() || runtime.ConfigRevision != second.ConfigRevision {
		t.Fatalf("runtime settings not applied monotonically: %+v", runtime)
	}

	// 越界间隔返回 400，不保存、不唤醒。
	if rec := put(`{"quota_refresh_interval_minutes":120}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT out-of-range status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := put(`{"quota_gate_enabled":"nope"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT wrong type status=%d body=%s", rec.Code, rec.Body.String())
	}
	if raw := saved(t); !strings.Contains(raw, `"config_revision":`+fmt.Sprint(second.ConfigRevision)) || strings.Contains(raw, `"quota_refresh_interval_minutes":120`) {
		t.Fatalf("rejected PUT must not touch stored config: %s", raw)
	}
	if got := auth.ConfiguredAntigravitySettings().GetQuotaRefreshIntervalMinutes(); got != 5 {
		t.Fatalf("rejected PUT must not change runtime interval, got %d", got)
	}

	// null 重置默认（gate 回到 true），不得顺带改动其他字段。
	rec = put(`{"quota_gate_enabled":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT null status=%d body=%s", rec.Code, rec.Body.String())
	}
	third := decode(rec)
	if !third.QuotaGateEnabled || third.QuotaRefreshIntervalMinutes != 5 || third.ConfigRevision != second.ConfigRevision+1 {
		t.Fatalf("PUT null payload = %+v", third)
	}
	if raw := saved(t); strings.Contains(raw, `"quota_gate_enabled"`) {
		t.Fatalf("null must reset to default and drop the key: %s", raw)
	}
}

// TestAntigravitySettingsPUTRestoresQuotaFieldsDroppedByOldWriter 断言混布覆盖时自动恢复被旧版本写者抹除的配额字段。
func TestAntigravitySettingsPUTRestoresQuotaFieldsDroppedByOldWriter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredAntigravitySettings()
	t.Cleanup(func() { auth.SetConfiguredAntigravitySettings(previous) })
	gateOff := false
	// 本机持有止血开关，但混布窗口内旧二进制整包覆盖把 quota_* 抹掉了。
	auth.SetConfiguredAntigravitySettings(auth.AntigravitySettings{QuotaGateEnabled: &gateOff, ConfigRevision: 7})
	db := newTestAdminDB(t)
	ctx := context.Background()
	if err := db.SaveAntigravityConfig(ctx, `{"model_redirects":{"gemini-3.8-flash":"gemini-3.8-flash-high"},"config_revision":2}`); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db}
	router := gin.New()
	router.PUT("/api/admin/settings/antigravity", handler.UpdateAntigravitySettings)

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/admin/settings/antigravity", strings.NewReader(`{"expose_thoughts":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload antigravitySettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.QuotaGateEnabled {
		t.Fatalf("quota_gate_enabled=false dropped by old writer must be merged back, payload = %+v", payload)
	}
	if payload.ConfigRevision <= 7 {
		t.Fatalf("revision must exceed the applied baseline 7, payload = %+v", payload)
	}
	raw, err := db.GetAntigravityConfigRaw(ctx)
	if err != nil || !strings.Contains(raw, `"quota_gate_enabled":false`) || !strings.Contains(raw, `"gemini-3.8-flash-high"`) {
		t.Fatalf("persisted=%q err=%v", raw, err)
	}
}

// TestAntigravitySettingsPUTExpiryPriority 覆盖临期优先开关与时间窗的保存、范围校验与 null 重置。
func TestAntigravitySettingsPUTExpiryPriority(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := auth.ConfiguredAntigravitySettings()
	t.Cleanup(func() { auth.SetConfiguredAntigravitySettings(previous) })
	auth.SetConfiguredAntigravitySettings(auth.AntigravitySettings{})
	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	router := gin.New()
	router.PUT("/api/admin/settings/antigravity", handler.UpdateAntigravitySettings)

	put := func(body string) (int, antigravitySettingsResponse) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPut, "/api/admin/settings/antigravity", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		var payload antigravitySettingsResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &payload)
		return rec.Code, payload
	}

	code, payload := put(`{"expiry_priority_enabled":true}`)
	if code != http.StatusOK || !payload.ExpiryPriorityEnabled || payload.ExpiryPriorityHorizonHours != 24 {
		t.Fatalf("enable: code=%d payload=%+v", code, payload)
	}
	if enabled, horizon := auth.AntigravityExpiryPriority(); !enabled || horizon != 24*time.Hour {
		t.Fatalf("runtime expiry priority = %v %v", enabled, horizon)
	}

	code, payload = put(`{"expiry_priority_horizon_hours":12}`)
	if code != http.StatusOK || !payload.ExpiryPriorityEnabled || payload.ExpiryPriorityHorizonHours != 12 {
		t.Fatalf("horizon: code=%d payload=%+v", code, payload)
	}
	if raw, err := db.GetAntigravityConfigRaw(context.Background()); err != nil ||
		!strings.Contains(raw, `"expiry_priority_enabled":true`) || !strings.Contains(raw, `"expiry_priority_horizon_hours":12`) {
		t.Fatalf("persisted=%q err=%v", raw, err)
	}
	for _, body := range []string{`{"expiry_priority_horizon_hours":0}`, `{"expiry_priority_horizon_hours":169}`, `{"expiry_priority_enabled":"yes"}`} {
		if code, _ := put(body); code != http.StatusBadRequest {
			t.Fatalf("PUT %s status=%d, want 400", body, code)
		}
	}

	code, payload = put(`{"expiry_priority_enabled":null}`)
	if code != http.StatusOK || payload.ExpiryPriorityEnabled || payload.ExpiryPriorityHorizonHours != 12 {
		t.Fatalf("null reset: code=%d payload=%+v", code, payload)
	}
	if enabled, _ := auth.AntigravityExpiryPriority(); enabled {
		t.Fatal("null must reset expiry priority to disabled")
	}
}
