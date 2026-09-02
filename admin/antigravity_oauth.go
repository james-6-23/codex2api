package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	antigravityOAuthSessionTTL   = 15 * time.Minute
	antigravityOAuthCallbackPath = "/oauth-callback"
)

type antigravityOAuthSession struct {
	mu sync.Mutex

	ID             string
	State          string
	CodeVerifier   string
	RedirectURI    string
	OAuthClientKey string
	ProxyURL       string
	Name           string
	GroupIDs       []int64
	CreatedAt      time.Time
	ExpiresAt      time.Time

	Status    string
	AccountID int64
	Email     string
	Warning   string
	Error     string
	Server    *http.Server
	Listener  net.Listener
}

type antigravityOAuthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*antigravityOAuthSession
}

var globalAntigravityOAuthSessions = &antigravityOAuthSessionStore{
	sessions: make(map[string]*antigravityOAuthSession),
}

func init() {
	go globalAntigravityOAuthSessions.cleanupLoop()
}

func (s *antigravityOAuthSessionStore) add(session *antigravityOAuthSession) {
	s.mu.Lock()
	s.sessions[session.ID] = session
	s.mu.Unlock()
}

func (s *antigravityOAuthSessionStore) get(id string) (*antigravityOAuthSession, bool) {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	session, ok := s.sessions[id]
	if !ok {
		s.mu.Unlock()
		return nil, false
	}
	if time.Now().After(session.ExpiresAt) {
		delete(s.sessions, id)
		s.mu.Unlock()
		// Expiry is lazy (the background janitor runs once per minute), but an
		// expired session must release its loopback listener as soon as it is
		// observed by a request.
		go closeAntigravityOAuthListener(session)
		return nil, false
	}
	s.mu.Unlock()
	return session, true
}

func (s *antigravityOAuthSessionStore) remove(id string) {
	s.mu.Lock()
	delete(s.sessions, strings.TrimSpace(id))
	s.mu.Unlock()
}

func (s *antigravityOAuthSessionStore) cleanupLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		s.mu.Lock()
		for id, session := range s.sessions {
			if now.After(session.ExpiresAt) {
				delete(s.sessions, id)
				go closeAntigravityOAuthListener(session)
			}
		}
		s.mu.Unlock()
	}
}

type antigravityOAuthExchangeInput struct {
	ID             string
	CodeVerifier   string
	RedirectURI    string
	OAuthClientKey string
	ProxyURL       string
	Name           string
	GroupIDs       []int64
}

func (s *antigravityOAuthSessionStore) claim(id, state string) (antigravityOAuthExchangeInput, bool) {
	session, ok := s.get(id)
	if !ok {
		return antigravityOAuthExchangeInput{}, false
	}
	session.mu.Lock()
	// State is an opaque CSRF nonce. Do not trim or otherwise normalize it:
	// accepting a whitespace-mutated value weakens the exact callback binding.
	expired := time.Now().After(session.ExpiresAt)
	if state == "" || subtle.ConstantTimeCompare([]byte(session.State), []byte(state)) != 1 || session.Status != "waiting" || expired {
		session.mu.Unlock()
		if expired {
			go closeAntigravityOAuthListener(session)
		}
		return antigravityOAuthExchangeInput{}, false
	}
	session.Status = "processing"
	input := antigravityOAuthExchangeInput{
		ID: session.ID, CodeVerifier: session.CodeVerifier, RedirectURI: session.RedirectURI,
		OAuthClientKey: session.OAuthClientKey, ProxyURL: session.ProxyURL, Name: session.Name,
		GroupIDs: append([]int64(nil), session.GroupIDs...),
	}
	session.mu.Unlock()
	return input, true
}

func (s *antigravityOAuthSessionStore) setResult(id, status string, accountID int64, email, warning, failure string) {
	session, ok := s.get(id)
	if !ok {
		return
	}
	session.mu.Lock()
	if time.Now().After(session.ExpiresAt) {
		session.mu.Unlock()
		go closeAntigravityOAuthListener(session)
		return
	}
	if session.Status == "completed" || session.Status == "failed" || session.Status == "cancelled" {
		session.mu.Unlock()
		return
	}
	session.Status = status
	session.AccountID = accountID
	session.Email = strings.TrimSpace(email)
	session.Warning = strings.TrimSpace(warning)
	session.Error = strings.TrimSpace(failure)
	session.mu.Unlock()
	go closeAntigravityOAuthListener(session)
}

func closeAntigravityOAuthListener(session *antigravityOAuthSession) {
	if session == nil {
		return
	}
	session.mu.Lock()
	server := session.Server
	listener := session.Listener
	session.Server = nil
	session.Listener = nil
	session.mu.Unlock()
	if server != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = server.Shutdown(ctx)
		cancel()
	}
	// Shutdown closes listeners owned by Serve, but an expiry/cancel can race
	// with the goroutine that is about to call Serve. Close the raw listener as
	// well so that pre-Serve and partially-started servers cannot leak a port.
	if listener != nil {
		_ = listener.Close()
	}
}

func randomAntigravityOAuthToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func antigravityOAuthCodeChallenge(verifier string) string {
	digest := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

type antigravityOAuthStartRequest struct {
	Name           string          `json:"name"`
	ProxyURL       string          `json:"proxy_url"`
	OAuthClientKey string          `json:"oauth_client_key"`
	GroupIDs       json.RawMessage `json:"group_ids"`
}

func (h *Handler) StartAntigravityOAuth(c *gin.Context) {
	var req antigravityOAuthStartRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	name, err := normalizeAntigravityAccountName(req.Name)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	proxyURL := security.SanitizeInput(strings.TrimSpace(req.ProxyURL))
	if err := security.ValidateProxyURL(proxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理 URL 无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	groupIDs, err := h.resolveAntigravityGroupIDs(ctx, req.GroupIDs)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	effectiveProxyURL, err := h.resolveAntigravityControlPlaneProxy(0, proxyURL, groupIDs)
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, err.Error())
		return
	}
	client, err := auth.NewAntigravityClient(effectiveProxyURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		writeError(c, http.StatusServiceUnavailable, "无法创建本地 OAuth 回调端口")
		return
	}
	port := listener.Addr().(*net.TCPAddr).Port
	redirectURI := fmt.Sprintf("http://127.0.0.1:%d%s", port, antigravityOAuthCallbackPath)
	sessionID, err := randomAntigravityOAuthToken(24)
	if err != nil {
		_ = listener.Close()
		writeInternalError(c, err)
		return
	}
	state, err := randomAntigravityOAuthToken(32)
	if err != nil {
		_ = listener.Close()
		writeInternalError(c, err)
		return
	}
	verifier, err := randomAntigravityOAuthToken(48)
	if err != nil {
		_ = listener.Close()
		writeInternalError(c, err)
		return
	}
	authURL, clientInfo, err := client.BuildOAuthAuthorizationURL(redirectURI, state, antigravityOAuthCodeChallenge(verifier), req.OAuthClientKey)
	if err != nil {
		_ = listener.Close()
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	session := &antigravityOAuthSession{
		ID: sessionID, State: state, CodeVerifier: verifier, RedirectURI: redirectURI,
		OAuthClientKey: clientInfo.Key, ProxyURL: proxyURL, Name: name, GroupIDs: groupIDs,
		CreatedAt: time.Now(), ExpiresAt: time.Now().Add(antigravityOAuthSessionTTL), Status: "waiting",
		Listener: listener,
	}
	session.Server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.handleAntigravityOAuthCallbackHTTP(session, w, r)
	})}
	globalAntigravityOAuthSessions.add(session)
	server := session.Server
	go func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			globalAntigravityOAuthSessions.setResult(session.ID, "failed", 0, "", "", "OAuth 回调服务异常")
		}
	}()

	c.JSON(http.StatusOK, gin.H{
		"session_id": session.ID, "auth_url": authURL, "redirect_uri": redirectURI,
		"expires_at": session.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

func (h *Handler) handleAntigravityOAuthCallbackHTTP(session *antigravityOAuthSession, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if r == nil || r.URL == nil || r.Method != http.MethodGet || r.URL.Path != antigravityOAuthCallbackPath || !antigravityOAuthHTTPHostMatches(session, r.Host) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	query := r.URL.Query()
	state := query.Get("state")
	if query.Get("error") != "" {
		// An OAuth denial is still an attacker-controlled callback. Require and
		// atomically claim the exact state before mutating the session.
		if _, ok := globalAntigravityOAuthSessions.claim(session.ID, state); !ok {
			w.WriteHeader(http.StatusBadRequest)
			writeAntigravityOAuthHTML(w, false)
			return
		}
		globalAntigravityOAuthSessions.setResult(session.ID, "failed", 0, "", "", "Google 授权未完成")
		writeAntigravityOAuthHTML(w, false)
		return
	}
	code := strings.TrimSpace(query.Get("code"))
	if code == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeAntigravityOAuthHTML(w, false)
		return
	}
	input, ok := globalAntigravityOAuthSessions.claim(session.ID, state)
	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		writeAntigravityOAuthHTML(w, false)
		return
	}
	writeAntigravityOAuthHTML(w, true)
	go h.processAntigravityOAuthExchange(input, code)
}

// antigravityOAuthHTTPHostMatches validates the Host header when a callback
// arrived through the loopback listener. Direct unit invocations may omit
// Host, but a supplied value must match the exact host:port bound to the
// session redirect URI.
func antigravityOAuthHTTPHostMatches(session *antigravityOAuthSession, host string) bool {
	if strings.TrimSpace(host) == "" {
		return true
	}
	if session == nil {
		return false
	}
	redirect, err := url.Parse(strings.TrimSpace(session.RedirectURI))
	if err != nil || redirect == nil || redirect.Host == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(host), redirect.Host)
}

// antigravityOAuthCallbackURLMatches binds a pasted callback to the exact
// loopback redirect that was allocated for this session. In particular, do
// not accept another port, a path variant, user-info, or a fragment that was
// never sent to the callback server.
func antigravityOAuthCallbackURLMatches(session *antigravityOAuthSession, u *url.URL) bool {
	if session == nil || u == nil || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	redirect, err := url.Parse(strings.TrimSpace(session.RedirectURI))
	if err != nil || redirect == nil || redirect.User != nil || redirect.Fragment != "" || redirect.Opaque != "" {
		return false
	}
	if !strings.EqualFold(redirect.Scheme, "http") || redirect.Host == "" || redirect.Path != antigravityOAuthCallbackPath || redirect.RawQuery != "" {
		return false
	}
	return strings.EqualFold(u.Scheme, redirect.Scheme) &&
		strings.EqualFold(u.Host, redirect.Host) &&
		u.Path == antigravityOAuthCallbackPath &&
		u.EscapedPath() == redirect.EscapedPath()
}

func writeAntigravityOAuthHTML(w http.ResponseWriter, success bool) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if success {
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Authorization complete</title><body style="font-family:sans-serif;text-align:center;padding:4rem"><h1>授权成功</h1><p>可以关闭此窗口并返回 codex2api。</p></body>`))
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Authorization failed</title><body style="font-family:sans-serif;text-align:center;padding:4rem"><h1>授权未完成</h1><p>请返回 codex2api 重试。</p></body>`))
}

type antigravityOAuthCompleteRequest struct {
	SessionID   string `json:"session_id"`
	CallbackURL string `json:"callback_url"`
}

func (h *Handler) CompleteAntigravityOAuth(c *gin.Context) {
	var req antigravityOAuthCompleteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	session, ok := globalAntigravityOAuthSessions.get(req.SessionID)
	if !ok {
		writeError(c, http.StatusGone, "OAuth 会话不存在或已过期")
		return
	}
	u, err := url.Parse(strings.TrimSpace(req.CallbackURL))
	if err != nil || !antigravityOAuthCallbackURLMatches(session, u) {
		writeError(c, http.StatusBadRequest, "请粘贴完整的 OAuth 回调 URL")
		return
	}
	state := u.Query().Get("state")
	if state == "" {
		writeError(c, http.StatusBadRequest, "回调 URL 缺少 code 或 state")
		return
	}
	if oauthError := strings.TrimSpace(u.Query().Get("error")); oauthError != "" {
		if _, ok := globalAntigravityOAuthSessions.claim(req.SessionID, state); !ok {
			writeError(c, http.StatusConflict, "OAuth session already processed or state invalid")
			return
		}
		globalAntigravityOAuthSessions.setResult(req.SessionID, "failed", 0, "", "", "Google authorization was not completed")
		writeError(c, http.StatusBadRequest, "Google authorization was not completed")
		return
	}
	code := strings.TrimSpace(u.Query().Get("code"))
	if code == "" {
		writeError(c, http.StatusBadRequest, "callback URL is missing code")
		return
	}
	input, ok := globalAntigravityOAuthSessions.claim(req.SessionID, state)
	if !ok {
		writeError(c, http.StatusConflict, "OAuth 会话已处理或 state 无效")
		return
	}
	go h.processAntigravityOAuthExchange(input, code)
	c.JSON(http.StatusOK, gin.H{"message": "OAuth 授权已提交", "session_id": req.SessionID})
}

func (h *Handler) GetAntigravityOAuthStatus(c *gin.Context) {
	session, ok := globalAntigravityOAuthSessions.get(c.Query("session_id"))
	if !ok {
		writeError(c, http.StatusGone, "OAuth 会话不存在或已过期")
		return
	}
	session.mu.Lock()
	status := gin.H{
		"session_id": session.ID, "status": session.Status,
		"account_id": session.AccountID, "email": session.Email,
		"warning": session.Warning, "error": session.Error,
		"expires_at": session.ExpiresAt.UTC().Format(time.RFC3339),
	}
	session.mu.Unlock()
	c.JSON(http.StatusOK, status)
}

func (h *Handler) CancelAntigravityOAuth(c *gin.Context) {
	session, ok := globalAntigravityOAuthSessions.get(c.Param("session_id"))
	if !ok {
		c.Status(http.StatusNoContent)
		return
	}
	session.mu.Lock()
	if session.Status == "waiting" {
		session.Status = "cancelled"
	}
	session.mu.Unlock()
	closeAntigravityOAuthListener(session)
	c.Status(http.StatusNoContent)
}

func (h *Handler) processAntigravityOAuthExchange(input antigravityOAuthExchangeInput, code string) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	effectiveProxyURL, err := h.resolveAntigravityControlPlaneProxy(0, input.ProxyURL, input.GroupIDs)
	if err != nil {
		globalAntigravityOAuthSessions.setResult(input.ID, "failed", 0, "", "", err.Error())
		return
	}
	client, err := auth.NewAntigravityClient(effectiveProxyURL)
	if err != nil {
		globalAntigravityOAuthSessions.setResult(input.ID, "failed", 0, "", "", err.Error())
		return
	}
	credential, err := client.ExchangeOAuthAuthorizationCode(ctx, code, input.RedirectURI, input.CodeVerifier, input.OAuthClientKey)
	if err != nil {
		globalAntigravityOAuthSessions.setResult(input.ID, "failed", 0, "", "", err.Error())
		return
	}
	if strings.TrimSpace(credential.RefreshToken) == "" {
		globalAntigravityOAuthSessions.setResult(input.ID, "failed", 0, "", "", "Google 未返回 refresh_token，请重新授权")
		return
	}
	result, err := h.persistAntigravityOAuthCredential(ctx, input, credential)
	if err != nil {
		globalAntigravityOAuthSessions.setResult(input.ID, "failed", 0, result.Email, result.Warning, err.Error())
		return
	}
	globalAntigravityOAuthSessions.setResult(input.ID, "completed", result.ID, result.Email, result.Warning, "")
}

type antigravityOAuthAccountResult struct {
	ID      int64
	Email   string
	Warning string
}

func (h *Handler) persistAntigravityOAuthCredential(ctx context.Context, input antigravityOAuthExchangeInput, credential auth.AntigravityCredential) (antigravityOAuthAccountResult, error) {
	effectiveProxyURL, err := h.resolveAntigravityControlPlaneProxy(0, input.ProxyURL, input.GroupIDs)
	if err != nil {
		return antigravityOAuthAccountResult{}, err
	}
	client, err := auth.NewAntigravityClient(effectiveProxyURL)
	if err != nil {
		return antigravityOAuthAccountResult{}, err
	}
	syncResult, syncErr := client.Sync(ctx, credential)
	if !antigravityAuthoritativeProfile(syncResult.Profile) {
		if syncErr != nil {
			return antigravityOAuthAccountResult{}, syncErr
		}
		return antigravityOAuthAccountResult{}, errors.New("Google profile is not verified")
	}
	familyID := antigravityCredentialFamilyID(syncResult.Credential, syncResult.Profile.ID)
	credentialsMap, err := antigravityCredentialsForInsert(syncResult.Credential, familyID, syncResult, syncErr)
	if err != nil {
		return antigravityOAuthAccountResult{}, err
	}
	name := strings.TrimSpace(input.Name)
	if name == "" {
		name = strings.TrimSpace(syncResult.Profile.Name)
	}
	if name == "" {
		name = strings.TrimSpace(syncResult.Profile.Email)
	}
	warning := strings.TrimSpace(syncResult.Warning)
	if syncErr != nil {
		warning = appendAntigravityWarning(warning, syncErr.Error())
	}
	h.mergeDuplicateMu.Lock()
	defer h.mergeDuplicateMu.Unlock()
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelAntigravity)
	if err != nil {
		return antigravityOAuthAccountResult{}, err
	}
	if duplicateID := findAntigravityDuplicateAccountID(rows, familyID, syncResult.Profile.Email, syncResult.Profile.ID, 0); duplicateID > 0 {
		return antigravityOAuthAccountResult{Email: syncResult.Profile.Email, Warning: warning}, fmt.Errorf("Antigravity 凭据身份已存在 (id=%d)", duplicateID)
	}
	id, err := h.db.InsertAccountWithUpstream(ctx, name, "google", auth.UpstreamAntigravity, credentialsMap, input.ProxyURL)
	if err != nil {
		return antigravityOAuthAccountResult{Email: syncResult.Profile.Email, Warning: warning}, err
	}
	if err := h.bindImportedAccountGroups(ctx, []int64{id}, input.GroupIDs); err != nil {
		warning = appendAntigravityWarning(warning, "分组绑定失败: "+err.Error())
	}
	if err := h.reloadAntigravityRuntimeAccount(ctx, id); err != nil {
		warning = appendAntigravityWarning(warning, "运行时加载失败: "+err.Error())
	}
	h.db.InsertAccountEventAsync(id, "added", "oauth_antigravity")
	security.SecurityAuditLog("ANTIGRAVITY_OAUTH_ACCOUNT_ADDED", fmt.Sprintf("account_id=%d", id))
	return antigravityOAuthAccountResult{ID: id, Email: syncResult.Profile.Email, Warning: warning}, nil
}
