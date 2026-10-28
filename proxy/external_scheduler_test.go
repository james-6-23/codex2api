package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

var pinnedSuccessEvents = []string{
	`{"type":"response.created","response":{"id":"resp_pinned"}}`,
	`{"type":"response.output_text.delta","delta":"pinned-ok"}`,
	`{"type":"response.completed","response":{"id":"resp_pinned","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
}

func writePinnedSuccessSSE(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range pinnedSuccessEvents {
		_, _ = io.WriteString(w, "data: "+event+"\n\n")
	}
}

type pinnedUpstream struct {
	calls atomic.Int32
	mu    sync.Mutex
	auths []string
}

func (u *pinnedUpstream) record(r *http.Request) {
	u.calls.Add(1)
	u.mu.Lock()
	u.auths = append(u.auths, r.Header.Get("Authorization"))
	u.mu.Unlock()
}

func (u *pinnedUpstream) authorizations() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.auths...)
}

// newPinnedCodexHandler builds two Codex OAuth accounts behind a fake upstream.
// Account 2 is unattractive to the internal scheduler on every axis: account
// cooldown, model cooldown, and a full concurrency cap.
func newPinnedCodexHandler(t *testing.T, db *database.DB, respond func(http.ResponseWriter, *http.Request)) (*Handler, *pinnedUpstream, []*auth.Account) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	upstream := &pinnedUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.5",
		MaxRetries: 3, MaxRateLimitRetries: 3,
	})
	t.Cleanup(store.Stop)
	accounts := []*auth.Account{
		{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"},
		{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"},
	}
	for _, account := range accounts {
		store.AddAccount(account)
	}
	store.MarkCooldown(accounts[1], time.Hour, "rate_limited")
	store.MarkModelCooldown(accounts[1], "gpt-5.5", time.Hour, "rate_limited")
	accounts[1].ActiveRequests.Store(1)
	accounts[1].OccupiedRequests.Store(1)
	return NewHandler(store, db, nil, nil), upstream, accounts
}

// pinnedTestContext mirrors what the API key middleware leaves on a request
// from an external scheduler key that set PinnedAccountHeader.
func pinnedTestContext(recorder *httptest.ResponseRecorder, path, body string, keyID, accountID int64) *gin.Context {
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	row := &database.APIKeyRow{
		ID: keyID, Name: "scheduler", Key: "sk-scheduler-test", Enabled: true,
		Limits: database.APIKeyLimits{ExternalScheduler: true},
	}
	ctx.Set(contextAPIKeyRow, row)
	ctx.Set(contextAPIKeyID, keyID)
	ctx.Set(contextPinnedAccountID, accountID)
	return ctx
}

const pinnedChatBody = `{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`

func TestPinnedChatRunsOnPinnedAccountDespiteSchedulerGates(t *testing.T) {
	handler, upstream, accounts := newPinnedCodexHandler(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writePinnedSuccessSSE(w)
	})

	recorder := httptest.NewRecorder()
	handler.ChatCompletions(pinnedTestContext(recorder, "/v1/chat/completions", pinnedChatBody, 7, 2))

	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || !strings.Contains(body, "pinned-ok") {
		t.Fatalf("status = %d body = %s, want the pinned account's reply", recorder.Code, body)
	}
	if got := upstream.authorizations(); len(got) != 1 || got[0] != "Bearer at-2" {
		t.Fatalf("upstream authorizations = %q, want one call with account 2's token", got)
	}
	if got := recorder.Header().Get(PinnedAccountHeader); got != "2" {
		t.Fatalf("%s = %q, want 2", PinnedAccountHeader, got)
	}
	if active := accounts[1].ActiveRequests.Load(); active != 1 {
		t.Fatalf("account 2 active requests = %d after the request, want the pinned slot released", active)
	}
}

func TestPinnedResponsesIgnoresModelCooldown(t *testing.T) {
	handler, upstream, _ := newPinnedCodexHandler(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writePinnedSuccessSSE(w)
	})

	recorder := httptest.NewRecorder()
	handler.Responses(pinnedTestContext(recorder, "/v1/responses", `{"model":"gpt-5.5","input":"hello","stream":true}`, 7, 2))

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "pinned-ok") {
		t.Fatalf("status = %d body = %s, want the pinned account's stream", recorder.Code, recorder.Body.String())
	}
	if got := upstream.authorizations(); len(got) != 1 || got[0] != "Bearer at-2" {
		t.Fatalf("upstream authorizations = %q, want one call with account 2's token", got)
	}
	if got := recorder.Header().Get(PinnedAccountHeader); got != "2" {
		t.Fatalf("%s = %q, want 2", PinnedAccountHeader, got)
	}
}

func TestPinnedResponsesContinuationDoesNotReleaseQuotaBlockedAccount(t *testing.T) {
	handler, upstream, accounts := newPinnedCodexHandler(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writePinnedSuccessSSE(w)
	})
	const session = "external-quota-turn"
	key := sessionAffinityKey(session, 7)
	handler.store.MarkResponsesPremium5hRateLimited(accounts[1], time.Now().Add(2*time.Hour))
	handler.store.BindSessionAffinity(key, accounts[1], "")

	recorder := httptest.NewRecorder()
	ctx := pinnedTestContext(recorder, "/v1/responses", `{"model":"gpt-5.5","input":"continue","stream":true}`, 7, 2)
	ctx.Request.Header.Set("Session-Id", session)
	ctx.Request.Header.Set(codexTurnStateHeader, "pinned-turn-state")
	handler.Responses(ctx)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "pinned-ok") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := upstream.authorizations(); len(got) != 1 || got[0] != "Bearer at-2" {
		t.Fatalf("externally pinned continuation selected the wrong account: %q", got)
	}
	if id, ok := handler.store.SessionAffinityAccountID(key); !ok || id != 2 {
		t.Fatalf("pinned affinity changed: id=%d present=%v", id, ok)
	}
}

// Retrying or failing over is the external scheduler's decision, even with
// gateway retry budgets configured and a healthy sibling account in the pool.
func TestPinnedChatReturnsUpstreamFailureWithoutFailover(t *testing.T) {
	handler, upstream, _ := newPinnedCodexHandler(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"upstream boom","type":"server_error"}}`)
	})

	recorder := httptest.NewRecorder()
	handler.ChatCompletions(pinnedTestContext(recorder, "/v1/chat/completions", pinnedChatBody, 7, 2))

	if got := upstream.calls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want exactly one attempt on the pinned account", got)
	}
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body = %s, want the upstream 500", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get(pinnedUpstreamStatusHeader); got != "500" {
		t.Fatalf("%s = %q, want 500", pinnedUpstreamStatusHeader, got)
	}
	if got := recorder.Header().Get(PinnedAccountHeader); got != "2" {
		t.Fatalf("%s = %q, want 2", PinnedAccountHeader, got)
	}
}

func TestPinnedResponsesForwardsRateLimitWithoutRetry(t *testing.T) {
	handler, upstream, _ := newPinnedCodexHandler(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"Too many requests","type":"rate_limit_error"}}`)
	})

	recorder := httptest.NewRecorder()
	handler.Responses(pinnedTestContext(recorder, "/v1/responses", `{"model":"gpt-5.5","input":"hello"}`, 7, 2))

	if got := upstream.authorizations(); len(got) != 1 || got[0] != "Bearer at-2" {
		t.Fatalf("upstream authorizations = %q, want one attempt on account 2 and no retry on account 1", got)
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body = %s, want 429", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want the upstream value 7", got)
	}
	if got := recorder.Header().Get(pinnedUpstreamStatusHeader); got != "429" {
		t.Fatalf("%s = %q, want 429", pinnedUpstreamStatusHeader, got)
	}
}

func TestPinnedMessagesReportsUnknownAccountInAnthropicFormat(t *testing.T) {
	handler, upstream, _ := newPinnedCodexHandler(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writePinnedSuccessSSE(w)
	})

	recorder := httptest.NewRecorder()
	handler.Messages(pinnedTestContext(recorder, "/v1/messages", `{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, 7, 99))

	body := recorder.Body.String()
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d body = %s, want 404", recorder.Code, body)
	}
	if gjson.Get(body, "type").String() != "error" || gjson.Get(body, "error.type").String() != "not_found_error" ||
		gjson.Get(body, "error.code").String() != "pinned_account_not_found" {
		t.Fatalf("body = %s, want an Anthropic not_found_error with code pinned_account_not_found", body)
	}
	if got := recorder.Header().Get(pinnedAccountErrorHeader); got != "pinned_account_not_found" {
		t.Fatalf("%s = %q, want pinned_account_not_found", pinnedAccountErrorHeader, got)
	}
	if got := recorder.Header().Get(PinnedAccountHeader); got != "" {
		t.Fatalf("%s = %q, want unset when no account ran the request", PinnedAccountHeader, got)
	}
	if got := upstream.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want none", got)
	}
}

// An account restricted to other API keys must be indistinguishable from a
// missing one, so a key cannot probe the pool outside its scope.
func TestPinnedChatTreatsAccountOutsideKeyScopeAsMissing(t *testing.T) {
	handler, upstream, accounts := newPinnedCodexHandler(t, nil, func(w http.ResponseWriter, _ *http.Request) {
		writePinnedSuccessSSE(w)
	})
	accounts[0].SetAllowedAPIKeyIDs([]int64{500})

	recorder := httptest.NewRecorder()
	handler.ChatCompletions(pinnedTestContext(recorder, "/v1/chat/completions", pinnedChatBody, 7, 1))

	if recorder.Code != http.StatusNotFound || gjson.Get(recorder.Body.String(), "error.code").String() != "pinned_account_not_found" {
		t.Fatalf("status = %d body = %s, want 404 pinned_account_not_found", recorder.Code, recorder.Body.String())
	}
	if got := upstream.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want none", got)
	}
}

func externalSchedulerIngressRouter(row *database.APIKeyRow) *gin.Engine {
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(contextAPIKeyRow, row)
		if rejectExternalSchedulerIngress(c, row) {
			c.Abort()
			return
		}
		c.Next()
	})
	echo := func(c *gin.Context) {
		id, pinned := pinnedAccountIDFromContext(c)
		c.JSON(http.StatusOK, gin.H{"pinned": pinned, "id": id})
	}
	router.POST("/v1/responses", echo)
	router.GET("/v1/responses", echo)
	router.POST("/v1/messages", echo)
	router.POST("/v1/images/generations", echo)
	router.GET("/v1/models", echo)
	router.POST("/backend-api/codex/responses/*subpath", echo)
	return router
}

func TestExternalSchedulerIngress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	external := &database.APIKeyRow{ID: 7, Enabled: true, Limits: database.APIKeyLimits{ExternalScheduler: true}}
	ordinary := &database.APIKeyRow{ID: 8, Enabled: true}
	cases := []struct {
		name       string
		row        *database.APIKeyRow
		method     string
		path       string
		header     string
		wantStatus int
		wantCode   string
		wantPinned int64
	}{
		{name: "pinned execution", row: external, method: http.MethodPost, path: "/v1/responses", header: " 12 ", wantStatus: http.StatusOK, wantPinned: 12},
		{name: "missing header", row: external, method: http.MethodPost, path: "/v1/responses", wantStatus: http.StatusBadRequest, wantCode: "pinned_account_required"},
		{name: "non-numeric header", row: external, method: http.MethodPost, path: "/v1/responses", header: "acct-1", wantStatus: http.StatusBadRequest, wantCode: "pinned_account_invalid"},
		{name: "zero account", row: external, method: http.MethodPost, path: "/v1/responses", header: "0", wantStatus: http.StatusBadRequest, wantCode: "pinned_account_invalid"},
		{name: "signed account", row: external, method: http.MethodPost, path: "/v1/responses", header: "+3", wantStatus: http.StatusBadRequest, wantCode: "pinned_account_invalid"},
		{name: "responses websocket is not pinned yet", row: external, method: http.MethodGet, path: "/v1/responses", header: "12", wantStatus: http.StatusForbidden, wantCode: "external_scheduler_endpoint_unsupported"},
		{name: "images are not pinned yet", row: external, method: http.MethodPost, path: "/v1/images/generations", header: "12", wantStatus: http.StatusForbidden, wantCode: "external_scheduler_endpoint_unsupported"},
		{name: "codex compact subpath is not pinned yet", row: external, method: http.MethodPost, path: "/backend-api/codex/responses/compact", header: "12", wantStatus: http.StatusForbidden, wantCode: "external_scheduler_endpoint_unsupported"},
		{name: "model catalog needs no account", row: external, method: http.MethodGet, path: "/v1/models", wantStatus: http.StatusOK},
		{name: "ordinary keys ignore the header", row: ordinary, method: http.MethodPost, path: "/v1/responses", header: "12", wantStatus: http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			if tc.header != "" {
				req.Header.Set(PinnedAccountHeader, tc.header)
			}
			recorder := httptest.NewRecorder()
			externalSchedulerIngressRouter(tc.row).ServeHTTP(recorder, req)

			body := recorder.Body.String()
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d body = %s, want %d", recorder.Code, body, tc.wantStatus)
			}
			if tc.wantCode != "" {
				if got := gjson.Get(body, "error.code").String(); got != tc.wantCode {
					t.Fatalf("error.code = %q body = %s, want %q", got, body, tc.wantCode)
				}
				if got := recorder.Header().Get(pinnedAccountErrorHeader); got != tc.wantCode {
					t.Fatalf("%s = %q, want %q", pinnedAccountErrorHeader, got, tc.wantCode)
				}
				return
			}
			if got := gjson.Get(body, "id").Int(); got != tc.wantPinned || gjson.Get(body, "pinned").Bool() != (tc.wantPinned > 0) {
				t.Fatalf("pinned context = %s, want account %d", body, tc.wantPinned)
			}
		})
	}
}

func TestExternalSchedulerIngressUsesAnthropicErrorsOnMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	external := &database.APIKeyRow{ID: 7, Enabled: true, Limits: database.APIKeyLimits{ExternalScheduler: true}}
	recorder := httptest.NewRecorder()
	externalSchedulerIngressRouter(external).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{}`)))

	body := recorder.Body.String()
	if recorder.Code != http.StatusBadRequest || gjson.Get(body, "type").String() != "error" ||
		gjson.Get(body, "error.type").String() != "invalid_request_error" || gjson.Get(body, "error.code").String() != "pinned_account_required" {
		t.Fatalf("status = %d body = %s, want an Anthropic invalid_request_error", recorder.Code, body)
	}
}

// End to end through the real API key middleware: the flag must survive
// persistence and key resolution, and the header must reach the handler.
func TestExternalSchedulerKeyEndToEnd(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const key = "sk-external-scheduler-1234567890"
	keyID, err := db.InsertAPIKey(context.Background(), "scheduler", key)
	if err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	if err := db.UpdateAPIKey(context.Background(), keyID, database.APIKeyUpdate{
		Limits: database.APIKeyLimits{ExternalScheduler: true}, LimitsSet: true,
	}); err != nil {
		t.Fatalf("UpdateAPIKey: %v", err)
	}

	handler, upstream, _ := newPinnedCodexHandler(t, db, func(w http.ResponseWriter, _ *http.Request) {
		writePinnedSuccessSSE(w)
	})
	router := gin.New()
	handler.RegisterRoutes(router)

	send := func(method, path, accountID string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(pinnedChatBody))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		if accountID != "" {
			req.Header.Set(PinnedAccountHeader, accountID)
		}
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, req)
		return recorder
	}

	if recorder := send(http.MethodPost, "/v1/chat/completions", ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing header: status = %d body = %s, want 400", recorder.Code, recorder.Body.String())
	}
	if recorder := send(http.MethodPost, "/v1/images/generations", "2"); recorder.Code != http.StatusForbidden {
		t.Fatalf("unsupported endpoint: status = %d body = %s, want 403", recorder.Code, recorder.Body.String())
	}
	if got := upstream.calls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d after refused requests, want none", got)
	}

	recorder := send(http.MethodPost, "/v1/chat/completions", "2")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "pinned-ok") {
		t.Fatalf("pinned request: status = %d body = %s, want 200", recorder.Code, recorder.Body.String())
	}
	if got := upstream.authorizations(); len(got) != 1 || got[0] != "Bearer at-2" {
		t.Fatalf("upstream authorizations = %q, want one call with account 2's token", got)
	}
	if got := recorder.Header().Get(PinnedAccountHeader); got != "2" {
		t.Fatalf("%s = %q, want 2", PinnedAccountHeader, got)
	}
}
