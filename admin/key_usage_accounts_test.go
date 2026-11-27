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
	now := time.Now().UTC().Truncate(time.Second)
	a.SubscriptionExpiresAt = now.Add(48 * time.Hour)
	a.SetUsageSnapshotSparkAt(0, now.Add(time.Hour), now)
	a.SetWindow7dSeconds(30 * 24 * 3600)
	a.CreditEnabled, a.CreditSkipUsageWindow = true, true
	a.SetUsageSnapshot(100, now)
	a.SetReset7dAt(now.Add(12 * time.Hour))
	a.HealthTier = auth.HealthTierHealthy
	a.LastUsedAt.Store(now.UnixNano())
	a.SetCreditBalance("12.50", true, false, false)
	a.SetRateLimitResetCredits(2)
	a.SetUsageSnapshot5hAt(25, time.Now().Add(time.Hour), time.Now())
	b := &auth.Account{DBID: bID, AccountID: "workspace-b", AccessToken: "secret-other-access-token", PlanType: "plus"}
	store.AddAccounts([]*auth.Account{a, b})
	store.ApplyAccountGroupMemberships(map[int64][]int64{aID: {groupA}, bID: {groupB}})
	otherKey, err := db.InsertAPIKey(ctx, "second", "sk-second-portal-test-key")
	if err != nil {
		t.Fatal(err)
	}
	insertUsage := func(k, accountID int64, at time.Time) float64 {
		t.Helper()
		entry := &database.UsageLogInput{APIKeyID: k, AccountID: accountID, Model: "gpt-5.4", StatusCode: 200, InputTokens: 1000000, CreatedAt: at}
		if err := db.InsertUsageLog(ctx, entry); err != nil {
			t.Fatal(err)
		}
		return database.UsageLogUserBilledCost(entry)
	}
	bill := insertUsage(keyID, aID, now)
	if bill <= 0 {
		t.Fatal("test request must have a nonzero user charge")
	}
	insertUsage(keyID, aID, now.Add(-8*time.Hour))
	insertUsage(otherKey, aID, now)
	insertUsage(keyID, bID, now)
	db.FlushUsageLogs()
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
			ID            int64      `json:"id"`
			Email         string     `json:"email"`
			Usage         *float64   `json:"usage_percent_5h"`
			Status        string     `json:"status"`
			ResetAllowed  bool       `json:"reset_allowed"`
			Requests5h    *int64     `json:"requests_5h"`
			Requests7d    *int64     `json:"requests_7d"`
			Billed5h      *float64   `json:"user_billed_5h"`
			Billed7d      *float64   `json:"user_billed_7d"`
			Health        string     `json:"health_tier"`
			Created       time.Time  `json:"created_at"`
			LastUsed      *time.Time `json:"last_used_at"`
			CreditValid   bool       `json:"credits_valid"`
			CreditBalance *string    `json:"credits_balance"`
			HasCredits    bool       `json:"credits_has_credits"`
			Unlimited     *bool      `json:"credits_unlimited"`
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
	var fields struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	active, unloaded := fields.Accounts[0], fields.Accounts[1]
	if active["updated_at"] == nil || unloaded["updated_at"] == nil {
		t.Fatal("missing account updated_at")
	}
	for _, key := range []string{"subscription", "using_credits", "usage_percent_spark", "reset_spark_at", "usage_updated_at_spark", "long_window_seconds", "limit_reason", "recovery_at"} {
		if active[key] == nil || unloaded[key] != nil {
			t.Fatalf("invalid runtime projection for %s", key)
		}
	}
	if string(active["using_credits"]) != "true" || string(active["limit_reason"]) != `"window_7d"` || string(active["usage_percent_spark"]) != "0" || string(active["long_window_seconds"]) != "2592000" {
		t.Fatalf("wrong usage windows: %s", w.Body.String())
	}
	var recovery time.Time
	if err := json.Unmarshal(active["recovery_at"], &recovery); err != nil || !recovery.Equal(now.Add(12*time.Hour)) {
		t.Fatalf("wrong recovery: %s", active["recovery_at"])
	}
	var sub auth.SubscriptionStatusView
	if err := json.Unmarshal(active["subscription"], &sub); err != nil || sub.BusinessStatus != auth.SubscriptionStatusActive || sub.ExpiresAt != a.SubscriptionExpiresAt.Format(time.RFC3339) {
		t.Fatalf("wrong public subscription: %s", active["subscription"])
	}
	loaded, unavailable := result.Accounts[0], result.Accounts[1]
	if loaded.Requests5h == nil || *loaded.Requests5h != 1 || loaded.Requests7d == nil || *loaded.Requests7d != 2 || loaded.Billed5h == nil || *loaded.Billed5h != bill || loaded.Billed7d == nil || *loaded.Billed7d != 2*bill {
		t.Fatalf("wrong rolling or cross-key usage: %s", w.Body.String())
	}
	if loaded.Health != "healthy" || loaded.Created.IsZero() || loaded.LastUsed == nil || !loaded.LastUsed.Equal(now) || !loaded.CreditValid || !loaded.HasCredits || loaded.CreditBalance == nil || *loaded.CreditBalance != "12.50" || loaded.Unlimited == nil || *loaded.Unlimited {
		t.Fatalf("wrong public account snapshot: %s", w.Body.String())
	}
	if unavailable.Created.IsZero() || unavailable.LastUsed != nil || unavailable.CreditBalance != nil || unavailable.Requests5h == nil || *unavailable.Requests5h != 0 || unavailable.Billed7d == nil || *unavailable.Billed7d != 0 {
		t.Fatalf("wrong unavailable account projection: %s", w.Body.String())
	}
	if len(result.Groups) != 1 || result.Groups[0].ID != groupA || result.Groups[0].Name != "group-a" {
		t.Fatalf("groups=%+v", result.Groups)
	}
	for _, secret := range []string{"secret-access-token", "secret-refresh-token", "secret-proxy", "secret-other-token", "other", "account_billed", "workspace-a", "allowed_api_key_ids"} {
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

func TestPublicResetAcrossInstancesUsesAutomaticCooldown(t *testing.T) {
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
	if got := call(r2, uuid.NewString()); got != 409 {
		t.Fatalf("cooldown status=%d", got)
	}
	now := time.Now().Unix()
	if ok, err := db.ReserveKeyUsageResetCooldown(ctx, scope, now+360, now-1); err != nil || !ok {
		t.Fatalf("expire cooldown: ok=%v err=%v", ok, err)
	}
	if got := call(r2, firstID); got != 409 {
		t.Fatalf("idempotent replay status=%d", got)
	}
	if ok, err := db.ReserveKeyUsageResetCooldown(ctx, scope, now+360, now-1); err != nil || !ok {
		t.Fatalf("expire cooldown: ok=%v err=%v", ok, err)
	}
	h2.consumeResetCredit = func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
		calls.Add(1)
		return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
	}
	h2.probeUsage = func(context.Context, *auth.Account) error { return nil }
	if got := call(r2, uuid.NewString()); got != 200 {
		t.Fatalf("retry after automatic cooldown status=%d", got)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls=%d, want 2", calls.Load())
	}
}

func TestFailedAutoResetDoesNotRequireManualReconciliation(t *testing.T) {
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
	h := &Handler{db: db, store: store, probeUsage: func(context.Context, *auth.Account) error { return nil }, consumeResetCredit: func(context.Context, *auth.Account, string, string) (*proxy.WhamResetResult, *http.Response, error) {
		if calls.Add(1) == 1 {
			return nil, nil, fmt.Errorf("synthetic lost response")
		}
		return &proxy.WhamResetResult{WindowsReset: 1}, &http.Response{StatusCode: http.StatusOK}, nil
	}}
	if _, failure := h.consumeResetCreditLocked(ctx, account, uuid.NewString(), "auto"); failure == nil {
		t.Fatal("expected lost response")
	}
	scope := resetCreditDatabaseScope(account)
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
	if w.Code != 200 || calls.Load() != 2 {
		t.Fatalf("public reset status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
	}
}

func TestPublicAccountSubscriptionRejectsInternalData(t *testing.T) {
	v := auth.BuildSubscriptionStatusView("plus", time.Now(), auth.SubscriptionMeta{Error: "private", Source: "private", LastKnownStatus: "private", SyncState: "private"}, time.Now(), time.UTC)
	sub := publicAccountSubscription(v)
	raw, err := json.Marshal(sub)
	if err != nil || strings.Contains(string(raw), "private") || sub.SyncState != "unknown" || strings.Contains(string(raw), `"error":`) || strings.Contains(string(raw), `"source":`) {
		t.Fatalf("unsafe subscription: %s", raw)
	}
}

func TestPublicAccountLimit(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour)
	for _, tc := range []struct {
		u              auth.AccountLiveUsageSnapshot
		status, reason string
		reset          time.Time
	}{
		{auth.AccountLiveUsageSnapshot{Status: "private diagnostic", CooldownUntil: later}, "cooldown", "cooldown", later},
		{auth.AccountLiveUsageSnapshot{Status: "quota_paused", QuotaPauseWindow: "5h", Reset5hAt: later}, "quota_paused", "quota_paused_5h", later},
		{auth.AccountLiveUsageSnapshot{Status: "rate_limited", UsagePercent7dValid: true, UsagePercent7d: 100, Reset7dAt: later, UsingCredits: true}, "rate_limited", "window_7d", later},
		{auth.AccountLiveUsageSnapshot{Status: "active", Reset5hAt: later}, "active", "", time.Time{}},
	} {
		reason, reset := publicAccountLimit(tc.u, now)
		if publicAccountStatus(tc.u.Status) != tc.status || reason != tc.reason || !reset.Equal(tc.reset) {
			t.Errorf("status=%s reason=%s reset=%v", publicAccountStatus(tc.u.Status), reason, reset)
		}
	}
}
