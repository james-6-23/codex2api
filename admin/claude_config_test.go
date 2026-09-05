package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
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

func TestGetClaudeConfigExposesCLIVersionSyncState(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("2.1.300")
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	h := &Handler{store: store}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h.GetClaudeConfig(c)
	body := recorder.Body.Bytes()
	if !gjson.GetBytes(body, "cli_version_sync_enabled").Bool() {
		t.Fatal("cli_version_sync_enabled should default true")
	}
	if got := gjson.GetBytes(body, "cli_version_sync_interval_hours").Int(); got != 12 {
		t.Fatalf("interval = %d", got)
	}
	if got := gjson.GetBytes(body, "synced_cli_version").String(); got != "2.1.300" {
		t.Fatalf("synced = %q", got)
	}
	if got := gjson.GetBytes(body, "builtin_cli_version").String(); got != auth.BuiltinClaudeCLIVersion {
		t.Fatalf("builtin = %q", got)
	}
	if got := gjson.GetBytes(body, "effective_cli_version").String(); got != "2.1.300" {
		t.Fatalf("effective = %q", got)
	}
}

func TestUpdateClaudeConfigPersistsCLIVersionSyncFields(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	h := &Handler{store: store, db: newTestAdminDB(t)}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("PUT", "/settings/claude-config", strings.NewReader(`{"fingerprint_mode":"force","cli_version_sync_enabled":false,"cli_version_sync_interval_hours":48,"synced_cli_version":"9.9.9"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateClaudeConfig(c)
	if recorder.Code != 200 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if store.ClaudeCLIVersionSyncEnabled() || store.ClaudeCLIVersionSyncIntervalHours() != 48 {
		t.Fatalf("store not updated: enabled=%v hours=%d", store.ClaudeCLIVersionSyncEnabled(), store.ClaudeCLIVersionSyncIntervalHours())
	}
	if auth.ClaudeSyncedCLIVersion() == "9.9.9" {
		t.Fatal("PUT must ignore read-only synced_cli_version")
	}
	settings, err := h.db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cfg := auth.ParseClaudeConfig(settings.ClaudeConfig)
	if cfg.CLIVersionSyncEnabledValue() || cfg.CLIVersionSyncIntervalHours != 48 {
		t.Fatalf("persisted cfg = %+v", cfg)
	}
}

func TestClaudeConfigSyncCLIVersion_PartialFailureStillReturns200WithWarning(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	db := newTestAdminDB(t)
	ctx := context.Background()

	id, err := db.InsertAccountWithUpstream(ctx, "claude-a", "anthropic", "oauth", map[string]interface{}{
		"upstream_type":  "claude",
		"access_token":   "tok",
		"custom_headers": map[string]interface{}{"User-Agent": "claude-cli/2.1.219 (external, cli)"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.300"}`))
	}))
	defer gh.Close()
	proxy.SetClaudeVersionSourceURLsForTest(gh.URL, gh.URL)
	t.Cleanup(func() { proxy.SetClaudeVersionSourceURLsForTest("", "") })

	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	store.SetAccountsForTest([]*auth.Account{{DBID: id, UpstreamType: auth.UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}})

	// Soft-delete the row directly in the DB (bypassing the store), so the
	// fingerprint persist inside SyncClaudeCLIVersion hits sql.ErrNoRows
	// while the in-memory Store still thinks the account is live.
	if err := db.SoftDeleteAccount(ctx, id); err != nil {
		t.Fatal(err)
	}

	h := &Handler{store: store, db: db}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/settings/claude-config/cli-version/sync", nil)
	h.SyncClaudeCLIVersion(c)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.Bytes()
	if got := gjson.GetBytes(body, "fetched_version").String(); got != "2.1.300" {
		t.Fatalf("fetched_version = %q, want 2.1.300", got)
	}
	if got := gjson.GetBytes(body, "warning").String(); got == "" {
		t.Fatal("warning should be non-empty when the fingerprint persist fails after a successful fetch")
	}
}

func TestClaudeConfigSyncCLIVersion_FetchFailureReturns502(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	}))
	defer bad.Close()
	proxy.SetClaudeVersionSourceURLsForTest(bad.URL, bad.URL)
	t.Cleanup(func() { proxy.SetClaudeVersionSourceURLsForTest("", "") })

	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	h := &Handler{store: store, db: newTestAdminDB(t)}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/settings/claude-config/cli-version/sync", nil)
	h.SyncClaudeCLIVersion(c)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", recorder.Code, recorder.Body.String())
	}
}

func TestClaudeConfigFirstTokenTimeoutAndKeepaliveRoundTrip(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	h := &Handler{store: store, db: db}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	h.GetClaudeConfig(c)
	if got := gjson.GetBytes(recorder.Body.Bytes(), "first_token_timeout_seconds").Int(); got != int64(auth.DefaultClaudeFirstTokenTimeoutSeconds) {
		t.Fatalf("default first_token_timeout_seconds = %d, want %d", got, auth.DefaultClaudeFirstTokenTimeoutSeconds)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "stream_keepalive_enabled"); !got.Exists() || !got.Bool() {
		t.Fatalf("stream_keepalive_enabled must default to true, got %s", got.Raw)
	}

	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("PUT", "/api/admin/settings/claude-config", strings.NewReader(`{"first_token_timeout_seconds":90,"stream_keepalive_enabled":false}`))
	h.UpdateClaudeConfig(c)
	if recorder.Code != 200 {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "first_token_timeout_seconds").Int(); got != 90 {
		t.Fatalf("response first_token_timeout_seconds = %d, want 90", got)
	}
	if store.ClaudeFirstTokenTimeoutSeconds() != 90 || store.ClaudeStreamKeepaliveEnabled() {
		t.Fatalf("runtime store not updated: timeout=%d keepalive=%v", store.ClaudeFirstTokenTimeoutSeconds(), store.ClaudeStreamKeepaliveEnabled())
	}
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw := settings.ClaudeConfig
	persisted := auth.ParseClaudeConfig(raw)
	if persisted.FirstTokenTimeoutSecondsValue() != 90 || persisted.StreamKeepaliveEnabledValue() {
		t.Fatalf("persisted config = %s", raw)
	}

	// Explicit 0 must persist as 0 (follow global), not be re-defaulted to 120.
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("PUT", "/api/admin/settings/claude-config", strings.NewReader(`{"first_token_timeout_seconds":0}`))
	h.UpdateClaudeConfig(c)
	if store.ClaudeFirstTokenTimeoutSeconds() != 0 {
		t.Fatalf("explicit 0 must disable the Claude timeout, got %d", store.ClaudeFirstTokenTimeoutSeconds())
	}
	settings, _ = db.GetSystemSettings(context.Background())
	raw = settings.ClaudeConfig
	if auth.ParseClaudeConfig(raw).FirstTokenTimeoutSecondsValue() != 0 {
		t.Fatalf("persisted explicit 0 was re-defaulted: %s", raw)
	}
}
