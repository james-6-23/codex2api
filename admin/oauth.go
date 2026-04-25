package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// ==================== OAuth 常量 ====================

const (
	oauthAuthorizeURL       = "https://auth.openai.com/oauth/authorize"
	oauthTokenURL           = "https://auth.openai.com/oauth/token"
	oauthClientID           = "app_EMoamEEZ73f0CkXaXp7hrann"
	oauthDefaultRedirectURI = "http://localhost:1455/auth/callback"
	oauthDefaultScopes      = "openid profile email offline_access"
	oauthSessionTTL         = 30 * time.Minute
)

// ==================== 内存 Session 存储 ====================

type oauthSession struct {
	State        string
	CodeVerifier string
	RedirectURI  string
	ProxyURL     string
	CreatedAt    time.Time

	// 回调自动捕获字段
	CallbackCode   string    // 回调收到的 authorization code
	CallbackState  string    // 回调收到的 state
	CallbackAt     time.Time // 回调时间
	ExchangeResult *oauthExchangeResult
}

// oauthExchangeResult 自动回调完成后的兑换结果
type oauthExchangeResult struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	ID       int64  `json:"id,omitempty"`
	Email    string `json:"email,omitempty"`
	PlanType string `json:"plan_type,omitempty"`
	Error    string `json:"error,omitempty"`
}

type oauthSessionStore struct {
	mu       sync.Mutex
	sessions map[string]*oauthSession
}

var globalOAuthStore = &oauthSessionStore{sessions: make(map[string]*oauthSession)}

func init() {
	go globalOAuthStore.cleanupLoop()
}

func (s *oauthSessionStore) set(id string, sess *oauthSession) {
	s.mu.Lock()
	s.sessions[id] = sess
	s.mu.Unlock()
}

func (s *oauthSessionStore) get(id string) (*oauthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok || time.Since(sess.CreatedAt) > oauthSessionTTL {
		return nil, false
	}
	return sess, true
}

func (s *oauthSessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// findByState 通过 state 查找 session（回调端点使用，返回 sessionID + session）
func (s *oauthSessionStore) findByState(state string) (string, *oauthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.State == state && time.Since(sess.CreatedAt) <= oauthSessionTTL {
			return id, sess, true
		}
	}
	return "", nil, false
}

func (s *oauthSessionStore) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		for id, sess := range s.sessions {
			if time.Since(sess.CreatedAt) > oauthSessionTTL {
				delete(s.sessions, id)
			}
		}
		s.mu.Unlock()
	}
}

// ==================== PKCE 工具函数 ====================

func oauthRandomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func oauthCodeChallenge(verifier string) string {
	h := sha256.Sum256([]byte(verifier))
	return strings.TrimRight(base64.URLEncoding.EncodeToString(h[:]), "=")
}

// ==================== Handlers ====================

// GenerateOAuthURL 生成 Codex CLI PKCE OAuth 授权 URL
// POST /api/admin/oauth/generate-auth-url
func (h *Handler) GenerateOAuthURL(c *gin.Context) {
	var req struct {
		ProxyURL    string `json:"proxy_url"`
		RedirectURI string `json:"redirect_uri"`
	}
	_ = c.ShouldBindJSON(&req)

	redirectURI := strings.TrimSpace(req.RedirectURI)
	if redirectURI == "" {
		// OpenAI OAuth 仅注册了 localhost:1455 回调，始终使用固定默认值
		// 避免因请求 Host 端口不同（如 localhost:3000）导致回调校验失败（#80）
		redirectURI = oauthDefaultRedirectURI
	}

	state, err := oauthRandomHex(32)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 state 失败")
		return
	}
	codeVerifier, err := oauthRandomHex(64)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 code_verifier 失败")
		return
	}
	sessionID, err := oauthRandomHex(16)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 session_id 失败")
		return
	}

	globalOAuthStore.set(sessionID, &oauthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		RedirectURI:  redirectURI,
		ProxyURL:     strings.TrimSpace(req.ProxyURL),
		CreatedAt:    time.Now(),
	})

	params := neturl.Values{}
	params.Set("response_type", "code")
	params.Set("client_id", oauthClientID)
	params.Set("redirect_uri", redirectURI)
	params.Set("scope", oauthDefaultScopes)
	params.Set("state", state)
	params.Set("code_challenge", oauthCodeChallenge(codeVerifier))
	params.Set("code_challenge_method", "S256")
	params.Set("id_token_add_organizations", "true")
	params.Set("codex_cli_simplified_flow", "true")

	c.JSON(http.StatusOK, gin.H{
		"auth_url":   oauthAuthorizeURL + "?" + params.Encode(),
		"session_id": sessionID,
	})
}

// ExchangeOAuthCode 用授权码兑换 token，并写入新账号
// POST /api/admin/oauth/exchange-code
func (h *Handler) ExchangeOAuthCode(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
		Name      string `json:"name"`
		ProxyURL  string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	if req.SessionID == "" || req.Code == "" || req.State == "" {
		writeError(c, http.StatusBadRequest, "session_id、code 和 state 均为必填")
		return
	}

	sess, ok := globalOAuthStore.get(req.SessionID)
	if !ok {
		writeError(c, http.StatusBadRequest, "OAuth 会话不存在或已过期（有效期 30 分钟）")
		return
	}
	if req.State != sess.State {
		writeError(c, http.StatusBadRequest, "state 不匹配，请重新发起授权")
		return
	}

	proxyURL := sess.ProxyURL
	if trimmed := strings.TrimSpace(req.ProxyURL); trimmed != "" {
		proxyURL = trimmed
	}

	// Resin 临时身份用于 OAuth 兑换（新账号尚无 DBID）
	resinTempID := "oauth-" + req.SessionID
	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), req.Code, sess.CodeVerifier, sess.RedirectURI, proxyURL, resinTempID)
	if err != nil {
		writeError(c, http.StatusBadGateway, "授权码兑换失败: "+err.Error())
		return
	}
	globalOAuthStore.delete(req.SessionID)

	if tokenResp.RefreshToken == "" {
		writeError(c, http.StatusBadGateway, "授权服务器未返回 refresh_token，请确认已开启 offline_access scope")
		return
	}
	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken: tokenResp.RefreshToken,
		accessToken:  tokenResp.AccessToken,
		idToken:      tokenResp.IDToken,
		expiresIn:    tokenResp.ExpiresIn,
	})

	name := strings.TrimSpace(req.Name)
	if name == "" && seed.email != "" {
		name = seed.email
	}
	if name == "" {
		name = "oauth-account"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	id, err := h.db.InsertAccount(ctx, name, tokenResp.RefreshToken, proxyURL)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "账号写入数据库失败: "+err.Error())
		return
	}
	if err := h.db.UpdateCredentials(ctx, id, tokenCredentialMap(seed)); err != nil {
		writeError(c, http.StatusInternalServerError, "Token 写入数据库失败: "+err.Error())
		return
	}
	h.db.InsertAccountEventAsync(id, "added", "oauth")

	// Resin 租约继承
	if proxy.IsResinEnabled() {
		go proxy.InheritLease(resinTempID, fmt.Sprintf("%d", id))
	}

	newAcc := accountFromCredentialSeed(id, proxyURL, seed)
	h.store.AddAccount(newAcc)

	email := ""
	planType := ""
	if accountInfo != nil {
		email = accountInfo.Email
		planType = accountInfo.PlanType
	}
	if email == "" {
		email = seed.email
	}
	if planType == "" {
		planType = seed.planType
	}

	c.JSON(http.StatusOK, gin.H{
		"message":   fmt.Sprintf("OAuth 账号 %s 添加成功", name),
		"id":        id,
		"email":     email,
		"plan_type": planType,
	})
}

// ==================== 内部 HTTP 调用 ====================

type rawOAuthTokenResp struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func doOAuthCodeExchange(ctx context.Context, code, codeVerifier, redirectURI, proxyURL string, resinTempID ...string) (*rawOAuthTokenResp, *auth.AccountInfo, error) {
	form := neturl.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", oauthClientID)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)

	// Resin 反代模式：改写 URL
	targetURL := oauthTokenURL
	tempID := ""
	if len(resinTempID) > 0 {
		tempID = resinTempID[0]
	}
	if proxy.IsResinEnabled() && tempID != "" {
		targetURL = proxy.BuildReverseProxyURL(oauthTokenURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, nil, fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-cli/0.91.0")

	// Resin 反代：注入临时账号身份头
	if proxy.IsResinEnabled() && tempID != "" {
		req.Header.Set("X-Resin-Account", tempID)
	}

	var client *http.Client
	if proxy.IsResinEnabled() && tempID != "" {
		client = &http.Client{Timeout: 30 * time.Second}
	} else {
		client = auth.BuildHTTPClient(proxyURL)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("请求失败: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("token 兑换失败 (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp rawOAuthTokenResp
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return nil, nil, fmt.Errorf("解析响应失败: %w", err)
	}
	if strings.TrimSpace(tokenResp.AccessToken) == "" {
		return nil, nil, fmt.Errorf("token 兑换响应缺少 access_token")
	}

	info := accountInfoFromTokens(tokenResp.IDToken, tokenResp.AccessToken)
	return &tokenResp, info, nil
}

// ==================== OAuth 自动回调捕获 ====================

// OAuthCallback 接收 OpenAI OAuth 回调，自动完成 code exchange 并添加账号
// GET /auth/callback?code=xxx&state=xxx
func (h *Handler) OAuthCallback(c *gin.Context) {
	code := c.Query("code")
	state := c.Query("state")

	if code == "" || state == "" {
		c.HTML(http.StatusBadRequest, "", nil)
		c.String(http.StatusBadRequest, oauthCallbackPage("授权失败", "缺少 code 或 state 参数", false))
		return
	}

	sessionID, sess, ok := globalOAuthStore.findByState(state)
	if !ok {
		c.String(http.StatusBadRequest, oauthCallbackPage("授权失败", "OAuth 会话不存在或已过期，请重新发起授权", false))
		return
	}

	// 记录回调信息
	sess.CallbackCode = code
	sess.CallbackState = state
	sess.CallbackAt = time.Now()

	// 执行 code exchange（Resin 临时身份）
	resinTempID := "oauth-" + sessionID
	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), code, sess.CodeVerifier, sess.RedirectURI, sess.ProxyURL, resinTempID)
	if err != nil {
		sess.ExchangeResult = &oauthExchangeResult{
			Success: false,
			Error:   err.Error(),
		}
		c.String(http.StatusOK, oauthCallbackPage("授权失败", "兑换 token 失败: "+err.Error(), false))
		return
	}

	if tokenResp.RefreshToken == "" {
		sess.ExchangeResult = &oauthExchangeResult{
			Success: false,
			Error:   "授权服务器未返回 refresh_token",
		}
		c.String(http.StatusOK, oauthCallbackPage("授权失败", "未获取到 refresh_token，请确认已开启 offline_access", false))
		return
	}
	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken: tokenResp.RefreshToken,
		accessToken:  tokenResp.AccessToken,
		idToken:      tokenResp.IDToken,
		expiresIn:    tokenResp.ExpiresIn,
	})

	// 自动添加账号
	name := ""
	if seed.email != "" {
		name = seed.email
	}
	if name == "" {
		name = "oauth-account"
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	id, err := h.db.InsertAccount(ctx, name, tokenResp.RefreshToken, sess.ProxyURL)
	if err != nil {
		sess.ExchangeResult = &oauthExchangeResult{
			Success: false,
			Error:   "账号写入数据库失败: " + err.Error(),
		}
		c.String(http.StatusOK, oauthCallbackPage("授权失败", "写入数据库失败: "+err.Error(), false))
		return
	}
	if err := h.db.UpdateCredentials(ctx, id, tokenCredentialMap(seed)); err != nil {
		sess.ExchangeResult = &oauthExchangeResult{
			Success: false,
			Error:   "Token 写入数据库失败: " + err.Error(),
		}
		c.String(http.StatusOK, oauthCallbackPage("授权失败", "写入 token 失败: "+err.Error(), false))
		return
	}
	h.db.InsertAccountEventAsync(id, "added", "oauth_callback")

	// Resin 租约继承：将临时身份的 IP 租约迁移到正式 DBID
	if proxy.IsResinEnabled() {
		go proxy.InheritLease(resinTempID, fmt.Sprintf("%d", id))
	}

	newAcc := accountFromCredentialSeed(id, sess.ProxyURL, seed)
	h.store.AddAccount(newAcc)

	email := ""
	planType := ""
	if accountInfo != nil {
		email = accountInfo.Email
		planType = accountInfo.PlanType
	}
	if email == "" {
		email = seed.email
	}
	if planType == "" {
		planType = seed.planType
	}

	sess.ExchangeResult = &oauthExchangeResult{
		Success:  true,
		Message:  fmt.Sprintf("账号 %s 添加成功", name),
		ID:       id,
		Email:    email,
		PlanType: planType,
	}

	log.Printf("OAuth 回调自动添加账号成功: id=%d email=%s", id, email)
	c.String(http.StatusOK, oauthCallbackPage("授权成功", fmt.Sprintf("账号 %s 已自动添加，可以关闭此页面。", name), true))
}

// PollOAuthCallback 前端轮询回调结果
// GET /api/admin/oauth/poll-callback?session_id=xxx
func (h *Handler) PollOAuthCallback(c *gin.Context) {
	sessionID := c.Query("session_id")
	if sessionID == "" {
		writeError(c, http.StatusBadRequest, "session_id 为必填")
		return
	}

	sess, ok := globalOAuthStore.get(sessionID)
	if !ok {
		writeError(c, http.StatusNotFound, "OAuth 会话不存��或��过期")
		return
	}

	if sess.ExchangeResult != nil {
		// 回调已完成，返回结果并清理 session
		c.JSON(http.StatusOK, gin.H{
			"status": "completed",
			"result": sess.ExchangeResult,
		})
		globalOAuthStore.delete(sessionID)
		return
	}

	if sess.CallbackCode != "" {
		// 收到回调但尚未完成兑换（罕见竞态）
		c.JSON(http.StatusOK, gin.H{
			"status": "processing",
		})
		return
	}

	// 尚未收到回调
	c.JSON(http.StatusOK, gin.H{
		"status": "waiting",
	})
}

// oauthCallbackPage 生成简单的 HTML 回调结果页面
func oauthCallbackPage(title, message string, success bool) string {
	color := "#e53e3e"
	icon := "&#10060;"
	if success {
		color = "#38a169"
		icon = "&#10004;"
	}
	return fmt.Sprintf(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>%s</title>
<style>
body{font-family:-apple-system,sans-serif;display:flex;justify-content:center;align-items:center;min-height:100vh;margin:0;background:#f7fafc}
.card{background:#fff;border-radius:12px;padding:40px;box-shadow:0 4px 20px rgba(0,0,0,.08);text-align:center;max-width:420px}
.icon{font-size:48px;margin-bottom:16px}
h1{color:%s;font-size:24px;margin:0 0 12px}
p{color:#4a5568;line-height:1.6;margin:0}
</style></head>
<body><div class="card"><div class="icon">%s</div><h1>%s</h1><p>%s</p></div></body></html>`,
		title, color, icon, title, message)
}
