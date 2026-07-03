package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestParseImportJSONTokensSupportsFlatObjectWithBOM(t *testing.T) {
	data := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"refresh_token":"rt-flat","email":"flat@example.com"}`)...)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}

	if len(tokens) != 1 {
		t.Fatalf("tokens len = %d, want 1", len(tokens))
	}
	if tokens[0].refreshToken != "rt-flat" {
		t.Fatalf("refreshToken = %q, want %q", tokens[0].refreshToken, "rt-flat")
	}
	if tokens[0].name != "flat@example.com" {
		t.Fatalf("name = %q, want %q", tokens[0].name, "flat@example.com")
	}
	if tokens[0].accessToken != "" {
		t.Fatalf("accessToken = %q, want empty", tokens[0].accessToken)
	}
}

func TestParseImportJSONTokensSupportsFlatArray(t *testing.T) {
	data := []byte(`[
		{"refresh_token":"rt-1","email":"one@example.com"},
		{"access_token":"at-2","email":"two@example.com"},
		{"refresh_token":"","access_token":"","email":"ignored@example.com"}
	]`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}

	if len(tokens) != 2 {
		t.Fatalf("tokens len = %d, want 2", len(tokens))
	}
	if tokens[0].refreshToken != "rt-1" || tokens[0].name != "one@example.com" {
		t.Fatalf("first token = %+v, want rt-1 / one@example.com", tokens[0])
	}
	if tokens[1].accessToken != "at-2" || tokens[1].name != "two@example.com" {
		t.Fatalf("second token = %+v, want at-2 / two@example.com", tokens[1])
	}
}

func TestParseImportJSONTokensSupportsSub2API(t *testing.T) {
	data := []byte(`{
		"exported_at": "2026-04-03T14:49:53Z",
		"proxies": [
			{"proxy_key":"http|10.0.1.4|80|user|pass","name":"ignored proxy"}
		],
		"accounts": [
			{
				"name": "Primary Account",
				"proxy_key": "http|10.0.1.4|80|user|pass",
				"credentials": {
					"refresh_token": "rt-primary",
					"access_token": "at-primary",
					"email": "primary@example.com"
				},
				"extra": {"ignored": true}
			},
			{
				"credentials": {
					"access_token": "at-email-fallback",
					"email": "fallback@example.com"
				}
			},
			{
				"credentials": {
					"access_token": "at-default-name"
				}
			},
			{
				"name": "Ignored Account",
				"credentials": {}
			}
		]
	}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}

	if len(tokens) != 3 {
		t.Fatalf("tokens len = %d, want 3", len(tokens))
	}

	if tokens[0].refreshToken != "rt-primary" {
		t.Fatalf("first refreshToken = %q, want %q", tokens[0].refreshToken, "rt-primary")
	}
	if tokens[0].accessToken != "at-primary" {
		t.Fatalf("first accessToken = %q, want %q", tokens[0].accessToken, "at-primary")
	}
	if tokens[0].name != "Primary Account" {
		t.Fatalf("first name = %q, want %q", tokens[0].name, "Primary Account")
	}

	if tokens[1].accessToken != "at-email-fallback" || tokens[1].name != "fallback@example.com" {
		t.Fatalf("second token = %+v, want access token with email fallback", tokens[1])
	}

	if tokens[2].accessToken != "at-default-name" || tokens[2].name != "" {
		t.Fatalf("third token = %+v, want access token with empty name for default naming", tokens[2])
	}
}

func TestParseImportJSONTokensSupportsSub2APINumericExpiresAt(t *testing.T) {
	data := []byte(`{
		"accounts": [
			{
				"name": "Numeric Expiry",
				"credentials": {
					"refresh_token": "rt-numeric",
					"access_token": "at-numeric",
					"expires_at": 1779071020
				}
			}
		]
	}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}

	if len(tokens) != 1 {
		t.Fatalf("tokens len = %d, want 1", len(tokens))
	}
	if tokens[0].expiresAt != "1779071020" {
		t.Fatalf("expiresAt = %q, want numeric value preserved", tokens[0].expiresAt)
	}
}

func TestConflictingImportChatGPTIDs(t *testing.T) {
	tokens := []importToken{
		{chatgptAccountID: "shared", refreshToken: "rt-1"},
		{chatgptAccountID: "shared", refreshToken: "rt-2"},
		{chatgptAccountID: "stable", refreshToken: "rt-3"},
		{chatgptAccountID: "stable", refreshToken: "rt-3"},
	}

	conflicts := conflictingImportChatGPTIDs(tokens)
	if !conflicts["shared"] {
		t.Fatal("shared chatgpt_account_id should be marked conflicting")
	}
	if conflicts["stable"] {
		t.Fatal("stable chatgpt_account_id should not be marked conflicting")
	}
	if got := reliableImportChatGPTID(tokens[0], conflicts); got != "" {
		t.Fatalf("reliableImportChatGPTID(shared) = %q, want empty", got)
	}
	if got := reliableImportChatGPTID(tokens[2], conflicts); got != "stable" {
		t.Fatalf("reliableImportChatGPTID(stable) = %q, want stable", got)
	}
}

func TestParseCredentialExpiresAtSupportsUnixSeconds(t *testing.T) {
	got := parseCredentialExpiresAt("1779071020").UTC()
	want := time.Unix(1779071020, 0).UTC()
	if !got.Equal(want) {
		t.Fatalf("parseCredentialExpiresAt = %s, want %s", got, want)
	}
}

func TestParseImportJSONTokensPreservesCPAFields(t *testing.T) {
	data := []byte(`{
		"type": "codex",
		"email": "cpa@example.com",
		"plan_type": "free",
		"codex_7d_used_percent": 3,
		"codex_7d_reset_at": "2026-05-15T20:33:11+08:00",
		"codex_5h_used_percent": 0,
		"codex_5h_reset_at": "2026-05-11T11:39:07+08:00",
		"codex_5h_usage_updated_at": "2026-05-11T10:39:07+08:00",
		"codex_usage_updated_at": "2026-05-11T11:39:07+08:00",
		"expired": "2026-04-25T12:00:00Z",
		"id_token": "id-cpa",
		"account_id": "acc-cpa",
		"access_token": "at-cpa",
		"refresh_token": "rt-cpa"
	}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("tokens len = %d, want 1", len(tokens))
	}

	token := tokens[0]
	if token.refreshToken != "rt-cpa" || token.accessToken != "at-cpa" {
		t.Fatalf("token = %+v, want RT and AT preserved", token)
	}
	if token.email != "cpa@example.com" || token.name != "cpa@example.com" {
		t.Fatalf("identity = name:%q email:%q, want cpa@example.com", token.name, token.email)
	}
	if token.planType != "free" {
		t.Fatalf("planType = %q, want free", token.planType)
	}
	if token.codex7DUsedPercent != "3" || token.codex7DResetAt != "2026-05-15T20:33:11+08:00" {
		t.Fatalf("7d usage = %q/%q, want 3/reset", token.codex7DUsedPercent, token.codex7DResetAt)
	}
	if token.codex5HUsedPercent != "0" || token.codex5HResetAt != "2026-05-11T11:39:07+08:00" {
		t.Fatalf("5h usage = %q/%q, want 0/reset", token.codex5HUsedPercent, token.codex5HResetAt)
	}
	if token.codex5HUsageUpdatedAt != "2026-05-11T10:39:07+08:00" {
		t.Fatalf("5h usageUpdatedAt = %q, want timestamp", token.codex5HUsageUpdatedAt)
	}
	if token.codexUsageUpdatedAt != "2026-05-11T11:39:07+08:00" {
		t.Fatalf("usageUpdatedAt = %q, want timestamp", token.codexUsageUpdatedAt)
	}
	if token.idToken != "id-cpa" || token.accountID != "acc-cpa" || token.expiresAt != "2026-04-25T12:00:00Z" {
		t.Fatalf("metadata = %+v, want CPA token metadata preserved", token)
	}
}

func TestAccountFromCredentialSeedRestoresUsageSnapshots(t *testing.T) {
	account := accountFromCredentialSeed(42, "", tokenCredentialSeed{
		planType:              "free",
		codex7DUsedPercent:    "3",
		codex7DResetAt:        "2026-05-15T20:33:11+08:00",
		codex5HUsedPercent:    "0",
		codex5HResetAt:        "2026-05-11T11:39:07+08:00",
		codex5HUsageUpdatedAt: "2026-05-11T10:39:07+08:00",
		codexUsageUpdatedAt:   "2026-05-11T11:39:07+08:00",
	})

	if got := account.GetPlanType(); got != "free" {
		t.Fatalf("PlanType = %q, want free", got)
	}
	pct7d, ok := account.GetUsagePercent7d()
	if !ok || pct7d != 3 {
		t.Fatalf("7d usage = %v/%t, want 3/true", pct7d, ok)
	}
	if account.GetReset7dAt().IsZero() {
		t.Fatal("Reset7dAt is zero")
	}
	pct5h, ok := account.GetUsagePercent5h()
	if !ok || pct5h != 0 {
		t.Fatalf("5h usage = %v/%t, want 0/true", pct5h, ok)
	}
	if account.GetUsageUpdatedAt5h().IsZero() {
		t.Fatal("UsageUpdatedAt5h is zero")
	}
	if account.GetUsageUpdatedAt5h().Equal(account.GetUsageUpdatedAt()) {
		t.Fatalf("UsageUpdatedAt5h = %s, want separate 5h timestamp from 7d", account.GetUsageUpdatedAt5h())
	}
}

func TestAccountFromCredentialSeedDoesNotReuse7dFreshnessForMissing5hTimestamp(t *testing.T) {
	account := accountFromCredentialSeed(42, "", tokenCredentialSeed{
		codex7DUsedPercent:  "3",
		codex5HUsedPercent:  "95",
		codex5HResetAt:      time.Now().Add(time.Hour).Format(time.RFC3339),
		codexUsageUpdatedAt: time.Now().Format(time.RFC3339),
	})

	if account.GetUsageUpdatedAt().IsZero() {
		t.Fatal("UsageUpdatedAt is zero")
	}
	if !account.GetUsageUpdatedAt5h().IsZero() {
		t.Fatalf("UsageUpdatedAt5h = %s, want zero when codex_5h_usage_updated_at is missing", account.GetUsageUpdatedAt5h())
	}
}

func TestParseImportJSONTokensReturnsNoTokensForValidUnsupportedJSON(t *testing.T) {
	data := []byte(`{"accounts":[{"credentials":{}}],"proxies":[{"proxy_key":"ignored"}]}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("tokens len = %d, want 0", len(tokens))
	}
}

func TestParseImportJSONTokensRejectsInvalidJSON(t *testing.T) {
	if _, err := parseImportJSONTokens([]byte(`{"accounts":[}`)); err == nil {
		t.Fatal("expected invalid JSON error, got nil")
	}
}

func TestImportTokensFromTextFilesReadsAllUploadedFiles(t *testing.T) {
	files := []uploadedImportFile{
		{name: "one.txt", data: append([]byte{0xef, 0xbb, 0xbf}, []byte("rt-1\nrt-shared\n")...)},
		{name: "two.txt", data: []byte("rt-2\nrt-shared\n")},
	}

	tokens := importTokensFromTextFiles(files, func(token string) importToken {
		return importToken{refreshToken: token}
	})

	if len(tokens) != 3 {
		t.Fatalf("tokens len = %d, want 3", len(tokens))
	}
	for i, want := range []string{"rt-1", "rt-shared", "rt-2"} {
		if tokens[i].refreshToken != want {
			t.Fatalf("tokens[%d] = %q, want %q", i, tokens[i].refreshToken, want)
		}
	}
}

func TestReadUploadedImportFilesReadsRepeatedFileFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := newMultipartRequest(t, map[string]string{
		"one.txt": "rt-1",
		"two.txt": "rt-2",
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	files, err := readUploadedImportFiles(ctx)
	if err != nil {
		t.Fatalf("readUploadedImportFiles returned error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files len = %d, want 2", len(files))
	}
	got := map[string]bool{}
	for _, file := range files {
		got[string(file.data)] = true
	}
	if !got["rt-1"] || !got["rt-2"] {
		t.Fatalf("files = %+v, want both uploaded files", files)
	}
}

func TestValidateImportFileSize(t *testing.T) {
	if err := validateImportFileSize(&multipart.FileHeader{Filename: "ok.txt", Size: importFileSizeLimitBytes}); err != nil {
		t.Fatalf("validateImportFileSize returned error for boundary size: %v", err)
	}

	err := validateImportFileSize(&multipart.FileHeader{Filename: "too-big.txt", Size: importFileSizeLimitBytes + 1})
	if err == nil {
		t.Fatal("expected oversized file error, got nil")
	}
	if got, want := err.Error(), "文件 too-big.txt 大小超过 20MB"; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}

func TestImportAccountsJSONReturnsExistingNoTokenMessageForUnsupportedJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := newMultipartJSONRequest(t, "accounts.json", `{"accounts":[{"credentials":{}}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler := &Handler{}
	handler.importAccountsJSON(ctx, "", false)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "JSON 文件中未找到有效的 refresh_token 或 access_token" {
		t.Fatalf("error = %q, want %q", got, "JSON 文件中未找到有效的 refresh_token 或 access_token")
	}
}

func TestImportAccountsJSONRejectsInvalidJSONFile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := newMultipartJSONRequest(t, "broken.json", `{"accounts":[}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler := &Handler{}
	handler.importAccountsJSON(ctx, "", false)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "文件 broken.json 不是有效的 JSON 格式" {
		t.Fatalf("error = %q, want %q", got, "文件 broken.json 不是有效的 JSON 格式")
	}
}

func TestImportAccountsCommonDoesNotCollapseConflictingChatGPTAccountID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{
		{name: "sub2api-1", refreshToken: "rt-shared-id-1", accessToken: "at-shared-id-1", chatgptAccountID: "same-exported-id"},
		{name: "sub2api-2", refreshToken: "rt-shared-id-2", accessToken: "at-shared-id-2", chatgptAccountID: "same-exported-id"},
	}, "", false)

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("active rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if got := row.GetCredential("account_id"); got != "" {
			t.Fatalf("account_id = %q, want empty for conflicting chatgpt_account_id", got)
		}
	}
}

func TestImportAccountsCommonUpdatesExistingOAuthIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	probed := make(chan int64, 1)
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(_ context.Context, acc *auth.Account) error {
			probed <- acc.DBID
			return nil
		},
	}

	existingID, err := db.InsertAccountWithCredentials(context.Background(), "existing", map[string]interface{}{
		"refresh_token": "rt-old",
		"email":         "import@example.com",
		"account_id":    "acc-import",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{{
		refreshToken: "rt-new",
		accessToken:  "at-new",
		email:        "Import@Example.com",
		accountID:    "acc-import",
		planType:     "team",
	}}, "", false)

	select {
	case id := <-probed:
		if id != existingID {
			t.Fatalf("probed account id = %d, want %d", id, existingID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage probe was not triggered for updated OAuth identity")
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("active rows = %d, want 1", len(rows))
	}
	row, err := db.GetAccountByID(context.Background(), existingID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "rt-new" {
		t.Fatalf("refresh_token = %q, want rt-new", got)
	}
	if got := row.GetCredential("access_token"); got != "at-new" {
		t.Fatalf("access_token = %q, want at-new", got)
	}
	if got := row.GetCredential("plan_type"); got != "team" {
		t.Fatalf("plan_type = %q, want team", got)
	}
	if account := store.FindByID(existingID); account == nil {
		t.Fatalf("runtime account %d not found after import update", existingID)
	}
}

func TestImportAccountsCommonSkipsExistingOAuthIdentityWithSameCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			t.Fatal("usage probe should not run for unchanged duplicate import")
			return nil
		},
	}

	existingID, err := db.InsertAccountWithCredentials(context.Background(), "existing", map[string]interface{}{
		"refresh_token": "rt-same",
		"session_token": "st-same",
		"access_token":  "at-same",
		"email":         "same@example.com",
		"account_id":    "acc-same",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{{
		refreshToken: "rt-same",
		sessionToken: "st-same",
		accessToken:  "at-same",
		email:        "Same@Example.com",
		accountID:    "acc-same",
	}}, "", false)

	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if got := int(payload["success"].(float64)); got != 0 {
		t.Fatalf("success = %d, want 0", got)
	}
	if got := int(payload["duplicate"].(float64)); got != 1 {
		t.Fatalf("duplicate = %d, want 1", got)
	}
	if got := int(payload["total"].(float64)); got != 1 {
		t.Fatalf("total = %d, want 1", got)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != existingID {
		t.Fatalf("active rows = %+v, want only existing id %d", rows, existingID)
	}
}

func TestImportAccountsCommonSkipsAmbiguousOAuthIdentityWithExistingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			return nil
		},
	}

	existingID, err := db.InsertAccountWithCredentials(context.Background(), "existing", map[string]interface{}{
		"refresh_token": "rt-old",
		"email":         "ambiguous@example.com",
		"account_id":    "acc-ambiguous",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{
		{refreshToken: "rt-new-1", email: "ambiguous@example.com", accountID: "acc-ambiguous"},
		{refreshToken: "rt-new-2", email: "Ambiguous@Example.com", accountID: "acc-ambiguous"},
	}, "", false)

	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if got := int(payload["success"].(float64)); got != 0 {
		t.Fatalf("success = %d, want 0", got)
	}
	if got := int(payload["duplicate"].(float64)); got != 2 {
		t.Fatalf("duplicate = %d, want 2", got)
	}
	if got := int(payload["total"].(float64)); got != 2 {
		t.Fatalf("total = %d, want 2", got)
	}

	row, err := db.GetAccountByID(context.Background(), existingID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "rt-old" {
		t.Fatalf("refresh_token = %q, want rt-old", got)
	}
}

func TestImportAccountsCommonSkipsAmbiguousOAuthIdentityWithoutExistingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{
		{refreshToken: "rt-new-1", email: "new-ambiguous@example.com", accountID: "acc-new-ambiguous"},
		{refreshToken: "rt-new-2", email: "New-Ambiguous@Example.com", accountID: "acc-new-ambiguous"},
	}, "", false)

	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if got := int(payload["success"].(float64)); got != 0 {
		t.Fatalf("success = %d, want 0", got)
	}
	if got := int(payload["duplicate"].(float64)); got != 2 {
		t.Fatalf("duplicate = %d, want 2", got)
	}
	if got := int(payload["total"].(float64)); got != 2 {
		t.Fatalf("total = %d, want 2", got)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("active rows = %d, want 0", len(rows))
	}
}

func TestImportAccountsCommonCollapsesIdenticalOAuthIdentityInFile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{
		{refreshToken: "rt-same-file", accessToken: "at-same-file", email: "same-file@example.com", accountID: "acc-same-file"},
		{refreshToken: "rt-same-file", accessToken: "at-same-file", email: "Same-File@Example.com", accountID: "acc-same-file"},
	}, "", false)

	if !strings.Contains(recorder.Body.String(), `"type":"complete"`) ||
		!strings.Contains(recorder.Body.String(), `"success":1`) ||
		!strings.Contains(recorder.Body.String(), `"total":1`) {
		t.Fatalf("SSE payload = %q, want complete success=1 total=1", recorder.Body.String())
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("active rows = %d, want 1", len(rows))
	}
	if got := rows[0].GetCredential("refresh_token"); got != "rt-same-file" {
		t.Fatalf("refresh_token = %q, want rt-same-file", got)
	}
}

func TestImportAccountsCommonTriggersUsageProbeForImportedAccountWithAccessToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	probed := make(chan int64, 1)
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(_ context.Context, acc *auth.Account) error {
			probed <- acc.DBID
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{{
		refreshToken: "rt-import-probe",
		accessToken:  "at-import-probe",
	}}, "", false)

	select {
	case id := <-probed:
		if id == 0 {
			t.Fatal("probed account id is zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage probe was not triggered for imported account with access token")
	}
}

func TestImportAccountsCommonMarksImported7dUsageAsRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	resetAt := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)
	handler.importAccountsCommon(ctx, []importToken{{
		refreshToken:       "rt-import-limited",
		accessToken:        "at-import-limited",
		planType:           "team",
		codex7DUsedPercent: "100",
		codex7DResetAt:     resetAt.Format(time.RFC3339),
	}}, "", false)

	accounts := store.Accounts()
	if len(accounts) != 1 {
		t.Fatalf("store accounts = %d, want 1", len(accounts))
	}
	account := accounts[0]
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	reason, until := account.GetCooldownSnapshot()
	if reason != "rate_limited" || !until.After(time.Now()) {
		t.Fatalf("cooldown = (%q, %s), want active rate_limited", reason, until)
	}

	row, err := db.GetAccountByID(context.Background(), account.DBID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.CooldownReason != "rate_limited" || !row.CooldownUntil.Valid {
		t.Fatalf("persisted cooldown = (%q, %v), want active rate_limited", row.CooldownReason, row.CooldownUntil)
	}
}

func TestImportAccountsCommonRefreshesAndProbesRTOnlyImport(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	probed := make(chan int64, 1)
	handler := &Handler{
		db:    db,
		store: store,
		refreshAccount: func(_ context.Context, id int64) error {
			acc := store.FindByID(id)
			if acc == nil {
				return fmt.Errorf("account %d not found", id)
			}
			acc.Mu().Lock()
			acc.AccessToken = "at-refreshed"
			acc.Mu().Unlock()
			return nil
		},
		probeUsage: func(_ context.Context, acc *auth.Account) error {
			probed <- acc.DBID
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{{refreshToken: "rt-import-refresh-probe"}}, "", false)

	select {
	case id := <-probed:
		if id == 0 {
			t.Fatal("probed account id is zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage probe was not triggered after RT-only import refresh")
	}
}

func TestImportAccountsCommonRefreshesOAuthIdentityRTOnlyImport(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	refreshed := make(chan int64, 2)
	probed := make(chan int64, 1)
	handler := &Handler{
		db:    db,
		store: store,
		refreshAccount: func(_ context.Context, id int64) error {
			refreshed <- id
			acc := store.FindByID(id)
			if acc == nil {
				return fmt.Errorf("account %d not found", id)
			}
			acc.Mu().Lock()
			acc.AccessToken = "at-oauth-identity-refreshed"
			acc.Mu().Unlock()
			return nil
		},
		probeUsage: func(_ context.Context, acc *auth.Account) error {
			probed <- acc.DBID
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{{
		refreshToken: "rt-oauth-identity-refresh-probe",
		email:        "identity-refresh@example.com",
		accountID:    "acc-identity-refresh",
	}}, "", false)

	select {
	case id := <-probed:
		if id == 0 {
			t.Fatal("probed account id is zero")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("usage probe was not triggered after OAuth identity RT-only import refresh")
	}
	select {
	case id := <-refreshed:
		if id == 0 {
			t.Fatal("refreshed account id is zero")
		}
	default:
		t.Fatal("refresh was not triggered")
	}
	select {
	case id := <-refreshed:
		t.Fatalf("refresh triggered more than once, second id=%d", id)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestAddAccountStreamReportsProgressAndProbesAfterRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	probed := make(chan int64, 2)
	handler := &Handler{
		db:    db,
		store: store,
		refreshAccount: func(_ context.Context, id int64) error {
			acc := store.FindByID(id)
			if acc == nil {
				return fmt.Errorf("account %d not found", id)
			}
			acc.Mu().Lock()
			acc.AccessToken = fmt.Sprintf("at-%d", id)
			acc.Mu().Unlock()
			return nil
		},
		probeUsage: func(_ context.Context, acc *auth.Account) error {
			probed <- acc.DBID
			return nil
		},
	}

	body := bytes.NewBufferString(`{"refresh_token":"rt-stream-1\nrt-stream-2"}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts?stream=true", body)
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.AddAccount(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	payload := recorder.Body.String()
	if !strings.Contains(payload, `"type":"complete"`) || !strings.Contains(payload, `"success":2`) {
		t.Fatalf("SSE payload = %q, want complete success=2", payload)
	}

	seen := map[int64]bool{}
	deadline := time.After(2 * time.Second)
	for len(seen) < 2 {
		select {
		case id := <-probed:
			seen[id] = true
		case <-deadline:
			t.Fatalf("usage probes = %v, want 2 accounts probed", seen)
		}
	}
}

func newMultipartJSONRequest(t *testing.T, filename string, content string) *http.Request {
	t.Helper()

	return newMultipartRequest(t, map[string]string{filename: content})
}

func newMultipartRequest(t *testing.T, files map[string]string) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for filename, content := range files {
		part, err := writer.CreateFormFile("file", filename)
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatalf("part.Write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

// TestImportAccountsCommonAllowDuplicateBypassesDedup 验证：勾选"允许重复添加"后，
// 同一 OAuth 身份会被作为独立账号新建，而不是更新已有账号。
func TestImportAccountsCommonAllowDuplicateBypassesDedup(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			return nil
		},
	}

	if _, err := db.InsertAccountWithCredentials(context.Background(), "existing", map[string]interface{}{
		"refresh_token": "rt-dup",
		"email":         "dup@example.com",
		"account_id":    "acc-dup",
	}, ""); err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)

	handler.importAccountsCommon(ctx, []importToken{{
		refreshToken: "rt-dup-2",
		email:        "dup@example.com",
		accountID:    "acc-dup",
	}}, "", true)

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("active rows = %d, want 2 (duplicate allowed)", len(rows))
	}
}

// TestAddAccountDedupsRefreshToken 验证：RT 单账号添加默认按 RT 原文对已有库去重。
func TestAddAccountDedupsRefreshToken(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			return nil
		},
	}

	if _, err := db.InsertAccountWithCredentials(context.Background(), "existing", map[string]interface{}{
		"refresh_token": "rt-existing",
	}, ""); err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	doAdd := func(body string) map[string]interface{} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.AddAccount(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
		}
		var resp map[string]interface{}
		if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}

	// 默认：重复 RT 应被跳过
	resp := doAdd(`{"refresh_token":"rt-existing"}`)
	if dup := resp["duplicate"]; dup != float64(1) {
		t.Fatalf("duplicate = %v, want 1", dup)
	}
	if rows, _ := db.ListActive(context.Background()); len(rows) != 1 {
		t.Fatalf("active rows = %d, want 1 (duplicate skipped)", len(rows))
	}

	// 勾选允许重复：同一 RT 应被新建
	resp = doAdd(`{"refresh_token":"rt-existing","allow_duplicate":true}`)
	if suc := resp["success"]; suc != float64(1) {
		t.Fatalf("success = %v, want 1", suc)
	}
	if rows, _ := db.ListActive(context.Background()); len(rows) != 2 {
		t.Fatalf("active rows = %d, want 2 (duplicate allowed)", len(rows))
	}
}

// 先导入 AT（个人账号，只有 user_id 身份），后导入裸 RT——RT 刷新后身份可知，
// 应把新凭证合并进已有 AT 账号（RT 升级）、软删新账号，且旧账号的用量快照保留。
func TestMergeRefreshedDuplicateIntoExisting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{db: db, store: store}

	// 已有 AT 账号：带用量统计
	oldID, err := db.InsertAccountWithCredentials(context.Background(), "at-first", map[string]interface{}{
		"access_token":          "at-rotation-1",
		"email":                 "solo@example.com",
		"user_id":               "user-merge1",
		"codex_7d_used_percent": "42.5",
	}, "")
	if err != nil {
		t.Fatalf("Insert old: %v", err)
	}

	// 新导入的 RT 账号：刷新完成后身份与旧账号相同
	newID, err := db.InsertAccountWithCredentials(context.Background(), "rt-later", map[string]interface{}{
		"refresh_token": "rt-fresh",
		"access_token":  "at-rotation-2",
		"email":         "solo@example.com",
		"user_id":       "user-merge1",
	}, "")
	if err != nil {
		t.Fatalf("Insert new: %v", err)
	}
	store.AddAccount(&auth.Account{DBID: newID, RefreshToken: "rt-fresh"})

	if merged := handler.mergeRefreshedDuplicateIntoExisting(newID, "test"); !merged {
		t.Fatal("expected duplicate to be merged into existing account")
	}

	oldRow, err := db.GetAccountByID(context.Background(), oldID)
	if err != nil {
		t.Fatalf("GetAccountByID old: %v", err)
	}
	if got := oldRow.GetCredential("refresh_token"); got != "rt-fresh" {
		t.Fatalf("refresh_token = %q, want rt-fresh (RT 应升级进旧账号)", got)
	}
	if got := oldRow.GetCredential("access_token"); got != "at-rotation-2" {
		t.Fatalf("access_token = %q, want at-rotation-2", got)
	}
	if got := oldRow.GetCredential("codex_7d_used_percent"); got != "42.5" {
		t.Fatalf("codex_7d_used_percent = %q, want 42.5 (用量统计必须保留)", got)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != oldID {
		t.Fatalf("active rows = %d (first id %d), want 1 row with id %d", len(rows), rows[0].ID, oldID)
	}
}

// 勾选"允许重复添加"导入的 RT 账号刷新后不得被合并。
func TestMergeRefreshedDuplicateSkipsAllowDuplicate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{db: db, store: store}

	if _, err := db.InsertAccountWithCredentials(context.Background(), "primary", map[string]interface{}{
		"access_token": "at-primary",
		"email":        "solo@example.com",
		"user_id":      "user-keep1",
	}, ""); err != nil {
		t.Fatalf("Insert primary: %v", err)
	}
	forcedID, err := db.InsertAccountWithCredentials(context.Background(), "forced", map[string]interface{}{
		"refresh_token":   "rt-forced",
		"email":           "solo@example.com",
		"user_id":         "user-keep1",
		"allow_duplicate": "true",
	}, "")
	if err != nil {
		t.Fatalf("Insert forced: %v", err)
	}

	if merged := handler.mergeRefreshedDuplicateIntoExisting(forcedID, "test"); merged {
		t.Fatal("allow_duplicate copy must not be merged")
	}
	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("active rows = %d, want 2 (forced copy preserved)", len(rows))
	}
}

func TestParseImportJSONTokensSupportsChatGPTSessionJSON(t *testing.T) {
	data := []byte(`{"user":{"id":"user-abc123","name":"John Doe","email":"john@example.com"},"accessToken":"at-session-test","expires":"2026-12-31T23:59:59Z"}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("tokens len = %d, want 1", len(tokens))
	}
	if tokens[0].accessToken != "at-session-test" {
		t.Fatalf("accessToken = %q, want at-session-test", tokens[0].accessToken)
	}
	if tokens[0].email != "john@example.com" {
		t.Fatalf("email = %q, want john@example.com", tokens[0].email)
	}
	if tokens[0].name != "John Doe" {
		t.Fatalf("name = %q, want John Doe", tokens[0].name)
	}
	if tokens[0].expiresAt != "2026-12-31T23:59:59Z" {
		t.Fatalf("expiresAt = %q, want 2026-12-31T23:59:59Z", tokens[0].expiresAt)
	}
}

func TestParseImportJSONTokensSupportsChatGPTSessionJSONArray(t *testing.T) {
	data := []byte(`[{"user":{"id":"user-1","name":"Alice","email":"alice@example.com"},"accessToken":"at-alice"},{"user":{"id":"user-2","name":"Bob"},"accessToken":"at-bob","expires":1767225600}]`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("tokens len = %d, want 2", len(tokens))
	}
	if tokens[0].accessToken != "at-alice" || tokens[0].email != "alice@example.com" {
		t.Fatalf("first token = %+v, want at-alice / alice@example.com", tokens[0])
	}
	if tokens[1].accessToken != "at-bob" {
		t.Fatalf("second accessToken = %q, want at-bob", tokens[1].accessToken)
	}
	if tokens[1].name != "Bob" {
		t.Fatalf("second name = %q, want Bob (from user.name)", tokens[1].name)
	}
	if tokens[1].email != "" {
		t.Fatalf("second email = %q, want empty (no user.email, no top-level email)", tokens[1].email)
	}
	if tokens[1].expiresAt != "1767225600" {
		t.Fatalf("second expiresAt = %q, want 1767225600", tokens[1].expiresAt)
	}
}

func TestParseImportJSONTokensHandlesSessionJSONWithoutAccessToken(t *testing.T) {
	data := []byte(`{"user":{"email":"no-token@example.com"},"expires":"2026-12-31T23:59:59Z"}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("tokens len = %d, want 0 (no access_token or refresh_token)", len(tokens))
	}
}
