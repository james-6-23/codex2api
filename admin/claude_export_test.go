package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestClaudeAccountRowToExportEntryIncludesPortableMetadataAndAllowlistedFingerprint(t *testing.T) {
	row := &database.AccountRow{
		ID: 42, Name: "Claude operator", Platform: "anthropic", Enabled: false,
		Tags: []string{"prod", "claude"},
		Credentials: map[string]interface{}{
			"upstream_type":                         auth.UpstreamClaude,
			"email":                                 "claude@example.com",
			"account_id":                            "account-42",
			"access_token":                          "access-secret",
			"refresh_token":                         "refresh-secret",
			"expires_at":                            "2026-09-01T00:00:00Z",
			"plan_type":                             "max-5x",
			"models":                                []string{"claude-sonnet-4-5"},
			"timezone":                              "Asia/Shanghai",
			auth.ClaudeFingerprintModeCredentialKey: "force",
			"custom_headers": map[string]string{
				"User-Agent":          "claude-cli/test",
				"X-Stainless-OS":      "MacOS",
				"Authorization":       "must-not-export",
				"X-Api-Key":           "must-not-export",
				"X-Internal-Operator": "must-not-export",
			},
		},
	}

	entry, ok := claudeAccountRowToExportEntry(row, []claudeGroupRef{{Name: "Claude", Channel: "claude"}})
	if !ok {
		t.Fatal("Claude OAuth row should be exportable")
	}
	if entry.Type != "claude" || entry.Version != claudeCredentialExportVersion || entry.AuthKind != "oauth" {
		t.Fatalf("export identity = %+v", entry)
	}
	if entry.Email != "claude@example.com" || entry.AccountID != "account-42" || entry.Name != "Claude operator" {
		t.Fatalf("export metadata = %+v", entry)
	}
	if entry.Enabled {
		t.Fatal("disabled state must be preserved in an export")
	}
	if entry.ClaudeFingerprintMode != "force" || entry.Timezone != "Asia/Shanghai" {
		t.Fatalf("fingerprint metadata = %+v", entry)
	}
	if len(entry.FingerprintHeaders) != 2 || entry.FingerprintHeaders["Authorization"] != "" || entry.FingerprintHeaders["X-Api-Key"] != "" {
		t.Fatalf("secret/non-identity headers leaked: %+v", entry.FingerprintHeaders)
	}
	if len(entry.Tags) != 2 || len(entry.GroupRefs) != 1 || entry.GroupRefs[0].Name != "Claude" {
		t.Fatalf("portable metadata = %+v", entry)
	}
	encoded, err := marshalClaudeExportEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"must-not-export", "Authorization", "X-Api-Key", "X-Internal-Operator"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("export contains forbidden value %q: %s", forbidden, encoded)
		}
	}
}

func TestBuildAccountResponseClaudeRedactsNonIdentityHeaders(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	row := &database.AccountRow{
		ID: 7, Name: "claude", Platform: "anthropic", Status: "active", Enabled: true,
		Credentials: map[string]interface{}{
			"upstream_type": auth.UpstreamClaude,
			"access_token":  "access-secret",
			"refresh_token": "refresh-secret",
			"custom_headers": map[string]string{
				"User-Agent":    "claude-cli/test",
				"Authorization": "must-not-leak",
				"Cookie":        "must-not-leak",
				"X-Operator":    "must-not-leak",
			},
		},
	}
	response := (&Handler{store: store}).buildAccountResponse(row, nil, nil, nil, nil, true)
	if response.ClaudeUserAgent != "claude-cli/test" {
		t.Fatalf("identity User-Agent missing from safe detail field: %q", response.ClaudeUserAgent)
	}
	if response.CustomHeaders["User-Agent"] != "claude-cli/test" {
		t.Fatalf("identity header missing from detail response: %+v", response.CustomHeaders)
	}
	for _, forbidden := range []string{"Authorization", "Cookie", "X-Operator"} {
		if _, ok := response.CustomHeaders[forbidden]; ok {
			t.Fatalf("Claude detail response leaked %s: %+v", forbidden, response.CustomHeaders)
		}
	}
}

func TestClaudeImportParserAcceptsArrayAndRejectsNonOAuth(t *testing.T) {
	raw := `[{"type":"claude","version":1,"auth_kind":"oauth","name":"one","access_token":"at-1","refresh_token":"rt-1","account_id":"acct-1","models":["claude-sonnet-4-5"],"timezone":"Asia/Shanghai","tags":["prod"],"group_refs":[{"name":"Claude","channel":"claude"}],"enabled":false},{"upstream_type":"claude","access_token":"at-2","refresh_token":"rt-2"}]`
	docs, err := parseClaudeImportDocuments([]byte(raw))
	if err != nil {
		t.Fatalf("parse array: %v", err)
	}
	if len(docs) != 2 || docs[0].Name != "one" || docs[0].Enabled == nil || *docs[0].Enabled {
		t.Fatalf("parsed documents = %+v", docs)
	}
	if docs[0].Models[0] != "claude-sonnet-4-5" || docs[0].Timezone != "Asia/Shanghai" {
		t.Fatalf("parsed metadata = %+v", docs[0])
	}
	if _, err := parseClaudeImportDocuments([]byte(`{"type":"claude","auth_kind":"api_key","access_token":"at","refresh_token":"rt"}`)); err == nil {
		t.Fatal("API-key auth_kind must be rejected")
	}
}

func TestClaudeImportParserRoundTripsExportAndRejectsSecretHeaders(t *testing.T) {
	entry := claudeExportEntry{
		Type: "claude", Version: claudeCredentialExportVersion, AuthKind: "oauth",
		Name: "round-trip", Email: "round@example.com", AccountID: "acct-round",
		AccessToken: "at-round", RefreshToken: "rt-round", ExpiresAt: "2026-09-01T00:00:00Z",
		Models: []string{"claude-haiku-4-5"}, Timezone: "UTC", ClaudeFingerprintMode: "preserve",
		FingerprintHeaders: map[string]string{"User-Agent": "claude-cli/test"},
		Tags:               []string{"one"}, GroupRefs: []claudeGroupRef{{Name: "Claude", Channel: "claude"}}, Enabled: true,
	}
	raw, err := marshalClaudeExportEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := parseClaudeImportDocuments(raw)
	if err != nil {
		t.Fatalf("round-trip parse: %v", err)
	}
	if len(docs) != 1 || docs[0].AccountID != entry.AccountID || docs[0].RefreshToken != entry.RefreshToken {
		t.Fatalf("round-trip document = %+v", docs)
	}
	if _, err := parseClaudeImportDocuments([]byte(`{"type":"claude","auth_kind":"oauth","access_token":"at","refresh_token":"rt","fingerprint_headers":{"Authorization":"blocked"}}`)); err == nil {
		t.Fatal("secret identity headers must be rejected rather than silently imported")
	}
}

func TestResolveClaudeGroupRefsMapsByNameAndChannel(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	ctx := context.Background()
	claudeID, err := db.CreateAccountGroup(ctx, "Claude", "", "", 0, 0, database.OptionalNullInt64{}.Value)
	if err != nil {
		t.Fatal(err)
	}
	channel := database.AccountGroupChannelClaude
	if err := db.UpdateAccountGroup(ctx, claudeID, nil, nil, nil, &database.UpdateAccountGroupOpts{Channel: &channel}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateAccountGroup(ctx, "Codex", "", "", 0, 0, database.OptionalNullInt64{}.Value); err != nil {
		t.Fatal(err)
	}
	ids, missing, err := h.resolveClaudeGroupRefs(ctx, []claudeGroupRef{
		{Name: "claude", Channel: "claude"},
		{Name: "Codex", Channel: "codex"},
		{Name: "missing", Channel: "claude"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != claudeID || len(missing) != 2 {
		t.Fatalf("resolved ids=%v missing=%v", ids, missing)
	}
}

func TestBuildClaudeExportZIPUsesSafeNames(t *testing.T) {
	entries := []claudeExportEntry{
		{Email: "../../a@example.com", AccessToken: "at-a", RefreshToken: "rt-a", Type: "claude", Version: 1, AuthKind: "oauth"},
		{Email: "a@example.com", AccessToken: "at-b", RefreshToken: "rt-b", Type: "claude", Version: 1, AuthKind: "oauth"},
	}
	archive, err := buildClaudeExportZIP(entries)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) != 2 {
		t.Fatalf("zip files=%d err=%v", len(reader.File), err)
	}
	for _, member := range reader.File {
		if strings.Contains(member.Name, "/") || strings.Contains(member.Name, "\\") {
			t.Fatalf("unsafe member %q", member.Name)
		}
		body, err := member.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(body)
		_ = body.Close()
		var decoded claudeExportEntry
		if err := json.Unmarshal(data, &decoded); err != nil || decoded.Type != "claude" {
			t.Fatalf("member %q decode err=%v body=%s", member.Name, err, data)
		}
	}
}

func TestExportClaudeAccountsSingleSetsSecretDownloadHeaders(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "claude", "anthropic", auth.UpstreamClaude, map[string]interface{}{
		"upstream_type": auth.UpstreamClaude, "email": "single@example.com", "account_id": "single-acct",
		"access_token": "single-at", "refresh_token": "single-rt", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db, store: auth.NewStore(db, nil, nil)}
	t.Cleanup(h.store.Stop)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/claude/export?ids="+strconv.FormatInt(id, 10), nil)
	h.ExportClaudeAccounts(c)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store, max-age=0" || recorder.Header().Get("Pragma") != "no-cache" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("secret headers = %#v", recorder.Header())
	}
	if recorder.Header().Get("X-Export-Count") != "1" || !strings.Contains(recorder.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download headers = %#v", recorder.Header())
	}
}

func TestExportClaudeAccountsSupportsHealthyFilterAndRejectsWrongSelection(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	first, err := db.InsertAccountWithUpstream(ctx, "healthy", "anthropic", auth.UpstreamClaude, map[string]interface{}{
		"upstream_type": auth.UpstreamClaude, "account_id": "healthy-acct", "access_token": "healthy-at", "refresh_token": "healthy-rt",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.InsertAccountWithUpstream(ctx, "error", "anthropic", auth.UpstreamClaude, map[string]interface{}{
		"upstream_type": auth.UpstreamClaude, "account_id": "error-acct", "access_token": "error-at", "refresh_token": "error-rt",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	wrongChannel, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", auth.UpstreamGrok, map[string]interface{}{
		"upstream_type": auth.UpstreamGrok, "access_token": "grok-at", "refresh_token": "grok-rt",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: first, UpstreamType: auth.UpstreamClaude, AccessToken: "healthy-at", RefreshToken: "healthy-rt", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: second, UpstreamType: auth.UpstreamClaude, AccessToken: "error-at", RefreshToken: "error-rt", Status: auth.StatusError})
	h := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/claude/export?filter=healthy", nil)
	h.ExportClaudeAccounts(c)
	if recorder.Code != http.StatusOK || recorder.Header().Get("X-Export-Count") != "1" {
		t.Fatalf("healthy export status=%d count=%q body=%s", recorder.Code, recorder.Header().Get("X-Export-Count"), recorder.Body.String())
	}
	var healthy claudeExportEntry
	if err := json.Unmarshal(recorder.Body.Bytes(), &healthy); err != nil || healthy.AccountID != "healthy-acct" {
		t.Fatalf("healthy export = %+v err=%v", healthy, err)
	}

	wrong := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(wrong)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/claude/export?ids="+strconv.FormatInt(wrongChannel, 10), nil)
	h.ExportClaudeAccounts(c)
	if wrong.Code != http.StatusNotFound || strings.Contains(wrong.Body.String(), "grok-rt") {
		t.Fatalf("wrong-channel export status=%d body=%s", wrong.Code, wrong.Body.String())
	}

	invalid := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(invalid)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/claude/export?ids=bad", nil)
	h.ExportClaudeAccounts(c)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid ids status=%d body=%s", invalid.Code, invalid.Body.String())
	}
}

func TestExportClaudeAccountsFormatSelection(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	for _, suffix := range []string{"one", "two"} {
		_, err := db.InsertAccountWithUpstream(ctx, suffix, "anthropic", auth.UpstreamClaude, map[string]interface{}{
			"upstream_type": auth.UpstreamClaude, "account_id": "format-" + suffix,
			"access_token": "at-format-" + suffix, "refresh_token": "rt-format-" + suffix,
		}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
	h := &Handler{db: db, store: auth.NewStore(db, nil, nil)}
	t.Cleanup(h.store.Stop)

	jsonRecorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(jsonRecorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/claude/export?format=json", nil)
	h.ExportClaudeAccounts(c)
	if jsonRecorder.Code != http.StatusOK || jsonRecorder.Header().Get("Content-Type") != "application/json; charset=utf-8" || jsonRecorder.Header().Get("X-Export-Count") != "2" {
		t.Fatalf("json export status=%d headers=%#v body=%s", jsonRecorder.Code, jsonRecorder.Header(), jsonRecorder.Body.String())
	}
	var documents []claudeExportEntry
	if err := json.Unmarshal(jsonRecorder.Body.Bytes(), &documents); err != nil || len(documents) != 2 {
		t.Fatalf("json export documents=%d err=%v", len(documents), err)
	}

	invalidRecorder := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(invalidRecorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/claude/export?format=csv", nil)
	h.ExportClaudeAccounts(c)
	if invalidRecorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid format status=%d body=%s", invalidRecorder.Code, invalidRecorder.Body.String())
	}

	zipRecorder := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(zipRecorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/claude/export?ids=1&format=zip", nil)
	h.ExportClaudeAccounts(c)
	if zipRecorder.Code != http.StatusOK || zipRecorder.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("forced zip status=%d content-type=%q", zipRecorder.Code, zipRecorder.Header().Get("Content-Type"))
	}
}

func TestPrepareClaudeTimezoneCredentialUpdateRegeneratesOnlyClaudeIdentityHeaders(t *testing.T) {
	row := &database.AccountRow{Credentials: map[string]interface{}{
		"upstream_type": auth.UpstreamClaude,
		"timezone":      "Asia/Shanghai",
		"custom_headers": map[string]string{
			"User-Agent":     "claude-cli/old",
			"X-Stainless-OS": "Linux",
			"X-Request-Id":   "keep-me",
			"X-Operator-Tag": "must-be-removed",
		},
	}}
	updates := map[string]interface{}{}
	if err := prepareClaudeTimezoneCredentialUpdate(row, "America/New_York", updates); err != nil {
		t.Fatalf("prepare timezone update: %v", err)
	}
	raw, ok := updates["custom_headers"].(map[string]string)
	if !ok {
		t.Fatalf("custom_headers update type = %T, want map[string]string", updates["custom_headers"])
	}
	if raw["X-Request-Id"] != "keep-me" {
		t.Fatalf("non-identity custom header was not preserved: %+v", raw)
	}
	if _, exists := raw["X-Operator-Tag"]; exists {
		t.Fatalf("unapproved non-identity header was preserved: %+v", raw)
	}
	if raw["User-Agent"] == "claude-cli/old" || strings.TrimSpace(raw["User-Agent"]) == "" {
		t.Fatalf("identity fingerprint was not regenerated: %+v", raw)
	}
	for _, secretHeader := range []string{"Authorization", "X-Api-Key", "Cookie"} {
		if _, exists := raw[secretHeader]; exists {
			t.Fatalf("secret header unexpectedly present: %q", secretHeader)
		}
	}
}

func TestPrepareClaudeTimezoneCredentialUpdateDoesNotRotateUnchangedFingerprint(t *testing.T) {
	row := &database.AccountRow{Platform: "anthropic", Credentials: map[string]interface{}{
		"upstream_type": auth.UpstreamClaude,
		"timezone":      "Asia/Shanghai",
		"custom_headers": map[string]string{
			"User-Agent":                  "claude-cli/stable",
			"X-App":                       "cli",
			"X-Stainless-Lang":            "js",
			"X-Stainless-Package-Version": "0.60.0",
			"X-Stainless-OS":              "Linux",
			"X-Stainless-Arch":            "x64",
			"X-Stainless-Runtime":         "node",
			"X-Stainless-Runtime-Version": "v20.18.1",
		},
	}}
	updates := map[string]interface{}{}
	if err := prepareClaudeTimezoneCredentialUpdate(row, "Asia/Shanghai", updates); err != nil {
		t.Fatalf("prepare unchanged timezone: %v", err)
	}
	headers, ok := updates["custom_headers"].(map[string]string)
	if !ok || headers["User-Agent"] != "claude-cli/stable" {
		t.Fatalf("unchanged timezone rotated fingerprint: %+v", updates["custom_headers"])
	}
}

func TestUpdateAccountSchedulerTimezoneUsesSafeExplicitHeadersAndSyncsRuntime(t *testing.T) {
	db := newTestAdminDB(t)
	id, err := db.InsertAccountWithUpstream(context.Background(), "timezone", "anthropic", auth.UpstreamClaude, map[string]interface{}{
		"upstream_type": auth.UpstreamClaude,
		"access_token":  "at-timezone", "refresh_token": "rt-timezone", "account_id": "acct-timezone",
		"timezone":       "Asia/Shanghai",
		"custom_headers": map[string]string{"User-Agent": "claude-cli/old", "X-Request-Id": "old"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	runtimeAccount := &auth.Account{DBID: id, UpstreamType: auth.UpstreamClaude, AccessToken: "at-timezone", RefreshToken: "rt-timezone", CustomHeaders: map[string]string{"User-Agent": "claude-cli/old", "X-Request-Id": "old"}}
	store.AddAccount(runtimeAccount)
	h := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+strconv.FormatInt(id, 10)+"/scheduler", strings.NewReader(`{"timezone":"America/New_York","custom_headers":{"User-Agent":"client-supplied","X-Request-Id":"new-safe","Authorization":"must-drop"}}`))
	h.UpdateAccountScheduler(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	persisted := row.GetCredentialStringMap("custom_headers")
	if row.GetCredential("timezone") != "America/New_York" {
		t.Fatalf("persisted timezone=%q", row.GetCredential("timezone"))
	}
	if persisted["X-Request-Id"] != "new-safe" || persisted["Authorization"] != "" {
		t.Fatalf("persisted safe/secret headers = %+v", persisted)
	}
	if persisted["User-Agent"] == "client-supplied" || persisted["User-Agent"] == "claude-cli/old" {
		t.Fatalf("timezone did not rebuild identity headers: %+v", persisted)
	}
	runtime := runtimeAccount.GetCustomHeaders()
	if runtime["X-Request-Id"] != "new-safe" || runtime["Authorization"] != "" || runtime["User-Agent"] != persisted["User-Agent"] {
		t.Fatalf("runtime headers=%+v persisted=%+v", runtime, persisted)
	}
}

func TestImportClaudeTokenArrayPreservesMetadataAndDeduplicates(t *testing.T) {
	db := newTestAdminDB(t)
	groupID, err := db.CreateAccountGroup(context.Background(), "Claude production", "", "", 0, 0, database.OptionalNullInt64{}.Value)
	if err != nil {
		t.Fatal(err)
	}
	channel := database.AccountGroupChannelClaude
	if err := db.UpdateAccountGroup(context.Background(), groupID, nil, nil, nil, &database.UpdateAccountGroupOpts{Channel: &channel}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	body := `[{"type":"claude","version":1,"auth_kind":"oauth","name":"one","email":"one@example.com","account_id":"acct-one","access_token":"at-one","refresh_token":"rt-one","models":["claude-haiku-4-5"],"timezone":"Asia/Shanghai","claude_fingerprint_mode":"force","tags":["prod"],"group_refs":[{"name":"Claude production","channel":"claude"}],"enabled":false},{"type":"claude","version":1,"auth_kind":"oauth","name":"two","account_id":"acct-two","access_token":"at-two","refresh_token":"rt-two","models":["claude-sonnet-4-5"]}]`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(body))
	h.ImportClaudeToken(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Total    int `json:"total"`
		Imported int `json:"imported"`
		Failed   int `json:"failed"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.Imported != 2 || result.Failed != 0 {
		t.Fatalf("import result = %+v", result)
	}
	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil || len(rows) != 2 {
		t.Fatalf("Claude rows=%d err=%v", len(rows), err)
	}
	var disabledRow *database.AccountRow
	for _, row := range rows {
		if row.GetCredential("account_id") == "acct-one" {
			disabledRow = row
		}
	}
	if disabledRow == nil || disabledRow.Enabled {
		t.Fatalf("disabled metadata not preserved: %+v", disabledRow)
	}
	if len(disabledRow.Tags) != 1 || disabledRow.Tags[0] != "prod" {
		t.Fatalf("tags not preserved: %v", disabledRow.Tags)
	}
	if disabledRow.GetCredential(auth.ClaudeFingerprintModeCredentialKey) != auth.ClaudeFingerprintModeForce {
		t.Fatalf("fingerprint mode not preserved: %q", disabledRow.GetCredential(auth.ClaudeFingerprintModeCredentialKey))
	}
	groups, err := db.GetAccountGroupIDs(context.Background(), disabledRow.ID)
	if err != nil || len(groups) != 1 || groups[0] != groupID {
		t.Fatalf("group mapping = %v err=%v", groups, err)
	}

	duplicate := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(duplicate)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(`{"type":"claude","access_token":"at-one-new","refresh_token":"rt-one-new","account_id":"acct-one","models":["claude-haiku-4-5"]}`))
	h.ImportClaudeToken(c)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}

	// The provider account identifier can change independently of a refresh
	// token.  The token must still prevent a second active account from being
	// created under a different account_id.
	sameRefreshToken := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(sameRefreshToken)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(`{"type":"claude","access_token":"at-two-new","refresh_token":"rt-two","account_id":"acct-two-rotated","models":["claude-sonnet-4-5"]}`))
	h.ImportClaudeToken(c)
	if sameRefreshToken.Code != http.StatusConflict {
		t.Fatalf("same refresh token status=%d body=%s", sameRefreshToken.Code, sameRefreshToken.Body.String())
	}
}

func TestClaudeImportPartialFingerprintHeadersAreCompleted(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	body := `{"type":"claude","version":1,"auth_kind":"oauth","account_id":"partial-fp","access_token":"at-partial","refresh_token":"rt-partial","models":["claude-haiku-4-5"],"fingerprint_headers":{"User-Agent":"claude-cli/custom"}}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(body))
	h.ImportClaudeToken(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	headers := rows[0].GetCredentialStringMap("custom_headers")
	if headers["User-Agent"] != "claude-cli/custom" {
		t.Fatalf("provided User-Agent was not preserved: %+v", headers)
	}
	for _, name := range auth.ClaudeIdentityHeaderNames {
		found := ""
		for key, value := range headers {
			if strings.EqualFold(key, name) {
				found = value
				break
			}
		}
		if strings.TrimSpace(found) == "" {
			t.Fatalf("partial fingerprint was not completed: missing %s in %+v", name, headers)
		}
	}
}

func TestClaudeImportPreservesFingerprintModeInRuntime(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	h := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(`{"type":"claude","version":1,"auth_kind":"oauth","account_id":"runtime-fp","access_token":"at-runtime-fp","refresh_token":"rt-runtime-fp","models":["claude-haiku-4-5"],"claude_fingerprint_mode":"force"}`))
	h.ImportClaudeToken(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if got := rows[0].GetCredential(auth.ClaudeFingerprintModeCredentialKey); got != auth.ClaudeFingerprintModeForce {
		t.Fatalf("persisted fingerprint mode=%q", got)
	}
	account := store.FindByID(rows[0].ID)
	if account == nil || account.ClaudeFingerprintMode != auth.ClaudeFingerprintModeForce {
		t.Fatalf("runtime account fingerprint mode=%v", account)
	}
}

func TestClaudeBatchImportCanSkipPerAccountModelFetch(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	created, err := h.createClaudeAccount(context.Background(), "batch-no-probe", "", "UTC", &auth.ClaudeTokenData{
		AccessToken: "at-batch-no-probe", RefreshToken: "rt-batch-no-probe", AccountUUID: "acct-batch-no-probe", ExpiresAt: time.Now().Add(time.Hour),
	}, "test", &claudeAccountImportOptions{SkipModelFetch: true})
	if err != nil {
		t.Fatalf("create without model probe: %v", err)
	}
	row, err := db.GetAccountByID(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if models := row.GetCredentialStringSlice("models"); len(models) != 0 {
		t.Fatalf("skip-model-fetch unexpectedly persisted upstream models: %v", models)
	}
}

func TestClaudeImportWarmupSkipsExplicitlyDisabledAccounts(t *testing.T) {
	disabled := false
	if shouldScheduleClaudeImportWarmup(&claudeAccountImportOptions{Enabled: &disabled}) {
		t.Fatal("explicitly disabled Claude imports must not schedule a warmup probe")
	}
	if !shouldScheduleClaudeImportWarmup(&claudeAccountImportOptions{}) {
		t.Fatal("legacy/unspecified Claude imports should retain warmup behavior")
	}
}

func TestNormalizeClaudeImportTagsRejectsControlCharacters(t *testing.T) {
	for _, value := range []string{"line\nbreak", "null\x00byte", "unit\x1fsep"} {
		if _, err := normalizeClaudeImportTags([]string{value}); err == nil {
			t.Fatalf("tags value %q with control character was accepted", value)
		}
	}
}

func TestClaudeImportMetadataRejectsControlCharactersAndOversizedValues(t *testing.T) {
	base := `{"type":"claude","access_token":"at-meta","refresh_token":"rt-meta","models":["claude-haiku-4-5"]}`
	for field, value := range map[string]string{
		"email":      "bad\nemail@example.com",
		"account_id": "acct\x00bad",
		"plan_type":  "plan\x1fbad",
	} {
		raw := strings.TrimSuffix(base, "}") + ",\"" + field + "\":\"" + value + "\"}"
		if _, err := parseClaudeImportDocuments([]byte(raw)); err == nil {
			t.Fatalf("metadata field %s accepted control character", field)
		}
	}
	oversized := strings.TrimSuffix(base, "}") + ",\"plan_type\":\"" + strings.Repeat("x", 81) + "\"}"
	if _, err := parseClaudeImportDocuments([]byte(oversized)); err == nil {
		t.Fatal("oversized plan_type was accepted")
	}
}

func TestClaudeCreateMetadataFailureReturnsCommittedWarning(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	codexGroupID, groupErr := db.CreateAccountGroup(context.Background(), "codex-only", "", "", 0, 0, database.OptionalNullInt64{}.Value)
	if groupErr != nil {
		t.Fatal(groupErr)
	}
	created, err := h.createClaudeAccount(context.Background(), "warning", "", "UTC", &auth.ClaudeTokenData{
		AccessToken: "at-warning", RefreshToken: "rt-warning", AccountUUID: "acct-warning", ExpiresAt: time.Now().Add(time.Hour),
	}, "test", &claudeAccountImportOptions{
		Models:           []string{"claude-haiku-4-5"},
		ResolvedGroupIDs: []int64{codexGroupID}, // force post-insert channel binding failure
	})
	if err != nil {
		t.Fatalf("committed metadata warning should not be returned as fatal: %v", err)
	}
	if created.ID <= 0 || len(created.Warnings) == 0 {
		t.Fatalf("create result = %+v, want committed id and warning", created)
	}
	if _, err := db.GetAccountByID(context.Background(), created.ID); err != nil {
		t.Fatalf("account should remain recoverable after metadata warning: %v", err)
	}
}

func TestImportClaudeTokenSinglePreservesCreateErrorStatus(t *testing.T) {
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", strings.NewReader(`{"type":"claude","access_token":"at-bad-model","refresh_token":"rt-bad-model","models":["gpt-5"]}`))
	h.ImportClaudeToken(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s, want 400 from provider validation", recorder.Code, recorder.Body.String())
	}
}
