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
	} {
		if _, err := parseClaudeImportDocuments([]byte(raw)); err == nil {
			t.Fatalf("invalid input accepted: %s", raw)
		}
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
		w.WriteHeader(http.StatusNotFound) // Model discovery is best-effort.
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
