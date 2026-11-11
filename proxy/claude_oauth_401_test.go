package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newClaudeOAuth401Account(id int64) *auth.Account {
	return &auth.Account{
		DBID: id, UpstreamType: auth.UpstreamClaude, ClaudeAuthKind: auth.ClaudeAuthKindOAuth,
		AccessToken: "expired-at", RefreshToken: "valid-rt", Status: auth.StatusReady,
		ExpiresAt: time.Now().Add(time.Hour), Models: []string{"claude-sonnet-4-6"},
	}
}

func postClaudeMessages(handler *Handler, body string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	handler.Messages(c)
	return recorder
}

func TestClaudeOAuth401RefreshesOnceAndRetries(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	t.Cleanup(store.Stop)
	account := newClaudeOAuth401Account(86701)
	store.AddAccount(account)
	var refreshes atomic.Int32
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*auth.ClaudeTokenData, error) {
		refreshes.Add(1)
		return &auth.ClaudeTokenData{AccessToken: "fresh-at", RefreshToken: "valid-rt", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	var calls atomic.Int32
	var sawFresh atomic.Bool
	installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
		n := calls.Add(1)
		if strings.Contains(req.Header.Get("Authorization"), "fresh-at") {
			sawFresh.Store(true)
		}
		if n == 1 {
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))}, nil
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := postClaudeMessages(handler, `{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusOK || calls.Load() != 2 || refreshes.Load() != 1 || !sawFresh.Load() {
		t.Fatalf("status=%d calls=%d refreshes=%d fresh=%t body=%s", recorder.Code, calls.Load(), refreshes.Load(), sawFresh.Load(), recorder.Body.String())
	}
	if atomic.LoadInt32(&account.Disabled) != 0 || account.IsBanned() || !account.ModelCatalogEligible() {
		t.Fatal("successful refresh left the Claude account hidden")
	}
}

func TestClaudeOAuth401RetriesAtMostOnceWhenRefreshedTokenIsRejected(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	t.Cleanup(store.Stop)
	account := newClaudeOAuth401Account(86702)
	store.AddAccount(account)
	var refreshes atomic.Int32
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*auth.ClaudeTokenData, error) {
		refreshes.Add(1)
		return &auth.ClaudeTokenData{AccessToken: "fresh-at", RefreshToken: "valid-rt", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	var calls atomic.Int32
	installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`))}, nil
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := postClaudeMessages(handler, `{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusServiceUnavailable || calls.Load() != 2 || refreshes.Load() != 1 || !account.IsBanned() || account.ModelCatalogEligible() {
		t.Fatalf("status=%d calls=%d refreshes=%d banned=%t eligible=%t body=%s", recorder.Code, calls.Load(), refreshes.Load(), account.IsBanned(), account.ModelCatalogEligible(), recorder.Body.String())
	}
}

func TestClaudeOAuth401TransportRefreshDoesNotBan(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	t.Cleanup(store.Stop)
	account := newClaudeOAuth401Account(86703)
	store.AddAccount(account)
	var refreshes atomic.Int32
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*auth.ClaudeTokenData, error) {
		refreshes.Add(1)
		return nil, context.DeadlineExceeded
	})
	var calls atomic.Int32
	installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`))}, nil
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := postClaudeMessages(handler, `{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusServiceUnavailable || calls.Load() != 1 || refreshes.Load() != 1 {
		t.Fatalf("status=%d calls=%d refreshes=%d body=%s", recorder.Code, calls.Load(), refreshes.Load(), recorder.Body.String())
	}
	if account.IsBanned() || atomic.LoadInt32(&account.Disabled) != 0 || !account.ModelCatalogEligible() {
		t.Fatalf("transport refresh latched auth state banned=%t disabled=%d eligible=%t", account.IsBanned(), atomic.LoadInt32(&account.Disabled), account.ModelCatalogEligible())
	}
}

func TestClaudeOAuth401InvalidGrantDoesNotRetryUpstream(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	t.Cleanup(store.Stop)
	account := newClaudeOAuth401Account(86704)
	store.AddAccount(account)
	var refreshes atomic.Int32
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*auth.ClaudeTokenData, error) {
		refreshes.Add(1)
		return nil, &claudeRefreshPermanentError{msg: "claude token 刷新失败: invalid_grant"}
	})
	var calls atomic.Int32
	installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error","message":"token expired"}}`))}, nil
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := postClaudeMessages(handler, `{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if recorder.Code != http.StatusServiceUnavailable || calls.Load() != 1 || refreshes.Load() != 1 || !account.IsBanned() {
		t.Fatalf("status=%d calls=%d refreshes=%d banned=%t body=%s", recorder.Code, calls.Load(), refreshes.Load(), account.IsBanned(), recorder.Body.String())
	}
}

type claudeRefreshPermanentError struct{ msg string }

func (e *claudeRefreshPermanentError) Error() string { return e.msg }

func TestClaudeOAuthMissingScopeDoesNotRefresh(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0})
	t.Cleanup(store.Stop)
	account := newClaudeOAuth401Account(86705)
	store.AddAccount(account)
	var refreshes atomic.Int32
	store.SetClaudeTokenRefreshForTest(func(context.Context, string, string) (*auth.ClaudeTokenData, error) {
		refreshes.Add(1)
		return &auth.ClaudeTokenData{AccessToken: "fresh-at", ExpiresAt: time.Now().Add(time.Hour)}, nil
	})
	var calls atomic.Int32
	installClaudeBoundaryTransport(t, account, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"missing_scope","message":"missing scope"}}`))}, nil
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	recorder := postClaudeMessages(handler, `{"model":"claude-sonnet-4-6","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`)
	if refreshes.Load() != 0 || calls.Load() != 1 || account.IsBanned() || atomic.LoadInt32(&account.Disabled) != 0 || !account.ModelCatalogEligible() {
		t.Fatalf("status=%d calls=%d refreshes=%d banned=%t disabled=%d eligible=%t body=%s", recorder.Code, calls.Load(), refreshes.Load(), account.IsBanned(), atomic.LoadInt32(&account.Disabled), account.ModelCatalogEligible(), recorder.Body.String())
	}
}
