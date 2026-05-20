package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestRefreshAccountRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		refreshAccount: func(context.Context, int64) error {
			t.Fatal("refresh should not be called for invalid id")
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "bad-id"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/bad-id/refresh", nil)

	handler.RefreshAccount(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "无效的账号 ID" {
		t.Fatalf("error = %q, want %q", got, "无效的账号 ID")
	}
}

func TestRefreshAccountRunsSingleRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var called bool
	var gotID int64
	handler := &Handler{
		refreshAccount: func(_ context.Context, id int64) error {
			called = true
			gotID = id
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "42"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/42/refresh", nil)

	handler.RefreshAccount(ctx)

	if !called {
		t.Fatal("expected refresh to be called")
	}
	if gotID != 42 {
		t.Fatalf("refresh id = %d, want %d", gotID, 42)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["message"]; got != "账号刷新成功" {
		t.Fatalf("message = %q, want %q", got, "账号刷新成功")
	}
}

func TestRefreshAccountReturnsNotFoundForMissingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		refreshAccount: func(context.Context, int64) error {
			return errors.New("账号 7 不存在")
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "7"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/7/refresh", nil)

	handler.RefreshAccount(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "账号 7 不存在" {
		t.Fatalf("error = %q, want %q", got, "账号 7 不存在")
	}
}

func TestRefreshAccountReturnsRefreshFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		refreshAccount: func(context.Context, int64) error {
			return errors.New("upstream unavailable")
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "9"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/9/refresh", nil)

	handler.RefreshAccount(ctx)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "刷新失败: upstream unavailable" {
		t.Fatalf("error = %q, want %q", got, "刷新失败: upstream unavailable")
	}
}

func TestResetAccountStatusSyncsPlanMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 42, AccessToken: "at", PlanType: "free"}
	account.SetUsageSnapshot(88, time.Now().Add(time.Hour))
	store.AddAccount(account)

	var called bool
	handler := &Handler{
		store: store,
		syncAccountPlanOnReset: func(_ context.Context, acc *auth.Account) error {
			called = true
			if acc == nil || acc.DBID != 42 {
				t.Fatalf("sync account = %#v, want DBID 42", acc)
			}
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "42"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/42/reset-status", nil)

	handler.ResetAccountStatus(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if !called {
		t.Fatal("expected reset to sync plan metadata")
	}
	if _, ok := account.GetUsagePercent7d(); ok {
		t.Fatal("expected reset to clear cached usage")
	}
}

func TestBatchResetStatusSyncsEachResolvedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: 11, AccessToken: "at-11", PlanType: "free"})
	store.AddAccount(&auth.Account{DBID: 22, AccessToken: "at-22", PlanType: "plus"})

	var gotIDs []int64
	handler := &Handler{
		store: store,
		syncAccountPlanOnReset: func(_ context.Context, acc *auth.Account) error {
			gotIDs = append(gotIDs, acc.DBID)
			if acc.DBID == 22 {
				return errors.New("temporary upstream failure")
			}
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-reset-status", strings.NewReader(`{"ids":[11,99,22]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.BatchResetStatus(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if fmt.Sprint(gotIDs) != "[11 22]" {
		t.Fatalf("synced ids = %v, want [11 22]", gotIDs)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["success"] != float64(2) || payload["failed"] != float64(1) {
		t.Fatalf("payload = %#v, want success=2 failed=1", payload)
	}
}

func TestCreateAPIKeyPersistsQuotaAndExpiration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	handler := &Handler{db: db}
	body := `{"name":"Client A","key":"sk-test-client-a-1234567890","quota_limit":0.25,"expires_in_days":7}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/keys", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.CreateAPIKey(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload createAPIKeyResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.ID <= 0 || payload.QuotaLimit != 0.25 || payload.ExpiresAt == nil {
		t.Fatalf("payload = %#v, want quota and expiration", payload)
	}

	row, err := db.GetAPIKeyByValue(context.Background(), "sk-test-client-a-1234567890")
	if err != nil {
		t.Fatalf("GetAPIKeyByValue 返回错误: %v", err)
	}
	if row.QuotaLimit != 0.25 || !row.ExpiresAt.Valid {
		t.Fatalf("row = %#v, want quota and expiration", row)
	}
}

func TestUpdateAPIKeyPreservesOmittedFieldsAndUpdatesLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	expiresAt := sql.NullTime{Time: time.Now().AddDate(0, 0, 3), Valid: true}
	id, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name:       "Client A",
		Key:        "sk-test-update-client-1234567890",
		QuotaLimit: 0.25,
		ExpiresAt:  expiresAt,
	})
	if err != nil {
		t.Fatalf("InsertAPIKeyWithOptions 返回错误: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/keys/1", strings.NewReader(`{"name":"Client B"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAPIKey(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err := db.GetAPIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAPIKeyByID 返回错误: %v", err)
	}
	if row.Name != "Client B" || row.QuotaLimit != 0.25 || !row.ExpiresAt.Valid {
		t.Fatalf("row = %#v, want renamed with quota/expiration preserved", row)
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/keys/1", strings.NewReader(`{"quota_limit":0,"expires_at":null}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAPIKey(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err = db.GetAPIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAPIKeyByID 返回错误: %v", err)
	}
	if row.Name != "Client B" || row.QuotaLimit != 0 || row.ExpiresAt.Valid {
		t.Fatalf("row = %#v, want quota/expiration cleared with name preserved", row)
	}
}

func TestUpdateAPIKeyRefreshesRuntimeStoreAndCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	groupID, err := db.CreateAccountGroup(ctx, "Team", "", "#2563eb", 0)
	if err != nil {
		t.Fatalf("CreateAccountGroup 返回错误: %v", err)
	}
	key := "sk-test-runtime-refresh-1234567890"
	keyID, err := db.InsertAPIKey(ctx, "Client A", key)
	if err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}
	store := auth.NewStore(nil, nil, nil)
	tc := cache.NewMemory(1)
	handler := &Handler{db: db, store: store, cache: tc}
	payload, err := json.Marshal(map[string]interface{}{
		"id":         keyID,
		"name":       "Client A",
		"created_at": time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal runtime cache: %v", err)
	}
	if err := tc.SetRuntime(ctx, adminAPIKeyCacheNamespace, key, payload, time.Minute); err != nil {
		t.Fatalf("SetRuntime api key: %v", err)
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", keyID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/keys/1", strings.NewReader(fmt.Sprintf(`{"allowed_group_ids":[%d]}`, groupID)))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAPIKey(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := store.GetAPIKeyAllowedGroups(keyID); len(got) != 1 || got[0] != groupID {
		t.Fatalf("runtime store allowed groups = %v, want [%d]", got, groupID)
	}
	if _, ok, err := tc.GetRuntime(ctx, adminAPIKeyCacheNamespace, key); err != nil || ok {
		t.Fatalf("runtime api key cache after update ok=%v err=%v, want miss", ok, err)
	}
	if _, ok, err := tc.GetRuntime(ctx, adminAPIKeyCountNamespace, "all"); err != nil || ok {
		t.Fatalf("runtime api key count cache after update ok=%v err=%v, want miss", ok, err)
	}
}

func TestGetAccountAuthJSONRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "bad-id"}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/bad-id/auth-json", nil)

	handler.GetAccountAuthJSON(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "无效的账号 ID")
}

func TestGetAccountAuthJSONReturnsCodexAuthFile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	if err := db.UpdateCredentials(context.Background(), accountID, map[string]interface{}{
		"id_token":     "id_test",
		"access_token": "access_test",
		"account_id":   "account_test",
	}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/accounts/%d/auth-json", accountID), nil)

	handler.GetAccountAuthJSON(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Disposition"); got != `attachment; filename="auth.json"` {
		t.Fatalf("Content-Disposition = %q, want auth.json attachment", got)
	}

	var payload struct {
		AuthMode     string  `json:"auth_mode"`
		OpenAIAPIKey *string `json:"OPENAI_API_KEY"`
		Tokens       struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.AuthMode != "chatgpt" {
		t.Fatalf("auth_mode = %q, want chatgpt", payload.AuthMode)
	}
	if payload.OpenAIAPIKey != nil {
		t.Fatalf("OPENAI_API_KEY = %q, want null", *payload.OpenAIAPIKey)
	}
	if payload.Tokens.IDToken != "id_test" || payload.Tokens.AccessToken != "access_test" || payload.Tokens.RefreshToken != "rt_test" || payload.Tokens.AccountID != "account_test" {
		t.Fatalf("tokens = %+v, want seeded credentials", payload.Tokens)
	}
	if payload.LastRefresh == "" {
		t.Fatal("last_refresh is empty")
	}
}

func TestGetAccountAuthJSONRejectsIncompleteTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/accounts/%d/auth-json", accountID), nil)

	handler.GetAccountAuthJSON(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "账号缺少 access_token 或 id_token，请先刷新账号后再生成 auth.json")
}

func TestGetUsageLogsRejectsInvalidAPIKeyID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/usage/logs?start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z&page=1&api_key_id=bad", nil)

	handler.GetUsageLogs(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "api_key_id 参数无效，需要正整数" {
		t.Fatalf("error = %q, want %q", got, "api_key_id 参数无效，需要正整数")
	}
}

func TestUpdateAccountSchedulerRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "bad-id"}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/bad-id/scheduler", http.NoBody)

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "无效的账号 ID")
}

func TestUpdateAccountSchedulerRejectsInvalidBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "1"}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/1/scheduler", strings.NewReader(`{"score_bias_override":"abc"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "score_bias_override 必须是整数或 null")
}

func TestUpdateAccountSchedulerRejectsInvalidAllowedAPIKeyIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	testCases := []struct {
		name    string
		body    string
		message string
	}{
		{
			name:    "invalid type",
			body:    `{"allowed_api_key_ids":"abc"}`,
			message: "allowed_api_key_ids 必须是整数数组或 null",
		},
		{
			name:    "non positive",
			body:    `{"allowed_api_key_ids":[0]}`,
			message: "allowed_api_key_ids 中的值必须是正整数",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
			ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			handler.UpdateAccountScheduler(ctx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			assertErrorMessage(t, recorder, tc.message)
		})
	}
}

func TestUpdateAccountSchedulerRejectsOutOfRangeValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	testCases := []struct {
		name    string
		body    string
		message string
	}{
		{
			name:    "score bias out of range",
			body:    `{"score_bias_override":201}`,
			message: "score_bias_override 超出范围，必须在 -200..200 之间",
		},
		{
			name:    "base concurrency out of range",
			body:    `{"base_concurrency_override":0}`,
			message: "base_concurrency_override 超出范围，必须在 1..50 之间",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
			ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			handler.UpdateAccountScheduler(ctx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			assertErrorMessage(t, recorder, tc.message)
		})
	}
}

func TestUpdateAccountSchedulerPersistsOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":88,"base_concurrency_override":7}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !rows[0].ScoreBiasOverride.Valid || rows[0].ScoreBiasOverride.Int64 != 88 {
		t.Fatalf("score_bias_override = %+v, want 88", rows[0].ScoreBiasOverride)
	}
	if !rows[0].BaseConcurrencyOverride.Valid || rows[0].BaseConcurrencyOverride.Int64 != 7 {
		t.Fatalf("base_concurrency_override = %+v, want 7", rows[0].BaseConcurrencyOverride)
	}
}

func TestUpdateAccountSchedulerPersistsAllowedAPIKeyIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID1 := insertTestAPIKey(t, db, "Team A")
	keyID2 := insertTestAPIKey(t, db, "Team B")
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(fmt.Sprintf(`{"score_bias_override":88,"base_concurrency_override":7,"allowed_api_key_ids":[%d,%d,%d]}`, keyID2, keyID1, keyID2)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if got := rows[0].GetCredentialInt64Slice("allowed_api_key_ids"); len(got) != 2 || got[0] != keyID1 || got[1] != keyID2 {
		t.Fatalf("allowed_api_key_ids = %v, want [%d %d]", got, keyID1, keyID2)
	}
}

func TestUpdateAccountSchedulerResetsToAutoOnNull(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	ctx := context.Background()
	if err := db.UpdateAccountSchedulerConfig(ctx, accountID, database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 20, Valid: true}}, database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 4, Valid: true}}, database.OptionalInt64Slice{}); err != nil {
		t.Fatalf("seed scheduler config: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":null,"base_concurrency_override":null}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].ScoreBiasOverride.Valid {
		t.Fatalf("score_bias_override = %+v, want null", rows[0].ScoreBiasOverride)
	}
	if rows[0].BaseConcurrencyOverride.Valid {
		t.Fatalf("base_concurrency_override = %+v, want null", rows[0].BaseConcurrencyOverride)
	}
}

func TestUpdateAccountSchedulerPartialMetadataPatchPreservesSchedulerConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID := insertTestAPIKey(t, db, "Team A")
	ctx := context.Background()
	if err := db.UpdateAccountSchedulerConfig(ctx, accountID,
		database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 20, Valid: true}},
		database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 4, Valid: true}},
		database.OptionalInt64Slice{Set: true, Values: []int64{keyID}},
	); err != nil {
		t.Fatalf("seed scheduler config: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"tags":["ops"]}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if !rows[0].ScoreBiasOverride.Valid || rows[0].ScoreBiasOverride.Int64 != 20 {
		t.Fatalf("score_bias_override = %+v, want 20", rows[0].ScoreBiasOverride)
	}
	if !rows[0].BaseConcurrencyOverride.Valid || rows[0].BaseConcurrencyOverride.Int64 != 4 {
		t.Fatalf("base_concurrency_override = %+v, want 4", rows[0].BaseConcurrencyOverride)
	}
	if got := rows[0].GetCredentialInt64Slice("allowed_api_key_ids"); len(got) != 1 || got[0] != keyID {
		t.Fatalf("allowed_api_key_ids = %v, want [%d]", got, keyID)
	}
}

func TestUpdateAccountSchedulerClearsAllowedAPIKeyIDsOnNull(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID := insertTestAPIKey(t, db, "Team A")
	if err := db.UpdateCredentials(context.Background(), accountID, map[string]interface{}{
		"allowed_api_key_ids": []int64{keyID},
	}); err != nil {
		t.Fatalf("seed allowed api keys: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":null,"base_concurrency_override":null,"allowed_api_key_ids":null}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if got := rows[0].GetCredentialInt64Slice("allowed_api_key_ids"); len(got) != 0 {
		t.Fatalf("allowed_api_key_ids = %v, want empty", got)
	}
}

func TestUpdateAccountSchedulerKeepsAllowedAPIKeyIDsWhenFieldOmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID := insertTestAPIKey(t, db, "Team A")
	if err := db.UpdateCredentials(context.Background(), accountID, map[string]interface{}{
		"allowed_api_key_ids": []int64{keyID},
	}); err != nil {
		t.Fatalf("seed allowed api keys: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":12,"base_concurrency_override":3}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if got := rows[0].GetCredentialInt64Slice("allowed_api_key_ids"); len(got) != 1 || got[0] != keyID {
		t.Fatalf("allowed_api_key_ids = %v, want [%d]", got, keyID)
	}
}

func TestUpdateAccountSchedulerRejectsMissingAllowedAPIKeyID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"allowed_api_key_ids":[999]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "allowed_api_key_ids 包含不存在的 API Key ID: 999")
}

func TestUpdateAccountSchedulerUpdatesRuntimeOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID1 := insertTestAPIKey(t, db, "Team A")
	keyID2 := insertTestAPIKey(t, db, "Team B")
	runtimeAccount := &auth.Account{
		DBID:        accountID,
		AccessToken: "token",
		Status:      auth.StatusReady,
		PlanType:    "pro",
	}
	store := &auth.Store{}
	store.AddAccount(runtimeAccount)

	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(fmt.Sprintf(`{"score_bias_override":33,"base_concurrency_override":5,"allowed_api_key_ids":[%d,%d]}`, keyID2, keyID1)))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	scoreBias, ok := runtimeAccount.GetScoreBiasOverride()
	if !ok || scoreBias != 33 {
		t.Fatalf("runtime score_bias_override = (%d, %t), want (33, true)", scoreBias, ok)
	}
	baseConcurrency, ok := runtimeAccount.GetBaseConcurrencyOverride()
	if !ok || baseConcurrency != 5 {
		t.Fatalf("runtime base_concurrency_override = (%d, %t), want (5, true)", baseConcurrency, ok)
	}
	if got := runtimeAccount.GetAllowedAPIKeyIDs(); len(got) != 2 || got[0] != keyID1 || got[1] != keyID2 {
		t.Fatalf("runtime allowed_api_key_ids = %v, want [%d %d]", got, keyID1, keyID2)
	}
}

// AT-only 账号(没有 refresh_token,只靠 access_token)是规避 Codex Plus "add
// phone" 流程的常用形态。导出/迁移以前会因为 rt=="" 直接跳过这些账号,导致
// issue #123 中的迁移丢号。下面两个测试保护已修好的过滤逻辑。
func TestExportAccountsIncludesATOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)

	rtID, err := db.InsertAccount(context.Background(), "rt-account", "rt_value", "")
	if err != nil {
		t.Fatalf("insert rt account: %v", err)
	}
	if err := db.UpdateCredentials(context.Background(), rtID, map[string]interface{}{
		"email":        "rt@example.com",
		"access_token": "at_for_rt",
	}); err != nil {
		t.Fatalf("update rt credentials: %v", err)
	}

	atID, err := db.InsertAccount(context.Background(), "at-account", "", "")
	if err != nil {
		t.Fatalf("insert at-only account: %v", err)
	}
	if err := db.UpdateCredentials(context.Background(), atID, map[string]interface{}{
		"email":        "at@example.com",
		"access_token": "at_only_value",
	}); err != nil {
		t.Fatalf("update at-only credentials: %v", err)
	}

	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/export?filter=all", nil)

	handler.ExportAccounts(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var entries []cpaExportEntry
	if err := json.Unmarshal(recorder.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (rt + at-only)", len(entries))
	}

	byEmail := make(map[string]cpaExportEntry, len(entries))
	for _, e := range entries {
		byEmail[e.Email] = e
	}

	rt, ok := byEmail["rt@example.com"]
	if !ok {
		t.Fatal("rt-based account missing from export")
	}
	if rt.RefreshToken != "rt_value" || rt.AccessToken != "at_for_rt" {
		t.Fatalf("rt entry tokens = (rt=%q, at=%q), want (rt_value, at_for_rt)", rt.RefreshToken, rt.AccessToken)
	}

	at, ok := byEmail["at@example.com"]
	if !ok {
		t.Fatal("AT-only account missing from export")
	}
	if at.RefreshToken != "" {
		t.Fatalf("AT-only RefreshToken = %q, want empty", at.RefreshToken)
	}
	if at.AccessToken != "at_only_value" {
		t.Fatalf("AT-only AccessToken = %q, want at_only_value", at.AccessToken)
	}
}

func TestExportAccountsSkipsAccountsWithoutCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)

	if _, err := db.InsertAccount(context.Background(), "empty-account", "", ""); err != nil {
		t.Fatalf("insert empty account: %v", err)
	}

	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/export?filter=all", nil)

	handler.ExportAccounts(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var entries []cpaExportEntry
	if err := json.Unmarshal(recorder.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0 (account has no credentials)", len(entries))
	}
}

func newTestAdminDB(t *testing.T) *database.DB {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "admin-handler-test.sqlite")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("new test db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(dbPath)
	})
	return db
}

func insertTestAccount(t *testing.T, db *database.DB) int64 {
	t.Helper()

	id, err := db.InsertAccount(context.Background(), "test-account", "rt_test", "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

func insertTestAPIKey(t *testing.T, db *database.DB, name string) int64 {
	t.Helper()

	id, err := db.InsertAPIKey(context.Background(), name, fmt.Sprintf("sk-test-%s-1234567890", strings.ToLower(strings.ReplaceAll(name, " ", "-"))))
	if err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return id
}

func assertErrorMessage(t *testing.T, recorder *httptest.ResponseRecorder, want string) {
	t.Helper()

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
