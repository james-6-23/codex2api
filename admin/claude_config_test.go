package admin

import (
	"context"
	"encoding/json"
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
	if got := gjson.GetBytes(recorder.Body.Bytes(), "client_platform").String(); got != "any" {
		t.Fatalf("client_platform = %q, want any", got)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "version_policy").String(); got != "passthrough" {
		t.Fatalf("version_policy = %q, want passthrough", got)
	}
}

func TestUpdateClaudeConfigPersistsSecurityPolicy(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	h := &Handler{store: store, db: db}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("PUT", "/api/admin/settings/claude-config", strings.NewReader(`{"fingerprint_mode":"force","client_platform":"claude_code_cli_only","version_policy":"minimum","client_version":"2.1.251","max_output_tokens":4096,"max_tool_count":4,"max_tool_schema_bytes":65536,"allowed_beta_headers":["approved-beta"],"allow_service_tier":true}`))
	h.UpdateClaudeConfig(c)
	if recorder.Code != 200 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	security := store.ClaudeSecurityConfig()
	if !security.AllowServiceTier || security.MaxOutputTokens != 4096 || security.MaxToolCount != 4 || security.MaxToolSchemaBytes != 65536 || len(security.AllowedBetaHeaders) != 1 || security.AllowedBetaHeaders[0] != "approved-beta" {
		t.Fatalf("runtime Claude security config = %+v", security)
	}
	if got := store.ClaudeClientPlatform(); got != auth.ClaudeClientPlatformCLIOnly {
		t.Fatalf("runtime client platform = %q", got)
	}
	if got := store.ClaudeVersionPolicy(); got != auth.ClaudeVersionPolicyMinimum || store.ClaudeClientVersion() != "2.1.251" {
		t.Fatalf("runtime client version policy = %q/%q", got, store.ClaudeClientVersion())
	}
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil || !strings.Contains(settings.ClaudeConfig, `"allow_service_tier":true`) {
		t.Fatalf("persisted Claude config = %q err=%v", settings.ClaudeConfig, err)
	}
}

func TestParseAccountSchedulerUpdateClaudeClientPolicy(t *testing.T) {
	update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
		ClaudeClientPlatform: json.RawMessage(`"claude_code_cli_only"`),
		ClaudeVersionPolicy:  json.RawMessage(`"fixed"`),
		ClaudeClientVersion:  json.RawMessage(`"2.1.251"`),
	})
	if err != nil {
		t.Fatalf("parseAccountSchedulerUpdate: %v", err)
	}
	if update.ClaudeClientPlatform.Value != string(auth.ClaudeClientPlatformCLIOnly) || update.ClaudeVersionPolicy.Value != string(auth.ClaudeVersionPolicyFixed) || update.ClaudeClientVersion.Value != "2.1.251" {
		t.Fatalf("parsed policy = %+v", update)
	}
	if update.CredentialUpdates[auth.ClaudeClientPlatformCredentialKey] != string(auth.ClaudeClientPlatformCLIOnly) {
		t.Fatalf("credential platform = %+v", update.CredentialUpdates)
	}
}

func TestParseAccountSchedulerUpdateRejectsVersionPolicyWithoutVersion(t *testing.T) {
	if _, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
		ClaudeVersionPolicy: json.RawMessage(`"minimum"`),
	}); err == nil {
		t.Fatal("minimum account policy without client version must be rejected")
	}
}
