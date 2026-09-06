package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestNextClaudeIndexedName(t *testing.T) {
	if got := nextClaudeIndexedName(nil, "claude"); got != 1 {
		t.Fatalf("empty = %d", got)
	}
	names := []string{"claude-3", "Claude-12", "claude-x", "claude-", "team-5", "claude-07"}
	if got := nextClaudeIndexedName(names, "claude"); got != 13 {
		t.Fatalf("next = %d, want 13", got)
	}
	if got := nextClaudeIndexedName(names, "team"); got != 6 {
		t.Fatalf("team next = %d, want 6", got)
	}
	if got := claudeSetupTokenAccountName("", 4); got != "claude-4" {
		t.Fatalf("default name = %q", got)
	}
}

func TestParseClaudeAuthMode(t *testing.T) {
	for _, raw := range []string{"", "oauth", " OAuth "} {
		if setup, err := parseClaudeAuthMode(raw); err != nil || setup {
			t.Fatalf("mode %q = (%v,%v)", raw, setup, err)
		}
	}
	for _, raw := range []string{"setup_token", "setup-token", "SETUP"} {
		if setup, err := parseClaudeAuthMode(raw); err != nil || !setup {
			t.Fatalf("mode %q = (%v,%v)", raw, setup, err)
		}
	}
	if _, err := parseClaudeAuthMode("api_key"); err == nil {
		t.Fatal("api_key mode must be rejected")
	}
}

func TestGenerateClaudeAuthURLSetupTokenMode(t *testing.T) {
	h := &Handler{}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/oauth/auth-url", strings.NewReader(`{"mode":"setup_token"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.GenerateClaudeAuthURL(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var resp struct {
		AuthURL     string `json:"auth_url"`
		State       string `json:"state"`
		Mode        string `json:"mode"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Mode != auth.ClaudeAuthKindSetupToken || resp.RedirectURI != auth.ClaudeOAuthRedirectURI || !strings.Contains(resp.AuthURL, "scope=user%3Ainference") {
		t.Fatalf("response = %+v", resp)
	}
	pending, ok := claudeOAuthTakeSession(resp.State)
	if !ok || !pending.exchange.SetupToken || pending.exchange.RedirectURI != auth.ClaudeOAuthRedirectURI {
		t.Fatalf("pending session = %+v ok=%v", pending, ok)
	}

	// 空请求体保持旧行为:完整 OAuth。
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/oauth/auth-url", nil)
	h.GenerateClaudeAuthURL(c)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"mode":"oauth"`) {
		t.Fatalf("default mode response = %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestClaudeImportParserAcceptsSetupTokenDocuments(t *testing.T) {
	docs, err := parseClaudeImportDocuments([]byte(`[{"type":"claude","auth_kind":"setup_token","access_token":"sk-ant-oat01-explicit","refresh_token":"ignored"},{"upstream_type":"claude","access_token":"sk-ant-oat01-shaped"}]`))
	if err != nil {
		t.Fatalf("parse setup token docs: %v", err)
	}
	if len(docs) != 2 || docs[0].AuthKind != auth.ClaudeAuthKindSetupToken || docs[0].RefreshToken != "" || docs[1].AuthKind != auth.ClaudeAuthKindSetupToken {
		t.Fatalf("docs = %+v", docs)
	}
	if _, err := parseClaudeImportDocuments([]byte(`{"type":"claude","access_token":"plain-at-without-rt"}`)); err == nil {
		t.Fatal("bare OAuth access token without refresh_token must be rejected")
	}
	if _, err := parseClaudeImportDocuments([]byte(`{"type":"claude","auth_kind":"oauth","access_token":"sk-ant-oat01-x"}`)); err == nil {
		t.Fatal("explicit oauth without refresh_token must be rejected")
	}
}

func TestClaudeExportEntryCarriesSetupTokenKind(t *testing.T) {
	row := &database.AccountRow{ID: 7, Name: "claude-7", Platform: "anthropic", Enabled: true, Credentials: map[string]interface{}{
		"upstream_type":                  auth.UpstreamClaude,
		"access_token":                   "sk-ant-oat01-export",
		auth.ClaudeAuthKindCredentialKey: auth.ClaudeAuthKindSetupToken,
	}}
	entry, ok := claudeAccountRowToExportEntry(row, nil)
	if !ok || entry.AuthKind != auth.ClaudeAuthKindSetupToken || entry.RefreshToken != "" || entry.AccessToken != "sk-ant-oat01-export" {
		t.Fatalf("entry=%+v ok=%v", entry, ok)
	}
	legacyNoRT := &database.AccountRow{ID: 8, Platform: "anthropic", Credentials: map[string]interface{}{"upstream_type": auth.UpstreamClaude, "access_token": "at-only"}}
	if _, ok := claudeAccountRowToExportEntry(legacyNoRT, nil); ok {
		t.Fatal("OAuth row without refresh_token must not be exported")
	}
}

func TestImportClaudeSetupTokensBatch(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	body := `{"text":"sk-ant-oat01-first\nsk-ant-oat01-second, sk-ant-oat01-first","tokens":["sk-ant-oat01-third"],"timezone":"Asia/Shanghai"}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import-setup-tokens", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ImportClaudeSetupTokens(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Total    int `json:"total"`
		Imported int `json:"imported"`
		Failed   int `json:"failed"`
		Items    []struct {
			ID int64 `json:"id"`
			OK bool  `json:"ok"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 3 || result.Imported != 3 || result.Failed != 0 {
		t.Fatalf("result = %+v", result)
	}
	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d", len(rows))
	}
	names := map[string]bool{}
	for _, row := range rows {
		names[row.Name] = true
		if row.GetCredential(auth.ClaudeAuthKindCredentialKey) != auth.ClaudeAuthKindSetupToken {
			t.Fatalf("row %d auth kind = %q", row.ID, row.GetCredential(auth.ClaudeAuthKindCredentialKey))
		}
		if row.GetCredential("refresh_token") != "" || row.GetCredential("expires_at") == "" {
			t.Fatalf("row %d credentials = %+v", row.ID, row.Credentials)
		}
		if row.GetCredential("timezone") != "Asia/Shanghai" {
			t.Fatalf("row %d timezone = %q", row.ID, row.GetCredential("timezone"))
		}
	}
	for _, want := range []string{"claude-1", "claude-2", "claude-3"} {
		if !names[want] {
			t.Fatalf("names = %v, missing %s", names, want)
		}
	}

	// 重复导入同一枚令牌:AT 即身份,必须 409。
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import-setup-tokens", strings.NewReader(`{"tokens":["sk-ant-oat01-first"]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ImportClaudeSetupTokens(c)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	// 没有任何令牌:400。
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import-setup-tokens", strings.NewReader(`{"text":"nothing"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ImportClaudeSetupTokens(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty status=%d", recorder.Code)
	}
}

func TestBuildAccountResponseCarriesClaudeAuthKind(t *testing.T) {
	row := &database.AccountRow{ID: 9, Name: "claude-setup", Status: "active", Enabled: true, Credentials: map[string]interface{}{
		"upstream_type": auth.UpstreamClaude, "access_token": "sk-ant-oat01-x", auth.ClaudeAuthKindCredentialKey: auth.ClaudeAuthKindSetupToken,
	}}
	h := &Handler{store: auth.NewStore(nil, nil, nil)}
	response := h.buildAccountResponse(row, nil, nil, nil, nil, false)
	if response.ClaudeAuthKind != auth.ClaudeAuthKindSetupToken || response.ATOnly {
		t.Fatalf("response auth kind = %q at_only=%v", response.ClaudeAuthKind, response.ATOnly)
	}
	item := h.buildAccountListSnapshotItem(row, nil, nil, nil, nil)
	if item.ClaudeAuthKind != auth.ClaudeAuthKindSetupToken {
		t.Fatalf("list item auth kind = %q", item.ClaudeAuthKind)
	}
	if !accountListItemMatches(item, accountPageQuery{AuthKind: auth.ClaudeAuthKindSetupToken}, database.UpstreamChannelClaude) ||
		accountListItemMatches(item, accountPageQuery{AuthKind: "oauth"}, database.UpstreamChannelClaude) {
		t.Fatal("Claude auth_kind filter must distinguish setup_token from oauth")
	}
	summary, _ := summarizeAccountList([]*accountListSnapshotItem{item}, database.UpstreamChannelClaude)
	if summary.SetupToken != 1 || summary.OAuth != 0 {
		t.Fatalf("summary = %+v", summary)
	}
}

func TestClaudeImportParserAcceptsRefreshTokenOnlyOAuth(t *testing.T) {
	docs, err := parseClaudeImportDocuments([]byte(`{"type":"claude","refresh_token":"sk-ant-ort01-only"}`))
	if err != nil {
		t.Fatalf("refresh-token-only document must parse: %v", err)
	}
	if len(docs) != 1 || docs[0].AuthKind != auth.ClaudeAuthKindOAuth || docs[0].AccessToken != "" || docs[0].RefreshToken != "sk-ant-ort01-only" {
		t.Fatalf("docs = %+v", docs)
	}
	if _, err := parseClaudeImportDocuments([]byte(`{"type":"claude","auth_kind":"setup_token","refresh_token":"sk-ant-ort01-only"}`)); err == nil {
		t.Fatal("setup_token without access_token must be rejected")
	}
	if _, err := parseClaudeImportDocuments([]byte(`{"type":"claude"}`)); err == nil {
		t.Fatal("document without any token must be rejected")
	}
}

func TestImportClaudePastedTokensMixesSetupAndRefreshTokens(t *testing.T) {
	db := newTestAdminDB(t)
	var refreshed []string
	h := &Handler{db: db, refreshClaudeTokensForImport: func(_ context.Context, _ string, rt string) (*auth.ClaudeTokenData, error) {
		refreshed = append(refreshed, rt)
		if strings.HasSuffix(rt, "dead") {
			return nil, errors.New("invalid_grant")
		}
		return &auth.ClaudeTokenData{
			AccessToken:  "sk-ant-oat01-fresh-" + rt[len(rt)-1:],
			RefreshToken: rt + "-rotated",
			Email:        "user-" + rt[len(rt)-1:] + "@example.com",
			AccountUUID:  "acct-" + rt[len(rt)-1:],
			PlanType:     "max-5x",
			ExpiresAt:    time.Now().Add(time.Hour),
		}, nil
	}}
	body := `{"text":"sk-ant-oat01-setupA\nsk-ant-ort01-rtA sk-ant-ort01-rtB, sk-ant-ort01-dead"}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import-tokens", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ImportClaudeSetupTokens(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Total    int `json:"total"`
		Imported int `json:"imported"`
		Failed   int `json:"failed"`
		Items    []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Email string `json:"email"`
		} `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 4 || result.Imported != 3 || result.Failed != 1 {
		t.Fatalf("result = %+v", result)
	}
	if len(refreshed) != 3 {
		t.Fatalf("refresh grant calls = %v", refreshed)
	}
	if !strings.Contains(result.Items[3].Error, "invalid_grant") {
		t.Fatalf("dead refresh token item = %+v", result.Items[3])
	}
	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*database.AccountRow{}
	for _, row := range rows {
		byName[row.Name] = row
	}
	setup := byName["claude-1"]
	if setup == nil || setup.GetCredential(auth.ClaudeAuthKindCredentialKey) != auth.ClaudeAuthKindSetupToken || setup.GetCredential("access_token") != "sk-ant-oat01-setupA" {
		t.Fatalf("setup token row = %+v", setup)
	}
	oauth := byName["user-A@example.com"]
	if oauth == nil {
		t.Fatalf("refresh token account should be named by email, names=%v", func() []string {
			out := []string{}
			for n := range byName {
				out = append(out, n)
			}
			return out
		}())
	}
	if oauth.GetCredential(auth.ClaudeAuthKindCredentialKey) != auth.ClaudeAuthKindOAuth ||
		oauth.GetCredential("access_token") != "sk-ant-oat01-fresh-A" ||
		oauth.GetCredential("refresh_token") != "sk-ant-ort01-rtA-rotated" ||
		oauth.GetCredential("plan_type") != "max-5x" || oauth.GetCredential("account_id") != "acct-A" {
		t.Fatalf("oauth row credentials = %+v", oauth.Credentials)
	}

	// 同一 RT 再导:刷新后 account_id 相同,按身份 409。
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import-tokens", strings.NewReader(`{"tokens":["sk-ant-ort01-rtA"]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	h.ImportClaudeSetupTokens(c)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("duplicate refresh token status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestImportClaudeTokenRefreshTokenOnlyDocumentRefreshesBeforeInsert(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db, refreshClaudeTokensForImport: func(_ context.Context, _ string, rt string) (*auth.ClaudeTokenData, error) {
		return &auth.ClaudeTokenData{AccessToken: "at-from-" + rt, RefreshToken: rt, Email: "rt@example.com", AccountUUID: "acct-rt", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(`{"type":"claude","refresh_token":"sk-ant-ort01-doc","models":["claude-sonnet-4-5"]}`))
	h.ImportClaudeToken(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].GetCredential("access_token") != "at-from-sk-ant-ort01-doc" || rows[0].GetCredential("email") != "rt@example.com" || rows[0].Name != "rt@example.com" {
		t.Fatalf("row = name=%q creds=%+v", rows[0].Name, rows[0].Credentials)
	}
}
