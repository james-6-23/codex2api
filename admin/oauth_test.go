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
		if "codex-cli/0.91.0" == r.Header.Get("User-Agent") {
			t.Error("token exchange still sends the obsolete hard-coded User-Agent")
		}
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

func TestUpsertOAuthIdentitySeparatesEffectiveWorkspaceRoutes(t *testing.T) {
	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	ctx := context.Background()
	idToken := makeOAuthTestIDToken("user@example.com", "personal-workspace", "team")

	personalID, updated, err := handler.upsertOAuthIdentityAccount(ctx, "personal", "", tokenCredentialSeed{
		accessToken: "personal-token",
		idToken:     idToken,
	}, "test")
	if err != nil {
		t.Fatalf("upsert personal: %v", err)
	}
	if updated {
		t.Fatal("personal route unexpectedly updated an existing account")
	}

	teamSeed := tokenCredentialSeed{
		accessToken: "shared-team-token",
		idToken:     idToken,
		customHeaders: map[string]string{
			"Chatgpt-Account-Id": "team-workspace",
		},
	}
	teamID, updated, err := handler.upsertOAuthIdentityAccount(ctx, "team", "", teamSeed, "test")
	if err != nil {
		t.Fatalf("upsert team: %v", err)
	}
	if updated {
		t.Fatal("team route unexpectedly updated the personal route")
	}
	if teamID == personalID {
		t.Fatalf("team route id = personal route id = %d", teamID)
	}

	teamSeed.accessToken = "rotated-team-token"
	gotID, updated, err := handler.upsertOAuthIdentityAccount(ctx, "team-again", "", teamSeed, "test")
	if err != nil {
		t.Fatalf("upsert same team route: %v", err)
	}
	if !updated || gotID != teamID {
		t.Fatalf("same team route = id:%d updated:%t, want id:%d updated:true", gotID, updated, teamID)
	}

	personalRow, err := db.GetAccountByID(ctx, personalID)
	if err != nil {
		t.Fatalf("GetAccountByID personal: %v", err)
	}
	if got := personalRow.GetCredentialStringMap("custom_headers")["Chatgpt-Account-Id"]; got != "" {
		t.Fatalf("personal route override = %q, want empty", got)
	}
	if got := personalRow.GetCredential("access_token"); got != "personal-token" {
		t.Fatalf("personal access token = %q, want personal-token", got)
	}
	teamRow, err := db.GetAccountByID(ctx, teamID)
	if err != nil {
		t.Fatalf("GetAccountByID team: %v", err)
	}
	if got := teamRow.GetCredentialStringMap("custom_headers")["Chatgpt-Account-Id"]; got != "team-workspace" {
		t.Fatalf("team route override = %q, want team-workspace", got)
	}
	if got := teamRow.GetCredential("access_token"); got != "rotated-team-token" {
		t.Fatalf("team access token = %q, want rotated-team-token", got)
	}
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
		"workspace_id":  "acc-duplicate",
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
		"workspace_id":  "acc-target",
	}, "")
	if err != nil {
		t.Fatalf("Insert target: %v", err)
	}
	duplicateID, err := db.InsertAccountWithCredentials(context.Background(), "duplicate", map[string]interface{}{
		"refresh_token": "duplicate-refresh",
		"email":         "duplicate@example.com",
		"account_id":    "acc-duplicate",
		"workspace_id":  "acc-duplicate",
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
		"workspace_id":  "acc-banned",
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
		workspaceID: "acc-banned",
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

// 重新授权(直接对已有账号 exchange-code)拿到新凭证后必须清除 error/401
// 状态,与重新导入合并路径对齐;否则账号一直挂"异常"等一次可能失败的
// 异步探针(issue #493)。限流冷却不受影响。
func TestUpdateOAuthAccountCodeClearsErrorState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, cache.NewMemory(1), nil)
	handler := &Handler{db: db, store: store}
	handler.probeUsage = func(_ context.Context, _ *auth.Account) error { return nil }

	newOAuthExchangeTestServer(t)

	id := insertOAuthEditTestAccount(t, db, "oauth-errored", "old-refresh", "")
	if err := db.SetError(context.Background(), id, "401 unauthorized"); err != nil {
		t.Fatalf("SetError: %v", err)
	}
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}

	sessionID := "oauth-edit-clear-error-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:        "state-clear-error",
		CodeVerifier: "verifier-clear-error",
		RedirectURI:  oauthDefaultRedirectURI,
		CreatedAt:    time.Now(),
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = newOAuthEditRequest(sessionID, "code-clear", "state-clear-error", "")

	handler.UpdateOAuthAccountCode(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if !strings.EqualFold(row.Status, "active") || row.ErrorMessage != "" {
		t.Fatalf("status=%q error=%q, want active with empty error", row.Status, row.ErrorMessage)
	}
}

func TestOAuthCallbackPageEscapesHTML(t *testing.T) {
	page := oauthCallbackPage(`<script>alert(1)</script>`, `账号 <img src=x onerror=alert(1)>`, false)
	if strings.Contains(page, "<script>") || strings.Contains(page, "<img") {
		t.Fatalf("callback page contains unescaped HTML: %s", page)
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatalf("callback page did not escape the title: %s", page)
	}
}

func TestOAuthSessionStoreFindByState(t *testing.T) {
	store := &oauthSessionStore{sessions: make(map[string]*oauthSession)}
	store.set("id-1", &oauthSession{State: "state-1", CreatedAt: time.Now()})
	store.set("id-2", &oauthSession{State: "state-2", CreatedAt: time.Now()})

	id, sess, ok := store.findByState("state-2")
	if !ok || id != "id-2" || sess.State != "state-2" {
		t.Fatalf("findByState = (%q, %v, %v), want id-2/state-2", id, sess, ok)
	}
	if _, _, ok := store.findByState("missing"); ok {
		t.Fatal("unknown state should not match")
	}
	store.set("expired", &oauthSession{State: "state-expired", CreatedAt: time.Now().Add(-2 * oauthSessionTTL)})
	if _, _, ok := store.findByState("state-expired"); ok {
		t.Fatal("expired session should not match")
	}
}

func TestOAuthCallbackRejectsCompletedSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const sessionID = "replay-test-session"
	globalOAuthStore.set(sessionID, &oauthSession{
		State:          "state-replay",
		CreatedAt:      time.Now(),
		ExchangeResult: &oauthExchangeResult{Success: true},
	})
	t.Cleanup(func() { globalOAuthStore.delete(sessionID) })

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/auth/callback?code=replayed&state=state-replay", nil)
	(&Handler{}).OAuthCallback(c)

	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "已处理") {
		t.Fatalf("replayed callback = %d %s, want 400 with 已处理", recorder.Code, recorder.Body.String())
	}
}
