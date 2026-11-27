package admin

import (
	"context"
	"encoding/json"
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

func TestClaudeOAuthKeepaliveSettingsAndAccountDiagnosticsAPI(t *testing.T) {
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

	// Default off.
	if store.GetClaudeOAuthKeepalive() {
		t.Fatal("claude_oauth_keepalive_enabled must default off")
	}

	// Enable via PUT /api/admin/settings alongside lazy_mode.
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"lazy_mode":true,"claude_oauth_keepalive_enabled":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdateSettings(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("UpdateSettings status=%d body=%s", rec.Code, rec.Body.String())
	}
	var putResp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &putResp); err != nil {
		t.Fatal(err)
	}
	if putResp["claude_oauth_keepalive_enabled"] != true || putResp["codex_oauth_keepalive_enabled"] != false {
		t.Fatalf("unexpected PUT response keepalive flags: claude=%v codex=%v", putResp["claude_oauth_keepalive_enabled"], putResp["codex_oauth_keepalive_enabled"])
	}
	if !store.GetClaudeOAuthKeepalive() || store.GetCodexOAuthKeepalive() {
		t.Fatalf("store runtime flags mismatch: claude=%t codex=%t", store.GetClaudeOAuthKeepalive(), store.GetCodexOAuthKeepalive())
	}
	stored, err := db.GetSystemSettings(context.Background())
	if err != nil || !stored.ClaudeOAuthKeepaliveEnabled || stored.CodexOAuthKeepaliveEnabled {
		t.Fatalf("persisted flags mismatch: claude=%t codex=%t err=%v", stored.ClaudeOAuthKeepaliveEnabled, stored.CodexOAuthKeepaliveEnabled, err)
	}

	// Verify runtime status exposes keepalive flags.
	rtStatus := h.buildRuntimeStatus(context.Background(), httptest.NewRequest(http.MethodGet, "/api/admin/runtime-status", nil))
	if !rtStatus.Probes.LazyMode || !rtStatus.Probes.ClaudeOAuthKeepaliveEnabled || rtStatus.Probes.CodexOAuthKeepaliveEnabled {
		t.Fatalf("runtime probes mismatch: %+v", rtStatus.Probes)
	}

	// Verify account detail response surfaces Claude OAuth refresh diagnostics only for OAuth accounts.
	ctx := context.Background()
	lastRefresh := time.Now().UTC().Format(time.RFC3339)
	nextRetry := time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)
	oauthID, err := db.InsertAccountWithUpstream(ctx, "claude-oauth", auth.UpstreamClaude, "oauth", map[string]any{
		"upstream_type":                            auth.UpstreamClaude,
		"claude_auth_kind":                         auth.ClaudeAuthKindOAuth,
		"access_token":                             "at",
		"refresh_token":                            "rt",
		auth.ClaudeLastRefreshAtCredentialKey:      lastRefresh,
		auth.ClaudeRefreshErrorCredentialKey:       "upstream 503",
		auth.ClaudeRefreshNextRetryAtCredentialKey: nextRetry,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	setupID, err := db.InsertAccountWithUpstream(ctx, "claude-setup", auth.UpstreamClaude, "oauth", map[string]any{
		"upstream_type":                       auth.UpstreamClaude,
		"claude_auth_kind":                    auth.ClaudeAuthKindSetupToken,
		"access_token":                        "setup-at",
		auth.ClaudeLastRefreshAtCredentialKey: lastRefresh,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	oauthRow, err := db.GetAccountByID(ctx, oauthID)
	if err != nil {
		t.Fatal(err)
	}
	setupRow, err := db.GetAccountByID(ctx, setupID)
	if err != nil {
		t.Fatal(err)
	}

	oauthResp := h.buildAccountResponse(oauthRow, nil, nil, nil, nil, true)
	if oauthResp.ClaudeLastRefreshAt != lastRefresh || oauthResp.ClaudeRefreshError != "upstream 503" || oauthResp.ClaudeRefreshNextRetryAt != nextRetry {
		t.Fatalf("oauth refresh diagnostics missing: last=%q err=%q retry=%q", oauthResp.ClaudeLastRefreshAt, oauthResp.ClaudeRefreshError, oauthResp.ClaudeRefreshNextRetryAt)
	}
	setupResp := h.buildAccountResponse(setupRow, nil, nil, nil, nil, true)
	if setupResp.ClaudeLastRefreshAt != "" || setupResp.ClaudeRefreshError != "" || setupResp.ClaudeRefreshNextRetryAt != "" {
		t.Fatalf("setup token should not expose oauth refresh diagnostics: %+v", setupResp)
	}
}
