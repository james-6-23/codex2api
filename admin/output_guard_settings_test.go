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
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func newTestAdminHandlerFixture(t *testing.T) (*Handler, *database.DB) {
	t.Helper()
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	return handler, db
}

func invokeAdminSettingsEndpoint(t *testing.T, handler *Handler, method string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	var bodyReader *strings.Reader
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal payload failed: %v", err)
		}
		bodyReader = strings.NewReader(string(body))
	} else {
		bodyReader = strings.NewReader("")
	}
	c.Request = httptest.NewRequest(method, "/api/admin/settings", bodyReader)
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodGet {
		handler.GetSettings(c)
	} else {
		handler.UpdateSettings(c)
	}
	return w
}

func TestAdminOutputGuardSettings_GetSymmetricFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newTestAdminHandlerFixture(t)

	w := invokeAdminSettingsEndpoint(t, handler, http.MethodGet, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d, body: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	fields := []string{
		"output_guard_enabled", "output_guard_max_tool_argument_bytes",
		"output_guard_max_tool_argument_deltas", "output_guard_max_channel_bytes",
		"output_guard_max_request_bytes", "output_guard_config_generation",
	}
	for _, f := range fields {
		if _, ok := resp[f]; !ok {
			t.Fatalf("expected field %q in GET /api/admin/settings response", f)
		}
	}
	if resp["output_guard_enabled"] != false {
		t.Fatalf("expected default enabled = false, got %v", resp["output_guard_enabled"])
	}
	if int64(resp["output_guard_max_tool_argument_bytes"].(float64)) != 262144 {
		t.Fatalf("expected default tool bytes 262144, got %v", resp["output_guard_max_tool_argument_bytes"])
	}
}

func TestAdminOutputGuardSettings_PutValidationAndReadonlyRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newTestAdminHandlerFixture(t)

	// 1. 提交只读 generation 必须报错 400
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_config_generation": 999})
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "只读") {
			t.Fatalf("expected 400 with readonly rejection, got code=%d body=%s", w.Code, w.Body.String())
		}
	}
	// 2. 超出单个字段合法范围必须报错 400
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_max_tool_argument_bytes": 1024})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on out of range tool bytes, got %d", w.Code)
		}
	}
	// 3. 违反 tool <= channel <= request 层级校验必须报错 400 且不改写 DB
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{
			"output_guard_max_tool_argument_bytes": 3145728,
			"output_guard_max_channel_bytes":       2097152,
		})
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on hierarchy violation, got %d", w.Code)
		}
		current, err := db.GetOutputGuardSettings(context.Background())
		if err != nil || current.MaxToolArgumentBytes == 3145728 {
			t.Fatalf("database should not be updated on illegal hierarchy validation, err=%v", err)
		}
	}
	// 4. 显式 true 准确持久化与热应用
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_enabled": true})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on explicit true update, got %d, body: %s", w.Code, w.Body.String())
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if resp["output_guard_enabled"] != true {
			t.Fatalf("expected output_guard_enabled = true in response, got %v", resp["output_guard_enabled"])
		}
		current, err := db.GetOutputGuardSettings(context.Background())
		if err != nil || current.Enabled == nil || *current.Enabled != true {
			t.Fatalf("database must store explicit true")
		}
	}
	// 5. 局部字段更新隔离：仅更新 deltas 不改变其他字段，并立即热应用
	{
		w := invokeAdminSettingsEndpoint(t, handler, http.MethodPut, map[string]any{"output_guard_max_tool_argument_deltas": 8192})
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 on partial update, got %d", w.Code)
		}
		var resp map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &resp)
		if int(resp["output_guard_max_tool_argument_deltas"].(float64)) != 8192 || resp["output_guard_enabled"] != true {
			t.Fatalf("expected deltas = 8192 and preserved enabled=true, got %v", resp)
		}
		if runtimeCfg := proxy.CurrentOutputGuardConfig(); runtimeCfg.MaxToolArgumentDeltas != 8192 {
			t.Fatalf("runtime snapshot must be immediately updated, got %d", runtimeCfg.MaxToolArgumentDeltas)
		}
	}
}
