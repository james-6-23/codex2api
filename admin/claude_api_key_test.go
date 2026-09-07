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
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestClaudeAPIKeyImportParsing(t *testing.T) {
	for _, raw := range []string{
		`{"auth_kind":"api_key","base_url":"https://example.com/v1/","api_key":"key"}`,
		`{"credentials":{"auth_kind":"api_key","base_url":"https://example.com/v1/","access_token":"key","refresh_token":"ignored","expires_at":"ignored"}}`,
	} {
		docs, err := parseClaudeImportDocuments([]byte(raw))
		if err != nil || len(docs) != 1 {
			t.Fatalf("parse: %v", err)
		}
		doc := docs[0]
		if doc.AuthKind != auth.ClaudeAuthKindAPIKey || doc.BaseURL != "https://example.com/v1" || doc.AccessToken != "key" || doc.RefreshToken != "" || doc.ExpiresAt != "" {
			t.Fatalf("parsed API key document = %+v", doc)
		}
	}
	for _, raw := range []string{
		`{"auth_kind":"api_key","api_key":"key"}`,
		`{"auth_kind":"api_key","base_url":"https://example.com"}`,
		`{"auth_kind":"api_key","base_url":"https://example.com","api_key":" "}`,
		`{"auth_kind":"api_key","base_url":"https://example.com","api_key":"a\nb"}`,
		`{"auth_kind":"api_key","base_url":"https://example.com","api_key":"a","access_token":"b"}`,
		// Reserved gateway-owned headers can't be smuggled in as custom headers.
		`{"auth_kind":"api_key","base_url":"https://example.com","api_key":"a","custom_headers":{"Authorization":"Bearer x"}}`,
		`{"auth_kind":"api_key","base_url":"https://example.com","api_key":"a","custom_headers":{"x-api-key":"x"}}`,
		`{"auth_kind":"api_key","base_url":"https://example.com","api_key":"a","fingerprint_headers":{"Accept":"text/plain"}}`,
		`{"auth_kind":"api_key","base_url":"https://example.com","api_key":"a","claude_fingerprint_mode":"random"}`,
	} {
		if _, err := parseClaudeImportDocuments([]byte(raw)); err == nil {
			t.Fatalf("invalid input accepted: %s", raw)
		}
	}
	// API Key documents accept arbitrary operator headers (issue #647), unlike
	// OAuth documents whose fingerprint_headers stay identity-only.
	docs, err := parseClaudeImportDocuments([]byte(`{"auth_kind":"api_key","base_url":"https://example.com","api_key":"a","claude_fingerprint_mode":"force","custom_headers":{"x-gateway-tenant":"team-a","User-Agent":"gateway/1"}}`))
	if err != nil || len(docs) != 1 || docs[0].ClaudeFingerprintMode != auth.ClaudeFingerprintModeForce || docs[0].CustomHeaders["X-Gateway-Tenant"] != "team-a" || docs[0].CustomHeaders["User-Agent"] != "gateway/1" || len(docs[0].FingerprintHeaders) != 0 {
		t.Fatalf("api_key custom headers: %+v %v", docs, err)
	}
	if _, err := parseClaudeImportDocuments([]byte(`{"auth_kind":"oauth","refresh_token":"rt","custom_headers":{"x-gateway-tenant":"team-a"}}`)); err == nil {
		t.Fatal("OAuth documents must keep rejecting non-identity headers")
	}
}

// TestClaudeAPIKeyCustomHeadersAndIdentityLifecycle covers issue #647 end to
// end on the admin side: import persists custom_headers + client identity mode,
// the detail response and export round trip carry them, the scheduler update
// endpoint validates reserved names, and model discovery presents them.
func TestClaudeAPIKeyCustomHeadersAndIdentityLifecycle(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	store.SetClaudeFingerprintModeDefault(auth.ClaudeFingerprintModePreserve)
	h := &Handler{db: db, store: store}
	var discovery http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discovery = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-4-5"}]}`)
	}))
	t.Cleanup(server.Close)
	input := fmt.Sprintf(`{"auth_kind":"api_key","base_url":%q,"api_key":"example-secret","name":"Gateway","claude_fingerprint_mode":"force","custom_headers":{"X-Gateway-Tenant":"team-a","x-api-key":"ignored"}}`, server.URL)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(input))
	h.ImportClaudeToken(c)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "x-api-key") && !strings.Contains(recorder.Body.String(), "X-Api-Key") {
		t.Fatalf("reserved header import must fail: %d %s", recorder.Code, recorder.Body.String())
	}
	input = fmt.Sprintf(`{"auth_kind":"api_key","base_url":%q,"api_key":"example-secret","name":"Gateway","claude_fingerprint_mode":"force","custom_headers":{"X-Gateway-Tenant":"team-a"}}`, server.URL)
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(input))
	h.ImportClaudeToken(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("import: %d %s", recorder.Code, recorder.Body.String())
	}
	if discovery == nil || discovery.Get("X-Gateway-Tenant") != "team-a" || discovery.Get("X-App") != "cli" || !strings.HasPrefix(discovery.Get("User-Agent"), "claude-cli/") || discovery.Get("x-api-key") != "example-secret" {
		t.Fatalf("model discovery must present the account headers: %v", discovery)
	}
	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	row := rows[0]
	if row.GetCredential(auth.ClaudeFingerprintModeCredentialKey) != auth.ClaudeFingerprintModeForce || row.GetCredentialStringMap("custom_headers")["X-Gateway-Tenant"] != "team-a" || row.GetCredential("timezone") != "" {
		t.Fatalf("persisted credentials: mode=%q headers=%v", row.GetCredential(auth.ClaudeFingerprintModeCredentialKey), row.GetCredentialStringMap("custom_headers"))
	}
	account := store.FindByID(row.ID)
	if account == nil || account.EffectiveClaudeFingerprintMode(store.ClaudeFingerprintModeDefault()) != auth.ClaudeFingerprintModeForce || account.GetCustomHeaders()["X-Gateway-Tenant"] != "team-a" {
		t.Fatal("runtime account lost custom headers or identity mode")
	}
	response := h.buildAccountResponse(row, account, nil, nil, nil, true)
	if response.CustomHeaders["X-Gateway-Tenant"] != "team-a" || response.ClaudeFingerprintMode != auth.ClaudeFingerprintModeForce || !strings.HasPrefix(response.ClaudeUserAgent, "claude-cli/") {
		t.Fatalf("detail response: headers=%v mode=%q ua=%q", response.CustomHeaders, response.ClaudeFingerprintMode, response.ClaudeUserAgent)
	}
	if public, _ := json.Marshal(response); strings.Contains(string(public), "example-secret") {
		t.Fatal("detail response leaked the API key")
	}
	entry, ok := claudeAccountRowToExportEntry(row, nil)
	if !ok || entry.ClaudeFingerprintMode != auth.ClaudeFingerprintModeForce || entry.CustomHeaders["X-Gateway-Tenant"] != "team-a" || len(entry.FingerprintHeaders) != 0 {
		t.Fatalf("export entry: %+v", entry)
	}
	encoded, err := marshalClaudeExportEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := parseClaudeImportDocuments(encoded)
	if err != nil || len(docs) != 1 || docs[0].ClaudeFingerprintMode != auth.ClaudeFingerprintModeForce || docs[0].CustomHeaders["X-Gateway-Tenant"] != "team-a" {
		t.Fatalf("roundtrip: %+v %v", docs, err)
	}

	// Scheduler update: reserved names rejected, valid headers + mode applied live.
	patch := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(row.ID)}}
		ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", row.ID), strings.NewReader(body))
		h.UpdateAccountScheduler(ctx)
		return rec
	}
	if rec := patch(`{"custom_headers":{"Authorization":"Bearer x"}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("reserved header update accepted: %d %s", rec.Code, rec.Body.String())
	}
	if rec := patch(`{"custom_headers":{"User-Agent":"gateway/2"},"claude_fingerprint_mode":"preserve"}`); rec.Code != http.StatusOK {
		t.Fatalf("update: %d %s", rec.Code, rec.Body.String())
	}
	updated, err := db.GetAccountByID(context.Background(), row.ID)
	if err != nil || updated.GetCredentialStringMap("custom_headers")["User-Agent"] != "gateway/2" || len(updated.GetCredentialStringMap("custom_headers")) != 1 || updated.GetCredential(auth.ClaudeFingerprintModeCredentialKey) != auth.ClaudeFingerprintModePreserve {
		t.Fatalf("updated credentials: %v %v", updated.GetCredentialStringMap("custom_headers"), err)
	}
	if account.GetCustomHeaders()["User-Agent"] != "gateway/2" || account.EffectiveClaudeFingerprintMode("") != auth.ClaudeFingerprintModePreserve {
		t.Fatal("runtime account not updated in place")
	}
	if rec := patch(`{"custom_headers":null,"claude_fingerprint_mode":""}`); rec.Code != http.StatusOK {
		t.Fatalf("clear: %d %s", rec.Code, rec.Body.String())
	}
	cleared, err := db.GetAccountByID(context.Background(), row.ID)
	if err != nil || len(cleared.GetCredentialStringMap("custom_headers")) != 0 || cleared.GetCredential(auth.ClaudeFingerprintModeCredentialKey) != "" {
		t.Fatalf("clear did not reset: %v %q", cleared.GetCredentialStringMap("custom_headers"), cleared.GetCredential(auth.ClaudeFingerprintModeCredentialKey))
	}
	if len(account.GetCustomHeaders()) != 0 || account.EffectiveClaudeFingerprintMode(store.ClaudeFingerprintModeDefault()) != "" {
		t.Fatal("API key account must fall back to passthrough, not the OAuth global default")
	}
}

func TestClaudeAPIKeyImportExportAndRuntime(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	h := &Handler{db: db, store: store}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/models" || r.Header.Get("x-api-key") != "example-secret" || r.Header.Get("Authorization") != "" {
			t.Errorf("model discovery must use account URL and API key: %s", r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-4-5"},{"id":"gpt-4o"},{"id":"deepseek-chat"}]}`)
	}))
	t.Cleanup(server.Close)
	input := fmt.Sprintf(`{"auth_kind":"api_key","base_url":%q,"api_key":"example-secret","refresh_token":"ignored","name":"My API"}`, server.URL+"/api/v1/")
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(input))
	h.ImportClaudeToken(c)
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "example-secret") {
		t.Fatalf("import: %d %s", recorder.Code, recorder.Body.String())
	}
	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	row := rows[0]
	if models := row.GetCredentialStringSlice("models"); len(models) != 1 || models[0] != "claude-sonnet-4-5" {
		t.Fatalf("mixed discovery leaked unsupported models: %v", models)
	}
	if row.GetCredential("access_token") != "example-secret" || row.GetCredential("refresh_token") != "" || row.GetCredential("expires_at") != "" || len(row.GetCredentialStringMap("custom_headers")) != 0 {
		t.Fatal("API key storage leaked OAuth metadata or lost the secret")
	}
	account := store.FindByID(row.ID)
	if account == nil || !account.IsClaudeAPIKey() || account.GetClaudeBaseURL() != server.URL+"/api/v1" || !account.ExpiresAt.IsZero() || !account.IsAvailable() {
		t.Fatal("API key is not available in runtime store")
	}
	reloaded, err := store.BuildTransientAccountByID(context.Background(), row.ID)
	if err != nil || !reloaded.IsClaudeAPIKey() || reloaded.GetClaudeBaseURL() != account.GetClaudeBaseURL() || !reloaded.ExpiresAt.IsZero() {
		t.Fatalf("reload: %v", err)
	}
	entry, ok := claudeAccountRowToExportEntry(row, nil)
	if !ok {
		t.Fatal("API key must be exportable")
	}
	encoded, err := marshalClaudeExportEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := parseClaudeImportDocuments(encoded)
	if err != nil || len(docs) != 1 || docs[0].BaseURL != account.GetClaudeBaseURL() || docs[0].AccessToken != "example-secret" || docs[0].RefreshToken != "" || docs[0].ExpiresAt != "" {
		t.Fatalf("roundtrip: %v", err)
	}
	response := h.buildAccountResponse(row, account, nil, nil, nil, true)
	public, err := json.Marshal(response)
	if err != nil || strings.Contains(string(public), "example-secret") || response.ClaudeAuthKind != "api_key" || response.ClaudeBaseURL != account.GetClaudeBaseURL() {
		t.Fatal("invalid public account response")
	}
	item := h.buildAccountListSnapshotItem(row, nil, nil, nil, nil)
	if !accountListItemMatches(item, accountPageQuery{AuthKind: "api_key"}, database.UpstreamChannelClaude) || accountListItemMatches(item, accountPageQuery{AuthKind: "oauth"}, database.UpstreamChannelClaude) {
		t.Fatal("API key filter mismatch")
	}
	summary, _ := summarizeAccountList([]*accountListSnapshotItem{item}, database.UpstreamChannelClaude)
	if summary.APIKey != 1 || summary.OAuth != 0 || summary.SetupToken != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	updates := map[string]interface{}{}
	if applied, err := prepareClaudeTimezoneCredentialUpdateWithHeaders(row, "Asia/Shanghai", updates, nil); err != nil || applied || len(updates) != 0 {
		t.Fatal("timezone must not create API key fingerprints")
	}
	h.executeClaudeUsageProbe = func(context.Context, *auth.Account, []byte) (*http.Response, error) {
		t.Error("API key usage probe executed")
		return nil, nil
	}
	if err := h.probeUsageViaClaudeMessages(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if windows, err := h.fetchClaudeOAuthUsage(context.Background(), account); err != nil || len(windows) != 0 {
		t.Fatal("API key OAuth usage must be skipped")
	}
	if err := h.store.RefreshSingle(context.Background(), account.ID()); err != nil {
		t.Fatal(err)
	}
	if account.NeedsUsageProbe(time.Minute) {
		t.Fatal("API key scheduled a usage probe")
	}
}

func TestClaudeAPIKeyModelCatalogsAreIndependent(t *testing.T) {
	accounts := []*auth.Account{
		{DBID: 1, UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, AccessToken: "a", Status: auth.StatusReady, PlanType: "claude"},
		{DBID: 2, UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindAPIKey, AccessToken: "b", Status: auth.StatusReady, PlanType: "claude"},
		{DBID: 3, UpstreamType: auth.UpstreamClaude, AccessToken: "c", Status: auth.StatusReady, PlanType: "claude"},
	}
	groups := groupAccountsByPlan(accounts, nil, func(int) int { return 0 })
	if len(groups) != 3 {
		t.Fatalf("API key catalogs must not overwrite other accounts: %+v", groups)
	}
}
