package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
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
	if proxyURL == "" {
		proxyURL = h.store.GetProxyURL()
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

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	name := strings.TrimSpace(req.Name)
	if name == "" && seed.email != "" {
		name = seed.email
	}
	if name == "" {
		name = "oauth-account"
	}

	id, updated, err := h.upsertOAuthIdentityAccount(ctx, name, proxyURL, seed, "oauth")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "账号写入数据库失败: "+err.Error())
		return
	}
	if proxy.IsResinEnabled() && !updated {
		go proxy.InheritLease(resinTempID, fmt.Sprintf("%d", id))
	}

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

	message := fmt.Sprintf("OAuth 账号 %s 添加成功", name)
	if updated {
		message = fmt.Sprintf("OAuth 账号已存在，已更新账号 %d", id)
	}
	c.JSON(http.StatusOK, gin.H{
		"message":   message,
		"id":        id,
		"email":     email,
		"plan_type": planType,
		"updated":   updated,
	})
}

var errDuplicateOAuthIdentity = errors.New("duplicate oauth identity")

func oauthIdentityDuplicateMessage(id int64) string {
	return fmt.Sprintf("OAuth 账号已存在 (id=%d)，请更新已有账号", id)
}

func (h *Handler) findOAuthIdentityDuplicate(ctx context.Context, seed tokenCredentialSeed, excludeID int64) (int64, error) {
	if h == nil || h.db == nil {
		return 0, nil
	}
	seed = normalizeTokenCredentialSeed(seed)
	// 身份键优先用工作区 ID；个人账号 JWT 可能只有 user_id，此时用它兜底，
	// 否则 AT 轮换后按原文去重永远失配，同一账号会被重复导入。
	identity := seed.accountID
	if identity == "" {
		identity = seed.userID
	}
	if seed.email == "" || identity == "" {
		return 0, nil
	}
	id, err := h.db.FindActiveAccountByOAuthIdentity(ctx, seed.email, identity, excludeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}

func (h *Handler) upsertOAuthIdentityAccount(ctx context.Context, name, proxyURL string, seed tokenCredentialSeed, source string) (int64, bool, error) {
	seed = normalizeTokenCredentialSeed(seed)
	if seed.email == "" || (seed.accountID == "" && seed.userID == "") {
		id, err := h.db.InsertAccountWithCredentials(ctx, name, tokenCredentialMap(seed), proxyURL)
		if err != nil {
			return 0, false, err
		}
		h.db.InsertAccountEventAsync(id, "added", source)
		h.loadInsertedTokenAccount(id, proxyURL, seed, source)
		return id, false, nil
	}

	if duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, 0); err != nil {
		return 0, false, err
	} else if duplicateID > 0 {
		row, err := h.db.GetAccountByID(ctx, duplicateID)
		if err != nil {
			return 0, false, err
		}
		effectiveProxyURL := strings.TrimSpace(proxyURL)
		if effectiveProxyURL == "" {
			effectiveProxyURL = strings.TrimSpace(row.ProxyURL)
		}
		if err := h.db.UpdateOAuthAccountCredentials(ctx, duplicateID, tokenCredentialMap(seed), effectiveProxyURL); err != nil {
			return 0, false, err
		}
		// 重新导入有效凭证时，若该账号此前处于错误/封禁（401）态，清除错误状态，
		// 让重新加载后的运行时账号脱离 banned，并交由后续 probe 重新判定。
		// 仅针对 error / unauthorized，避免误清合法的限速冷却（rate_limited）。
		if accountErrorStateNeedsReset(row) {
			if err := h.db.ClearError(ctx, duplicateID); err != nil {
				log.Printf("重新导入清除账号 %d 错误状态失败: %v", duplicateID, err)
			}
		}
		if err := h.reloadTokenAccount(ctx, duplicateID, source); err != nil {
			return 0, false, err
		}
		h.db.InsertAccountEventAsync(duplicateID, "updated", source)
		return duplicateID, true, nil
	}

	id, err := h.db.InsertAccountWithCredentials(ctx, name, tokenCredentialMap(seed), proxyURL)
	if err != nil {
		return 0, false, err
	}
	h.db.InsertAccountEventAsync(id, "added", source)
	h.loadInsertedTokenAccount(id, proxyURL, seed, source)
	return id, false, nil
}

// accountErrorStateNeedsReset 判断一个已存在账号是否处于"重新导入有效凭证后应清除"的
// 错误/封禁态：status='error'（RT 失效、鉴权失败等）或 401 unauthorized 冷却。
// 限速冷却（rate_limited*）不在此列，避免重新导入误清合法的限速窗口。
func accountErrorStateNeedsReset(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(row.Status), "error") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(row.CooldownReason), "unauthorized")
}

func (h *Handler) loadInsertedTokenAccount(id int64, proxyURL string, seed tokenCredentialSeed, source string) {
	if h == nil || h.store == nil {
		return
	}
	newAcc := accountFromCredentialSeed(id, proxyURL, seed)
	h.store.AddAccount(newAcc)
	h.triggerTokenAccountProbe(id, source)
}

func (h *Handler) reloadTokenAccount(ctx context.Context, id int64, source string) error {
	if h == nil || h.store == nil {
		return nil
	}
	h.store.RemoveAccount(id)
	if err := h.store.LoadAccountByID(ctx, id); err != nil {
		log.Printf("更新账号 %d 后重新加载运行时失败: %v", id, err)
		return err
	}
	h.triggerTokenAccountProbe(id, source)
	return nil
}

func (h *Handler) triggerTokenAccountProbe(id int64, source string) {
	if h == nil || h.store == nil {
		return
	}
	if account := h.store.FindByID(id); account != nil && account.GetAccessToken() != "" {
		h.triggerImportedAccountUsageProbe(id, source)
	} else if !h.store.GetLazyMode() && !strings.HasPrefix(source, "import") {
		go h.refreshImportedAccountAndProbe(id, source+"_refresh")
	}
}

// UpdateOAuthAccountCode 用授权码更新已有 OAuth 账号的授权参数。
// POST /api/admin/accounts/:id/oauth/exchange-code
func (h *Handler) UpdateOAuthAccountCode(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
		ProxyURL  string `json:"proxy_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Code = strings.TrimSpace(req.Code)
	req.State = strings.TrimSpace(req.State)
	req.ProxyURL = strings.TrimSpace(req.ProxyURL)
	if req.SessionID == "" || req.Code == "" || req.State == "" {
		writeError(c, http.StatusBadRequest, "session_id、code 和 state 均为必填")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.Type), "oauth") {
		writeError(c, http.StatusBadRequest, "当前账号不是 OAuth 授权类型，不能重新授权")
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
	if req.ProxyURL != "" {
		proxyURL = req.ProxyURL
	}
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(row.ProxyURL)
	}
	if proxyURL == "" && h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}

	resinAccountID := fmt.Sprintf("%d", id)
	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), req.Code, sess.CodeVerifier, sess.RedirectURI, proxyURL, resinAccountID)
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
	if duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, id); err != nil {
		writeInternalError(c, err)
		return
	} else if duplicateID > 0 {
		writeError(c, http.StatusConflict, oauthIdentityDuplicateMessage(duplicateID))
		return
	}
	if err := h.db.UpdateOAuthAccountCredentials(ctx, id, tokenCredentialMap(seed), proxyURL); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeError(c, http.StatusInternalServerError, "Token 写入数据库失败: "+err.Error())
		return
	}

	if err := h.reloadTokenAccount(ctx, id, "oauth_reauth"); err != nil {
		writeError(c, http.StatusInternalServerError, "重新加载运行时账号失败: "+err.Error())
		return
	}
	h.db.InsertAccountEventAsync(id, "updated", "oauth_reauth")

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
		"message":   "OAuth 账号授权参数更新成功",
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
		return nil, nil, fmt.Errorf("token 兑换失败 (HTTP %d)", resp.StatusCode)
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
	proxyURL := sess.ProxyURL
	if proxyURL == "" {
		proxyURL = h.store.GetProxyURL()
	}
	resinTempID := "oauth-" + sessionID
	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), code, sess.CodeVerifier, sess.RedirectURI, proxyURL, resinTempID)
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

	id, updated, err := h.upsertOAuthIdentityAccount(ctx, name, proxyURL, seed, "oauth_callback")
	if err != nil {
		sess.ExchangeResult = &oauthExchangeResult{
			Success: false,
			Error:   "账号写入数据库失败: " + err.Error(),
		}
		c.String(http.StatusOK, oauthCallbackPage("授权失败", "写入数据库失败: "+err.Error(), false))
		return
	}

	if proxy.IsResinEnabled() && !updated {
		go proxy.InheritLease(resinTempID, fmt.Sprintf("%d", id))
	}

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

	message := fmt.Sprintf("账号 %s 添加成功", name)
	pageMessage := fmt.Sprintf("账号 %s 已自动添加，可以关闭此页面。", name)
	if updated {
		message = fmt.Sprintf("账号已存在，已更新账号 %d", id)
		pageMessage = fmt.Sprintf("账号已存在，已更新账号 %d，可以关闭此页面。", id)
	}
	sess.ExchangeResult = &oauthExchangeResult{
		Success:  true,
		Message:  message,
		ID:       id,
		Email:    email,
		PlanType: planType,
	}

	log.Printf("OAuth 回调自动添加账号成功: id=%d email=%s", id, email)
	c.String(http.StatusOK, oauthCallbackPage("授权成功", pageMessage, true))
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
