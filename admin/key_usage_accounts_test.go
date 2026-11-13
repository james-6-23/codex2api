package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestPublicKeyUsageInternalErrorDoesNotExposeDetails(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	writePublicKeyUsageInternalError(c, errors.New("postgres://user:secret@internal-host"))
	if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "internal-host") {
		t.Fatalf("unsafe error response: status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestKeyUsageUnloadedAccountPermission(t *testing.T) {
	row := &database.APIKeyRow{ID: 5, AllowedGroupIDs: []int64{11}}
	item := database.KeyUsageAccountAccess{UpstreamType: "", PlanType: "plus", AllowedAPIKeyIDs: []int64{5}}
	if !keyUsageUnloadedAccountAllowed(row, item, []int64{11}) {
		t.Fatal("authorized unloaded account hidden")
	}
	if keyUsageUnloadedAccountAllowed(row, item, []int64{22}) {
		t.Fatal("cross-group unloaded account visible")
	}
	item.AllowedAPIKeyIDs = []int64{6}
	if keyUsageUnloadedAccountAllowed(row, item, []int64{11}) {
		t.Fatal("reverse-restricted unloaded account visible")
	}
	item.AllowedAPIKeyIDs = nil
	row.Limits.UpstreamChannel = database.UpstreamChannelGrok
	if keyUsageUnloadedAccountAllowed(row, item, []int64{11}) {
		t.Fatal("cross-channel unloaded account visible")
	}
	item.UpstreamType = "grok"
	if !keyUsageUnloadedAccountAllowed(row, item, []int64{11}) {
		t.Fatal("allowed channel hidden")
	}
	row.Limits.PlanAllow = []string{"plus"}
	if keyUsageUnloadedAccountAllowed(row, item, []int64{11}) {
		t.Fatal("unknown Grok entitlement should fail closed")
	}
}

func TestCachedResetCooldownDoesNotExtendDatabaseCooldown(t *testing.T) {
	db := newTestAdminDB(t)
	account := &auth.Account{DBID: 999, AccountID: "cached-cooldown", AccessToken: "test"}
	h := &Handler{db: db, store: auth.NewStore(nil, nil, nil)}
	h.markResetCreditSuccess(account)
	outcome, failure := h.consumeResetCreditLocked(context.Background(), account, uuid.NewString(), "auto")
	if failure != nil || !outcome.AlreadyHandled {
		t.Fatalf("outcome=%+v failure=%+v", outcome, failure)
	}
	now := time.Now().Unix()
	if ok, err := db.ReserveKeyUsageResetCooldown(context.Background(), resetCreditDatabaseScope(account), now, now+300); err != nil || !ok {
		t.Fatalf("cached cooldown extended DB cooldown: ok=%v err=%v", ok, err)
	}
}

func TestPublicKeyUsageAccountAuthorizationAndReset(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := newTestAdminDB(t)
	groupA, err := db.CreateAccountGroup(ctx, "group-a", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	groupB, err := db.CreateAccountGroup(ctx, "group-b", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: "user", Key: "sk-test-portal-account-key", AllowedGroupIDs: []int64{groupA}})
	if err != nil {
		t.Fatal(err)
	}
	aID, err := db.InsertAccountWithCredentials(ctx, "allowed", map[string]interface{}{"refresh_token": "secret-refresh-token", "email": "allowed@example.test"}, "http://secret-proxy")
	if err != nil {
		t.Fatal(err)
	}
	bID, err := db.InsertAccount(ctx, "other", "secret-other-token", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountGroups(ctx, aID, []int64{groupA}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountGroups(ctx, bID, []int64{groupB}); err != nil {
		t.Fatal(err)
	}
	unloadedID, err := db.InsertAccount(ctx, "unloaded", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAccountGroups(ctx, unloadedID, []int64{groupA}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	a := &auth.Account{DBID: aID, AccountID: "workspace-a", AccessToken: "secret-access-token", PlanType: "plus"}
	a.SetRateLimitResetCredits(2)
	a.SetUsageSnapshot5hAt(25, time.Now().Add(time.Hour), time.Now())
	b := &auth.Account{DBID: bID, AccountID: "workspace-b", AccessToken: "secret-other-access-token", PlanType: "plus"}
	store.AddAccounts([]*auth.Account{a, b})
	store.ApplyAccountGroupMemberships(map[int64][]int64{aID: {groupA}, bID: {groupB}})
	var calls atomic.Int32
	h := &Handler{db: db, store: store,
		consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			calls.Add(1)
			return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
		},
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	r := gin.New()
	r.GET("/accounts", h.GetPublicKeyUsageAccounts)
	r.POST("/accounts/:id/reset-credits", h.ResetPublicKeyUsageAccountCredits)
	call := func(method, path, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Idempotency-Key", uuid.NewString())
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}
	key := "sk-test-portal-account-key"
	if got := call("GET", "/accounts", key).Code; got != 404 {
		t.Fatalf("default policy status = %d", got)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, 0, database.KeyUsageAccountPermissions{ViewEnabled: true, ResetEnabled: true}); err != nil {
		t.Fatal(err)
	}
	w := call("GET", "/accounts", key)
	if w.Code != 200 {
		t.Fatalf("list status=%d body=%s", w.Code, w.Body.String())
	}
	var result struct {
		Accounts []struct {
			ID           int64    `json:"id"`
			Email        string   `json:"email"`
			Usage        *float64 `json:"usage_percent_5h"`
			Status       string   `json:"status"`
			ResetAllowed bool     `json:"reset_allowed"`
		} `json:"accounts"`
		Groups []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Accounts) != 2 || result.Accounts[0].ID != aID || result.Accounts[0].Email != "allowed@example.test" || result.Accounts[0].Usage == nil || *result.Accounts[0].Usage != 25 || result.Accounts[1].ID != unloadedID || result.Accounts[1].Status != "unavailable" || result.Accounts[1].ResetAllowed {
		t.Fatalf("list=%+v", result)
	}
	if len(result.Groups) != 1 || result.Groups[0].ID != groupA || result.Groups[0].Name != "group-a" {
		t.Fatalf("groups=%+v", result.Groups)
	}
	for _, secret := range []string{"secret-access-token", "secret-refresh-token", "secret-proxy", "secret-other-token", "other"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("unexpected sensitive or out-of-scope field %q", secret)
		}
	}
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", bID), key).Code; got != 404 {
		t.Fatalf("cross-group reset status=%d", got)
	}
	if calls.Load() != 0 {
		t.Fatal("cross-group reset reached upstream")
	}
	store.ApplyAccountAllowedAPIKeys(aID, []int64{keyID + 100})
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", aID), key).Code; got != 404 {
		t.Fatalf("reverse account authorization status=%d", got)
	}
	store.ApplyAccountAllowedAPIKeys(aID, nil)
	// Revocation must take effect before the runtime store has reloaded it.
	if err := db.SetAccountGroups(ctx, aID, []int64{groupB}); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", aID), key).Code; got != 404 {
		t.Fatalf("stale group reset status=%d", got)
	}
	if err := db.SetAccountGroups(ctx, aID, []int64{groupA}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateCredentials(ctx, aID, map[string]interface{}{"allowed_api_key_ids": []int64{keyID + 100}}); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", aID), key).Code; got != 404 {
		t.Fatalf("stale reverse authorization status=%d", got)
	}
	if err := db.UpdateCredentials(ctx, aID, map[string]interface{}{"allowed_api_key_ids": []int64{}}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, keyID, database.KeyUsageAccountPermissions{}); err != nil {
		t.Fatal(err)
	}
	if got := call("GET", "/accounts", key).Code; got != 404 {
		t.Fatalf("per-key denied list status=%d", got)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, keyID, database.KeyUsageAccountPermissions{ViewEnabled: true, ResetEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", aID), key).Code; got != 403 {
		t.Fatalf("per-key denied reset status=%d", got)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, keyID, database.KeyUsageAccountPermissions{ViewEnabled: true, ResetEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", aID), key).Code; got != 200 {
		t.Fatalf("allowed reset status=%d", got)
	}
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", aID), key).Code; got != 409 {
		t.Fatalf("repeat reset status=%d", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d", calls.Load())
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, 0, database.KeyUsageAccountPermissions{}); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", aID), key).Code; got != 403 {
		t.Fatalf("global denied status=%d", got)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, 0, database.KeyUsageAccountPermissions{ViewEnabled: true, ResetEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAPIKey(ctx, keyID, database.APIKeyUpdate{ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true}, ExpiresAtSet: true}); err != nil {
		t.Fatal(err)
	}
	if got := call("POST", fmt.Sprintf("/accounts/%d/reset-credits", aID), key).Code; got != 403 {
		t.Fatalf("expired key reset status=%d", got)
	}
}

func TestPublicResetAcrossInstancesLostResponseFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := newTestAdminDB(t)
	key := "sk-reset-test-multi-instance"
	if _, err := db.InsertAPIKey(ctx, "user", key); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertAccount(ctx, "account", "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, 0, database.KeyUsageAccountPermissions{ViewEnabled: true, ResetEnabled: true}); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	newHandler := func() *Handler {
		store := auth.NewStore(nil, nil, nil)
		acc := &auth.Account{DBID: id, AccountID: "shared-workspace", AccessToken: "secret"}
		acc.SetRateLimitResetCredits(3)
		store.AddAccount(acc)
		return &Handler{db: db, store: store, consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
			calls.Add(1)
			close(entered)
			<-release
			return nil, nil, fmt.Errorf("synthetic lost response")
		}}
	}
	h1, h2 := newHandler(), newHandler()
	newRouter := func(h *Handler) *gin.Engine {
		r := gin.New()
		r.POST("/accounts/:id/reset-credits", h.ResetPublicKeyUsageAccountCredits)
		return r
	}
	r1, r2 := newRouter(h1), newRouter(h2)
	call := func(r *gin.Engine, requestID string) int {
		req := httptest.NewRequest("POST", fmt.Sprintf("/accounts/%d/reset-credits", id), nil)
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Idempotency-Key", requestID)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	firstID := uuid.NewString()
	done := make(chan int, 1)
	go func() { done <- call(r1, firstID) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first consume did not start")
	}
	if got := call(r2, uuid.NewString()); got != 409 {
		t.Fatalf("concurrent instance status=%d", got)
	}
	close(release)
	if got := <-done; got != 502 {
		t.Fatalf("lost response status=%d", got)
	}
	scope := resetCreditDatabaseScope(h2.store.FindByID(id))
	if uncertain, err := db.KeyUsageResetUncertain(ctx, scope); err != nil || !uncertain {
		t.Fatalf("uncertain=%v err=%v", uncertain, err)
	}
	if got := call(r2, uuid.NewString()); got != 409 {
		t.Fatalf("cooldown status=%d", got)
	}
	if got := call(r2, firstID); got != 409 {
		t.Fatalf("idempotent replay status=%d", got)
	}
	// Expire the ordinary cooldown: uncertainty must still fence automatic work.
	now := time.Now().Unix()
	if ok, err := db.ReserveKeyUsageResetCooldown(ctx, scope, now+360, now-1); err != nil || !ok {
		t.Fatalf("expire cooldown: ok=%v err=%v", ok, err)
	}
	outcome, failure := h2.consumeResetCreditLocked(ctx, h2.store.FindByID(id), uuid.NewString(), "auto")
	if failure != nil || !outcome.AlreadyHandled {
		t.Fatalf("automatic retry after ambiguous result: outcome=%+v failure=%+v", outcome, failure)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls=%d, want 1", calls.Load())
	}
	adminRouter := gin.New()
	adminRouter.DELETE("/uncertain/:id", h2.ClearKeyUsageResetUncertain)
	recorder := httptest.NewRecorder()
	adminRouter.ServeHTTP(recorder, httptest.NewRequest("DELETE", fmt.Sprintf("/uncertain/%d", id), nil))
	if recorder.Code != 200 {
		t.Fatalf("admin reconcile status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if uncertain, err := db.KeyUsageResetUncertain(ctx, scope); err != nil || uncertain {
		t.Fatalf("reconciled uncertain=%v err=%v", uncertain, err)
	}
	// A prior automatic/manual attempt on another instance must fence a new
	// public action even after explicit uncertainty reconciliation.
	if err := db.RecordKeyUsageResetCooldown(ctx, scope, time.Now().Add(5*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if got := call(r2, uuid.NewString()); got != 409 {
		t.Fatalf("prior automatic reset status=%d", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls after cooldown=%d", calls.Load())
	}
}

func TestAutomaticLostResetResponseBlocksPublicAfterCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	db := newTestAdminDB(t)
	key := "sk-reset-test-auto-first"
	if _, err := db.InsertAPIKey(ctx, "user", key); err != nil {
		t.Fatal(err)
	}
	id, err := db.InsertAccount(ctx, "account", "secret", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, 0, database.KeyUsageAccountPermissions{ViewEnabled: true, ResetEnabled: true}); err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: id, AccountID: "auto-workspace", AccessToken: "secret"}
	account.SetRateLimitResetCredits(3)
	store.AddAccount(account)
	var calls atomic.Int32
	autoID := uuid.NewString()
	h := &Handler{db: db, store: store, probeUsage: func(context.Context, *auth.Account) error { return nil }, consumeResetCredit: func(_ context.Context, _ *auth.Account, _ string, requestID string) (*proxy.WhamResetResult, *http.Response, error) {
		if requestID != autoID {
			t.Errorf("unexpected auto id %s", requestID)
		}
		if calls.Add(1) == 1 {
			return nil, nil, fmt.Errorf("synthetic lost response")
		}
		return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
	}}
	if _, failure := h.consumeResetCreditLocked(ctx, account, autoID, "auto"); failure == nil {
		t.Fatal("expected lost response")
	}
	scope := resetCreditDatabaseScope(account)
	if uncertain, err := db.KeyUsageResetUncertain(ctx, scope); err != nil || !uncertain {
		t.Fatalf("uncertain=%v err=%v", uncertain, err)
	}
	now := time.Now().Unix()
	if ok, err := db.ReserveKeyUsageResetCooldown(ctx, scope, now+360, now-1); err != nil || !ok {
		t.Fatalf("expire cooldown: ok=%v err=%v", ok, err)
	}
	r := gin.New()
	r.POST("/accounts/:id/reset-credits", h.ResetPublicKeyUsageAccountCredits)
	req := httptest.NewRequest("POST", fmt.Sprintf("/accounts/%d/reset-credits", id), nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != 409 || calls.Load() != 1 {
		t.Fatalf("public reset status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
	if outcome, failure := h.consumeResetCreditLocked(ctx, account, uuid.NewString(), "auto"); failure != nil || !outcome.AlreadyHandled {
		t.Fatalf("different auto id: outcome=%+v failure=%+v", outcome, failure)
	}
	if outcome, failure := h.consumeResetCreditLocked(ctx, account, autoID, "auto"); failure != nil || outcome.AlreadyHandled || calls.Load() != 2 {
		t.Fatalf("same auto id retry: outcome=%+v failure=%+v calls=%d", outcome, failure, calls.Load())
	}
	if uncertain, err := db.KeyUsageResetUncertain(ctx, scope); err != nil || uncertain {
		t.Fatalf("successful retry uncertain=%v err=%v", uncertain, err)
	}
}
