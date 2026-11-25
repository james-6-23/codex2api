package admin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestScopedReauthPaths(t *testing.T) {
	h, db, dbPath := newTestAdminHandlerFixtureWithPath(t)
	ctx := context.Background()
	h.reauthExchange = func(context.Context, string, string, string, string, ...string) (*rawOAuthTokenResp, *auth.AccountInfo, error) {
		t.Fatal("unexpected token exchange")
		return nil, nil, nil
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const key = "sk-reauth-owner"
	const other = "sk-other-reauth"
	kid, err := db.InsertAPIKey(ctx, "owner", key)
	must(err)
	_, err = db.InsertAPIKey(ctx, "other", other)
	must(err)
	const proxyURL = "http://internal-proxy.test"
	cred := map[string]any{"refresh_token": "old-sensitive", "workspace_id": "workspace", "user_id": "user-a", "email": "test@example.test"}
	id, err := db.InsertAccountWithCredentials(ctx, "account", cred, proxyURL)
	must(err)
	cred["email"] = "other@example.test"
	id2, err := db.InsertAccountWithCredentials(ctx, "other", cred, proxyURL)
	must(err)
	policy := database.KeyUsageAccountPermissions{ViewEnabled: true}
	must(db.SetKeyUsageAccountPermissions(ctx, 0, policy))
	r := gin.New()
	h.RegisterRoutes(r)
	requestCtx := context.Background()
	call := func(method, path, key, body string, want int) string {
		t.Helper()
		req := httptest.NewRequestWithContext(requestCtx, method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s %s: status %d want %d: %s", method, path, w.Code, want, w.Body.String())
		}
		for _, secret := range []string{proxyURL, "old-sensitive", "new-sensitive", "winning-sensitive", "code_verifier"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatal("private data in response")
			}
		}
		return w.Body.String()
	}
	base := "/api/key-usage/accounts"
	prefix := fmt.Sprintf("%s/%d/reauth/", base, id)
	start := prefix + "start"
	complete := prefix + "complete"
	otherPath := fmt.Sprintf("%s/%d/reauth/complete", base, id2)
	if list := call("GET", base, key, "", 200); !strings.Contains(list, `"reauth_allowed":true`) || !strings.Contains(list, `"reset_allowed":false`) {
		t.Fatal(list)
	}
	call("POST", start, "invalid", "{}", 401)
	call("GET", start, key, "", 404)
	call("POST", start, key, `{"proxy_url":"no"}`, 400)
	var state, session, challenge string
	begin := func() string {
		t.Helper()
		var b map[string]string
		must(json.Unmarshal([]byte(call("POST", start, key, "{}", 200)), &b))
		if len(b) != 2 {
			t.Fatal("unexpected start payload")
		}
		u, err := url.Parse(b["auth_url"])
		must(err)
		q := u.Query()
		session = b["session_id"]
		state = q.Get("state")
		challenge = q.Get("code_challenge")
		if session == "" || state == "" || challenge == "" || q.Get("redirect_uri") != oauthDefaultRedirectURI || q.Get("code_challenge_method") != "S256" {
			t.Fatal("incomplete authorize URL")
		}
		if _, ok := globalOAuthStore.get(session); ok {
			t.Fatal("admin/portal store accepts scoped session")
		}
		return fmt.Sprintf(`{"session_id":%q,"code":"example-code","state":%q}`, session, state)
	}
	body := begin()
	call("POST", complete, other, body, 400)
	call("POST", otherPath, key, body, 400)
	call("POST", complete, key, strings.Replace(body, state, "wrong", 1), 400)
	call("POST", complete, key, strings.Replace(body, "}", `,"id_token":"forged"}`, 1), 400)
	// Both existing OAuth flows use the admin/portal store, not this store.
	for _, self := range []bool{false, true} {
		globalOAuthStore.set("other-flow", &oauthSession{State: state, CreatedAt: time.Now(), SelfService: self})
		call("POST", complete, key, strings.Replace(body, session, "other-flow", 1), 400)
		globalOAuthStore.delete("other-flow")
	}
	group, err := db.CreateAccountGroup(ctx, "restricted", "", "", 0, 0, sql.NullInt64{})
	must(err)
	must(db.UpdateAPIKeyAllowedGroupIDs(ctx, kid, []int64{group}))
	call("POST", start, key, "{}", 404)
	call("POST", complete, key, body, 404)
	must(db.UpdateAPIKeyAllowedGroupIDs(ctx, kid, nil))
	must(db.UpdateAPIKeyExpiresAt(ctx, kid, sql.NullTime{Time: time.Now().Add(-time.Minute), Valid: true}))
	call("POST", complete, key, body, 403)
	call("POST", start, key, "{}", 403)
	if strings.Contains(call("GET", base, key, "", 200), `"reauth_allowed":true`) {
		t.Fatal("expired key offered reauth")
	}
	must(db.UpdateAPIKeyExpiresAt(ctx, kid, sql.NullTime{}))
	must(db.SetKeyUsageAccountPermissions(ctx, kid, database.KeyUsageAccountPermissions{}))
	call("POST", complete, key, body, 404)
	must(db.SetKeyUsageAccountPermissions(ctx, kid, policy))
	calls := 0
	user, workspace := "user-b", "workspace"
	refreshToken := "new-sensitive"
	var during func()
	h.reauthExchange = func(_ context.Context, code, verifier, redirect, proxy string, ids ...string) (*rawOAuthTokenResp, *auth.AccountInfo, error) {
		calls++
		if code != "example-code" || oauthCodeChallenge(verifier) != challenge || redirect != oauthDefaultRedirectURI || proxy != proxyURL {
			t.Fatal("unexpected exchange inputs")
		}
		issuedToken := refreshToken
		if during != nil {
			during()
		}
		jwt := "e30." + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"email":"test@example.test","https://api.openai.com/auth":{"user_id":%q,"chatgpt_account_id":%q}}`, user, workspace))) + ".sig"
		return &rawOAuthTokenResp{RefreshToken: issuedToken, AccessToken: "mock-opaque", IDToken: jwt}, nil, nil
	}
	call("POST", complete, key, body, 409)
	call("POST", complete, key, body, 400)
	if calls != 1 {
		t.Fatal("replayed exchange")
	}
	user, workspace = "user-a", "foreign"
	call("POST", complete, key, begin(), 409)
	workspace = "workspace"
	during = func() { must(db.SetKeyUsageAccountPermissions(ctx, kid, database.KeyUsageAccountPermissions{})) }
	call("POST", complete, key, begin(), 404)
	during = nil
	must(db.SetKeyUsageAccountPermissions(ctx, kid, policy))
	row, err := db.GetAccountByID(ctx, id)
	must(err)
	if row.GetCredential("refresh_token") != "old-sensitive" {
		t.Fatal("unauthorized write")
	}
	// A second completion can finish while the first is still in its exchange.
	gen := h.accountCachesGen.Load()
	during = func() {
		during = nil
		refreshToken = "winning-sensitive"
		call("POST", complete, key, begin(), 200)
	}
	call("POST", complete, key, begin(), 409)
	row, err = db.GetAccountByID(ctx, id)
	must(err)
	if row.GetCredential("refresh_token") != refreshToken {
		t.Fatal("stale completion overwrote the winning grant")
	}
	refreshToken = "new-sensitive"
	if h.accountCachesGen.Load() != gen+1 {
		t.Fatal("only the winning reload should invalidate snapshots")
	}

	deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	must(db.SetError(ctx, id, "refresh error"))
	must(db.SetCooldown(ctx, id, "rate_limited", deadline))
	cacheKey := fmt.Sprint(id)
	setCached := func(reason string) {
		raw, err := json.Marshal(cache.RuntimeCooldown{Reason: reason, ResetAt: deadline})
		must(err)
		must(h.cache.SetRuntime(ctx, "account-cooldown", cacheKey, raw, time.Hour))
	}
	setCached("rate_limited")
	during = func() { must(db.UpdateUsageSnapshot(ctx, id, 43, time.Now())) }
	body = begin()
	if got := call("POST", complete, key, body, 200); got != fmt.Sprintf(`{"id":%d}`, id) {
		t.Fatal(got)
	}
	call("POST", complete, key, body, 400)
	row, err = db.GetAccountByID(ctx, id)
	must(err)
	if row.GetCredential("refresh_token") != "new-sensitive" || row.ProxyURL != proxyURL {
		t.Fatal("wrong persisted update")
	}
	if row.GetCredential("codex_7d_used_percent") == "" || row.Status != "active" || row.ErrorMessage != "" || row.CooldownReason != "rate_limited" || !row.CooldownUntil.Time.Equal(deadline) {
		t.Fatal("lost unrelated update or persisted cooldown")
	}
	if reason, until := h.store.FindByID(id).GetCooldownSnapshot(); reason != "rate_limited" || !until.Equal(deadline) {
		t.Fatal("lost runtime cooldown")
	}
	if _, ok, err := h.cache.GetRuntime(ctx, "account-cooldown", cacheKey); err != nil || !ok {
		t.Fatal("lost cached rate limit", err)
	}
	during = nil
	must(db.SetCooldown(ctx, id, "unauthorized", deadline))
	setCached("unauthorized")
	h.store.FindByID(id).SetCooldownUntil(deadline, "unauthorized")
	atomic.StoreInt32(&h.store.FindByID(id).Disabled, 1)
	call("POST", complete, key, begin(), 200)
	row, err = db.GetAccountByID(ctx, id)
	must(err)
	if row.CooldownReason != "" || row.CooldownUntil.Valid || h.store.FindByID(id).HasActiveCooldown() || atomic.LoadInt32(&h.store.FindByID(id).Disabled) != 0 {
		t.Fatal("did not recover unauthorized cooldown")
	}
	if _, ok, err := h.cache.GetRuntime(ctx, "account-cooldown", cacheKey); err != nil || ok {
		t.Fatal("cached unauthorized cooldown remains", err)
	}

	// Even a purely local throttle must survive a credential reload.
	h.store.FindByID(id).SetCooldownUntil(deadline, "rate_limited")
	call("POST", complete, key, begin(), 200)
	if reason, until := h.store.FindByID(id).GetCooldownSnapshot(); reason != "rate_limited" || !until.Equal(deadline) {
		t.Fatal("local-only gate was removed")
	}
	row, err = db.GetAccountByID(ctx, id)
	must(err)

	// Break only the post-commit group read, after the last permission check.
	previous := h.store.FindByID(id)
	gen = h.accountCachesGen.Load()
	var cancelRequest context.CancelFunc
	requestCtx, cancelRequest = context.WithCancel(ctx)
	defer cancelRequest()
	h.cache = scopedReloadTestCache{TokenCache: h.cache, onRead: func(reloadCtx context.Context) {
		cancelRequest()
		if reloadCtx.Err() != nil {
			t.Fatal("request cancellation reached post-commit reload")
		}
		if until, ok := reloadCtx.Deadline(); !ok || time.Until(until) > 5*time.Second {
			t.Fatal("reload is not bounded")
		}
		execAdminOutputGuardTestSQLite(t, dbPath, "ALTER TABLE account_group_members RENAME TO unavailable_members")
	}}
	before := row.CredentialGeneration
	if got := call("POST", complete, key, begin(), 200); got != fmt.Sprintf(`{"id":%d,"reload_pending":true}`, id) {
		t.Fatal(got)
	}
	row, err = db.GetAccountByID(ctx, id)
	must(err)
	if row.CredentialGeneration != before+1 || h.store.FindByID(id) != previous || previous.GetCredentialGeneration() != before || h.accountCachesGen.Load() != gen {
		t.Fatal("reload failure lost committed write, runtime account or snapshot")
	}
	row, err = db.GetAccountByID(ctx, id2)
	must(err)
	if row.GetCredential("refresh_token") != "old-sensitive" {
		t.Fatal("wrong account updated")
	}
}

func TestScopedSessionExpiryAndConcurrentClaim(t *testing.T) {
	var store scopedReauthStore
	v := scopedSession{keyID: 1, targetID: 2, state: "s", identity: reauthIdentity{"u", "w"}, expires: time.Now().Add(-time.Second)}
	store.add("id", v)
	claim := func() bool { _, ok := store.claim("id", "s", 1, 2, v.keyDigest, v.identity); return ok }
	if claim() {
		t.Fatal("expired state accepted")
	}
	v.expires = time.Now().Add(time.Minute)
	store.add("id", v)
	results := make(chan bool, 8)
	for range cap(results) {
		go func() { results <- claim() }()
	}
	wins := 0
	for range cap(results) {
		if <-results {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("claimed %d times", wins)
	}
}

func TestScopedReauthEligibility(t *testing.T) {
	base := database.KeyUsageAccountAccess{Platform: "openai", Type: "oauth", HasRefreshToken: true, UserID: "user-owner", WorkspaceID: "work", AccountID: "work"}
	if !scopedIdentity(base).valid() {
		t.Fatal("eligible OAuth account rejected")
	}
	for label, change := range map[string]func(*database.KeyUsageAccountAccess){
		"workspace alone": func(a *database.KeyUsageAccountAccess) { a.UserID = "" }, "user alone": func(a *database.KeyUsageAccountAccess) { a.WorkspaceID = ""; a.AccountID = "" }, "ambiguous workspace": func(a *database.KeyUsageAccountAccess) { a.AccountID = "other" }, "non OAuth": func(a *database.KeyUsageAccountAccess) { a.Type = "api_key" }, "other provider": func(a *database.KeyUsageAccountAccess) { a.UpstreamType = "grok" }, "not renewable": func(a *database.KeyUsageAccountAccess) { a.HasRefreshToken = false }} {
		a := base
		change(&a)
		if scopedIdentity(a).valid() {
			t.Errorf("offered reauthorization for %s", label)
		}
	}
}

type scopedReloadTestCache struct {
	cache.TokenCache
	onRead func(context.Context)
}

func (c scopedReloadTestCache) GetRuntime(ctx context.Context, ns, key string) (json.RawMessage, bool, error) {
	if ns == "account-cooldown" {
		c.onRead(ctx)
	}
	return c.TokenCache.GetRuntime(ctx, ns, key)
}
