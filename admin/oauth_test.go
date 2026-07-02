package admin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestExchangeOAuthCodeSeedsAccessTokenFromExchangeResponse(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token": "access-from-exchange",
			"refresh_token": "refresh-from-exchange",
			"id_token": "id-from-exchange",
			"expires_in": 3600
		}`))
	}))
	defer server.Close()

	oldResinCfg := proxy.GetResinConfig()
	oldDecorator := auth.ResinRequestDecorator
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex2api"})
	t.Cleanup(func() {
		proxy.SetResinConfig(oldResinCfg)
		auth.ResinRequestDecorator = oldDecorator
	})

	sessionID := "oauth-test-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-test",
		CodeVerifier: "verifier-test",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() {
		globalOAuthStore.delete(sessionID)
	})

	body := `{"session_id":"oauth-test-session","code":"code-test","state":"state-test"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/oauth/exchange-code", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.ExchangeOAuthCode(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.ID == 0 {
		t.Fatal("response id is empty")
	}

	account := store.FindByID(payload.ID)
	if account == nil {
		t.Fatalf("runtime account %d not found", payload.ID)
	}
	account.Mu().RLock()
	accessToken := account.AccessToken
	refreshToken := account.RefreshToken
	account.Mu().RUnlock()
	if accessToken != "access-from-exchange" || refreshToken != "refresh-from-exchange" {
		t.Fatalf("runtime tokens = access:%q refresh:%q, want exchange tokens", accessToken, refreshToken)
	}

	row, err := db.GetAccountByID(context.Background(), payload.ID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("access_token"); got != "access-from-exchange" {
		t.Fatalf("stored access_token = %q, want exchange access token", got)
	}
	if got := row.GetCredential("id_token"); got != "id-from-exchange" {
		t.Fatalf("stored id_token = %q, want exchange id token", got)
	}
}

func newOAuthExchangeTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newOAuthExchangeTestServerWithIDToken(t, "id-from-exchange")
}

func newOAuthExchangeTestServerWithIDToken(t *testing.T, idToken string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"access_token":  "access-from-exchange",
			"refresh_token": "refresh-from-exchange",
			"id_token":      idToken,
			"expires_in":    3600,
		})
	}))
	t.Cleanup(server.Close)

	oldResinCfg := proxy.GetResinConfig()
	oldDecorator := auth.ResinRequestDecorator
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex2api"})
	t.Cleanup(func() {
		proxy.SetResinConfig(oldResinCfg)
		auth.ResinRequestDecorator = oldDecorator
	})
	return server
}

func makeOAuthTestIDToken(email, accountID, planType string) string {
	payload, _ := json.Marshal(map[string]interface{}{
		"email": email,
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  planType,
		},
	})
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".fake_signature"
}

func TestExchangeOAuthCodeTriggersUsageProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	probed := make(chan int64, 1)
	handler := &Handler{db: db, store: store}
	handler.probeUsage = func(_ context.Context, account *auth.Account) error {
		probed <- account.DBID
		return nil
	}

	newOAuthExchangeTestServer(t)

	sessionID := "oauth-probe-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-probe",
		CodeVerifier: "verifier-probe",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() {
		globalOAuthStore.delete(sessionID)
	})

	body := `{"session_id":"oauth-probe-session","code":"code-probe","state":"state-probe"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/oauth/exchange-code", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.ExchangeOAuthCode(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	select {
	case dbID := <-probed:
		if dbID != payload.ID {
			t.Fatalf("usage probe ran for account %d, want %d", dbID, payload.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("usage probe was not triggered after OAuth account add")
	}
}

func TestExchangeOAuthCodeUpdatesDuplicateOAuthIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	existingID, err := db.InsertAccountWithCredentials(context.Background(), "existing", map[string]interface{}{
		"refresh_token": "existing-refresh",
		"email":         "duplicate@example.com",
		"account_id":    "acc-duplicate",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	newOAuthExchangeTestServerWithIDToken(t, makeOAuthTestIDToken("Duplicate@Example.com", "acc-duplicate", "team"))

	sessionID := "oauth-duplicate-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-duplicate",
		CodeVerifier: "verifier-duplicate",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() {
		globalOAuthStore.delete(sessionID)
	})

	body := `{"session_id":"oauth-duplicate-session","code":"code-duplicate","state":"state-duplicate"}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/oauth/exchange-code", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.ExchangeOAuthCode(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var resp struct {
		ID      int64 `json:"id"`
		Updated bool  `json:"updated"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ID != existingID {
		t.Fatalf("response id = %d, want %d", resp.ID, existingID)
	}
	if !resp.Updated {
		t.Fatal("response updated = false, want true")
	}
	count, err := db.CountAll(context.Background())
	if err != nil {
		t.Fatalf("CountAll: %v", err)
	}
	if count != 1 {
		t.Fatalf("account count = %d, want 1", count)
	}
	row, err := db.GetAccountByID(context.Background(), existingID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "refresh-from-exchange" {
		t.Fatalf("refresh_token = %q, want updated exchange token", got)
	}
	if account := store.FindByID(existingID); account == nil {
		t.Fatalf("runtime account %d not found after update", existingID)
	}
}

func TestOAuthCallbackTriggersUsageProbe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	probed := make(chan int64, 1)
	handler := &Handler{db: db, store: store}
	handler.probeUsage = func(_ context.Context, account *auth.Account) error {
		probed <- account.DBID
		return nil
	}

	newOAuthExchangeTestServer(t)

	sessionID := "oauth-callback-probe-session"
	sess := &oauthSession{
		State:        "state-callback-probe",
		CodeVerifier: "verifier-callback-probe",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	}
	globalOAuthStore.set(sessionID, sess)
	t.Cleanup(func() {
		globalOAuthStore.delete(sessionID)
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/oauth/callback?code=code-callback-probe&state=state-callback-probe", nil)

	handler.OAuthCallback(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if sess.ExchangeResult == nil || !sess.ExchangeResult.Success {
		t.Fatalf("exchange result = %+v, want success", sess.ExchangeResult)
	}

	select {
	case dbID := <-probed:
		if dbID != sess.ExchangeResult.ID {
			t.Fatalf("usage probe ran for account %d, want %d", dbID, sess.ExchangeResult.ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("usage probe was not triggered after OAuth callback account add")
	}
}

func insertOAuthEditTestAccount(t *testing.T, db *database.DB, name string, refreshToken string, proxyURL string) int64 {
	t.Helper()
	id, err := db.InsertAccount(context.Background(), name, refreshToken, proxyURL)
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	return id
}

func newOAuthEditRequest(sessionID, code, state, proxyURL string) *http.Request {
	body := fmt.Sprintf(`{"session_id":%q,"code":%q,"state":%q,"proxy_url":%q}`, sessionID, code, state, proxyURL)
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/oauth/exchange-code", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestUpdateOAuthAccountCodeRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "bad"}}
	ctx.Request = newOAuthEditRequest("session", "code", "state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsMissingFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}
	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/1/oauth/exchange-code", strings.NewReader(`{"session_id":"","code":"code","state":"state"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsMissingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "999999"}}
	ctx.Request = newOAuthEditRequest("session", "code", "state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusNotFound, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsNonOAuthAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	id, err := db.InsertOpenAIResponsesAccount(context.Background(), "responses", map[string]interface{}{
		"upstream_type": auth.UpstreamOpenAIResponses,
		"base_url":      "https://api.openai.com",
		"api_key":       "sk-test",
		"models":        []string{"gpt-4.1"},
		"plan_type":     "api",
		"email":         "https://api.openai.com",
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest("session", "code", "state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsMissingSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}
	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest("missing-session", "code", "state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeRejectsStateMismatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}
	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "")

	sessionID := "oauth-edit-state-mismatch"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "expected-state",
		CodeVerifier: "verifier-test",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest(sessionID, "code", "wrong-state", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadRequest, recorder.Body.String())
	}
}

func TestUpdateOAuthAccountCodeDoesNotExposeTokenErrorBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","refresh_token":"secret-refresh","access_token":"secret-access","id_token":"secret-id"}`))
	}))
	t.Cleanup(server.Close)

	oldResinCfg := proxy.GetResinConfig()
	oldDecorator := auth.ResinRequestDecorator
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "codex2api"})
	t.Cleanup(func() {
		proxy.SetResinConfig(oldResinCfg)
		auth.ResinRequestDecorator = oldDecorator
	})

	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "")
	sessionID := "oauth-edit-token-error"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-token-error",
		CodeVerifier: "verifier-token-error",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest(sessionID, "code-token-error", "state-token-error", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, leaked := range []string{"secret-refresh", "secret-access", "secret-id", "invalid_grant"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("response leaked token exchange error body value %q: %s", leaked, body)
		}
	}
	if !strings.Contains(body, "token 兑换失败 (HTTP 400)") {
		t.Fatalf("response = %s, want sanitized HTTP status error", body)
	}
}

func TestUpdateOAuthAccountCodeRejectsDuplicateOAuthIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	targetID, err := db.InsertAccountWithCredentials(context.Background(), "target", map[string]interface{}{
		"refresh_token": "target-refresh",
		"email":         "target@example.com",
		"account_id":    "acc-target",
	}, "")
	if err != nil {
		t.Fatalf("Insert target: %v", err)
	}
	duplicateID, err := db.InsertAccountWithCredentials(context.Background(), "duplicate", map[string]interface{}{
		"refresh_token": "duplicate-refresh",
		"email":         "duplicate@example.com",
		"account_id":    "acc-duplicate",
	}, "")
	if err != nil {
		t.Fatalf("Insert duplicate: %v", err)
	}
	if err := store.LoadAccountByID(context.Background(), targetID); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}

	newOAuthExchangeTestServerWithIDToken(t, makeOAuthTestIDToken("duplicate@example.com", "acc-duplicate", "team"))

	sessionID := "oauth-edit-duplicate-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-edit-duplicate",
		CodeVerifier: "verifier-edit-duplicate",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", targetID)}}
	ctx.Request = newOAuthEditRequest(sessionID, "code-edit-duplicate", "state-edit-duplicate", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	var errResp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(errResp.Error, fmt.Sprintf("%d", duplicateID)) {
		t.Fatalf("error = %q, want duplicate account id %d", errResp.Error, duplicateID)
	}
	row, err := db.GetAccountByID(context.Background(), targetID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "target-refresh" {
		t.Fatalf("target refresh_token = %q, want unchanged target-refresh", got)
	}
}

func TestUpdateOAuthAccountCodeUpdatesExistingAccountInPlace(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	probed := make(chan int64, 1)
	handler := &Handler{db: db, store: store}
	handler.probeUsage = func(_ context.Context, account *auth.Account) error {
		probed <- account.DBID
		return nil
	}

	newOAuthExchangeTestServer(t)

	id := insertOAuthEditTestAccount(t, db, "oauth-existing", "old-refresh", "http://old-proxy.example")
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	beforeCount, err := db.CountAll(context.Background())
	if err != nil {
		t.Fatalf("CountAll before: %v", err)
	}

	sessionID := "oauth-edit-success-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-success",
		CodeVerifier: "verifier-success",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest(sessionID, "code-success", "state-success", "http://new-proxy.example")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	afterCount, err := db.CountAll(context.Background())
	if err != nil {
		t.Fatalf("CountAll after: %v", err)
	}
	if afterCount != beforeCount {
		t.Fatalf("account count = %d, want unchanged %d", afterCount, beforeCount)
	}

	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "refresh-from-exchange" {
		t.Fatalf("stored refresh_token = %q, want exchange refresh token", got)
	}
	if got := row.GetCredential("access_token"); got != "access-from-exchange" {
		t.Fatalf("stored access_token = %q, want exchange access token", got)
	}
	if got := row.GetCredential("id_token"); got != "id-from-exchange" {
		t.Fatalf("stored id_token = %q, want exchange id token", got)
	}
	if row.ProxyURL != "http://new-proxy.example" {
		t.Fatalf("proxy_url = %q, want new proxy", row.ProxyURL)
	}

	account := store.FindByID(id)
	if account == nil {
		t.Fatalf("runtime account %d not found", id)
	}
	account.Mu().RLock()
	accessToken := account.AccessToken
	refreshToken := account.RefreshToken
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if accessToken != "access-from-exchange" || refreshToken != "refresh-from-exchange" {
		t.Fatalf("runtime tokens = access:%q refresh:%q, want exchange tokens", accessToken, refreshToken)
	}
	if proxyURL != "http://new-proxy.example" {
		t.Fatalf("runtime proxy = %q, want new proxy", proxyURL)
	}

	select {
	case dbID := <-probed:
		if dbID != id {
			t.Fatalf("usage probe ran for account %d, want %d", dbID, id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("usage probe was not triggered after OAuth account update")
	}
}

// TestUpsertOAuthIdentityAccountClearsBanOnReimport 验证：当一个账号此前被封禁
// （401 unauthorized）后，重新导入同一身份的有效凭证会清除封禁/错误状态，
// 使账号脱离 banned 并重新可用（具体可用性仍交由后续 probe 判定）。
func TestUpsertOAuthIdentityAccountClearsBanOnReimport(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}

	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "banned", map[string]interface{}{
		"refresh_token": "old-refresh",
		"access_token":  "old-access",
		"email":         "banned@example.com",
		"account_id":    "acc-banned",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	if err := store.LoadAccountByID(ctx, id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	acc := store.FindByID(id)
	if acc == nil {
		t.Fatalf("runtime account %d not found", id)
	}
	// 模拟上游 401 封禁
	store.MarkCooldownWithError(acc, time.Hour, "unauthorized", "401 unauthorized")
	acc.Mu().RLock()
	bannedTier := acc.HealthTier
	acc.Mu().RUnlock()
	if bannedTier != auth.HealthTierBanned {
		t.Fatalf("precondition: HealthTier = %q, want banned", bannedTier)
	}

	// 重新导入同一身份的有效凭证（新的 access token）
	seed := tokenCredentialSeed{
		accessToken: "fresh-access",
		email:       "banned@example.com",
		accountID:   "acc-banned",
	}
	newID, updated, err := handler.upsertOAuthIdentityAccount(ctx, "banned", "", seed, "manual_at")
	if err != nil {
		t.Fatalf("upsertOAuthIdentityAccount: %v", err)
	}
	if newID != id {
		t.Fatalf("upsert created new account %d, want update of %d (dedup failed)", newID, id)
	}
	if !updated {
		t.Fatal("upsert reported insert, want update of existing banned account")
	}

	// DB 错误/封禁态应已被清除
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.Status == "error" {
		t.Fatalf("DB status = %q, want cleared", row.Status)
	}
	if strings.EqualFold(strings.TrimSpace(row.CooldownReason), "unauthorized") {
		t.Fatalf("DB cooldown_reason = %q, want cleared", row.CooldownReason)
	}

	// 运行时账号应脱离 banned
	acc = store.FindByID(id)
	if acc == nil {
		t.Fatalf("runtime account %d missing after reimport", id)
	}
	acc.Mu().RLock()
	tier := acc.HealthTier
	acc.Mu().RUnlock()
	if tier == auth.HealthTierBanned {
		t.Fatalf("runtime HealthTier = %q, want not banned after reimport", tier)
	}
}
