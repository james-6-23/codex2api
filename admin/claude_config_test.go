package admin

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestGetClaudeConfigReturnsSecurityDefaults(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	h := &Handler{store: store}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h.GetClaudeConfig(c)
	if recorder.Code != 200 {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "max_output_tokens").Int(); got != 0 {
		t.Fatalf("max_output_tokens = %d, want 0 (unlimited application cap)", got)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "max_tool_count").Int(); got != 0 {
		t.Fatalf("max_tool_count = %d, want 0 (unlimited application cap)", got)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "allow_service_tier").Bool(); got {
		t.Fatal("service_tier should be denied by default")
	}
}

func TestUpdateClaudeConfigPersistsSecurityPolicy(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	h := &Handler{store: store, db: db}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("PUT", "/api/admin/settings/claude-config", strings.NewReader(`{"fingerprint_mode":"force","max_output_tokens":4096,"max_tool_count":4,"max_tool_schema_bytes":65536,"allowed_beta_headers":["approved-beta"],"allow_service_tier":true}`))
	h.UpdateClaudeConfig(c)
	if recorder.Code != 200 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	security := store.ClaudeSecurityConfig()
	if !security.AllowServiceTier || security.MaxOutputTokens != 4096 || security.MaxToolCount != 4 || security.MaxToolSchemaBytes != 65536 || len(security.AllowedBetaHeaders) != 1 || security.AllowedBetaHeaders[0] != "approved-beta" {
		t.Fatalf("runtime Claude security config = %+v", security)
	}
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil || !strings.Contains(settings.ClaudeConfig, `"allow_service_tier":true`) {
		t.Fatalf("persisted Claude config = %q err=%v", settings.ClaudeConfig, err)
	}
}
