package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestParseAntigravityImportContentSupportsManagerExports(t *testing.T) {
	content := `{
		"version":"1.0",
		"accounts":[{
			"email":"user@example.com",
			"name":"User",
			"avatar_url":"https://example.com/a.png",
			"token":{"access_token":"access","refresh_token":"refresh","project_id":"project","oauth_client_key":"enterprise","expiry_timestamp":1786863600}
		}]
	}`
	credentials, err := parseAntigravityImportContent(content, antigravityImportDefaults{})
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(credentials) != 1 {
		t.Fatalf("credentials len = %d", len(credentials))
	}
	got := credentials[0]
	if got.Email != "user@example.com" || got.RefreshToken != "refresh" || got.ProjectID != "project" || got.OAuthClientKey != "enterprise" {
		t.Fatalf("credential = %+v", got)
	}
	if got.ExpiresAt.IsZero() {
		t.Fatal("expiry timestamp was not parsed")
	}
}

func TestParseAntigravityImportContentSupportsCredentialStoreAndRawToken(t *testing.T) {
	credentials, err := parseAntigravityImportContent(`{"token":{"refresh_token":"nested","expiry":"2026-08-16T09:00:00Z"},"auth_method":"consumer"}`, antigravityImportDefaults{OAuthClientKey: "default"})
	if err != nil || len(credentials) != 1 || credentials[0].RefreshToken != "nested" || credentials[0].OAuthClientKey != "default" {
		t.Fatalf("nested credentials = %+v, err=%v", credentials, err)
	}
	raw, err := parseAntigravityImportContent("raw-refresh-token", antigravityImportDefaults{ClientID: "id", ClientSecret: "secret"})
	if err != nil || len(raw) != 1 || raw[0].RefreshToken != "raw-refresh-token" || raw[0].ClientID != "id" {
		t.Fatalf("raw credentials = %+v, err=%v", raw, err)
	}
	expiryDate, err := parseAntigravityImportContent(`{"refresh_token":"dated","expiry_date":1786863600000}`, antigravityImportDefaults{})
	if err != nil || len(expiryDate) != 1 || expiryDate[0].ExpiresAt.Unix() != 1786863600 {
		t.Fatalf("expiry_date credentials = %+v, err=%v", expiryDate, err)
	}
}

func TestParseAntigravityImportDocumentsSupportsAPIKeyMetadata(t *testing.T) {
	const secret = "api-key-must-not-leak"
	documents, err := parseAntigravityImportDocuments(`{
		"name":"Imported key",
		"api_key":"`+secret+`",
		"models":["gemini-2.5-flash","gemini-2.5-flash",""],
		"model_mapping":{"fast":"gemini-2.5-flash"},
		"disabled":"false",
		"proxyUrl":"http://127.0.0.1:18080"
	}`, antigravityImportDefaults{OAuthClientKey: "oauth-default", ClientID: "oauth-client", ClientSecret: "oauth-secret"})
	if err != nil || len(documents) != 1 {
		t.Fatalf("documents=%+v err=%v", documents, err)
	}
	document := documents[0]
	if document.AuthKind != auth.AntigravityAuthKindAPIKey || document.APIKey != secret || document.Name != "Imported key" ||
		document.ProxyURL != "http://127.0.0.1:18080" || !document.DisabledPresent || document.Disabled {
		t.Fatalf("API-key document = %+v", document)
	}
	if len(document.Models) != 1 || document.Models[0] != "gemini-2.5-flash" || document.ModelMapping != `{"fast":"gemini-2.5-flash"}` {
		t.Fatalf("API-key catalog metadata = models=%v mapping=%q", document.Models, document.ModelMapping)
	}
	if document.Credential.OAuthClientKey != "" || document.Credential.ClientID != "" || document.Credential.ClientSecret != "" {
		t.Fatalf("OAuth defaults applied to API-key document: %+v", document.Credential)
	}
	if _, err := parseAntigravityImportContent(`{"auth_kind":"api_key","api_key":"`+secret+`"}`, antigravityImportDefaults{}); err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("legacy parser error = %v", err)
	}
}

func TestParseAntigravityImportDocumentsRejectsAmbiguousSecretsWithoutLeakingThem(t *testing.T) {
	const apiSecret = "api-secret-never-echo"
	const oauthSecret = "oauth-secret-never-echo"
	_, err := parseAntigravityImportDocuments(`{"auth_kind":"api_key","api_key":"`+apiSecret+`","refresh_token":"`+oauthSecret+`"}`, antigravityImportDefaults{})
	if err == nil {
		t.Fatal("mixed API-key/OAuth credential was accepted")
	}
	if strings.Contains(err.Error(), apiSecret) || strings.Contains(err.Error(), oauthSecret) {
		t.Fatalf("parse error leaked a secret: %v", err)
	}
}

func TestParseAntigravityImportInputsPreservesOriginalIndices(t *testing.T) {
	parsed, failures, err := parseAntigravityImportInputs([]string{
		"{invalid",
		`{"accounts":[{"refresh_token":"refresh-a"},{"refresh_token":"refresh-b"}]}`,
		"refresh-c",
	}, antigravityImportDefaults{})
	if err != nil {
		t.Fatalf("parse inputs: %v", err)
	}
	if len(failures) != 1 || failures[0].Index != 1 {
		t.Fatalf("parse failures = %+v, want original index 1", failures)
	}
	if len(parsed) != 3 {
		t.Fatalf("parsed credentials = %d, want 3", len(parsed))
	}
	want := []struct {
		source int
		sub    int
		token  string
	}{
		{source: 2, sub: 1, token: "refresh-a"},
		{source: 2, sub: 2, token: "refresh-b"},
		{source: 3, sub: 1, token: "refresh-c"},
	}
	for index, expected := range want {
		got := parsed[index]
		if got.SourceIndex != expected.source || got.SubIndex != expected.sub || got.Credential.RefreshToken != expected.token {
			t.Errorf("parsed[%d] = %+v, want source=%d sub=%d token=%q", index, got, expected.source, expected.sub, expected.token)
		}
	}
	encoded, err := json.Marshal(antigravityImportItem{Index: 2, SubIndex: 3, OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"sub_index":3`)) {
		t.Fatalf("import result omitted sub_index: %s", encoded)
	}
}

func TestAntigravityImportedVerifiedEmailClaimIsNonAuthoritative(t *testing.T) {
	parsed, err := parseAntigravityImportContent(`{"refresh_token":"refresh","verified_email":false}`, antigravityImportDefaults{})
	if err != nil || len(parsed) != 1 || !parsed[0].VerifiedEmailPresent || parsed[0].VerifiedEmail {
		t.Fatalf("parsed verified_email claim = %+v, err=%v", parsed, err)
	}
	result := auth.AntigravitySyncResult{
		Credential:           parsed[0],
		Profile:              auth.AntigravityProfile{ID: "subject", Email: "user@example.com", VerifiedEmail: true},
		Entitlements:         auth.AntigravityEntitlements{Allowed: true, ProjectID: "project", UpdatedAt: time.Now().UTC()},
		Quota:                auth.AntigravityQuotaSnapshot{UpdatedAt: time.Now().UTC()},
		EntitlementsObserved: true,
	}
	updates, err := antigravityCredentialUpdates(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if verified, ok := updates["verified_email"].(bool); !ok || !verified {
		t.Fatalf("live verified_email = %#v, want true", updates["verified_email"])
	}
	if imported, ok := updates[antigravityImportedVerifiedEmailKey].(bool); !ok || imported {
		t.Fatalf("imported verified_email claim = %#v, want false metadata", updates[antigravityImportedVerifiedEmailKey])
	}
}

func TestAntigravityAccountResponseProjectsSanitizedSnapshots(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	entitlements := auth.AntigravityEntitlements{Allowed: true, ProjectID: "project", EffectiveTier: "Free (Restricted)", Restricted: true, UpdatedAt: now}
	quota := auth.AntigravityQuotaSnapshot{Forbidden: false, UpdatedAt: now, Models: []auth.AntigravityModelQuota{{ModelID: "gemini", RemainingPercent: 80}}}
	entitlementsJSON, _ := json.Marshal(entitlements)
	quotaJSON, _ := json.Marshal(quota)
	row := &database.AccountRow{
		ID: 7, Name: "User", Status: "active", Enabled: true, CreatedAt: now, UpdatedAt: now,
		Credentials: map[string]interface{}{
			"upstream_type": auth.UpstreamAntigravity, "email": "user@example.com",
			"project_id": "project", "plan_type": "Free (Restricted)",
			"antigravity_permissions": string(entitlementsJSON), "antigravity_entitlements": string(entitlementsJSON), "antigravity_quota": string(quotaJSON),
			"refresh_token": "must-not-leak", "antigravity_client_secret": "must-not-leak",
		},
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	defer store.Stop()
	response := (&Handler{store: store}).buildAccountResponse(row, nil, nil, nil, nil, false)
	if !response.AntigravityAPI || response.Status != "active" || response.Email != "user@example.com" || len(response.AntigravityQuota) == 0 || len(response.AntigravityPermissions) == 0 {
		t.Fatalf("response = %+v", response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || strings.Contains(string(encoded), "must-not-leak") ||
		strings.Contains(string(encoded), "refresh_token") || strings.Contains(string(encoded), "client_secret") {
		t.Fatalf("response leaked credential fields: %s", encoded)
	}
}

func TestAntigravityCredentialUpdatesPersistIdentityAndPermissions(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	result := auth.AntigravitySyncResult{
		Credential: auth.AntigravityCredential{
			AccessToken: "access", RefreshToken: "refresh", ClientID: "client-id", ClientSecret: "client-secret",
		},
		Profile: auth.AntigravityProfile{
			ID: "google-subject", Email: "user@example.com", VerifiedEmail: true, Picture: "https://example.com/avatar.png",
		},
		Entitlements: auth.AntigravityEntitlements{Allowed: true, ProjectID: "project", EffectiveTier: "Pro", UpdatedAt: now},
		Quota: auth.AntigravityQuotaSnapshot{
			Models: []auth.AntigravityModelQuota{{ModelID: "gemini-2.5-pro", RemainingPercent: 75}}, UpdatedAt: now,
		},
		EntitlementsObserved: true,
	}
	updates, err := antigravityCredentialUpdates(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if verified, _ := updates["verified_email"].(bool); !verified {
		t.Fatalf("verified_email = %#v", updates["verified_email"])
	}
	permissions, _ := updates["antigravity_permissions"].(string)
	entitlements, _ := updates["antigravity_entitlements"].(string)
	if permissions == "" || permissions != entitlements || !json.Valid([]byte(permissions)) {
		t.Fatalf("permissions=%q entitlements=%q", permissions, entitlements)
	}
	if updates["antigravity_client_id"] != "client-id" || updates["antigravity_client_secret"] != "client-secret" {
		t.Fatalf("OAuth client persistence = %#v / %#v", updates["antigravity_client_id"], updates["antigravity_client_secret"])
	}
	if value, ok := updates["expires_at"]; !ok || value != "" {
		t.Fatalf("expires_at = %#v, want explicit clear for replacement credential without expiry", value)
	}
}

func TestAntigravityCredentialUpdatesPreserveUnobservedSnapshots(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previousPermissions := auth.AntigravityEntitlements{Allowed: true, EffectiveTier: "Pro", UpdatedAt: now}
	previousQuota := auth.AntigravityQuotaSnapshot{
		Models:    []auth.AntigravityModelQuota{{ModelID: "old-model"}},
		Groups:    []auth.AntigravityQuotaGroup{{DisplayName: "Weekly"}},
		AICredits: &auth.AntigravityAICredits{Credits: 42}, UpdatedAt: now,
	}
	permissionsJSON, _ := json.Marshal(previousPermissions)
	quotaJSON, _ := json.Marshal(previousQuota)
	previous := &database.AccountRow{Credentials: map[string]interface{}{
		"account_id": "google-subject", "plan_type": "Pro",
		"antigravity_permissions": string(permissionsJSON), "antigravity_quota": string(quotaJSON),
	}}
	result := auth.AntigravitySyncResult{
		Credential: auth.AntigravityCredential{AccessToken: "new-access", ProjectID: "project"},
		Profile:    auth.AntigravityProfile{ID: "google-subject", Email: "user@example.com", VerifiedEmail: true},
		Quota: auth.AntigravityQuotaSnapshot{
			Models: []auth.AntigravityModelQuota{{ModelID: "new-model"}}, UpdatedAt: now.Add(time.Minute),
		},
		Warning: "loadCodeAssist temporarily unavailable",
	}
	updates, err := antigravityCredentialUpdates(result, previous)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updates["plan_type"]; ok {
		t.Fatal("unobserved entitlements overwrote plan_type")
	}
	if _, ok := updates["antigravity_permissions"]; ok {
		t.Fatal("unobserved entitlements overwrote the last permissions snapshot")
	}
	var got auth.AntigravityQuotaSnapshot
	if err := json.Unmarshal([]byte(updates["antigravity_quota"].(string)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 1 || got.Models[0].ModelID != "new-model" || len(got.Groups) != 1 || got.AICredits == nil || got.AICredits.Credits != 42 {
		t.Fatalf("merged quota = %+v", got)
	}
	encoded := updates["antigravity_quota"].(string)
	if !strings.Contains(encoded, `"quota_groups"`) || strings.Contains(encoded, `"groups"`) {
		t.Fatalf("quota field compatibility = %s", encoded)
	}
}

func TestAntigravityCredentialUpdatesClearSnapshotsForDifferentIdentity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := &database.AccountRow{Credentials: map[string]interface{}{
		"email": "old@example.com", "plan_type": "Pro",
		"antigravity_permissions":  `{"allowed":true}`,
		"antigravity_entitlements": `{"allowed":true}`,
	}}
	result := auth.AntigravitySyncResult{
		Credential: auth.AntigravityCredential{AccessToken: "new-access"},
		Profile:    auth.AntigravityProfile{Email: "new@example.com", VerifiedEmail: true},
		Quota:      auth.AntigravityQuotaSnapshot{Models: []auth.AntigravityModelQuota{}, UpdatedAt: now},
		Warning:    "loadCodeAssist temporarily unavailable",
	}
	snapshotSource := antigravitySnapshotSource(previous, result)
	if snapshotSource != nil {
		t.Fatal("different email identity reused the previous snapshot")
	}
	updates, err := antigravityCredentialUpdates(result, snapshotSource)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"plan_type", "antigravity_permissions", "antigravity_entitlements"} {
		if value, ok := updates[key]; !ok || value != "" {
			t.Fatalf("%s = %#v, want explicit empty value", key, value)
		}
	}
}

func TestAntigravityCredentialUpdatesClearSnapshotsWhenPermissionsAreUnobserved(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	previous := &database.AccountRow{Credentials: map[string]interface{}{
		"account_id": "subject-a", "email": "a@example.com", "verified_email": true,
		"project_id": "project-a", "plan_type": "Tier A",
		"antigravity_permissions":  `{"allowed":true,"effective_tier":"Tier A"}`,
		"antigravity_entitlements": `{"allowed":true,"effective_tier":"Tier A"}`,
	}}
	result := auth.AntigravitySyncResult{
		Credential: auth.AntigravityCredential{AccessToken: "access-b", RefreshToken: "refresh-b", IDToken: "id-token-a", ProjectID: "project-a"},
		Profile:    auth.AntigravityProfile{ID: "subject-b", Email: "b@example.com", VerifiedEmail: true},
		Quota: auth.AntigravityQuotaSnapshot{
			Models:    []auth.AntigravityModelQuota{{ModelID: "gemini-b", RemainingPercent: 65}},
			UpdatedAt: now,
		},
		Warning: "loadCodeAssist temporarily unavailable",
	}
	updates, err := antigravityCredentialUpdates(result, previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"project_id", "plan_type", "antigravity_permissions", "antigravity_entitlements"} {
		if value, ok := updates[key]; !ok || value != "" {
			t.Fatalf("%s = %#v, want explicit empty value", key, value)
		}
	}
	if updates["id_token"] != "" {
		t.Fatalf("id_token = %#v, want old principal token cleared", updates["id_token"])
	}
	models, ok := updates["models"].([]string)
	if !ok || len(models) != 0 || updates["antigravity_quota"] != "" || updates["antigravity_last_synced_at"] != "" {
		t.Fatalf("replacement quota state = models=%#v quota=%#v synced=%#v", models, updates["antigravity_quota"], updates["antigravity_last_synced_at"])
	}
}

func TestResolveAntigravityGroupIDsEnforcesChannel(t *testing.T) {
	handler, db, _, codexGroupID := newImportGroupsTestHandler(t)
	ctx := context.Background()
	antigravityGroupID, err := db.CreateAccountGroup(ctx, "antigravity", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	channel := database.AccountGroupChannelAntigravity
	if err := db.UpdateAccountGroup(ctx, antigravityGroupID, nil, nil, nil, &database.UpdateAccountGroupOpts{Channel: &channel}); err != nil {
		t.Fatal(err)
	}
	if _, err := handler.resolveAntigravityGroupIDs(ctx, json.RawMessage(`[`+itoa(codexGroupID)+`]`)); err == nil {
		t.Fatal("Codex group must be rejected for Antigravity accounts")
	}
	ids, err := handler.resolveAntigravityGroupIDs(ctx, json.RawMessage(`[`+itoa(antigravityGroupID)+`]`))
	if err != nil || len(ids) != 1 || ids[0] != antigravityGroupID {
		t.Fatalf("Antigravity group resolution = %v, %v", ids, err)
	}
}

func TestAntigravityPagedSnapshotUsesPersistedControlPlaneStatus(t *testing.T) {
	row := &database.AccountRow{
		ID: 9, Name: "broken", Status: "active", Enabled: true,
		Credentials: map[string]interface{}{
			"upstream_type": auth.UpstreamAntigravity,
			"email":         "broken@example.com", "antigravity_sync_error": "quota sync failed",
		},
	}
	item := (&Handler{}).buildAccountListSnapshotItem(row, nil, nil, map[int64]string{}, map[int64]string{})
	if item.Status != "error" {
		t.Fatalf("snapshot status = %q", item.Status)
	}
	if !accountListItemMatches(item, accountPageQuery{Status: "error"}, database.UpstreamChannelAntigravity) {
		t.Fatal("error filter excluded an Antigravity row returned as error")
	}
}

func TestAntigravityHealthySnapshotIsActiveAndNotUnsampled(t *testing.T) {
	row := &database.AccountRow{
		ID: 10, Name: "healthy", Status: "active", Enabled: true,
		Credentials: map[string]interface{}{
			"upstream_type": auth.UpstreamAntigravity,
			"email":         "healthy@example.com",
			"project_id":    "project-healthy",
			"plan_type":     "Persisted Pro",
		},
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	defer store.Stop()
	store.AddAccount(&auth.Account{DBID: row.ID, UpstreamType: auth.UpstreamAntigravity, AccessToken: "access", PlanType: "Stale Runtime Tier"})
	item := (&Handler{store: store}).buildAccountListSnapshotItem(row, nil, nil, map[int64]string{}, map[int64]string{})
	if item.PlanType != "Persisted Pro" {
		t.Fatalf("plan type = %q, want persisted control-plane value", item.PlanType)
	}
	if accountListUnsampled(item) {
		t.Fatal("Antigravity control-plane account was treated as an unsampled Codex account")
	}
	if !accountListItemMatches(item, accountPageQuery{Status: "active"}, database.UpstreamChannelAntigravity) {
		t.Fatal("active filter excluded a healthy Antigravity account")
	}
	summary, _ := summarizeAccountList([]*accountListSnapshotItem{item}, database.UpstreamChannelAntigravity)
	if summary.Active != 1 || summary.Normal != 1 || summary.Unsampled != 0 {
		t.Fatalf("summary = %+v, want active=1 normal=1 unsampled=0", summary)
	}
}

func TestUpdateAntigravityAccountClearsGroupsAndReturnsMessage(t *testing.T) {
	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	groupID, err := db.CreateAccountGroup(ctx, "antigravity-edit", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	channel := database.AccountGroupChannelAntigravity
	if err := db.UpdateAccountGroup(ctx, groupID, nil, nil, nil, &database.UpdateAccountGroupOpts{Channel: &channel}); err != nil {
		t.Fatal(err)
	}
	accountID, err := db.InsertAccountWithUpstream(ctx, "before", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "refresh_token": "refresh", "email": "user@example.com",
	}, "http://127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountGroups(ctx, accountID, []int64{groupID}); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: itoa(accountID)}}
	ginContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(accountID)+"/antigravity", bytes.NewBufferString(`{"name":"after","proxy_url":"","group_ids":[]}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAntigravityAccount(ginContext)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"message"`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil || row.Name != "after" || row.ProxyURL != "" {
		t.Fatalf("updated row = %+v, err=%v", row, err)
	}
	groupIDs, err := db.GetAccountGroupIDs(ctx, accountID)
	if err != nil || len(groupIDs) != 0 {
		t.Fatalf("groups after clear = %v, err=%v", groupIDs, err)
	}
}

func TestUpdateAntigravityMetadataFailureReloadsPersistedPartialMutation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "antigravity-partial-mutation.sqlite")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	accountID, err := db.InsertAccountWithUpstream(ctx, "before", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "partial-access", "refresh_token": "partial-refresh",
	}, "http://old-proxy.invalid:8080")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	if _, err := rawDB.ExecContext(ctx, `
		CREATE TRIGGER reject_antigravity_name_update
		BEFORE UPDATE OF name ON accounts
		BEGIN
			SELECT RAISE(ABORT, 'forced name update failure');
		END`); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: itoa(accountID)}}
	ginContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(accountID)+"/antigravity", strings.NewReader(`{"proxy_url":"http://new-proxy.invalid:8080","name":"after"}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	(&Handler{db: db, store: store}).UpdateAntigravityAccount(ginContext)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("partial update response = %d %s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := store.FindByID(accountID)
	if row.ProxyURL != "http://new-proxy.invalid:8080" || row.Name != "before" || runtime == nil || runtime.ProxyURL != row.ProxyURL {
		t.Fatalf("partial durable mutation was not reloaded: row name=%q proxy=%q runtime=%+v", row.Name, row.ProxyURL, runtime)
	}
}

func TestAddAndUpdateAntigravityAPIKeyAccount(t *testing.T) {
	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()

	create := httptest.NewRecorder()
	createContext, _ := gin.CreateTestContext(create)
	createContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity", bytes.NewBufferString(`{
		"name":"google key",
		"auth_kind":"api_key",
		"api_key":"secret-google-key",
		"models":["gemini-2.5-flash","gemini-2.5-flash"],
		"model_mapping":"{\"fast\":\"gemini-2.5-flash\"}"
	}`))
	createContext.Request.Header.Set("Content-Type", "application/json")
	handler.AddAntigravityAccount(createContext)
	if create.Code != http.StatusOK {
		t.Fatalf("create response = %d %s", create.Code, create.Body.String())
	}
	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &payload); err != nil || payload.ID <= 0 {
		t.Fatalf("create payload = %+v, err=%v", payload, err)
	}
	row, err := db.GetAccountByID(ctx, payload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredential("api_key") != "secret-google-key" || row.GetCredential("plan_type") != "api" || row.GetCredential("email") != "google-api-key" {
		t.Fatalf("created credentials = %#v", row.Credentials)
	}
	if got := row.GetCredentialStringSlice("models"); len(got) != 1 || got[0] != "gemini-2.5-flash" {
		t.Fatalf("created models = %v", got)
	}

	duplicate := httptest.NewRecorder()
	duplicateContext, _ := gin.CreateTestContext(duplicate)
	duplicateContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity", bytes.NewBufferString(`{"auth_kind":"api_key","api_key":"secret-google-key"}`))
	duplicateContext.Request.Header.Set("Content-Type", "application/json")
	handler.AddAntigravityAccount(duplicateContext)
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate response = %d %s", duplicate.Code, duplicate.Body.String())
	}
	if err := db.UpdateCredentials(ctx, payload.ID, map[string]interface{}{
		"antigravity_capabilities":             `[{"protocol":"interactions","status":"ok"}]`,
		"antigravity_capability_last_probe_at": "2026-08-22T00:00:00Z",
		"antigravity_catalog_source":           "explicit_probe",
		"antigravity_catalog_verified":         true,
		"antigravity_sync_error":               "old key sync error",
		"antigravity_sync_warning":             "old key sync warning",
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetError(ctx, payload.ID, antigravityAccessDeniedError+": old key denied"); err != nil {
		t.Fatal(err)
	}
	beforeRotation, err := db.GetAccountByID(ctx, payload.ID)
	if err != nil {
		t.Fatal(err)
	}

	update := httptest.NewRecorder()
	updateContext, _ := gin.CreateTestContext(update)
	updateContext.Params = gin.Params{{Key: "id", Value: itoa(payload.ID)}}
	updateContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(payload.ID)+"/antigravity", bytes.NewBufferString(`{
		"api_key":"rotated-google-key",
		"models":["gemini-2.5-pro"],
		"model_mapping":"{}"
	}`))
	updateContext.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAntigravityAccount(updateContext)
	if update.Code != http.StatusOK {
		t.Fatalf("update response = %d %s", update.Code, update.Body.String())
	}
	updated, err := db.GetAccountByID(ctx, payload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.GetCredential("api_key") != "rotated-google-key" {
		t.Fatalf("updated API key = %q", updated.GetCredential("api_key"))
	}
	wantFamily := antigravityAPIKeyFamilyID("rotated-google-key")
	if updated.CredentialGeneration != beforeRotation.CredentialGeneration+1 ||
		updated.CredentialFamilyID != wantFamily || updated.GetCredential("credential_family_id") != wantFamily {
		t.Fatalf("rotated identity = generation %d family %q credentials %#v", updated.CredentialGeneration, updated.CredentialFamilyID, updated.Credentials)
	}
	if updated.GetCredential("antigravity_capabilities") != "" ||
		updated.GetCredential("antigravity_capability_last_probe_at") != "" ||
		updated.GetCredential("antigravity_catalog_source") != "" ||
		updated.GetCredentialBool("antigravity_catalog_verified") {
		t.Fatalf("old key capability proof survived rotation: %#v", updated.Credentials)
	}
	if updated.GetCredential("antigravity_sync_error") != "" || updated.GetCredential("antigravity_sync_warning") != "" {
		t.Fatalf("old key sync state survived rotation: %#v", updated.Credentials)
	}
	if updated.Status != "active" || updated.ErrorMessage != "" {
		t.Fatalf("new key retained old access fence: status=%q error=%q", updated.Status, updated.ErrorMessage)
	}
	if got := updated.GetCredentialStringSlice("models"); len(got) != 1 || got[0] != "gemini-2.5-pro" {
		t.Fatalf("updated models = %v", got)
	}
	responseStore := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	defer responseStore.Stop()
	response := (&Handler{store: responseStore}).buildAccountResponse(updated, nil, nil, nil, nil, false)
	if response.AntigravityAuthKind != auth.AntigravityAuthKindAPIKey {
		t.Fatalf("response auth kind = %q", response.AntigravityAuthKind)
	}
	encoded, _ := json.Marshal(response)
	if strings.Contains(string(encoded), "rotated-google-key") {
		t.Fatalf("response leaked API key: %s", encoded)
	}
}

func TestUpdateAntigravityAPIKeyAccountPreservesUnrelatedAdminError(t *testing.T) {
	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	oldKey := "old-unrelated-error-key"
	accountID, err := db.InsertAccountWithUpstream(ctx, "google key", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "api_key": oldKey,
		"credential_family_id":   antigravityAPIKeyFamilyID(oldKey),
		"antigravity_sync_error": "old sync error", "antigravity_sync_warning": "old sync warning",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	const adminError = "manually quarantined by operator"
	if err := db.SetError(ctx, accountID, adminError); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	newKey := "new-unrelated-error-key"
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	handler.updateAntigravityAPIKeyAccount(ginContext, ctx, row, updateAntigravityAccountRequest{APIKey: &newKey}, database.OptionalInt64Slice{})
	if recorder.Code != http.StatusOK {
		t.Fatalf("update response = %d %s", recorder.Code, recorder.Body.String())
	}
	updated, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.GetCredential("api_key") != newKey || updated.CredentialFamilyID != antigravityAPIKeyFamilyID(newKey) {
		t.Fatalf("rotated credential = generation %d family %q credentials %#v", updated.CredentialGeneration, updated.CredentialFamilyID, updated.Credentials)
	}
	if updated.Status != "error" || updated.ErrorMessage != adminError {
		t.Fatalf("unrelated admin error was cleared: status=%q error=%q", updated.Status, updated.ErrorMessage)
	}
	if updated.GetCredential("antigravity_sync_error") != "" || updated.GetCredential("antigravity_sync_warning") != "" {
		t.Fatalf("old key sync state survived rotation: %#v", updated.Credentials)
	}
}

func TestUpdateAntigravityAPIKeyAccountRejectsStaleGeneration(t *testing.T) {
	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	oldKey := "old-google-key"
	accountID, err := db.InsertAccountWithUpstream(ctx, "google key", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type":        auth.UpstreamAntigravity,
		"api_key":              oldKey,
		"credential_family_id": antigravityAPIKeyFamilyID(oldKey),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	staleRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	concurrentKey := "concurrent-google-key"
	concurrentFamily := antigravityAPIKeyFamilyID(concurrentKey)
	if _, applied, err := db.ReplaceAccountCredentialsCAS(ctx, accountID, staleRow.CredentialGeneration, concurrentFamily, map[string]any{
		"upstream_type": auth.UpstreamAntigravity,
		"api_key":       concurrentKey,
	}); err != nil || !applied {
		t.Fatalf("concurrent replacement = applied %t, err %v", applied, err)
	}

	requestedKey := "stale-request-google-key"
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	handler.updateAntigravityAPIKeyAccount(ginContext, ctx, staleRow, updateAntigravityAccountRequest{APIKey: &requestedKey}, database.OptionalInt64Slice{})
	if recorder.Code != http.StatusConflict {
		t.Fatalf("stale update response = %d %s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != staleRow.CredentialGeneration+1 || row.CredentialFamilyID != concurrentFamily ||
		row.GetCredential("api_key") != concurrentKey {
		t.Fatalf("stale update overwrote concurrent identity: generation=%d family=%q credentials=%#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
}

func TestReloadAntigravityRuntimeAccountFailsClosed(t *testing.T) {
	ctx := context.Background()
	db := newTestAdminDB(t)
	accountID, err := db.InsertAccountWithUpstream(ctx, "antigravity", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity,
		"access_token":  "access-old",
		"refresh_token": "refresh-old",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	if store.FindByID(accountID) == nil {
		t.Fatal("runtime account was not loaded")
	}
	if err := db.SoftDeleteAccount(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, store: store}
	if err := handler.reloadAntigravityRuntimeAccount(ctx, accountID); err == nil {
		t.Fatal("reload unexpectedly succeeded for a deleted account")
	}
	if store.FindByID(accountID) != nil {
		t.Fatal("failed reload left the stale runtime account dispatchable")
	}
}

func TestBatchImportAntigravityAccountsRoundTripsMixedPortableDocuments(t *testing.T) {
	var userInfoHits atomic.Int32
	var tokenHits atomic.Int32
	authServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			tokenHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"unexpected refresh"}`)
		case "/userinfo":
			userInfoHits.Add(1)
			_, _ = io.WriteString(w, `{"id":"portable-oauth-subject","email":"portable@example.com","verified_email":true,"name":"Live Google Name"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"portable-project","paidTier":{"id":"pro","name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"live-quota-model":{"quotaInfo":{"remainingFraction":0.75}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer authServer.Close()

	forwardTransport := &http.Transport{Proxy: nil}
	defer forwardTransport.CloseIdleConnections()
	var proxyHits atomic.Int32
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		outbound := r.Clone(r.Context())
		outbound.RequestURI = ""
		outbound.Header = r.Header.Clone()
		outbound.Header.Del("Proxy-Connection")
		response, err := forwardTransport.RoundTrip(outbound)
		if err != nil {
			http.Error(w, "proxy forwarding failed", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for name, values := range response.Header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer proxyServer.Close()

	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		TokenURL: authServer.URL + "/token", UserInfoURL: authServer.URL + "/userinfo",
		LoadProject: []string{authServer.URL + "/load"}, Quota: []string{authServer.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	const enabledAPIKey = "portable-enabled-api-secret"
	const disabledAPIKey = "portable-disabled-api-secret"
	const invalidAPIKey = "portable-invalid-api-secret"
	documents := []antigravityExportEntry{
		{
			Type: "antigravity", Version: 1, AuthKind: auth.AntigravityAuthKindOAuth,
			Name: "Exported OAuth Name", AccessToken: "portable-oauth-access-secret", RefreshToken: "portable-oauth-refresh-secret",
			ExpiresAt: "2099-01-01T00:00:00Z", Models: []string{"portable-oauth-model"},
			ModelMapping: `{"portable-oauth-alias":"portable-oauth-model"}`, Disabled: true,
		},
		{
			Type: "antigravity", Version: 1, AuthKind: auth.AntigravityAuthKindAPIKey,
			Name: "Enabled portable API key", APIKey: enabledAPIKey,
			Models: []string{"portable-api-model"}, ModelMapping: `{"portable-api-alias":"portable-api-model"}`,
			ProxyURL: "http://api-proxy.invalid:18080", Disabled: false,
		},
		{
			Type: "antigravity", Version: 1, AuthKind: auth.AntigravityAuthKindAPIKey,
			Name: "Disabled portable API key", APIKey: disabledAPIKey,
			Models: []string{"portable-disabled-model"}, Disabled: true,
		},
		{
			Type: "antigravity", Version: 1, AuthKind: auth.AntigravityAuthKindAPIKey,
			Name: "Duplicate portable API key", APIKey: enabledAPIKey,
		},
		{
			Type: "antigravity", Version: 1, AuthKind: auth.AntigravityAuthKindAPIKey,
			Name: "Invalid proxy portable API key", APIKey: invalidAPIKey,
			ProxyURL: strings.Repeat("x", 501),
		},
	}
	portableJSON, err := json.Marshal(documents)
	if err != nil {
		t.Fatal(err)
	}
	requestJSON, err := json.Marshal(antigravityImportRequest{Files: []string{string(portableJSON)}, ProxyURL: proxyServer.URL})
	if err != nil {
		t.Fatal(err)
	}

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/import", bytes.NewReader(requestJSON))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchImportAntigravityAccounts(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("batch import response = %d %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Total    int                     `json:"total"`
		Imported int                     `json:"imported"`
		Synced   int                     `json:"synced"`
		Failed   int                     `json:"failed"`
		Items    []antigravityImportItem `json:"items"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Total != 5 || response.Imported != 3 || response.Synced != 3 || response.Failed != 2 || len(response.Items) != 5 {
		t.Fatalf("batch import summary = %+v body=%s", response, recorder.Body.String())
	}
	for index, item := range response.Items {
		if item.Index != 1 || item.SubIndex != index+1 {
			t.Fatalf("items lost stable source positions: %+v", response.Items)
		}
	}
	if !response.Items[0].OK || !response.Items[1].OK || !response.Items[2].OK || response.Items[3].OK || response.Items[4].OK ||
		response.Items[3].Error == "" || response.Items[4].Error == "" {
		t.Fatalf("mixed item results = %+v", response.Items)
	}
	for _, secret := range []string{enabledAPIKey, disabledAPIKey, invalidAPIKey, "portable-oauth-access-secret", "portable-oauth-refresh-secret"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("batch response leaked secret %q: %s", secret, recorder.Body.String())
		}
	}
	if userInfoHits.Load() != 1 || tokenHits.Load() != 0 {
		t.Fatalf("OAuth requests = userinfo %d token %d, want exactly one OAuth document and no refresh", userInfoHits.Load(), tokenHits.Load())
	}
	if proxyHits.Load() < 3 {
		t.Fatalf("request-level fallback proxy hits = %d, want OAuth sync through fallback proxy", proxyHits.Load())
	}

	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("persisted Antigravity rows = %d, want 3", len(rows))
	}
	byName := make(map[string]*database.AccountRow, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	oauthRow := byName["Exported OAuth Name"]
	enabledRow := byName["Enabled portable API key"]
	disabledRow := byName["Disabled portable API key"]
	if oauthRow == nil || enabledRow == nil || disabledRow == nil {
		t.Fatalf("portable names were not restored: %#v", byName)
	}
	if oauthRow.Enabled || oauthRow.ProxyURL != proxyServer.URL || oauthRow.GetCredential("model_mapping") != `{"portable-oauth-alias":"portable-oauth-model"}` {
		t.Fatalf("OAuth document metadata = enabled %t proxy %q credentials %#v", oauthRow.Enabled, oauthRow.ProxyURL, oauthRow.Credentials)
	}
	if got := oauthRow.GetCredentialStringSlice("models"); len(got) != 1 || got[0] != "portable-oauth-model" {
		t.Fatalf("OAuth document models = %v", got)
	}
	if !enabledRow.Enabled || enabledRow.ProxyURL != "http://api-proxy.invalid:18080" || enabledRow.GetCredential("api_key") != enabledAPIKey ||
		enabledRow.GetCredential("model_mapping") != `{"portable-api-alias":"portable-api-model"}` {
		t.Fatalf("enabled API-key metadata = enabled %t proxy %q credentials %#v", enabledRow.Enabled, enabledRow.ProxyURL, enabledRow.Credentials)
	}
	if disabledRow.Enabled || disabledRow.ProxyURL != proxyServer.URL || disabledRow.GetCredential("api_key") != disabledAPIKey {
		t.Fatalf("disabled API-key metadata = enabled %t proxy %q credentials %#v", disabledRow.Enabled, disabledRow.ProxyURL, disabledRow.Credentials)
	}
	if store.FindByID(oauthRow.ID) != nil || store.FindByID(disabledRow.ID) != nil {
		t.Fatalf("disabled imports remained in runtime: oauth=%+v api=%+v", store.FindByID(oauthRow.ID), store.FindByID(disabledRow.ID))
	}
	if store.FindByID(enabledRow.ID) == nil {
		t.Fatal("enabled API-key import was not loaded into runtime")
	}
}

func TestAntigravityManualRefreshUsesPersistedGroupProxy(t *testing.T) {
	var groupProxyHits atomic.Int32
	groupProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		groupProxyHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"group-proxy-subject","email":"group-proxy@example.com","verified_email":true,"name":"Group Proxy"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"group-proxy-project","paidTier":{"name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"group-proxy-model":{"quotaInfo":{"remainingFraction":0.9}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer groupProxy.Close()
	var globalProxyHits atomic.Int32
	globalProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		globalProxyHits.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer globalProxy.Close()

	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		UserInfoURL: "http://antigravity-upstream.invalid/userinfo",
		LoadProject: []string{"http://antigravity-upstream.invalid/load"},
		Quota:       []string{"http://antigravity-upstream.invalid/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	ctx := context.Background()
	db := newTestAdminDB(t)
	groupID, err := db.CreateAccountGroup(ctx, "proxy-group", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	accountID, err := db.InsertAccountWithUpstream(ctx, "group proxy", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "group-proxy-access", "refresh_token": "group-proxy-refresh",
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountGroups(ctx, accountID, []int64{groupID}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	store.SetProxyURL(globalProxy.URL)
	store.SetGroupProxyURLs(groupID, []string{groupProxy.URL})
	handler := &Handler{db: db, store: store}

	item := handler.refreshAntigravityAccount(ctx, accountID)
	if !item.OK {
		t.Fatalf("refresh item = %+v", item)
	}
	if groupProxyHits.Load() < 3 || globalProxyHits.Load() != 0 {
		t.Fatalf("proxy hits = group %d global %d, want persisted group route", groupProxyHits.Load(), globalProxyHits.Load())
	}
}

func TestAntigravityControlPlaneFailsClosedOnlyForUpstreamCalls(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	store.SetProxyPoolEnabled(true)
	store.SetProxyURL("")
	handler := &Handler{db: db, store: store}

	apiAdd := httptest.NewRecorder()
	apiContext, _ := gin.CreateTestContext(apiAdd)
	apiContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity", strings.NewReader(`{"auth_kind":"api_key","api_key":"offline-api-key","name":"offline api"}`))
	apiContext.Request.Header.Set("Content-Type", "application/json")
	handler.AddAntigravityAccount(apiContext)
	if apiAdd.Code != http.StatusOK {
		t.Fatalf("offline API-key add should not require egress: %d %s", apiAdd.Code, apiAdd.Body.String())
	}

	oauthID, err := db.InsertAccountWithUpstream(ctx, "offline oauth", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "offline-access", "refresh_token": "offline-refresh",
		"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	metadata := httptest.NewRecorder()
	metadataContext, _ := gin.CreateTestContext(metadata)
	metadataContext.Params = gin.Params{{Key: "id", Value: itoa(oauthID)}}
	metadataContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(oauthID)+"/antigravity", strings.NewReader(`{"name":"offline metadata edit"}`))
	metadataContext.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAntigravityAccount(metadataContext)
	if metadata.Code != http.StatusOK {
		t.Fatalf("metadata-only edit should not require egress: %d %s", metadata.Code, metadata.Body.String())
	}

	replacement := httptest.NewRecorder()
	replacementContext, _ := gin.CreateTestContext(replacement)
	replacementContext.Params = gin.Params{{Key: "id", Value: itoa(oauthID)}}
	replacementContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(oauthID)+"/antigravity", strings.NewReader(`{"auth_json":"{\"access_token\":\"replacement-access\",\"refresh_token\":\"replacement-refresh\"}"}`))
	replacementContext.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAntigravityAccount(replacementContext)
	if replacement.Code != http.StatusServiceUnavailable || !strings.Contains(replacement.Body.String(), antigravityNoUsableEgressError) {
		t.Fatalf("credential replacement did not fail closed: %d %s", replacement.Code, replacement.Body.String())
	}

	refresh := handler.refreshAntigravityAccount(ctx, oauthID)
	if refresh.OK || !strings.Contains(refresh.Error, antigravityNoUsableEgressError) {
		t.Fatalf("manual refresh did not fail closed: %+v", refresh)
	}

	mixed, err := json.Marshal([]antigravityExportEntry{
		{Type: "antigravity", Version: 1, AuthKind: auth.AntigravityAuthKindAPIKey, APIKey: "offline-batch-api-key"},
		{Type: "antigravity", Version: 1, AuthKind: auth.AntigravityAuthKindOAuth, AccessToken: "offline-batch-access", RefreshToken: "offline-batch-refresh"},
	})
	if err != nil {
		t.Fatal(err)
	}
	batchBody, _ := json.Marshal(antigravityImportRequest{Files: []string{string(mixed)}})
	batch := httptest.NewRecorder()
	batchContext, _ := gin.CreateTestContext(batch)
	batchContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/import", bytes.NewReader(batchBody))
	batchContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchImportAntigravityAccounts(batchContext)
	if batch.Code != http.StatusOK || !strings.Contains(batch.Body.String(), `"imported":1`) || !strings.Contains(batch.Body.String(), antigravityNoUsableEgressError) {
		t.Fatalf("mixed offline batch result = %d %s", batch.Code, batch.Body.String())
	}
}

func TestRefreshAntigravityCASConflictReloadsDurableWinner(t *testing.T) {
	ctx := context.Background()
	db := newTestAdminDB(t)
	accountID, err := db.InsertAccountWithUpstream(ctx, "old oauth", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "old-access", "refresh_token": "old-refresh",
		"expires_at":           time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"credential_family_id": antigravityCredentialFamilyID(auth.AntigravityCredential{RefreshToken: "old-refresh"}, ""),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	initialRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	winnerFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "durable-winner-subject")
	var mutated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			if mutated.CompareAndSwap(false, true) {
				_, applied, mutationErr := db.ReplaceAccountCredentialsCAS(context.Background(), accountID, initialRow.CredentialGeneration, winnerFamily, map[string]any{
					"upstream_type": auth.UpstreamAntigravity, "access_token": "durable-winner-access", "refresh_token": "durable-winner-refresh",
					"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					"account_id": "durable-winner-subject", "email": "winner@example.com", "verified_email": true,
					"credential_family_id": winnerFamily,
				})
				if mutationErr != nil || !applied {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":"winner mutation failed"}`)
					return
				}
			}
			_, _ = io.WriteString(w, `{"id":"losing-refresh-subject","email":"loser@example.com","verified_email":true,"name":"Loser"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"losing-project","paidTier":{"name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"losing-model":{"quotaInfo":{"remainingFraction":0.8}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo", LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	item := (&Handler{db: db, store: store}).refreshAntigravityAccount(ctx, accountID)
	if item.OK || !strings.Contains(item.Error, "已被其他操作更新") {
		t.Fatalf("CAS-losing refresh item = %+v", item)
	}
	winnerRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := store.FindByID(accountID)
	if winnerRow.CredentialFamilyID != winnerFamily || winnerRow.GetCredential("access_token") != "durable-winner-access" ||
		runtime == nil || runtime.AccessToken != "durable-winner-access" || runtime.CredentialGeneration != winnerRow.CredentialGeneration {
		t.Fatalf("durable winner was not reloaded: row generation=%d family=%q credentials=%#v runtime=%+v", winnerRow.CredentialGeneration, winnerRow.CredentialFamilyID, winnerRow.Credentials, runtime)
	}
}

func TestUpdateAntigravityCredentialCASConflictReloadsDurableWinner(t *testing.T) {
	ctx := context.Background()
	db := newTestAdminDB(t)
	accountID, err := db.InsertAccountWithUpstream(ctx, "old oauth", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "old-update-access", "refresh_token": "old-update-refresh",
		"expires_at":           time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"credential_family_id": antigravityCredentialFamilyID(auth.AntigravityCredential{RefreshToken: "old-update-refresh"}, ""),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	initialRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	winnerFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "durable-update-winner")
	var mutated atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			if mutated.CompareAndSwap(false, true) {
				_, applied, mutationErr := db.ReplaceAccountCredentialsCAS(context.Background(), accountID, initialRow.CredentialGeneration, winnerFamily, map[string]any{
					"upstream_type": auth.UpstreamAntigravity, "access_token": "durable-update-winner-access", "refresh_token": "durable-update-winner-refresh",
					"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
					"account_id": "durable-update-winner", "email": "update-winner@example.com", "verified_email": true,
					"credential_family_id": winnerFamily,
				})
				if mutationErr != nil || !applied {
					w.WriteHeader(http.StatusInternalServerError)
					_, _ = io.WriteString(w, `{"error":"winner mutation failed"}`)
					return
				}
			}
			_, _ = io.WriteString(w, `{"id":"losing-update-subject","email":"losing-update@example.com","verified_email":true,"name":"Loser"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"losing-update-project","paidTier":{"name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"losing-update-model":{"quotaInfo":{"remainingFraction":0.8}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo", LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: itoa(accountID)}}
	ginContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(accountID)+"/antigravity", strings.NewReader(`{"auth_json":"{\"access_token\":\"losing-update-access\",\"refresh_token\":\"losing-update-refresh\",\"expires_at\":\"2099-01-01T00:00:00Z\"}"}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	(&Handler{db: db, store: store}).UpdateAntigravityAccount(ginContext)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("CAS-losing update response = %d %s", recorder.Code, recorder.Body.String())
	}
	winnerRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	runtime := store.FindByID(accountID)
	if winnerRow.CredentialFamilyID != winnerFamily || winnerRow.GetCredential("access_token") != "durable-update-winner-access" ||
		runtime == nil || runtime.AccessToken != "durable-update-winner-access" || runtime.CredentialGeneration != winnerRow.CredentialGeneration {
		t.Fatalf("durable update winner was not reloaded: row generation=%d family=%q credentials=%#v runtime=%+v", winnerRow.CredentialGeneration, winnerRow.CredentialFamilyID, winnerRow.Credentials, runtime)
	}
}

func TestAntigravityCASErrorRemovesUnconfirmedRuntime(t *testing.T) {
	for _, mode := range []string{"update", "refresh"} {
		t.Run(mode, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "antigravity-cas-error.sqlite")
			db, err := database.New("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			rawDB, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rawDB.Close() })
			accountID, err := db.InsertAccountWithUpstream(ctx, "durable old", "google", auth.UpstreamAntigravity, map[string]interface{}{
				"upstream_type": auth.UpstreamAntigravity, "access_token": "durable-old-access", "refresh_token": "durable-old-refresh",
				"expires_at":           time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"credential_family_id": antigravityCredentialFamilyID(auth.AntigravityCredential{RefreshToken: "durable-old-refresh"}, ""),
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
			t.Cleanup(store.Stop)
			if err := store.LoadAccountByID(ctx, accountID); err != nil {
				t.Fatal(err)
			}
			if _, err := rawDB.ExecContext(ctx, `
				CREATE TRIGGER reject_antigravity_credential_cas
				BEFORE UPDATE OF credentials ON accounts
				BEGIN
					SELECT RAISE(ABORT, 'forced credential CAS failure');
				END`); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/userinfo":
					_, _ = io.WriteString(w, `{"id":"cas-error-subject","email":"cas-error@example.com","verified_email":true,"name":"CAS Error"}`)
				case "/load":
					_, _ = io.WriteString(w, `{"cloudaicompanionProject":"cas-error-project","paidTier":{"name":"Pro"}}`)
				case "/quota":
					_, _ = io.WriteString(w, `{"models":{"cas-error-model":{"quotaInfo":{"remainingFraction":0.8}}}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			previousEndpoints := auth.DefaultAntigravityEndpoints
			auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
				UserInfoURL: server.URL + "/userinfo", LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
			}
			defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()
			handler := &Handler{db: db, store: store}

			if mode == "update" {
				recorder := httptest.NewRecorder()
				ginContext, _ := gin.CreateTestContext(recorder)
				ginContext.Params = gin.Params{{Key: "id", Value: itoa(accountID)}}
				ginContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(accountID)+"/antigravity", strings.NewReader(`{"auth_json":"{\"access_token\":\"cas-error-new-access\",\"refresh_token\":\"cas-error-new-refresh\",\"expires_at\":\"2099-01-01T00:00:00Z\"}"}`))
				ginContext.Request.Header.Set("Content-Type", "application/json")
				handler.UpdateAntigravityAccount(ginContext)
				if recorder.Code != http.StatusInternalServerError {
					t.Fatalf("CAS-error update response = %d %s", recorder.Code, recorder.Body.String())
				}
			} else {
				item := handler.refreshAntigravityAccount(ctx, accountID)
				if item.OK || item.Error == "" {
					t.Fatalf("CAS-error refresh item = %+v", item)
				}
			}
			row, err := db.GetAccountByID(ctx, accountID)
			if err != nil {
				t.Fatal(err)
			}
			runtime := store.FindByID(accountID)
			if row.GetCredential("access_token") != "durable-old-access" || runtime != nil {
				t.Fatalf("unconfirmed old generation remained after CAS error: row=%#v runtime=%+v", row.Credentials, runtime)
			}
		})
	}
}

func TestAntigravityFailedSyncProgressPersistErrorRemovesRuntime(t *testing.T) {
	for _, mode := range []string{"update", "refresh"} {
		t.Run(mode, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "antigravity-progress-persist-error.sqlite")
			db, err := database.New("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			rawDB, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = rawDB.Close() })
			accountID, err := db.InsertAccountWithUpstream(ctx, "progress source", "google", auth.UpstreamAntigravity, map[string]interface{}{
				"upstream_type": auth.UpstreamAntigravity, "access_token": "consumed-old-access", "refresh_token": "consumed-old-refresh",
				"expires_at":            time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
				"antigravity_client_id": "client-id", "antigravity_client_secret": "client-secret",
				"credential_family_id": antigravityCredentialFamilyID(auth.AntigravityCredential{RefreshToken: "consumed-old-refresh"}, ""),
			}, "")
			if err != nil {
				t.Fatal(err)
			}
			store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
			t.Cleanup(store.Stop)
			if err := store.LoadAccountByID(ctx, accountID); err != nil {
				t.Fatal(err)
			}
			if _, err := rawDB.ExecContext(ctx, `
				CREATE TRIGGER reject_antigravity_progress_persist
				BEFORE UPDATE OF credentials ON accounts
				BEGIN
					SELECT RAISE(ABORT, 'forced progress persistence failure');
				END`); err != nil {
				t.Fatal(err)
			}
			var tokenHits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/token":
					tokenHits.Add(1)
					_, _ = io.WriteString(w, `{"access_token":"rotated-progress-access","refresh_token":"rotated-progress-refresh","expires_in":3600}`)
				case "/userinfo":
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = io.WriteString(w, `{"error":"profile unavailable"}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			previousEndpoints := auth.DefaultAntigravityEndpoints
			auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo"}
			defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()
			handler := &Handler{db: db, store: store}

			if mode == "update" {
				recorder := httptest.NewRecorder()
				ginContext, _ := gin.CreateTestContext(recorder)
				ginContext.Params = gin.Params{{Key: "id", Value: itoa(accountID)}}
				ginContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(accountID)+"/antigravity", strings.NewReader(`{"auth_json":"{\"refresh_token\":\"replacement-source-refresh\",\"expires_at\":\"2020-01-01T00:00:00Z\"}"}`))
				ginContext.Request.Header.Set("Content-Type", "application/json")
				handler.UpdateAntigravityAccount(ginContext)
				if recorder.Code != http.StatusBadGateway {
					t.Fatalf("progress update response = %d %s", recorder.Code, recorder.Body.String())
				}
			} else {
				item := handler.refreshAntigravityAccount(ctx, accountID)
				if item.OK || item.Error == "" {
					t.Fatalf("progress refresh item = %+v", item)
				}
			}
			if tokenHits.Load() == 0 {
				t.Fatal("test did not observe provider token rotation")
			}
			row, err := db.GetAccountByID(ctx, accountID)
			if err != nil {
				t.Fatal(err)
			}
			if row.GetCredential("refresh_token") != "consumed-old-refresh" || store.FindByID(accountID) != nil {
				t.Fatalf("consumed old runtime survived failed progress persistence: row=%#v runtime=%+v", row.Credentials, store.FindByID(accountID))
			}
		})
	}
}

func TestFetchAndBatchUpdateAntigravityModels(t *testing.T) {
	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	accountID, err := db.InsertAccountWithUpstream(ctx, "google key", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity,
		"api_key":       "secret",
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	fetch := httptest.NewRecorder()
	fetchContext, _ := gin.CreateTestContext(fetch)
	fetchContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/models", bytes.NewBufferString(`{}`))
	fetchContext.Request.Header.Set("Content-Type", "application/json")
	handler.FetchAntigravityModels(fetchContext)
	if fetch.Code != http.StatusOK || !strings.Contains(fetch.Body.String(), "gemini-3.7-flash-low") || !strings.Contains(fetch.Body.String(), "gemini-3.7-flash-high") || !strings.Contains(fetch.Body.String(), "gpt-oss-120b-medium") || strings.Contains(fetch.Body.String(), `"gemini-3.7-flash"`) {
		t.Fatalf("fetch response = %d %s", fetch.Code, fetch.Body.String())
	}

	batch := httptest.NewRecorder()
	batchContext, _ := gin.CreateTestContext(batch)
	batchContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/batch-models", bytes.NewBufferString(fmt.Sprintf(`{"ids":[%d],"models":["gemini-3-pro-preview"]}`, accountID)))
	batchContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchUpdateAntigravityModels(batchContext)
	if batch.Code != http.StatusOK || !strings.Contains(batch.Body.String(), `"success":1`) {
		t.Fatalf("batch response = %d %s", batch.Code, batch.Body.String())
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if got := row.GetCredentialStringSlice("models"); len(got) != 1 || got[0] != "gemini-3-pro-preview" {
		t.Fatalf("batch models = %v", got)
	}
}

func TestRefreshAntigravityQuotaPersistsAccountInformation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"google-subject","email":"user@example.com","verified_email":true,"name":"User","picture":"https://example.com/avatar.png"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-1","paidTier":{"id":"pro","name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.75}}}}`)
		case "/summary":
			_, _ = io.WriteString(w, `{"groups":[{"displayName":"Weekly","buckets":[]}]}`)
		case "/credits":
			_, _ = io.WriteString(w, `{"paidTier":{"availableCredits":[{"creditAmount":"12"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo", LoadProject: []string{server.URL + "/load"},
		Quota: []string{server.URL + "/quota"}, QuotaSummary: []string{server.URL + "/summary"}, AICredits: []string{server.URL + "/credits"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	accountID, err := db.InsertAccountWithUpstream(ctx, "before", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access",
		"refresh_token": "refresh", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"credential_family_id": "ag_family",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: itoa(accountID)}}
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+itoa(accountID)+"/antigravity/quota", nil)
	handler.RefreshAntigravityQuota(ginContext)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"message"`) {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	wantFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "google-subject")
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != wantFamily || row.GetCredential("credential_family_id") != wantFamily || !row.GetCredentialBool("verified_email") || row.GetCredential("project_id") != "project-1" || row.GetCredential("antigravity_permissions") == "" {
		t.Fatalf("refreshed row = generation %d credentials %#v", row.CredentialGeneration, row.Credentials)
	}
	quota := row.GetCredential("antigravity_quota")
	if !strings.Contains(quota, `"quota_groups"`) || !strings.Contains(quota, `"ai_credits"`) {
		t.Fatalf("quota snapshot = %s", quota)
	}
}

func TestRefreshAntigravityAccountPersistsAndClearsAccessFence(t *testing.T) {
	var forbidden atomic.Bool
	forbidden.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"google-subject","email":"user@example.com","verified_email":true,"name":"User"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-1","paidTier":{"id":"pro","name":"Pro"}}`)
		case "/quota":
			if forbidden.Load() {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"forbidden"}`)
				return
			}
			_, _ = io.WriteString(w, `{"models":{"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.75}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"},
		Quota:       []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	ctx := context.Background()
	db := newTestAdminDB(t)
	accountID, err := db.InsertAccountWithUpstream(ctx, "antigravity", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type":        auth.UpstreamAntigravity,
		"access_token":         "access",
		"refresh_token":        "refresh",
		"expires_at":           time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"credential_family_id": antigravityCredentialFamilyID(auth.AntigravityCredential{}, "google-subject"),
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, store: store}

	denied := handler.refreshAntigravityAccount(ctx, accountID)
	if !denied.OK || !strings.Contains(denied.Warning, antigravityAccessDeniedError) {
		t.Fatalf("denied refresh item = %+v", denied)
	}
	deniedRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if deniedRow.Status != "error" || !strings.Contains(deniedRow.ErrorMessage, antigravityAccessDeniedError) ||
		!strings.Contains(deniedRow.GetCredential("antigravity_quota"), `"forbidden":true`) ||
		!strings.Contains(deniedRow.GetCredential("antigravity_permissions"), `"allowed":false`) {
		t.Fatalf("denied persisted state = status %q error %q credentials %#v", deniedRow.Status, deniedRow.ErrorMessage, deniedRow.Credentials)
	}
	deniedRuntime := store.FindByID(accountID)
	if deniedRuntime == nil || deniedRuntime.Status != auth.StatusError {
		t.Fatalf("denied runtime did not receive hard fence: %+v", deniedRuntime)
	}

	forbidden.Store(false)
	recovered := handler.refreshAntigravityAccount(ctx, accountID)
	if !recovered.OK {
		t.Fatalf("recovered refresh item = %+v", recovered)
	}
	recoveredRow, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if recoveredRow.Status != "active" || recoveredRow.ErrorMessage != "" ||
		strings.Contains(recoveredRow.GetCredential("antigravity_quota"), `"forbidden":true`) ||
		!strings.Contains(recoveredRow.GetCredential("antigravity_permissions"), `"allowed":true`) {
		t.Fatalf("recovered persisted state = status %q error %q credentials %#v", recoveredRow.Status, recoveredRow.ErrorMessage, recoveredRow.Credentials)
	}
	recoveredRuntime := store.FindByID(accountID)
	if recoveredRuntime == nil || recoveredRuntime.Status == auth.StatusError ||
		recoveredRuntime.CredentialGeneration != recoveredRow.CredentialGeneration {
		t.Fatalf("recovered runtime retained hard fence: runtime=%+v persisted_generation=%d", recoveredRuntime, recoveredRow.CredentialGeneration)
	}
}

func TestRefreshAntigravityAccountPreservesRotatedTokenOnQuotaFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-new","refresh_token":"refresh-rotated","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"google-subject","email":"user@example.com","verified_email":true,"name":"User"}`)
		case "/load":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		case "/quota":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	accountID, err := db.InsertAccountWithUpstream(ctx, "before", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-old",
		"refresh_token": "refresh-old", "expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"antigravity_client_id": "client-id", "antigravity_client_secret": "client-secret",
		"email": "user@example.com", "verified_email": true, "account_id": "google-subject", "credential_family_id": "ag_family",
		"project_id": "project-old", "plan_type": "Tier Old", "models": []string{"gemini-old"},
		"antigravity_quota":       `{"models":[{"model_id":"gemini-old","remaining_percent":80}]}`,
		"antigravity_permissions": `{"allowed":true,"effective_tier":"Tier Old"}`,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	runtimeStore := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(runtimeStore.Stop)
	handler.store = runtimeStore
	if err := runtimeStore.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	cacheGeneration := handler.accountCachesGen.Load()
	item := handler.refreshAntigravityAccount(ctx, accountID)
	if item.OK || !strings.Contains(item.Error, "HTTP 500") {
		t.Fatalf("refresh item = %+v", item)
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	wantFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "google-subject")
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != wantFamily || row.GetCredential("credential_family_id") != wantFamily || row.GetCredential("access_token") != "access-new" || row.GetCredential("refresh_token") != "refresh-rotated" || row.GetCredential("antigravity_sync_error") == "" {
		t.Fatalf("failed refresh row = generation %d credentials %#v", row.CredentialGeneration, row.Credentials)
	}
	if handler.accountCachesGen.Load() <= cacheGeneration {
		t.Fatal("failed refresh mutated persisted state without invalidating the account snapshot cache")
	}
	runtimeAccount := runtimeStore.FindByID(accountID)
	if runtimeAccount == nil || runtimeAccount.AccessToken != "access-new" || runtimeAccount.RefreshToken != "refresh-rotated" ||
		runtimeAccount.CredentialGeneration != row.CredentialGeneration || runtimeAccount.CredentialFamilyID != row.CredentialFamilyID {
		t.Fatalf("failed refresh left stale runtime credentials: runtime=%+v persisted_generation=%d persisted_family=%q", runtimeAccount, row.CredentialGeneration, row.CredentialFamilyID)
	}
	if row.GetCredential("project_id") != "project-old" || row.GetCredential("plan_type") != "Tier Old" || row.GetCredential("antigravity_quota") == "" || row.GetCredential("antigravity_permissions") == "" || len(row.GetCredentialStringSlice("models")) != 1 {
		t.Fatalf("same-identity entitlement/quota state was not preserved: %#v", row.Credentials)
	}
}

func TestRefreshAntigravityAccountTransitionsIdentityAtomicallyOnQuotaFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		authorization := r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-b","refresh_token":"refresh-b-rotated","expires_in":3600}`)
		case "/userinfo":
			if authorization == "Bearer access-a" {
				_, _ = io.WriteString(w, `{"id":"subject-a","email":"a@example.com","verified_email":true,"name":"Account A"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"subject-b","email":"b@example.com","verified_email":true,"name":"Account B"}`)
		case "/load":
			if authorization == "Bearer access-a" {
				_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-a","paidTier":{"name":"Tier A"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-b","paidTier":{"name":"Tier B"}}`)
		case "/quota":
			if authorization == "Bearer access-a" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"expired"}`)
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	oldQuota := `{"models":[{"model_id":"gemini-a","remaining_percent":90}]}`
	oldPermissions := `{"allowed":true,"effective_tier":"Tier A"}`
	accountID, err := db.InsertAccountWithUpstream(ctx, "before", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-a", "refresh_token": "refresh-b",
		"expires_at":            time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"antigravity_client_id": "client-id", "antigravity_client_secret": "client-secret",
		"email": "a@example.com", "account_id": "subject-a", "credential_family_id": "family-a",
		"project_id": "project-a", "plan_type": "Tier A", "models": []string{"gemini-a"},
		"antigravity_quota": oldQuota, "antigravity_permissions": oldPermissions,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	item := handler.refreshAntigravityAccount(ctx, accountID)
	if item.OK || !strings.Contains(item.Error, "HTTP 500") {
		t.Fatalf("refresh item = %+v", item)
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	wantFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-b")
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != wantFamily || row.GetCredential("account_id") != "subject-b" || row.GetCredential("email") != "b@example.com" || row.GetCredential("refresh_token") != "refresh-b-rotated" {
		t.Fatalf("transitioned row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
	if row.GetCredential("antigravity_quota") != "" || len(row.GetCredentialStringSlice("models")) != 0 || strings.Contains(row.GetCredential("antigravity_permissions"), "Tier A") || !strings.Contains(row.GetCredential("antigravity_permissions"), "Tier B") {
		t.Fatalf("old identity snapshots survived transition: %#v", row.Credentials)
	}
}

func TestRefreshAntigravityAccountAcquiresSubjectAndClearsUnverifiedSnapshotsOnQuotaFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-new","refresh_token":"refresh-rotated","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-new","email":"user@example.com","verified_email":true,"name":"Verified User"}`)
		case "/load", "/quota":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	oldQuota := `{"models":[{"model_id":"stale-model","remaining_percent":90}]}`
	oldPermissions := `{"allowed":true,"effective_tier":"Stale Tier"}`
	accountID, err := db.InsertAccountWithUpstream(ctx, "unsynced", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-old", "refresh_token": "refresh-old", "id_token": "id-token-a",
		"expires_at":            time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"antigravity_client_id": "client-id", "antigravity_client_secret": "client-secret",
		"email": "user@example.com", "credential_family_id": "ag_unverified_import",
		"project_id": "stale-project", "plan_type": "Stale Tier", "models": []string{"stale-model"},
		"antigravity_quota": oldQuota, "antigravity_permissions": oldPermissions,
		"antigravity_entitlements": oldPermissions, "antigravity_last_synced_at": "2025-01-02T03:04:05Z",
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	item := handler.refreshAntigravityAccount(ctx, accountID)
	if item.OK || !strings.Contains(item.Error, "HTTP 500") {
		t.Fatalf("refresh item = %+v", item)
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	wantFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-new")
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != wantFamily || row.GetCredential("credential_family_id") != wantFamily || row.GetCredential("account_id") != "subject-new" || row.GetCredential("refresh_token") != "refresh-rotated" || !row.GetCredentialBool("verified_email") {
		t.Fatalf("acquired identity row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
	if row.GetCredential("project_id") != "" || row.GetCredential("plan_type") != "" || row.GetCredential("antigravity_quota") != "" || row.GetCredential("antigravity_permissions") != "" || row.GetCredential("antigravity_entitlements") != "" || row.GetCredential("antigravity_last_synced_at") != "" || len(row.GetCredentialStringSlice("models")) != 0 {
		t.Fatalf("unverified identity snapshots survived subject acquisition: %#v", row.Credentials)
	}
}

func TestRefreshAntigravityAccountClearsOldPrincipalStateWhenReplacementEntitlementsFail(t *testing.T) {
	quotaBodies := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-b","refresh_token":"refresh-b-rotated","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-b","email":"b@example.com","verified_email":true,"name":"Account B"}`)
		case "/load":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		case "/quota":
			body, _ := io.ReadAll(r.Body)
			quotaBodies <- string(body)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	oldQuota := `{"models":[{"model_id":"gemini-a","remaining_percent":90}]}`
	oldPermissions := `{"allowed":true,"project_id":"project-a","effective_tier":"Tier A"}`
	accountID, err := db.InsertAccountWithUpstream(ctx, "account-a", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-a", "refresh_token": "refresh-b",
		"expires_at":            time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"antigravity_client_id": "client-id", "antigravity_client_secret": "client-secret",
		"email": "a@example.com", "verified_email": true, "account_id": "subject-a",
		"credential_family_id": antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-a"),
		"project_id":           "project-a", "plan_type": "Tier A", "models": []string{"gemini-a"},
		"antigravity_quota": oldQuota, "antigravity_permissions": oldPermissions,
		"antigravity_entitlements": oldPermissions, "antigravity_last_synced_at": "2025-01-02T03:04:05Z",
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	item := handler.refreshAntigravityAccount(ctx, accountID)
	if item.OK || !strings.Contains(item.Error, "HTTP 500") {
		t.Fatalf("refresh item = %+v", item)
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	wantFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-b")
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != wantFamily || row.GetCredential("credential_family_id") != wantFamily || row.GetCredential("account_id") != "subject-b" || row.GetCredential("email") != "b@example.com" || row.GetCredential("refresh_token") != "refresh-b-rotated" {
		t.Fatalf("replacement identity row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
	if row.GetCredential("project_id") != "" || row.GetCredential("plan_type") != "" || row.GetCredential("antigravity_quota") != "" || row.GetCredential("antigravity_permissions") != "" || row.GetCredential("antigravity_entitlements") != "" || row.GetCredential("antigravity_last_synced_at") != "" || len(row.GetCredentialStringSlice("models")) != 0 {
		t.Fatalf("old principal state survived failed replacement sync: %#v", row.Credentials)
	}
	select {
	case body := <-quotaBodies:
		if strings.Contains(body, "project-a") {
			t.Fatalf("old principal project was sent to quota: %s", body)
		}
	default:
		t.Fatal("quota request body was not captured")
	}
}

func TestRefreshAntigravityAccountPersistsRotationWhenProfileLookupFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-new","refresh_token":"refresh-rotated","expires_in":3600}`)
		case "/userinfo":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	oldFamilyID := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-a")
	provisionalFamilyID := antigravityCredentialFamilyID(auth.AntigravityCredential{RefreshToken: "refresh-old"}, "")
	oldQuota := `{"models":[{"model_id":"gemini-a","remaining_percent":90}]}`
	oldPermissions := `{"allowed":true,"project_id":"project-a","effective_tier":"Tier A"}`
	accountID, err := db.InsertAccountWithUpstream(ctx, "account-a", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-old", "refresh_token": "refresh-old", "id_token": "id-token-a",
		"expires_at":            time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"antigravity_client_id": "client-id", "antigravity_client_secret": "client-secret",
		"email": "a@example.com", "verified_email": true, "account_id": "subject-a", "credential_family_id": oldFamilyID,
		"project_id": "project-a", "plan_type": "Tier A", "models": []string{"gemini-a"},
		"antigravity_quota": oldQuota, "antigravity_permissions": oldPermissions,
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	item := handler.refreshAntigravityAccount(ctx, accountID)
	if item.OK || !strings.Contains(item.Error, "HTTP 503") {
		t.Fatalf("refresh item = %+v", item)
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != provisionalFamilyID || row.GetCredential("credential_family_id") != provisionalFamilyID || row.GetCredential("access_token") != "access-new" || row.GetCredential("refresh_token") != "refresh-rotated" || row.GetCredential("account_id") != "" || row.GetCredential("email") != "" || row.GetCredential("id_token") != "" || row.GetCredential("antigravity_sync_error") == "" {
		t.Fatalf("failed profile refresh row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
	if row.GetCredential("project_id") != "" || row.GetCredential("plan_type") != "" || row.GetCredential("antigravity_quota") != "" || row.GetCredential("antigravity_permissions") != "" || len(row.GetCredentialStringSlice("models")) != 0 {
		t.Fatalf("old principal snapshots survived profile outage: %#v", row.Credentials)
	}
}

func TestRefreshAntigravityAccountRejectsDuplicatePrincipalWithoutMovingCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-b","refresh_token":"refresh-b-rotated","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-b","email":"b@example.com","verified_email":true,"name":"Account B"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-b","paidTier":{"name":"Tier B"}}`)
		case "/quota":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	targetFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-b")
	targetID, err := db.InsertAccountWithUpstream(ctx, "target-b", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "target-access", "refresh_token": "target-refresh",
		"email": "b@example.com", "verified_email": true, "account_id": "subject-b", "credential_family_id": targetFamily,
		"project_id": "project-b", "plan_type": "Tier B", "antigravity_permissions": `{"effective_tier":"Tier B"}`,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	sourceID, err := db.InsertAccountWithUpstream(ctx, "source-a", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-a", "refresh_token": "refresh-b",
		"expires_at":            time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"antigravity_client_id": "client-id", "antigravity_client_secret": "client-secret",
		"email": "a@example.com", "verified_email": true, "account_id": "subject-a",
		"credential_family_id": antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-a"),
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	item := handler.refreshAntigravityAccount(ctx, sourceID)
	if item.OK || !strings.Contains(item.Error, "already belongs to account") {
		t.Fatalf("duplicate refresh item = %+v", item)
	}
	source, err := db.GetAccountByID(ctx, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	target, err := db.GetAccountByID(ctx, targetID)
	if err != nil {
		t.Fatal(err)
	}
	if source.CredentialGeneration != 1 || source.GetCredential("access_token") != "access-a" || source.GetCredential("refresh_token") != "refresh-b" || source.GetCredential("account_id") != "subject-a" {
		t.Fatalf("duplicate source was mutated: generation=%d credentials=%#v", source.CredentialGeneration, source.Credentials)
	}
	if source.GetCredential("antigravity_sync_error") == "" {
		t.Fatal("duplicate source did not record the conflict status")
	}
	if target.CredentialGeneration != 1 || target.GetCredential("access_token") != "target-access" || target.GetCredential("refresh_token") != "target-refresh" {
		t.Fatalf("duplicate target was unexpectedly mutated: generation=%d credentials=%#v", target.CredentialGeneration, target.Credentials)
	}
	rows, err := db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, row := range rows {
		if antigravityRowFamilyID(row) == targetFamily {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("duplicate family owners = %d, want 1", count)
	}
}

func TestFindAntigravityDuplicateDoesNotUseEmailAcrossAuthoritativeSubjects(t *testing.T) {
	rows := []*database.AccountRow{{
		ID: 1, CredentialFamilyID: "family-a", Credentials: map[string]interface{}{
			"email": "shared@example.com", "account_id": "subject-a",
		},
	}}
	if got := findAntigravityDuplicateAccountID(rows, "family-b", "shared@example.com", "subject-b", 0); got != 0 {
		t.Fatalf("same-email different-subject duplicate = %d, want none", got)
	}
}

func TestUpdateAntigravityAccountPersistsFailedCredentialReplacement(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENTS", "test|test-client|test-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-b","refresh_token":"refresh-b-rotated","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject-b","email":"b@example.com","verified_email":true,"name":"Account B"}`)
		case "/load", "/quota":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	previousEndpoints := auth.DefaultAntigravityEndpoints
	auth.DefaultAntigravityEndpoints = auth.AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	}
	defer func() { auth.DefaultAntigravityEndpoints = previousEndpoints }()

	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()
	accountID, err := db.InsertAccountWithUpstream(ctx, "account-a", "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-a", "refresh_token": "refresh-a",
		"email": "a@example.com", "verified_email": true, "account_id": "subject-a",
		"credential_family_id": antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-a"),
		"project_id":           "project-a", "plan_type": "Tier A", "models": []string{"gemini-a"},
		"antigravity_quota":       `{"models":[{"model_id":"gemini-a","remaining_percent":90}]}`,
		"antigravity_permissions": `{"effective_tier":"Tier A"}`,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	runtimeStore := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(runtimeStore.Stop)
	handler.store = runtimeStore
	if err := runtimeStore.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: itoa(accountID)}}
	ginContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(accountID)+"/antigravity", bytes.NewBufferString(`{"auth_json":"{\"access_token\":\"access-old\",\"refresh_token\":\"refresh-b\",\"expires_at\":\"2020-01-01T00:00:00Z\"}"}`))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAntigravityAccount(ginContext)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "HTTP 500") {
		t.Fatalf("failed replacement response = %d %s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	wantFamily := antigravityCredentialFamilyID(auth.AntigravityCredential{}, "subject-b")
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != wantFamily || row.GetCredential("credential_family_id") != wantFamily || row.GetCredential("access_token") != "access-b" || row.GetCredential("refresh_token") != "refresh-b-rotated" || row.GetCredential("account_id") != "subject-b" || row.GetCredential("email") != "b@example.com" {
		t.Fatalf("failed replacement row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
	if row.GetCredential("project_id") != "" || row.GetCredential("plan_type") != "" || row.GetCredential("antigravity_quota") != "" || row.GetCredential("antigravity_permissions") != "" || len(row.GetCredentialStringSlice("models")) != 0 {
		t.Fatalf("old replacement identity state survived: %#v", row.Credentials)
	}
	runtimeAccount := runtimeStore.FindByID(accountID)
	if runtimeAccount == nil || runtimeAccount.AccessToken != "access-b" || runtimeAccount.RefreshToken != "refresh-b-rotated" ||
		runtimeAccount.AccountID != "subject-b" || runtimeAccount.CredentialGeneration != row.CredentialGeneration ||
		runtimeAccount.CredentialFamilyID != row.CredentialFamilyID {
		t.Fatalf("failed replacement left stale runtime credentials: runtime=%+v persisted_generation=%d persisted_family=%q", runtimeAccount, row.CredentialGeneration, row.CredentialFamilyID)
	}
}
