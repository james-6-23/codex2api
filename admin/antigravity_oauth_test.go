package admin

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func newAntigravityOAuthTestSession(id, state string) *antigravityOAuthSession {
	return &antigravityOAuthSession{
		ID:          id,
		State:       state,
		RedirectURI: "http://127.0.0.1:45678" + antigravityOAuthCallbackPath,
		CreatedAt:   time.Now(),
		ExpiresAt:   time.Now().Add(time.Hour),
		Status:      "waiting",
	}
}

func installAntigravityOAuthGlobalSession(t *testing.T, session *antigravityOAuthSession) {
	t.Helper()
	globalAntigravityOAuthSessions.add(session)
	t.Cleanup(func() {
		globalAntigravityOAuthSessions.remove(session.ID)
		closeAntigravityOAuthListener(session)
	})
}

func sessionStatus(session *antigravityOAuthSession) string {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.Status
}

func TestAntigravityOAuthClaimIsOneShotAndStateExact(t *testing.T) {
	store := &antigravityOAuthSessionStore{sessions: make(map[string]*antigravityOAuthSession)}
	session := newAntigravityOAuthTestSession("claim-session", "state-token")
	store.add(session)

	if _, ok := store.claim(session.ID, " state-token"); ok {
		t.Fatal("claim accepted a whitespace-mutated state")
	}
	input, ok := store.claim(session.ID, session.State)
	if !ok || input.ID != session.ID {
		t.Fatalf("first claim = %+v, ok=%v", input, ok)
	}
	if _, ok := store.claim(session.ID, session.State); ok {
		t.Fatal("second claim unexpectedly succeeded")
	}
	if got := sessionStatus(session); got != "processing" {
		t.Fatalf("status after claim = %q, want processing", got)
	}
}

func TestStartAntigravityOAuthUsesBuiltinOfficialClientWhenUnconfigured(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENTS", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_KEY", "")
	auth.SetConfiguredAntigravityOAuth(auth.AntigravityOAuthSettings{})
	t.Cleanup(func() { auth.SetConfiguredAntigravityOAuth(auth.AntigravityOAuthSettings{}) })
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/oauth/start", strings.NewReader(`{"name":"oauth-builtin"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.StartAntigravityOAuth(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		SessionID string `json:"session_id"`
		AuthURL   string `json:"auth_url"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	session, ok := globalAntigravityOAuthSessions.get(payload.SessionID)
	if !ok {
		t.Fatalf("session %q was not stored", payload.SessionID)
	}
	t.Cleanup(func() {
		globalAntigravityOAuthSessions.remove(payload.SessionID)
		closeAntigravityOAuthListener(session)
	})
	authURL, err := url.Parse(payload.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := authURL.Query().Get("client_id"); got != auth.AntigravityDefaultOAuthClientID {
		t.Fatalf("client_id = %q, want official desktop client", got)
	}
}

func TestStartAntigravityOAuthCreatesStateBoundPKCESession(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENTS", "test|test-client|test-secret")
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/oauth/start", strings.NewReader(`{"name":"oauth-test"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.StartAntigravityOAuth(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		SessionID   string `json:"session_id"`
		AuthURL     string `json:"auth_url"`
		RedirectURI string `json:"redirect_uri"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	session, ok := globalAntigravityOAuthSessions.get(payload.SessionID)
	if !ok {
		t.Fatalf("session %q was not stored", payload.SessionID)
	}
	t.Cleanup(func() {
		globalAntigravityOAuthSessions.remove(payload.SessionID)
		closeAntigravityOAuthListener(session)
	})
	if session.State == "" || session.CodeVerifier == "" || session.RedirectURI != payload.RedirectURI {
		t.Fatalf("session = %+v", session)
	}
	authURL, err := url.Parse(payload.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authURL.Query()
	if query.Get("state") != session.State {
		t.Fatalf("authorization state = %q, session state = %q", query.Get("state"), session.State)
	}
	if query.Get("code_challenge") != antigravityOAuthCodeChallenge(session.CodeVerifier) || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization PKCE params = challenge:%q method:%q", query.Get("code_challenge"), query.Get("code_challenge_method"))
	}
}

func TestStartAntigravityOAuthFailsClosedWithoutUsableEgress(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	store.SetProxyPoolEnabled(true)
	h := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/oauth/start", strings.NewReader(`{"name":"oauth-offline"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.StartAntigravityOAuth(ctx)
	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), antigravityNoUsableEgressError) {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAntigravityOAuthExpiredLookupClosesListener(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{}
	session := newAntigravityOAuthTestSession("expired-session", "state")
	session.ExpiresAt = time.Now().Add(-time.Second)
	session.Listener = listener
	session.Server = server
	store := &antigravityOAuthSessionStore{sessions: make(map[string]*antigravityOAuthSession)}
	store.add(session)

	if _, ok := store.get(session.ID); ok {
		t.Fatal("expired session was returned")
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		session.mu.Lock()
		closed := session.Listener == nil && session.Server == nil
		session.mu.Unlock()
		if closed {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expired session listener was not closed")
}

func TestAntigravityOAuthErrorCallbackRequiresState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	session := newAntigravityOAuthTestSession("error-callback-session", "state-good")
	installAntigravityOAuthGlobalSession(t, session)

	cases := []struct {
		name string
		url  string
	}{
		{name: "missing state", url: session.RedirectURI + "?error=access_denied"},
		{name: "wrong state", url: session.RedirectURI + "?error=access_denied&state=state-bad"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			h.handleAntigravityOAuthCallbackHTTP(session, recorder, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if got := sessionStatus(session); got != "waiting" {
				t.Fatalf("invalid callback changed status to %q", got)
			}
		})
	}

	recorder := httptest.NewRecorder()
	validURL := session.RedirectURI + "?error=access_denied&state=state-good"
	h.handleAntigravityOAuthCallbackHTTP(session, recorder, httptest.NewRequest(http.MethodGet, validURL, nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("valid denial status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := sessionStatus(session); got != "failed" {
		t.Fatalf("valid denial status = %q, want failed", got)
	}

	// A claimed denial cannot be replayed.
	recorder = httptest.NewRecorder()
	h.handleAntigravityOAuthCallbackHTTP(session, recorder, httptest.NewRequest(http.MethodGet, validURL, nil))
	if recorder.Code != http.StatusBadRequest || sessionStatus(session) != "failed" {
		t.Fatalf("replayed denial was accepted: code=%d status=%q", recorder.Code, sessionStatus(session))
	}
}

func TestAntigravityOAuthHTTPCallbackRejectsHostMismatch(t *testing.T) {
	session := newAntigravityOAuthTestSession("host-callback-session", "state-good")
	installAntigravityOAuthGlobalSession(t, session)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "http://localhost:45678/oauth-callback?error=access_denied&state=state-good", nil)
	(&Handler{}).handleAntigravityOAuthCallbackHTTP(session, recorder, request)
	if recorder.Code != http.StatusNotFound || sessionStatus(session) != "waiting" {
		t.Fatalf("host mismatch: code=%d status=%q", recorder.Code, sessionStatus(session))
	}
}

func TestAntigravityOAuthSuccessCallbackDoesNotClaimWithoutCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	session := newAntigravityOAuthTestSession("missing-code-session", "state-good")
	installAntigravityOAuthGlobalSession(t, session)
	recorder := httptest.NewRecorder()
	(&Handler{}).handleAntigravityOAuthCallbackHTTP(session, recorder, httptest.NewRequest(http.MethodGet, session.RedirectURI+"?state=state-good", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := sessionStatus(session); got != "waiting" {
		t.Fatalf("missing-code callback changed status to %q", got)
	}
}

func TestAntigravityOAuthCallbackURLBindsHostPortAndPath(t *testing.T) {
	session := newAntigravityOAuthTestSession("url-session", "state")
	for name, raw := range map[string]string{
		"other host": "http://localhost:45678/oauth-callback?code=c&state=state",
		"other port": "http://127.0.0.1:45679/oauth-callback?code=c&state=state",
		"other path": "http://127.0.0.1:45678/other?code=c&state=state",
		"userinfo":   "http://user@127.0.0.1:45678/oauth-callback?code=c&state=state",
		"fragment":   "http://127.0.0.1:45678/oauth-callback?code=c&state=state#fragment",
	} {
		t.Run(name, func(t *testing.T) {
			u, err := url.Parse(raw)
			if err != nil {
				t.Fatal(err)
			}
			if antigravityOAuthCallbackURLMatches(session, u) {
				t.Fatalf("accepted invalid callback URL %q", raw)
			}
		})
	}
	valid, err := url.Parse(session.RedirectURI + "?code=c&state=state")
	if err != nil || !antigravityOAuthCallbackURLMatches(session, valid) {
		t.Fatalf("valid callback URL rejected: %v", err)
	}
}

func TestAntigravityOAuthCancelClosesListenerAndMarksSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{}
	session := newAntigravityOAuthTestSession("cancel-session", "state")
	session.Listener = listener
	session.Server = server
	installAntigravityOAuthGlobalSession(t, session)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/admin/accounts/antigravity/oauth/cancel/"+session.ID, nil)
	ctx.Params = gin.Params{{Key: "session_id", Value: session.ID}}
	(&Handler{}).CancelAntigravityOAuth(ctx)
	if ctx.Writer.Status() != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", ctx.Writer.Status(), http.StatusNoContent)
	}
	if got := sessionStatus(session); got != "cancelled" {
		t.Fatalf("status = %q, want cancelled", got)
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.Listener != nil || session.Server != nil {
		t.Fatal("cancel did not detach listener/server")
	}
}

func TestCloseAntigravityOAuthListenerIsIdempotent(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	session := &antigravityOAuthSession{Server: server, Listener: listener}
	closeAntigravityOAuthListener(session)
	closeAntigravityOAuthListener(session)
	select {
	case serveErr := <-done:
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
			t.Fatalf("Serve() error = %v", serveErr)
		}
	case <-time.After(time.Second):
		t.Fatal("listener server did not stop")
	}
}

func TestCompleteAntigravityOAuthErrorCallbackValidatesState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	session := newAntigravityOAuthTestSession("complete-error-session", "state-good")
	installAntigravityOAuthGlobalSession(t, session)

	call := func(callback string) *httptest.ResponseRecorder {
		body, _ := json.Marshal(antigravityOAuthCompleteRequest{SessionID: session.ID, CallbackURL: callback})
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/oauth/complete", strings.NewReader(string(body)))
		ctx.Request.Header.Set("Content-Type", "application/json")
		(&Handler{}).CompleteAntigravityOAuth(ctx)
		return recorder
	}

	if got := call(session.RedirectURI + "?error=access_denied"); got.Code != http.StatusBadRequest || sessionStatus(session) != "waiting" {
		t.Fatalf("missing-state denial: code=%d status=%q", got.Code, sessionStatus(session))
	}
	if got := call(session.RedirectURI + "?error=access_denied&state=wrong"); got.Code != http.StatusConflict || sessionStatus(session) != "waiting" {
		t.Fatalf("wrong-state denial: code=%d status=%q", got.Code, sessionStatus(session))
	}
	if got := call(session.RedirectURI + "?error=access_denied&state=state-good"); got.Code != http.StatusBadRequest || sessionStatus(session) != "failed" {
		t.Fatalf("valid denial: code=%d status=%q", got.Code, sessionStatus(session))
	}
}
