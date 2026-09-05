package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func patchAccountScheduler(t *testing.T, handler *Handler, accountID int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID),
		strings.NewReader(body),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAccountScheduler(ctx)
	return recorder
}

func accountFingerprintCredential(t *testing.T, db *database.DB, accountID int64) string {
	t.Helper()
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	return row.GetCredential(auth.CodexFingerprintModeCredentialKey)
}

func TestUpdateAccountSchedulerPersistsCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	// 未配置时默认 off，出站行为与升级前一致。
	if got := accountFingerprintCredential(t, db, accountID); got != "" {
		t.Fatalf("initial credential = %q, want empty", got)
	}

	recorder := patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":"session"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := accountFingerprintCredential(t, db, accountID); got != auth.CodexFingerprintModeSession {
		t.Fatalf("credential = %q, want %q", got, auth.CodexFingerprintModeSession)
	}

	// null 表示重置为默认档 off。
	recorder = patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":null}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := accountFingerprintCredential(t, db, accountID); got != auth.CodexFingerprintModeOff {
		t.Fatalf("credential after reset = %q, want %q", got, auth.CodexFingerprintModeOff)
	}
}

func TestUpdateAccountSchedulerRejectsInvalidCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":"converge"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if got := accountFingerprintCredential(t, db, accountID); got != "" {
		t.Fatalf("credential = %q, want the rejected update to leave it untouched", got)
	}
}

// TestUpdateAccountSchedulerSyncsRuntimeCodexFingerprintMode 验证改动立即对运行时账号
// 生效，不必等下一次全量重载。
func TestUpdateAccountSchedulerSyncsRuntimeCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	if err := store.LoadAccountByID(context.Background(), accountID); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	handler := &Handler{db: db, store: store}

	runtime := store.FindByID(accountID)
	if runtime == nil {
		t.Fatal("runtime account not loaded")
	}
	if got := runtime.EffectiveCodexFingerprintMode(); got != auth.CodexFingerprintModeOff {
		t.Fatalf("runtime mode = %q, want %q before any update", got, auth.CodexFingerprintModeOff)
	}

	recorder := patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":"device"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := runtime.EffectiveCodexFingerprintMode(); got != auth.CodexFingerprintModeDevice {
		t.Fatalf("runtime mode = %q, want %q", got, auth.CodexFingerprintModeDevice)
	}
}

func TestAccountResponseExposesCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	if recorder := patchAccountScheduler(t, handler, accountID, `{"codex_fingerprint_mode":"full"}`); recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}

	detailed := handler.buildAccountResponse(row, nil, nil, nil, nil, true)
	if detailed.CodexFingerprintMode != auth.CodexFingerprintModeFull {
		t.Fatalf("detailed mode = %q, want %q", detailed.CodexFingerprintMode, auth.CodexFingerprintModeFull)
	}

	// 摘要响应也要带上指纹模式，供列表和快捷抽屉渲染。
	summary := handler.buildAccountResponse(row, nil, nil, nil, nil, false)
	if summary.CodexFingerprintMode != auth.CodexFingerprintModeFull {
		t.Fatalf("summary mode = %q, want %q", summary.CodexFingerprintMode, auth.CodexFingerprintModeFull)
	}
}

// TestAccountResponseOmitsCodexFingerprintModeForRelayAccounts 验证中转账号不暴露该字段：
// 它们不走 Codex 官方出站路径，收敛对其无意义。
func TestAccountResponseOmitsCodexFingerprintModeForRelayAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	accountID, err := db.InsertAccountWithCredentials(context.Background(), "relay", map[string]interface{}{
		"upstream_type": auth.UpstreamOpenAIResponses,
		"base_url":      "https://relay.example.com",
		"api_key":       "relay-token",
		auth.CodexFingerprintModeCredentialKey: auth.CodexFingerprintModeFull,
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	resp := handler.buildAccountResponse(row, nil, nil, nil, nil, true)
	if resp.CodexFingerprintMode != "" {
		t.Fatalf("relay account mode = %q, want empty", resp.CodexFingerprintMode)
	}
}

// TestUpdateSettingsPersistsCodexFingerprintDefaultMode 验证系统设置里的
// 「新账号默认指纹收敛档位」经 PUT /settings 保存后：响应回显、落库、运行时 Store 三处一致。
func TestUpdateSettingsPersistsCodexFingerprintDefaultMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/admin/settings",
		strings.NewReader(`{"codex_fingerprint_default_mode":"session"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response settingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.CodexFingerprintDefaultMode != auth.CodexFingerprintModeSession {
		t.Fatalf("response mode = %q, want %q", response.CodexFingerprintDefaultMode, auth.CodexFingerprintModeSession)
	}
	if got := store.GetCodexFingerprintDefaultMode(); got != auth.CodexFingerprintModeSession {
		t.Fatalf("store mode = %q, want %q", got, auth.CodexFingerprintModeSession)
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if persisted.CodexFingerprintDefaultMode != auth.CodexFingerprintModeSession {
		t.Fatalf("persisted mode = %q, want %q", persisted.CodexFingerprintDefaultMode, auth.CodexFingerprintModeSession)
	}

	// 非法档位必须整体拒绝，不落任何变更。
	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/admin/settings",
		strings.NewReader(`{"codex_fingerprint_default_mode":"converge"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("invalid mode status = %d, want %d body=%s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
	if got := store.GetCodexFingerprintDefaultMode(); got != auth.CodexFingerprintModeSession {
		t.Fatalf("store mode after rejected update = %q, want %q", got, auth.CodexFingerprintModeSession)
	}
}

// TestNewCodexAccountCredentialsStampsDefaultFingerprintMode 验证新建账号的
// credentials 与内存态都盖上系统默认档位；tokenCredentialMap（更新已有账号的路径）
// 永远不带该键，避免重新导入覆盖用户手动调整过的档位。
func TestNewCodexAccountCredentialsStampsDefaultFingerprintMode(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}
	seed := tokenCredentialSeed{refreshToken: "rt-stamp"}

	// 默认 off：不写入键，与升级前行为完全一致。
	if _, ok := handler.newCodexAccountCredentials(&seed)[auth.CodexFingerprintModeCredentialKey]; ok {
		t.Fatal("默认 off 时不应写入 codex_fingerprint_mode 键")
	}

	store.SetCodexFingerprintDefaultMode(auth.CodexFingerprintModeSession)
	credentials := handler.newCodexAccountCredentials(&seed)
	if got := credentials[auth.CodexFingerprintModeCredentialKey]; got != auth.CodexFingerprintModeSession {
		t.Fatalf("credential = %v, want %q", got, auth.CodexFingerprintModeSession)
	}
	account := handler.newCodexAccountFromSeed(7, "", seed)
	if got := account.EffectiveCodexFingerprintMode(); got != auth.CodexFingerprintModeSession {
		t.Fatalf("runtime mode = %q, want %q", got, auth.CodexFingerprintModeSession)
	}

	if _, ok := tokenCredentialMap(seed)[auth.CodexFingerprintModeCredentialKey]; ok {
		t.Fatal("tokenCredentialMap 不应携带 codex_fingerprint_mode，更新路径会覆盖已有账号档位")
	}
}

func TestBatchUpdateAccountsAppliesCodexFingerprintMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	first := insertTestAccount(t, db)
	second, err := db.InsertAccountWithCredentials(context.Background(), "second", map[string]interface{}{
		"refresh_token": "rt-second",
		"email":         "second@example.com",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	store := auth.NewStore(db, nil, nil)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api/admin/accounts/batch",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d],"codex_fingerprint_mode":"session"}`, first, second)),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.BatchUpdateAccounts(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	for _, id := range []int64{first, second} {
		if got := accountFingerprintCredential(t, db, id); got != auth.CodexFingerprintModeSession {
			t.Fatalf("account %d credential = %q, want %q", id, got, auth.CodexFingerprintModeSession)
		}
	}
}
